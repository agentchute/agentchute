package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestPollerRunOnceWritesHeartbeat(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdPoller([]string{"run", "--as", "claude-code", "--vendor", "anthropic", "--once", "--quiet"})
		})
		if err != nil {
			t.Fatalf("poller run --once err = %v", err)
		}
	})
	hb, err := loop.LoadPollerHeartbeat(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if hb.Method != "poller-run" {
		t.Errorf("Method = %q, want poller-run", hb.Method)
	}
}

func TestPollerStatusBlocksWhenHeartbeatMissing(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPoller([]string{"status", "--as", "claude-code"})
		})
		if !errors.Is(err, errBlocked) {
			t.Fatalf("poller status err = %v, want errBlocked", err)
		}
		if !strings.Contains(out, "stale") {
			t.Errorf("status output should mention stale: %q", out)
		}
	})
}
