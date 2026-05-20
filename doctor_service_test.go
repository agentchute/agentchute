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
		`agentchute pending --as claude-code --fail-if-any`,
		`flock -n /tmp/agentchute-claude-code.lock`,
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
		`agentchute pending --as codex --fail-if-any`,
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
	if !strings.Contains(got, "agentchute check --as claude-code") {
		t.Errorf("generated artifact missing concrete agent id:\n%s", got)
	}
}
