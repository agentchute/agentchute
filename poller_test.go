package main

import (
	"errors"
	"os"
	"path/filepath"
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
	if hb.LaunchEnabled {
		t.Error("LaunchEnabled = true for default poller run; want heartbeat-only")
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
	startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{
		OK:        true,
		RunnerPID: os.Getpid(),
		Status:    "active",
	})

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

func TestPollerEnsureNoopsWhenActiveSessionReachable(t *testing.T) {
	root, cfg := setupSendFixture(t)
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		Host:        localHostname(),
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := loop.SaveActiveSession(cfg, loop.ActiveSession{
		AgentID:  "codex",
		Source:   "self-check",
		Host:     localHostname(),
		PID:      os.Getpid(),
		LastSeen: time.Now().UTC(),
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

func TestPollerLaunchedWrapperReceivesAgentchuteEnv(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"
	envPath := filepath.Join(root, "poller-child-env.txt")

	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", agentID, "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		content := loop.ComposeMessage(now, "claude-code", agentID, "wake", "request", "", "body")
		if _, err := loop.WriteInboxMessage(cfg.AgentInboxDir(agentID), now, "claude-code", content); err != nil {
			t.Fatal(err)
		}

		t.Setenv("AGENTCHUTE_AGENT_ID", "wrong-agent")
		t.Setenv("AGENTCHUTE_CONTROL_REPO", "/wrong/control")
		t.Setenv("AGENTCHUTE_LOOP_DIR", "/wrong/loop")

		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Command:  `printf '%s\n%s\n%s\n' "$AGENTCHUTE_AGENT_ID" "$AGENTCHUTE_CONTROL_REPO" "$AGENTCHUTE_LOOP_DIR" > ` + shellQuote(envPath),
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     root,
			Launch:   true,
		}
		if err := pollerTick(cfg, params, nil, now); err != nil {
			t.Fatal(err)
		}
	})

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	want := []string{agentID, cfg.ControlRepo, cfg.LoopDir}
	if len(lines) != len(want) {
		t.Fatalf("env output lines = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("env line %d = %q, want %q (all lines: %q)", i, lines[i], want[i], lines)
		}
	}
}

func TestPollerDefaultDoesNotLaunchWrapperWhenWorkPending(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"
	envPath := filepath.Join(root, "poller-child-env.txt")

	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", agentID, "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		content := loop.ComposeMessage(now, "claude-code", agentID, "wake", "request", "", "body")
		if _, err := loop.WriteInboxMessage(cfg.AgentInboxDir(agentID), now, "claude-code", content); err != nil {
			t.Fatal(err)
		}

		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Command:  `touch ` + shellQuote(envPath),
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     root,
		}
		if err := pollerTick(cfg, params, nil, now); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("default poller launched wrapper; marker stat err = %v, want missing marker", err)
	}
	msgs, err := loop.ListInboxMessages(cfg.AgentInboxDir(agentID))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("default poller consumed inbox messages; got %d, want 1", len(msgs))
	}
	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if hb.LaunchEnabled {
		t.Fatal("LaunchEnabled = true for default poller tick; want false")
	}
}
