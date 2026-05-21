package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

func TestPollerEnsureNoopsWhenRunnerWakeReachable(t *testing.T) {
	root, cfg := setupSendFixture(t)
	socketPath := cfg.RunnerSocketPath("codex")
	listener, err := startRunnerSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()

	now := time.Now().UTC()
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		WorkingRepos: []string{
			root,
		},
		Host:       localHostname(),
		WakeMethod: loop.RunnerWakeMethod,
		WakeTarget: loop.RunnerWakeTarget(socketPath),
		LastSeen:   now,
		Status:     loop.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdPoller([]string{"ensure", "--as", "codex", "--vendor", "openai", "--quiet"})
		})
		if err != nil {
			t.Fatalf("poller ensure err = %v", err)
		}
	})
	if _, err := loop.LoadPollerHeartbeat(cfg, "codex"); !os.IsNotExist(err) {
		t.Fatalf("poller heartbeat err = %v, want missing heartbeat", err)
	}
}
