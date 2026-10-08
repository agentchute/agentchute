package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// ---------- H5: Guarded follows the binary ----------

func TestWrapperGuardedForBinary(t *testing.T) {
	gemini, _ := wrapperSpecForName("gemini")
	grok, _ := wrapperSpecForName("grok")
	codex, _ := wrapperSpecForName("codex")
	rows := []struct {
		spec   wrapperSpec
		binary string
		want   bool
		reason bool
	}{
		{gemini, "/usr/local/bin/gemini", true, false},
		{gemini, "gemini-cli", true, false},
		{gemini, "/Users/alex/.local/bin/agy", false, true},
		{gemini, "agy", false, true},
		{grok, "/Users/alex/.grok/bin/grok", false, false},
		{codex, "codex", true, false},
	}
	for _, row := range rows {
		got, reason := row.spec.guardedFor(row.binary)
		if got != row.want || (reason != "") != row.reason {
			t.Fatalf("%s + %s: guarded=%v reason=%q, want guarded=%v reason=%v", row.spec.Key, row.binary, got, reason, row.want, row.reason)
		}
		if reason != "" && (!strings.Contains(reason, "agy") || !strings.Contains(reason, "UNGUARDED")) {
			t.Fatalf("reason must name the binary and say unguarded: %q", reason)
		}
	}
}

// `ac serve gemini` resolving to agy: the child gets no AGENTCHUTE_GUARD,
// stderr says which binary and why, and the gemini template is NOT written.
func TestServeAgyLaunchesUnguardedWithoutGeminiTemplate(t *testing.T) {
	root := setupShortRunFixture(t)
	envPath := filepath.Join(root, "child-env.txt")
	wrapper := filepath.Join(root, "agy")
	mustWrite(t, wrapper, []byte("#!/bin/sh\nenv | grep -E '^(AGENTCHUTE_GUARD|AGENTCHUTE_AGENT_ID)=' > "+shellQuote(envPath)+"\n"))
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	var serveErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			serveErr = cmdServe([]string{"--as", "gemini-cli", "--control-repo", root, "--loop-dir", filepath.Join(root, ".agentchute", "loop"), "--interval", "5", "--idle-grace", "100ms", "--", wrapper})
		})
	})
	if serveErr != nil {
		t.Fatalf("serve: %v", serveErr)
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("wrapper did not run: %v", err)
	}
	if strings.Contains(string(got), "AGENTCHUTE_GUARD=") {
		t.Fatalf("agy child is armed:\n%s", got)
	}
	if !strings.Contains(string(got), "AGENTCHUTE_AGENT_ID=gemini-cli") {
		t.Fatalf("child env missing the id:\n%s", got)
	}
	if !strings.Contains(stderr, "resolved to agy") || !strings.Contains(stderr, "UNGUARDED") {
		t.Fatalf("stderr did not say which binary and why:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("gemini template written for agy: stat err = %v", err)
	}
}

// ---------- H3a: codex hook trust ----------

const trustFixtureHooks = `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"a"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"b"},{"type":"command","command":"c"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"d"}]}],"Stop":[{"hooks":[{"type":"command","command":"e"}]}]}}`

func writeCodexTrustFixture(t *testing.T, dir, hooksPath string, keys ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("model = \"x\"\n\n[hooks.state]\n\n")
	for _, k := range keys {
		b.WriteString("[hooks.state.\"" + hooksPath + ":" + k + "\"]\ntrusted_hash = \"sha256:0\"\n\n")
	}
	path := filepath.Join(dir, "config.toml")
	mustWrite(t, path, []byte(b.String()))
	return path
}

func TestCodexHookTrustMissing(t *testing.T) {
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, "hooks-under-test.json")
	mustWrite(t, hooksPath, []byte(trustFixtureHooks))
	all := []string{"pre_tool_use:0:0", "session_start:0:0", "stop:0:0", "user_prompt_submit:0:0", "user_prompt_submit:0:1"}

	cfg := writeCodexTrustFixture(t, dir, hooksPath, all...)
	if missing, err := codexHookTrustMissing(cfg, hooksPath); err != nil || len(missing) != 0 {
		t.Fatalf("all trusted: missing=%v err=%v", missing, err)
	}

	cfg = writeCodexTrustFixture(t, dir, hooksPath, "pre_tool_use:0:0", "session_start:0:0", "user_prompt_submit:0:0", "user_prompt_submit:0:1")
	missing, err := codexHookTrustMissing(cfg, hooksPath)
	if err != nil || strings.Join(missing, ",") != "stop:0:0" {
		t.Fatalf("stop untrusted: missing=%v err=%v", missing, err)
	}

	// Stale positions from an older template do not count for a key they
	// do not name; a different root's entries do not count either.
	cfg = writeCodexTrustFixture(t, dir, "/elsewhere/hooks.json", all...)
	missing, err = codexHookTrustMissing(cfg, hooksPath)
	if err != nil || len(missing) != len(all) {
		t.Fatalf("foreign root: missing=%v err=%v", missing, err)
	}

	if _, err := codexHookTrustMissing(filepath.Join(dir, "absent.toml"), hooksPath); err == nil {
		t.Fatal("unreadable config must be an error (treated as untrusted)")
	}
	if got := codexHookEventKey("UserPromptSubmit"); got != "user_prompt_submit" {
		t.Fatalf("event key = %q", got)
	}
}

// serve arms the guard for codex only when every hook position is trusted.
func TestServeCodexGuardFollowsHookTrust(t *testing.T) {
	for _, row := range []struct {
		name      string
		trustAll  bool
		wantGuard bool
	}{
		{"all trusted: guarded", true, true},
		{"one untrusted: unguarded with warning", false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := setupShortRunFixture(t)
			envPath := filepath.Join(root, "child-env.txt")
			wrapper := filepath.Join(root, "codex")
			mustWrite(t, wrapper, []byte("#!/bin/sh\ncase \"${1-}\" in --help) exit 0;; esac\nenv | grep -E '^AGENTCHUTE_GUARD=' > "+shellQuote(envPath)+"; true\n"))
			if err := os.Chmod(wrapper, 0o755); err != nil {
				t.Fatal(err)
			}
			// serve installs the real template first; its trust keys are
			// what codex would record for this root.
			installed := filepath.Join(root, ".codex", "hooks.json")
			keys := []string{"pre_tool_use:0:0", "session_start:0:0", "stop:0:0", "user_prompt_submit:0:0", "user_prompt_submit:0:1"}
			if !row.trustAll {
				keys = keys[1:]
			}
			codexHome := t.TempDir()
			writeCodexTrustFixture(t, codexHome, installed, keys...)
			t.Setenv("CODEX_HOME", codexHome)

			var serveErr error
			stderr := captureStderr(t, func() {
				withCwd(t, root, func() {
					serveErr = cmdServe([]string{"--as", "codex", "--control-repo", root, "--loop-dir", filepath.Join(root, ".agentchute", "loop"), "--interval", "5", "--idle-grace", "100ms", "--", wrapper})
				})
			})
			if serveErr != nil {
				t.Fatalf("serve: %v", serveErr)
			}
			got, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatalf("wrapper did not run: %v", err)
			}
			if armed := strings.Contains(string(got), "AGENTCHUTE_GUARD=1"); armed != row.wantGuard {
				t.Fatalf("guard armed=%v, want %v; env:\n%s\nstderr:\n%s", armed, row.wantGuard, got, stderr)
			}
			if !row.wantGuard && (!strings.Contains(stderr, "not trusted every agentchute hook") || !strings.Contains(stderr, "pre_tool_use:0:0")) {
				t.Fatalf("stderr did not name the untrusted position:\n%s", stderr)
			}
			if row.wantGuard && strings.Contains(stderr, "UNGUARDED") {
				t.Fatalf("spurious warning:\n%s", stderr)
			}
		})
	}
}

// The expected keys for the shipped codex template, by position: the test
// that fails if someone inserts a hook in the middle (see hookWrappers).
func TestCodexTemplateTrustPositions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shipped.json")
	data, err := fs.ReadFile(hooksFS, "examples/hooks/codex/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, data)
	keys, err := codexExpectedTrustKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range keys {
		keys[i] = strings.TrimPrefix(keys[i], path+":")
	}
	want := "pre_tool_use:0:0,session_start:0:0,stop:0:0,user_prompt_submit:0:0,user_prompt_submit:0:1"
	if got := strings.Join(keys, ","); got != want {
		t.Fatalf("template trust positions changed:\n got %s\nwant %s\n(append new hooks at the end of their group; existing positions are codex trust keys)", got, want)
	}
}

// ---------- H3c: apply_patch matches targets only ----------

func TestGuardApplyPatchMatchesTargetsOnly(t *testing.T) {
	const bodyMentionsAck = `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-old\n+run ` + "`agentchute ack`" + ` and then rm -rf nothing, curl nowhere\n*** End Patch\n"}}`
	const targetsHookFile = `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: .codex/hooks.json\n@@\n-a\n+b\n*** End Patch\n"}}`
	const movesOntoHookFile = `{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: notes.md\n*** Move to: .claude/settings.json\n@@\n-a\n+b\n*** End Patch\n"}}`
	const bashStillMatchesBody = `{"tool_name":"Bash","tool_input":{"command":"echo hi && agentchute ack"}}`

	if cmd := parseGuardToolCommand([]byte(bodyMentionsAck)); guardCommandDenied(cmd) {
		t.Fatalf("a patch whose diff body mentions deny words was denied; command text = %q", cmd)
	}
	if cmd := parseGuardToolCommand([]byte(targetsHookFile)); !guardCommandDenied(cmd) {
		t.Fatalf("a patch targeting a hook config file was allowed; command text = %q", cmd)
	}
	if cmd := parseGuardToolCommand([]byte(movesOntoHookFile)); !guardCommandDenied(cmd) {
		t.Fatalf("a patch moving onto a hook config file was allowed; command text = %q", cmd)
	}
	if cmd := parseGuardToolCommand([]byte(bashStillMatchesBody)); !guardCommandDenied(cmd) {
		t.Fatalf("Bash command text is still matched whole; command text = %q", cmd)
	}
	if got := guardApplyPatchTargets("*** Add File: a/b.md\n*** Delete File: c.txt\nbody *** Update File: not-a-header\n"); strings.Join(got, ",") != "a/b.md,c.txt" {
		t.Fatalf("targets = %v", got)
	}
}

// ---------- H4: grok is launched hookless ----------

func TestRunnerChildEnvDisablesGrokClaudeHookCompat(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "1") // an operator's shell default must not win
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}
	grok := runnerChildEnv(cfg, runnerOptions{AgentID: "grok", Vendor: "xai", WrapperArgs: []string{"/Users/alex/.grok/bin/grok"}}, "tok")
	if !has(grok, "GROK_CLAUDE_HOOKS_ENABLED=0") || has(grok, "GROK_CLAUDE_HOOKS_ENABLED=1") {
		t.Fatalf("grok child env does not disable Claude hook compat: %v", grok)
	}
	claude := runnerChildEnv(cfg, runnerOptions{AgentID: "claude-code", Vendor: "anthropic", Guarded: true, WrapperArgs: []string{"claude"}}, "tok")
	if has(claude, "GROK_CLAUDE_HOOKS_ENABLED=0") {
		t.Fatalf("non-grok child env carries the grok override: %v", claude)
	}
}
