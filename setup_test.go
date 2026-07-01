package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestSetupRunnerInstallsAllFourShimsRegardlessOfDetection(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	realCodex := filepath.Join(realDir, "codex")
	mustWrite(t, realCodex, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(realCodex, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	withCwd(t, root, func() {
		for i := 0; i < 2; i++ {
			if err := cmdSetup([]string{
				"--wake", "runner",
				"--wrappers", "all",
				"--profile", profile,
				"--yes",
			}); err != nil {
				t.Fatalf("cmdSetup run %d: %v", i+1, err)
			}
		}
	})

	if _, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md")); err != nil {
		t.Fatalf("AGENTCHUTE.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".codex", "hooks.json")); err != nil {
		t.Fatalf("codex hooks not installed: %v", err)
	}

	// INVARIANT (v0.8.8): runner mode installs the single wrapper-agnostic `ac`
	// dispatcher (no per-wrapper ac-* launchers, no same-name aliases).
	acPath := filepath.Join(home, ".agentchute", "bin", "ac")
	acData, err := os.ReadFile(acPath)
	if err != nil {
		t.Fatalf("ac dispatcher not installed: %v", err)
	}
	if !strings.Contains(string(acData), "dispatch --shim-dir") {
		t.Fatalf("ac dispatcher does not exec `agentchute dispatch`:\n%s", acData)
	}
	for _, name := range []string{"ac-claude", "ac-codex", "ac-gemini", "ac-grok", "claude", "claude-code", "codex", "gemini", "gemini-cli", "grok"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); !os.IsNotExist(err) {
			t.Fatalf("legacy launcher/alias %s should not be installed: %v", name, err)
		}
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data), setupPathBlockBegin); count != 1 {
		t.Fatalf("profile block count = %d, want 1\n%s", count, data)
	}
}

func TestNormalizeSetupWakeCombinations(t *testing.T) {
	cases := []struct {
		in        string
		want      string
		deprecate bool
		wantErr   bool
	}{
		// Pull-only redesign: runner is the only installable wake path. tmux/herdr
		// (alone or in a combo) are rejected; all/both are deprecated aliases that
		// now resolve to runner only.
		{in: "runner", want: "runner"},
		{in: "RUNNER", want: "runner"}, // case-insensitive
		{in: " runner ", want: "runner"},
		{in: "runner,runner", want: "runner"},        // dedup
		{in: "all", want: "runner", deprecate: true}, // legacy keyword → runner
		{in: "both", want: "runner", deprecate: true},
		{in: "tmux", wantErr: true},        // adapter removed
		{in: "herdr", wantErr: true},       // adapter removed
		{in: "runner,tmux", wantErr: true}, // combo with removed adapter
		{in: "tmux,runner", wantErr: true},
		{in: "runner,tmux,herdr", wantErr: true},
		{in: "", wantErr: true},
		{in: ",", wantErr: true},
		{in: "runner,", wantErr: true},    // trailing comma → empty token
		{in: "runner,,", wantErr: true},   // doubled comma → empty token
		{in: "all, ,both", wantErr: true}, // whitespace-only token
		{in: "bogus", wantErr: true},
		{in: "runner,bogus", wantErr: true},
	}
	for _, tc := range cases {
		got, deprecation, err := normalizeSetupWake(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeSetupWake(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeSetupWake(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeSetupWake(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if (deprecation != "") != tc.deprecate {
			t.Errorf("normalizeSetupWake(%q) deprecation = %q, want deprecate=%v", tc.in, deprecation, tc.deprecate)
		}
	}
}

func TestRemoveInstallShPathBlocks(t *testing.T) {
	legacyZsh := `export FOO=bar

# agentchute PATH entry for binary ($HOME/.local/bin) begin
case "$PATH" in
  "$HOME/.local/bin:"*) ;;
  *) export PATH="$HOME/.local/bin:$PATH" ;;
esac
# agentchute PATH entry for binary ($HOME/.local/bin) end

alias ll='ls -la'
`
	out, changed := removeInstallShPathBlocks(legacyZsh)
	if !changed {
		t.Fatalf("expected legacy block to be removed")
	}
	if strings.Contains(out, installShPathMarkerPrefix) {
		t.Fatalf("install.sh marker survived removal:\n%s", out)
	}
	if !strings.Contains(out, "export FOO=bar") || !strings.Contains(out, "alias ll='ls -la'") {
		t.Fatalf("surrounding content lost:\n%s", out)
	}

	// fish-style legacy region plus a second region (binary + launcher shims).
	legacyFish := `# agentchute PATH entry for binary ($HOME/.local/bin) begin
if test "$PATH[1]" != $HOME/.local/bin
    set -gx PATH $HOME/.local/bin $PATH
end
# agentchute PATH entry for binary ($HOME/.local/bin) end
# agentchute PATH entry for launcher shims ($HOME/.agentchute/bin) begin
if test "$PATH[1]" != $HOME/.agentchute/bin
    set -gx PATH $HOME/.agentchute/bin $PATH
end
# agentchute PATH entry for launcher shims ($HOME/.agentchute/bin) end
`
	out, changed = removeInstallShPathBlocks(legacyFish)
	if !changed {
		t.Fatalf("expected fish legacy blocks removed")
	}
	if strings.Contains(out, installShPathMarkerPrefix) {
		t.Fatalf("install.sh markers survived removal:\n%q", out)
	}

	// No legacy region → no change.
	clean := "export PATH=\"/usr/bin:$PATH\"\n"
	if out, changed = removeInstallShPathBlocks(clean); changed || out != clean {
		t.Fatalf("clean profile mutated: changed=%v out=%q", changed, out)
	}
}

// replaceSetupBlock must collapse a pre-existing install.sh region into the
// single setup-managed region (one managed PATH block, not two).
func TestReplaceSetupBlockSupersedesInstallShRegion(t *testing.T) {
	existing := `# agentchute PATH entry for binary ($HOME/.local/bin) begin
case "$PATH" in
  "$HOME/.local/bin:"*) ;;
  *) export PATH="$HOME/.local/bin:$PATH" ;;
esac
# agentchute PATH entry for binary ($HOME/.local/bin) end
`
	block := setupRenderPathBlock("/home/u/.zshrc", "/home/u/.agentchute/bin")
	out := replaceSetupBlock(existing, block)
	if strings.Contains(out, installShPathMarkerPrefix) {
		t.Fatalf("install.sh region not superseded:\n%s", out)
	}
	if n := strings.Count(out, setupPathBlockBegin); n != 1 {
		t.Fatalf("setup block count = %d, want 1:\n%s", n, out)
	}
}

func TestSetupShimWrappersForWakeCombinations(t *testing.T) {
	wrappers := []string{"claude-code", "codex", "gemini-cli", "grok"}
	// runner anywhere in the set installs all four shims.
	for _, wake := range []string{"runner", "runner,tmux", "runner,tmux,herdr"} {
		if got := setupShimWrappers(wake, wrappers); len(got) != 4 {
			t.Errorf("setupShimWrappers(%q) = %v, want all 4", wake, got)
		}
		if !setupNeedsShims(wake) {
			t.Errorf("setupNeedsShims(%q) = false, want true", wake)
		}
	}
	// tmux/herdr without runner installs hookless shims only (grok).
	for _, wake := range []string{"tmux", "herdr", "tmux,herdr"} {
		got := setupShimWrappers(wake, wrappers)
		if len(got) != 1 || got[0] != "grok" {
			t.Errorf("setupShimWrappers(%q) = %v, want [grok]", wake, got)
		}
		if setupNeedsShims(wake) {
			t.Errorf("setupNeedsShims(%q) = true, want false", wake)
		}
	}
}

// Legacy installs persisted Wake:"both" (a runner-equivalent that installed all
// shims). Read-time canonicalization must normalize it to a runner-equivalent so
// previousSetupShimWrappers reports the full shim set the old install actually
// wrote — not just the detected wrapper list — otherwise a re-setup orphans
// shims. Post pull-only redesign "both" canonicalizes to "runner" (which still
// resolves to all four shims because runner installs them all).
func TestPersistedBothWakeCanonicalizedForShimCleanup(t *testing.T) {
	if got := canonicalizePersistedWake("both"); got != "runner" {
		t.Fatalf(`canonicalizePersistedWake("both") = %q, want "runner"`, got)
	}
	canon := setupGlobalState{Wake: canonicalizePersistedWake("both"), Wrappers: []string{"codex"}, ShimsInstalled: true}
	if got := previousSetupShimWrappers(canon); len(got) != 4 {
		t.Fatalf("previousSetupShimWrappers(canonicalized both) = %v, want all 4 setup wrappers", got)
	}
	// Document the bug canonicalization prevents: raw "both" falls back to the
	// detected wrapper list (1), so the other 3 prior shims would be orphaned.
	raw := setupGlobalState{Wake: "both", Wrappers: []string{"codex"}, ShimsInstalled: true}
	if got := previousSetupShimWrappers(raw); len(got) == 4 {
		t.Fatalf("raw \"both\" unexpectedly resolved all 4 (%v); canonicalization would be redundant", got)
	}
	// Invalid/empty persisted values pass through untouched.
	if got := canonicalizePersistedWake(""); got != "" {
		t.Fatalf(`canonicalizePersistedWake("") = %q, want ""`, got)
	}
}

// Pins the read-time canonicalization in BOTH state readers — not just the
// helper. If the `state.Wake = canonicalizePersistedWake(...)` assignments are
// removed, these reads return raw "both" and this fails.
func TestReadSetupStateCanonicalizesLegacyBothWake(t *testing.T) {
	// Pool state reader.
	root := t.TempDir()
	cfg := &loop.Config{ControlRepo: root, LoopDir: filepath.Join(root, "loop")}
	stateDir := filepath.Join(cfg.LoopDir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(stateDir, "setup.json"), []byte(`{"version":1,"wake":"both","wrappers":["codex"]}`))
	pool, err := readSetupPoolState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Wake != "runner" {
		t.Fatalf("readSetupPoolState legacy both Wake = %q, want canonical runner", pool.Wake)
	}

	// Global state reader (XDG path).
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	gpath := filepath.Join(home, ".config", "agentchute", "setup.json")
	if err := os.MkdirAll(filepath.Dir(gpath), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, gpath, []byte(`{"version":1,"wake":"both","wrappers":["codex"],"shims_installed":true}`))
	global, err := readSetupGlobalState()
	if err != nil {
		t.Fatal(err)
	}
	if global.Wake != "runner" {
		t.Fatalf("readSetupGlobalState legacy both Wake = %q, want canonical runner", global.Wake)
	}
}

// The v0.8.8 dispatcher_installed flag must survive a write/read round-trip and
// serialize under its documented JSON key.
func TestSetupGlobalStateDispatcherInstalledRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	in := setupGlobalState{
		Version:             1,
		Wake:                "runner",
		Wrappers:            []string{"codex"},
		ShimsInstalled:      true,
		DispatcherInstalled: true,
	}
	if err := writeSetupGlobalState(in); err != nil {
		t.Fatal(err)
	}
	got, err := readSetupGlobalState()
	if err != nil {
		t.Fatal(err)
	}
	if !got.DispatcherInstalled {
		t.Fatalf("DispatcherInstalled did not round-trip: %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "agentchute", "setup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"dispatcher_installed": true`) {
		t.Fatalf("setup.json missing dispatcher_installed key:\n%s", raw)
	}
}

func TestSetupHelpAndInvalidWakeRunnerOnly(t *testing.T) {
	help := setupHelp()
	for _, want := range []string{
		"--wake runner",
		"runner is the only supported wake path",
		"tmux/herdr wake adapters",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("setup help missing %q:\n%s", want, help)
		}
	}

	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	withCwd(t, root, func() {
		err := cmdSetup([]string{"--wake", "bogus", "--wrappers", "none", "--yes"})
		if err == nil {
			t.Fatal("expected invalid wake error")
		}
		if !strings.Contains(err.Error(), "the only supported wake path is runner") {
			t.Fatalf("invalid wake error should name runner as the only supported path, got %v", err)
		}
	})
}

func TestSetupHerdrPlanLabelsHooklessShim(t *testing.T) {
	var out strings.Builder
	printSetupPlan(&out, "/repo", setupOptions{
		Wake:      setupWakeHerdr,
		Wrappers:  "grok",
		ShimDir:   "/shim",
		NoProfile: true,
	}, []string{"grok"}, map[string]string{"grok": "/usr/bin/grok"})

	text := out.String()
	if !strings.Contains(text, "shim wrappers: grok (hookless startup enrollment)") {
		t.Fatalf("herdr plan should label hookless shim wrappers:\n%s", text)
	}
}

func TestSetupClearsExistingLiveRegistrations(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	agentsDir := filepath.Join(root, ".agentchute", "loop", "agents")
	mustMkdir(t, agentsDir)
	mustWrite(t, filepath.Join(agentsDir, "codex-agentchute.md"), []byte("---\nagent_id: codex-agentchute\nvendor: openai\ncontrol_repo: "+root+"\nhost: test\nlast_seen: 2026-01-01T00:00:00Z\nstatus: active\n---\n"))
	mustWrite(t, filepath.Join(agentsDir, "codex.example.md"), []byte("tracked example\n"))
	mustWrite(t, filepath.Join(agentsDir, "README.md"), []byte("format reference\n"))

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(agentsDir, "codex-agentchute.md")); !os.IsNotExist(err) {
		t.Fatalf("live registration should be cleared by setup: %v", err)
	}
	for _, keep := range []string{"codex.example.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(agentsDir, keep)); err != nil {
			t.Fatalf("%s should be preserved: %v", keep, err)
		}
	}
}

func TestSetupResetsRuntimeStateButPreservesPendingReplies(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	loopDir := filepath.Join(root, ".agentchute", "loop")
	agentsDir := filepath.Join(loopDir, "agents")
	mustMkdir(t, agentsDir)
	mustWrite(t, filepath.Join(agentsDir, "codex-agentchute.md"), []byte("---\nagent_id: codex-agentchute\nvendor: openai\ncontrol_repo: "+root+"\nhost: test\nlast_seen: 2026-01-01T00:00:00Z\nstatus: active\n---\n"))
	stateDir := filepath.Join(loopDir, "state", "codex-agentchute")
	mustWrite(t, filepath.Join(stateDir, "poller.json"), []byte(`{"agent_id":"codex-agentchute","method":"poller-run","host":"`+localHostname()+`","pid":111,"interval_seconds":30,"last_seen":"2026-01-01T00:00:00Z"}`+"\n"))
	mustWrite(t, filepath.Join(stateDir, "runner.json"), []byte(`{"agent_id":"codex-agentchute","runner_pid":222,"socket_path":"`+filepath.Join(stateDir, "runner.sock")+`","started_at":"2026-01-01T00:00:00Z","status":"active"}`+"\n"))
	mustWrite(t, filepath.Join(stateDir, "session.json"), []byte(`{"agent_id":"codex-agentchute","source":"self-check","host":"`+localHostname()+`","pid":333,"last_seen":"2026-01-01T00:00:00Z"}`+"\n"))
	mustWrite(t, filepath.Join(stateDir, "owed.json"), []byte(`{"owed":[]}`+"\n"))
	mustWrite(t, filepath.Join(stateDir, "poller.log"), []byte("keep log\n"))

	oldAlive, oldCommandLine, oldSignal := setupProcessAlive, setupProcessCommandLine, setupSignalProcess
	signaled := map[int]bool{}
	setupProcessAlive = func(pid int) bool {
		if signaled[pid] {
			return false
		}
		return pid == 111 || pid == 222
	}
	setupProcessCommandLine = func(pid int) string {
		switch pid {
		case 111:
			return filepath.Join(home, "agentchute") + " poller run --as codex-agentchute --control-repo " + root + " --loop-dir " + loopDir
		case 222:
			return filepath.Join(home, "agentchute") + " run --as codex-agentchute --control-repo " + root + " --loop-dir " + loopDir + " -- codex"
		default:
			return ""
		}
	}
	setupSignalProcess = func(pid int, sig os.Signal) error {
		signaled[pid] = true
		return nil
	}
	t.Cleanup(func() {
		setupProcessAlive = oldAlive
		setupProcessCommandLine = oldCommandLine
		setupSignalProcess = oldSignal
	})

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if !signaled[111] || !signaled[222] {
		t.Fatalf("setup did not signal poller and runner pids: %#v", signaled)
	}
	for _, removed := range []string{"poller.json", "runner.json", "session.json"} {
		if _, err := os.Stat(filepath.Join(stateDir, removed)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed by setup reset: %v", removed, err)
		}
	}
	for _, keep := range []string{"owed.json", "poller.log"} {
		if _, err := os.Stat(filepath.Join(stateDir, keep)); err != nil {
			t.Fatalf("%s should be preserved: %v", keep, err)
		}
	}
}

func TestSetupCommandAgentIDMatchIsBounded(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
	}
	cmdline := filepath.Join(root, "bin", "agentchute") + " poller run --as codex-agentchute-2 --control-repo " + root
	if setupCommandMatches(cmdline, "codex-agentchute", "poller run", cfg) {
		t.Fatal("setupCommandMatches matched codex-agentchute as a substring of codex-agentchute-2")
	}
	if !setupCommandMatches(cmdline, "codex-agentchute-2", "poller run", cfg) {
		t.Fatal("setupCommandMatches did not match the exact --as agent id")
	}
}

func TestSetupRefreshesExistingEnrollmentBlocks(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	stale := "<!-- agentchute-enrollment v10 begin -->\nstale identity instructions\n<!-- agentchute-enrollment v10 end -->\n\nLocal notes.\n"
	mustWrite(t, filepath.Join(root, "CODEX.md"), []byte(stale))

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	got, err := os.ReadFile(filepath.Join(root, "CODEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "stale identity instructions") {
		t.Fatalf("setup did not replace stale enrollment block:\n%s", text)
	}
	if !strings.Contains(text, "agentchute-enrollment v18 begin") || !strings.Contains(text, "AGENTCHUTE_AGENT_ID") {
		t.Fatalf("setup did not refresh CODEX.md to v18 env identity guidance:\n%s", text)
	}
	if !strings.Contains(text, "Local notes.") {
		t.Fatalf("setup lost non-enrollment content:\n%s", text)
	}
}

func TestSetupRemovesOwnedSameNameAliasesOnly(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	realCodex := filepath.Join(realDir, "codex")
	mustWrite(t, realCodex, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(realCodex, 0o755); err != nil {
		t.Fatal(err)
	}

	shimDir := filepath.Join(home, ".agentchute", "bin")
	mustMkdir(t, shimDir)
	mustWrite(t, filepath.Join(shimDir, "codex"), []byte(renderShimScript("/usr/local/bin/agentchute", shimDir, "codex")))
	mustWrite(t, filepath.Join(shimDir, "claude"), []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(filepath.Join(shimDir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(shimDir, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "all", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(shimDir, "ac")); err != nil {
		t.Fatalf("ac dispatcher should be installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shimDir, "codex")); !os.IsNotExist(err) {
		t.Fatalf("owned same-name codex alias should be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shimDir, "claude")); err != nil {
		t.Fatalf("non-agentchute claude file should be preserved: %v", err)
	}
}

func TestSetupRefusesNonProjectInitWithYes(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	withCwd(t, root, func() {
		err := cmdSetup([]string{"--wake", "runner", "--wrappers", "none", "--yes"})
		if err == nil {
			t.Fatal("expected non-project init guard")
		}
		if !strings.Contains(err.Error(), "refusing to initialize non-project directory") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if _, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md")); !os.IsNotExist(err) {
		t.Fatalf("AGENTCHUTE.md should not be written: %v", err)
	}
}

func TestSetupWrapperNarrowingRemovesDroppedHooksAndShims(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	for _, name := range []string{"codex", "gemini"} {
		path := filepath.Join(realDir, name)
		mustWrite(t, path, []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	withCwd(t, root, func() {
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "codex,gemini-cli", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "codex", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(root, ".codex", "hooks.json")); err != nil {
		t.Fatalf("codex hook should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("gemini hook should be removed: %v", err)
	}
	// The wrapper-agnostic `ac` dispatcher persists across a narrowing re-setup
	// (it routes every wrapper); no per-wrapper ac-* launchers are written.
	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", "ac")); err != nil {
		t.Fatalf("ac dispatcher should remain: %v", err)
	}
	for _, name := range []string{"ac-codex", "ac-gemini", "ac-grok", "ac-claude"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); !os.IsNotExist(err) {
			t.Fatalf("legacy launcher %s should not be installed: %v", name, err)
		}
	}
}

// ORDERING INVARIANT: the idempotent, recoverable writes (init/enrollment,
// hooks, shims, PATH block, saved setup state) must all land BEFORE the
// destructive runtime reset. We inject a failure into the reset seam and assert
// every wake-infrastructure artifact already exists — so a mid-setup failure can
// never leave the bus with cleared registrations AND no wake infrastructure. A
// pre-reorder build (reset first) leaves these unwritten when the reset fails and
// this test goes red.
func TestSetup_TemplatesWrittenBeforeRuntimeReset(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	home := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, realDir)
	realCodex := filepath.Join(realDir, "codex")
	mustWrite(t, realCodex, []byte("#!/bin/sh\nexit 0\n"))
	if err := os.Chmod(realCodex, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", realDir)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	profile := filepath.Join(home, ".zshrc")

	// Inject a failure into the DESTRUCTIVE reset phase. Record whether any
	// wake-infrastructure write had already happened by the time it ran.
	resetRan := false
	var hadEnrollment, hadHook, hadShim bool
	oldReset := setupRunRuntimeReset
	setupRunRuntimeReset = func(rt string, cfg *loop.Config, wrappers []string) error {
		resetRan = true
		_, e1 := os.Stat(filepath.Join(root, "CODEX.md"))
		hadEnrollment = e1 == nil
		_, e2 := os.Stat(filepath.Join(root, ".codex", "hooks.json"))
		hadHook = e2 == nil
		_, e3 := os.Stat(filepath.Join(home, ".agentchute", "bin", "ac"))
		hadShim = e3 == nil
		return errors.New("injected runtime-reset failure")
	}
	t.Cleanup(func() { setupRunRuntimeReset = oldReset })

	var setupErr error
	withCwd(t, root, func() {
		setupErr = cmdSetup([]string{"--wake", "runner", "--wrappers", "codex", "--profile", profile, "--yes"})
	})

	// The injected reset failure must surface (reset is still part of setup)...
	if setupErr == nil || !strings.Contains(setupErr.Error(), "injected runtime-reset failure") {
		t.Fatalf("injected reset failure should surface from setup; got %v", setupErr)
	}
	if !resetRan {
		t.Fatal("destructive reset seam was never invoked")
	}
	// ...but the recoverable writes were already on disk when it ran: wake
	// infrastructure stays intact and a re-run recovers cleanly.
	if !hadEnrollment {
		t.Error("enrollment block (CODEX.md) was NOT written before the destructive reset")
	}
	if !hadHook {
		t.Error("codex hook (.codex/hooks.json) was NOT written before the destructive reset")
	}
	if !hadShim {
		t.Error("ac dispatcher was NOT written before the destructive reset")
	}
	// And they are durable on disk after the failed setup returns.
	for _, p := range []string{
		filepath.Join(root, "CODEX.md"),
		filepath.Join(root, ".codex", "hooks.json"),
		filepath.Join(home, ".agentchute", "bin", "ac"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("wake-infra artifact missing after failed setup: %s (%v)", p, err)
		}
	}
}
