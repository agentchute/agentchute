package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func selfCheckArgs(extra ...string) []string {
	return append([]string{"--as", "claude-code", "--vendor", "anthropic"}, extra...)
}

func TestSelfCheckRegistersAndUpdatesLastSeen(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdSelfCheck(selfCheckArgs("--json")) }); err != nil {
			t.Fatalf("self-check: %v", err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.AgentID != "claude-code" || reg.Vendor != "anthropic" {
		t.Fatalf("unexpected registration: %+v", reg)
	}
	if reg.LastSeen.IsZero() {
		t.Fatal("last_seen was not populated")
	}
	if reg.WakeMethod != "" || reg.WakeTarget != "" {
		t.Fatalf("non-tmux self-check should be non-pokable, got %q %q", reg.WakeMethod, reg.WakeTarget)
	}
}

func TestSelfCheckRefreshesLastSeenAndClearsStaleTmuxWake(t *testing.T) {
	withFakeTmuxTargets(t, "%1")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdSelfCheck(selfCheckArgs("--quiet")) }); err != nil {
			t.Fatalf("initial self-check: %v", err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	regPath := cfg.AgentRegistrationPath("claude-code")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if reg.WakeMethod != "tmux" || reg.WakeTarget != "%1" {
		t.Fatalf("expected tmux registration, got %q %q", reg.WakeMethod, reg.WakeTarget)
	}
	backdated := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	reg.LastSeen = backdated
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		os.Unsetenv("TMUX_PANE")
		if _, err := captureStdout(t, func() error { return cmdSelfCheck(selfCheckArgs("--quiet")) }); err != nil {
			t.Fatalf("second self-check: %v", err)
		}
	})
	reg, err = loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reg.LastSeen.After(backdated) {
		t.Fatalf("last_seen did not refresh: %v <= %v", reg.LastSeen, backdated)
	}
	if reg.WakeMethod != "" || reg.WakeTarget != "" {
		t.Fatalf("stale tmux wake was not cleared, got %q %q", reg.WakeMethod, reg.WakeTarget)
	}
}

func TestSelfCheckPrunesStaleSameHostPeerTmuxRegistration(t *testing.T) {
	withFakeTmuxTargets(t, "%1")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdSelfCheck(selfCheckArgs("--quiet")) }); err != nil {
			t.Fatalf("self-check: %v", err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	peer := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		Host:        host,
		WakeMethod:  "tmux",
		WakeTarget:  "%9",
		LastSeen:    time.Now().UTC().Truncate(time.Second),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), peer); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, cfg.AgentInboxDir("codex"))

	var out string
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		var err error
		out, err = captureStdout(t, func() error { return cmdSelfCheck(selfCheckArgs("--json")) })
		if err != nil {
			t.Fatalf("self-check repair: %v", err)
		}
	})
	var got selfCheckStatus
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.PeerWakeStaleCount != 1 {
		t.Fatalf("PeerWakeStaleCount = %d, want 1; output=%s", got.PeerWakeStaleCount, out)
	}
	if len(got.PeerWakeStale) != 1 || got.PeerWakeStale[0].AgentID != "codex" || got.PeerWakeStale[0].Target != "%9" {
		t.Fatalf("PeerWakeStale = %+v, want codex %%9", got.PeerWakeStale)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("codex")); !os.IsNotExist(err) {
		t.Fatalf("peer registration should be removed, stat err=%v", err)
	}
}

func TestSelfCheckQuietEmitsNothing(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error { return cmdSelfCheck(selfCheckArgs("--quiet")) })
		if err != nil {
			t.Fatalf("self-check quiet: %v", err)
		}
		if out != "" {
			t.Fatalf("--quiet emitted output: %q", out)
		}
	})
}
