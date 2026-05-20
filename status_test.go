package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestPrintStatusIncludesAgentsAndInboxDepth(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	mustMkdir(t, cfg.AgentsDir())
	mustMkdir(t, cfg.AgentInboxDir("codex"))
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	regs := map[string]*loop.Registration{
		"codex": {
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: root,
			WakeMethod:  "tmux",
			WakeTarget:  "%1",
			LastSeen:    now.Add(-2 * time.Minute),
			Status:      loop.StatusActive,
		},
	}

	var out bytes.Buffer
	printStatus(&out, cfg, regs, now)
	text := out.String()
	for _, want := range []string{"control_repo:", "codex", "active", "%1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output missing %q:\n%s", want, text)
		}
	}
	foundDepth := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "codex" && fields[2] == "1" {
			foundDepth = true
		}
	}
	if !foundDepth {
		t.Fatalf("status output missing inbox depth 1 for codex:\n%s", text)
	}
}

// v0.1.2 UX nit: status without --as / $AGENTCHUTE_AGENT_ID prints the
// pool overview without claiming an agent identity and without touching
// anyone's last_seen. Codex review aligned: diagnostic command, not a
// lifecycle event.
func TestCmdStatusWithoutAgentIDPrintsPoolWithoutSideEffects(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".rehumanlabs", "loop"))

	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
	})

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}

	// Backdate codex's last_seen so we can detect a side-effecting update.
	regPath := cfg.AgentRegistrationPath("codex")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	// Registration stores last_seen at second precision; use a wall-second
	// timestamp so the post-write read compares equal.
	backdated := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	reg.LastSeen = backdated
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}

	// Make sure no env var is set; otherwise --as would be implicit.
	// Use t.Setenv with empty value so test cleanup restores any prior
	// shell state (codex review on 37d87e1).
	t.Setenv("AGENTCHUTE_AGENT_ID", "")

	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error { return cmdStatus(nil) })
		if err != nil {
			t.Fatalf("cmdStatus with no --as returned err: %v", err)
		}
		if !strings.Contains(out, "codex") {
			t.Errorf("pool overview missing codex agent: %q", out)
		}
	})

	// last_seen must not have been touched by the no-flag invocation.
	after, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeen.Equal(backdated) {
		t.Errorf("status without --as updated last_seen: %v -> %v", backdated, after.LastSeen)
	}
}

// With --as set, status still ticks the caller's last_seen (preserves the
// historical acting-agent mode).
func TestCmdStatusWithAgentIDRefreshesLastSeen(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".rehumanlabs", "loop"))

	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
	})

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	regPath := cfg.AgentRegistrationPath("codex")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate to second precision so the stored value matches what's
	// written, then read-back to anchor the assertion on the actual
	// pre-test stored state (codex review on 37d87e1: nanosecond
	// precision was masking a missing-update bug).
	backdated := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	reg.LastSeen = backdated
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	before, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdStatus([]string{"--as", "codex"}) }); err != nil {
			t.Fatal(err)
		}
	})

	after, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeen.After(before.LastSeen) {
		t.Errorf("status with --as did NOT refresh last_seen: %v → %v", before.LastSeen, after.LastSeen)
	}
}
