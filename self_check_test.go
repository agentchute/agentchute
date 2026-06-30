package main

import (
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Pull-only (simple-again Gate 6c): self-check writes a no-wake registration.
// The wake-reconcile / stale-tmux-clear / same-host-peer-prune tests were removed
// with the apparatus they exercised; self-check's retained job is refreshing
// last_seen + the registration record.

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
}

// A second self-check refreshes last_seen on an existing (backdated) registration.
func TestSelfCheckRefreshesLastSeen(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
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
