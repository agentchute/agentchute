package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// All four kinds emit non-empty content with the agent id woven in,
// and each emits the kind-specific structural marker so an operator can
// pipe straight to the right install target.

// Regression: real-bake on 2026-05-20 found plutil rejecting the
// generated plist with "unknown ampersand-escape sequence" because the
// preflight shell line contains `2>&1` and the `&` was emitted raw.
// Every plist <string> body must XML-escape its content.
func TestGenerateServiceLaunchdXMLEscapesAmpersand(t *testing.T) {
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindLaunchd,
			AgentID:  "claude-code",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "2>&1") {
		t.Errorf("raw `2>&1` in plist body — plutil rejects (must be `2&gt;&amp;1`):\n%s", got)
	}
	if !strings.Contains(got, "2&gt;&amp;1") {
		t.Errorf("expected XML-escaped `2&gt;&amp;1` in plist body:\n%s", got)
	}
}

func TestGenerateServiceLaunchdShape(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "claude.plist")
	err := generateService(serviceParams{
		Kind:     serviceKindLaunchd,
		AgentID:  "claude-code",
		Interval: 30,
		Repo:     root,
		Out:      out,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readFileForTest(t, out)
	for _, want := range []string{
		`<?xml version="1.0"`,
		`<plist version="1.0">`,
		`com.agentchute.preflight.claude-code`,
		`<key>StartInterval</key>`,
		`<integer>30</integer>`,
		`agentchute self-poll --as claude-code`,
		`mkdir /tmp/agentchute-claude-code.lock`,
		`claude -p`,
		// the XML-escaped form of `2>&1`
		`2&gt;&amp;1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("launchd plist missing %q\n%s", want, got)
		}
	}
}

func TestGenerateServiceSystemdServiceShape(t *testing.T) {
	root := t.TempDir()
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindSystemdService,
			AgentID:  "codex",
			Interval: 30,
			Repo:     root,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[Unit]`,
		`Description=agentchute preflighted scheduler for codex`,
		`[Service]`,
		`Type=oneshot`,
		`ExecStart=/bin/sh -c '`,
		`agentchute self-poll --as codex`,
		`codex exec`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd-service missing %q\n%s", want, got)
		}
	}
}

func TestGenerateServiceSystemdTimerShape(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindSystemdTimer,
			AgentID:  "gemini-cli",
			Interval: 45,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[Timer]`,
		`OnUnitActiveSec=45s`,
		`Unit=agentchute-gemini-cli.service`,
		`WantedBy=timers.target`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd-timer missing %q\n%s", want, got)
		}
	}
}

func TestGenerateServiceScriptHasNoSetE(t *testing.T) {
	// Regression: `set -e` aborts the loop when `agentchute pending`
	// exits 2 (the EXPECTED work-exists signal). The script must keep
	// looping past every tick regardless of how subcommands exit.
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "claude-code",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Look for `set -e` as an actual directive on its own line, not in
	// the comment explaining why we omit it.
	for _, line := range strings.Split(got, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "set -e" {
			t.Errorf("script contains 'set -e' directive — aborts when pending exits 2 (the work-exists signal):\n%s", got)
		}
	}
	if !strings.Contains(got, "while true; do") {
		t.Errorf("script missing loop:\n%s", got)
	}
	if !strings.Contains(got, "sleep") {
		t.Errorf("script missing sleep:\n%s", got)
	}
	if !strings.HasPrefix(got, "#!/bin/sh") {
		t.Errorf("script missing shebang:\n%s", got)
	}
}

func TestGenerateServiceRequiresAgentID(t *testing.T) {
	err := generateService(serviceParams{
		Kind:     serviceKindLaunchd,
		Interval: 30,
		Repo:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "--as is required") {
		t.Errorf("err = %v; want --as required error", err)
	}
}

func TestGenerateServiceRejectsUnknownKind(t *testing.T) {
	err := generateService(serviceParams{
		Kind:     "tmux-keybinding",
		AgentID:  "claude-code",
		Interval: 30,
		Repo:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("err = %v; want unknown kind error", err)
	}
}

func TestGenerateServiceRejectsTooShortInterval(t *testing.T) {
	// 1s polling on local FS is a footgun — we set a 5s floor so
	// operators don't accidentally hammer the inbox.
	err := generateService(serviceParams{
		Kind:     serviceKindLaunchd,
		AgentID:  "claude-code",
		Interval: 1,
		Repo:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), ">= 5 seconds") {
		t.Errorf("err = %v; want interval >= 5 error", err)
	}
}

func TestGenerateServiceUnknownAgentRequiresCommandOrWrapper(t *testing.T) {
	// Codex caveat from self-poll signoff: generated artifacts must
	// supply CONCRETE wrapper identity, not template placeholders. If
	// we can't infer the wrapper, refuse rather than emit a broken unit.
	err := generateService(serviceParams{
		Kind:     serviceKindLaunchd,
		AgentID:  "my-custom-agent",
		Interval: 30,
		Repo:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot infer wrapper command") {
		t.Errorf("err = %v; want cannot-infer-wrapper error", err)
	}
}

func TestGenerateServiceCommandOverride(t *testing.T) {
	// Operator override for non-standard wrappers (e.g., a fork of
	// codex CLI or a vendored agent runner).
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindLaunchd,
			AgentID:  "my-custom-agent",
			Command:  "my-runner --task agentchute-mail",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "my-runner --task agentchute-mail") {
		t.Errorf("--command override missing from output:\n%s", got)
	}
}

func TestGenerateServicePromptHasConcreteAgentID(t *testing.T) {
	// Codex signoff caveat: the generated wrapper prompt must name the
	// concrete agent (and implicitly the vendor), not literal `<vendor>`
	// or `<agent>` placeholder tokens that self-poll's needs_boot
	// prompt-text uses.
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "claude-code",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "<vendor>") || strings.Contains(got, "<agent>") {
		t.Errorf("generated artifact contains placeholder token:\n%s", got)
	}
	if !strings.Contains(got, "agentchute boot --as claude-code --vendor anthropic") {
		t.Errorf("generated artifact missing concrete agent id + vendor in wrapper prompt:\n%s", got)
	}
	if strings.Contains(got, "`agentchute") {
		t.Errorf("backticks around agentchute commands in prompt — outer scheduler shell will command-substitute them:\n%s", got)
	}
}

func TestGenerateServiceExportsAgentIDToWrapper(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "codex",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `AGENTCHUTE_AGENT_ID=codex sh -c "codex exec`) {
		t.Errorf("generated scheduler does not export AGENTCHUTE_AGENT_ID to wrapper launch:\n%s", got)
	}
}

func TestGenerateServiceExportsControlAndLoopToWrapper(t *testing.T) {
	root := setupBootFixture(t)
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "codex",
			Interval: 30,
			Repo:     root,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`AGENTCHUTE_AGENT_ID=codex`,
		`AGENTCHUTE_CONTROL_REPO="` + root + `"`,
		`AGENTCHUTE_LOOP_DIR="` + filepath.Join(root, ".examplecorp", "loop") + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated scheduler missing %q:\n%s", want, got)
		}
	}
}

// Codex review #3 (2026-05-20): unvalidated agent ids land directly in
// shell strings — `bad;id` would emit injection-shaped output. Validate
// before render.
func TestGenerateServiceValidatesAgentID(t *testing.T) {
	err := generateService(serviceParams{
		Kind:     serviceKindLaunchd,
		AgentID:  "bad;id",
		Command:  "claude -p test",
		Interval: 30,
		Repo:     t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error for shell-injection-shaped agent id; got nil")
	}
}

// Codex review #2 (2026-05-20): macOS does not ship flock by default —
// generated launchd plists that depend on it are nonfunctional out of
// the box. Use POSIX mkdir-as-lock instead (atomic on POSIX).
func TestGenerateServicePreflightUsesPosixLock(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindLaunchd,
			AgentID:  "claude-code",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "flock") {
		t.Errorf("preflight uses flock (not on macOS by default); want POSIX mkdir lock:\n%s", got)
	}
	if !strings.Contains(got, "mkdir") {
		t.Errorf("preflight missing mkdir-as-lock:\n%s", got)
	}
	if !strings.Contains(got, "trap") {
		t.Errorf("preflight missing trap-based lock cleanup:\n%s", got)
	}
}

// Codex review #4 (2026-05-20): the `script` kind embedded a bare
// `exit 0` inside `while true; do ... done`, which exited the whole
// loop on the first idle tick. The fix wraps the tick in a `(...)`
// subshell so `exit 0` only exits the subshell.
func TestGenerateServiceScriptLoopSurvivesIdleTick(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "claude-code",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	// The tick body must run inside `( ... )` so `exit 0` is local to
	// the subshell and the `while` keeps looping.
	if !strings.Contains(got, "( cd ") {
		t.Errorf("script kind missing subshell wrapper around tick body — `exit 0` would kill the loop:\n%s", got)
	}
}

// Codex re-review #4 (2026-05-20): --repo flows into `cd %q` in the
// generated shell tick; Go %q is not shell quoting, and $() / backticks
// expand inside double quotes. Validate against a conservative
// whitelist before rendering.
func TestGenerateServiceValidatesRepo(t *testing.T) {
	for _, malicious := range []string{
		"/tmp/repo$(touch /tmp/agentchute-repo-pwn)",
		"/tmp/repo`whoami`",
		"/tmp/repo;rm -rf /",
		"/tmp/repo\"injected\"",
		"/tmp/repo'injected'",
	} {
		err := generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "claude-code",
			Vendor:   "anthropic",
			Wrapper:  "claude",
			Repo:     malicious,
			Interval: 30,
		})
		if err == nil {
			t.Errorf("expected error for shell-injection-shaped repo %q; got nil", malicious)
		}
	}
}

// Codex re-review #3 (2026-05-20): --vendor lands inside the generated
// prompt and would shell-substitute if it contains $() or backticks.
// Validate against the same slug rule as agent_id.
func TestGenerateServiceValidatesVendor(t *testing.T) {
	for _, malicious := range []string{
		"bad$(touch /tmp/pwn)",
		"bad`whoami`",
		"bad;rm -rf /",
		"bad vendor",
	} {
		err := generateService(serviceParams{
			Kind:     serviceKindLaunchd,
			AgentID:  "claude-code",
			Vendor:   malicious,
			Interval: 30,
			Repo:     t.TempDir(),
		})
		if err == nil {
			t.Errorf("expected error for shell-injection-shaped vendor %q; got nil", malicious)
		}
	}
}

// Codex re-review #2 (2026-05-20): the generated prompt was using
// backticks (\`agentchute boot...\`) for command formatting. After
// scheduler shell + preflight inline-sh both consume their layer of
// backslash-escaping, the backticks become active in the outer sh —
// command-substituting agentchute boot/send/defer before the wrapper
// ever sees the prompt. The prompt body must contain no shell-special
// characters at all.
func TestGenerateServicePromptHasNoShellSpecialChars(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindScript,
			AgentID:  "codex",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Locate the wrapper invocation. For the script kind we know it sits
	// inside `sh -c "..."` after the lock setup.
	const marker = `sh -c "codex exec `
	idx := strings.Index(got, marker)
	if idx < 0 {
		t.Fatalf("wrapper invocation not found in:\n%s", got)
	}
	// Take the rest of the line through the next `)` (subshell close).
	rest := got[idx:]
	closeIdx := strings.Index(rest, ` )`)
	if closeIdx < 0 {
		t.Fatalf("subshell close not found:\n%s", rest)
	}
	wrapperFragment := rest[:closeIdx]
	for _, bad := range []string{"`", "$(", "\\$"} {
		if strings.Contains(wrapperFragment, bad) {
			t.Errorf("wrapper-invocation fragment contains shell-special %q (outer shells will interpret):\n%s", bad, wrapperFragment)
		}
	}
}

// Codex re-review (2026-05-20): the systemd-service ExecStart wraps
// preflightTick in outer single quotes. A single-quoted trap action
// inside (`trap 'rmdir ...' EXIT`) would terminate the ExecStart string
// at the inner quote and the unit would fail to parse. Regression: the
// generated systemd-service body must contain no inner single quotes,
// since the wrapper is single-quoted.
func TestGenerateServiceSystemdServiceHasNoInnerSingleQuotes(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindSystemdService,
			AgentID:  "codex",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Find ExecStart=/bin/sh -c '...' and extract the inside-single-quotes body.
	const start = "ExecStart=/bin/sh -c '"
	idx := strings.Index(got, start)
	if idx < 0 {
		t.Fatalf("ExecStart line missing in:\n%s", got)
	}
	body := got[idx+len(start):]
	endIdx := strings.LastIndex(body, "'")
	if endIdx < 0 {
		t.Fatalf("ExecStart string not closed:\n%s", got)
	}
	inside := body[:endIdx]
	if strings.Contains(inside, "'") {
		t.Errorf("ExecStart body contains inner single quote — closes the outer quote early:\n%s", inside)
	}
}

// Codex review #5 (2026-05-20): preflight via `self-poll` rather than
// `pending --fail-if-any`. self-poll exits 2 on needs_boot too, so the
// first-run wake actually fires the wrapper through to boot.
func TestGenerateServicePreflightUsesSelfPoll(t *testing.T) {
	got, err := captureStdout(t, func() error {
		return generateService(serviceParams{
			Kind:     serviceKindLaunchd,
			AgentID:  "claude-code",
			Interval: 30,
			Repo:     t.TempDir(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "agentchute pending --as") {
		t.Errorf("preflight uses pending (doesn't surface needs_boot); want self-poll:\n%s", got)
	}
	if !strings.Contains(got, "agentchute self-poll --as claude-code") {
		t.Errorf("preflight missing self-poll command:\n%s", got)
	}
}
