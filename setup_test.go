package main

import (
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

	// INVARIANT: In runner mode, all four namespaced shims are installed even if only one
	// wrapper is detected on PATH.
	for _, name := range []string{"ac-claude", "ac-codex", "ac-gemini", "ac-grok"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); err != nil {
			t.Fatalf("%s shim not installed: %v", name, err)
		}
	}
	for _, name := range []string{"claude", "claude-code", "codex", "gemini", "gemini-cli", "grok"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); !os.IsNotExist(err) {
			t.Fatalf("%s same-name alias should not be installed by default: %v", name, err)
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
		{in: "runner", want: "runner"},
		{in: "tmux", want: "tmux"},
		{in: "herdr", want: "herdr"},
		{in: "runner,tmux", want: "runner,tmux"},
		{in: "tmux,runner", want: "runner,tmux"},   // canonical order
		{in: "herdr,runner", want: "runner,herdr"}, // canonical order
		{in: "runner,tmux,herdr", want: "runner,tmux,herdr"},
		{in: "RUNNER, TMUX", want: "runner,tmux"},       // case + spaces
		{in: "runner,runner,tmux", want: "runner,tmux"}, // dedup
		{in: "all", want: "runner,tmux,herdr"},          // keyword
		{in: "both", want: "runner,tmux,herdr", deprecate: true},
		{in: "", wantErr: true},
		{in: ",", wantErr: true},
		{in: "runner,", wantErr: true},      // trailing comma → empty token
		{in: "runner,,tmux", wantErr: true}, // doubled comma → empty token
		{in: "tmux, ,herdr", wantErr: true}, // whitespace-only token
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
// shims). Read-time canonicalization must expand it so previousSetupShimWrappers
// reports the full shim set the old install actually wrote — not just the
// detected wrapper list — otherwise a narrowing re-setup orphans shims.
func TestPersistedBothWakeCanonicalizedForShimCleanup(t *testing.T) {
	if got := canonicalizePersistedWake("both"); got != "runner,tmux,herdr" {
		t.Fatalf(`canonicalizePersistedWake("both") = %q, want "runner,tmux,herdr"`, got)
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
	if pool.Wake != "runner,tmux,herdr" {
		t.Fatalf("readSetupPoolState legacy both Wake = %q, want canonical runner,tmux,herdr", pool.Wake)
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
	if global.Wake != "runner,tmux,herdr" {
		t.Fatalf("readSetupGlobalState legacy both Wake = %q, want canonical runner,tmux,herdr", global.Wake)
	}
}

func TestSetupHelpAndInvalidWakeMentionHerdr(t *testing.T) {
	help := setupHelp()
	for _, want := range []string{
		"--wake runner[,tmux][,herdr] | all",
		"any comma-separated combination of runner, tmux, herdr",
		"hookless wrappers when only tmux/herdr are selected",
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
		if !strings.Contains(err.Error(), "runner, tmux, herdr") {
			t.Fatalf("invalid wake error should mention herdr, got %v", err)
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

func TestSetupTmuxDoesNotInstallShims(t *testing.T) {
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
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(root, "AGENTCHUTE.md")); err != nil {
		t.Fatalf("AGENTCHUTE.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin")); !os.IsNotExist(err) {
		t.Fatalf("shim dir should not exist for tmux-only setup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("profile should not be written for tmux-only setup: %v", err)
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
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
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
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
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
	if !strings.Contains(text, "agentchute-enrollment v14 begin") || !strings.Contains(text, "AGENTCHUTE_AGENT_ID") {
		t.Fatalf("setup did not refresh CODEX.md to v14 env identity guidance:\n%s", text)
	}
	if !strings.Contains(text, "Local notes.") {
		t.Fatalf("setup lost non-enrollment content:\n%s", text)
	}
}

func TestSetupModeSwitchToTmuxRemovesPriorSetupShimsAndProfileBlock(t *testing.T) {
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
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "all", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", filepath.Join(home, ".agentchute", "bin")+string(os.PathListSeparator)+realDir)
		if err := cmdSetup([]string{"--wake", "runner", "--wrappers", "all", "--profile", profile, "--yes"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", "ac-codex")); !os.IsNotExist(err) {
		t.Fatalf("ac-codex shim should be removed on tmux switch: %v", err)
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), setupPathBlockBegin) {
		t.Fatalf("profile block should be removed on tmux switch:\n%s", data)
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

	if _, err := os.Stat(filepath.Join(shimDir, "ac-codex")); err != nil {
		t.Fatalf("ac-codex shim should be installed: %v", err)
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
		err := cmdSetup([]string{"--wake", "tmux", "--wrappers", "none", "--yes"})
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
	if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", "ac-codex")); err != nil {
		t.Fatalf("ac-codex shim should remain: %v", err)
	}
	for _, name := range []string{"ac-gemini", "ac-grok", "ac-claude"} {
		if _, err := os.Stat(filepath.Join(home, ".agentchute", "bin", name)); err != nil {
			t.Fatalf("%s shim should remain (INVARIANT: all four shims in runner mode): %v", name, err)
		}
	}
}
