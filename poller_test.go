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
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		// Resolve the config exactly as the poller command does (cwd-relative,
		// symlink-normalized). On macOS t.TempDir() returns /var/... while
		// os.Getwd() returns /private/var/..., which yields a different
		// runner-socket temp-path hash. The owned-check (RunnerWakeTargetOwnedBy)
		// recomputes that path from LoopDir, so the registered wake_target MUST
		// be built from the same normalized cfg the recipient resolves — exactly
		// as production, where every party Discovers LoopDir from the shared
		// control repo. Building it from the unnormalized fixture cfg made the
		// owned-check (correctly) reject a target it could not match.
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}

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
			ControlRepo: cfg.ControlRepo,
			WorkingRepos: []string{
				cfg.ControlRepo,
			},
			Host:       localHostname(),
			WakeMethod: loop.RunnerWakeMethod,
			WakeTarget: loop.RunnerWakeTarget(socketPath),
			LastSeen:   now,
			Status:     loop.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}

		_, err = captureStdout(t, func() error {
			return cmdPoller([]string{"ensure", "--as", "codex", "--vendor", "openai", "--quiet"})
		})
		if err != nil {
			t.Fatalf("poller ensure err = %v", err)
		}
		if _, err := loop.LoadPollerHeartbeat(cfg, "codex"); !os.IsNotExist(err) {
			t.Fatalf("poller heartbeat err = %v, want missing heartbeat", err)
		}
	})
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

// WI-4 Fix 2: the heartbeat must be refreshed only AFTER a successful poll
// computation. If computeSelfPollResult errors, the heartbeat must NOT be
// re-stamped (its age keeps growing) so liveness can tell "beating but
// failing" apart from "healthy" — and the error is recorded as last_error.
func TestPollerTick_NoHeartbeatRefreshOnPollError(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"

	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", agentID, "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
		// Seed a known-old heartbeat so we can detect whether a tick re-stamps it.
		oldSeen := time.Now().UTC().Add(-7 * time.Minute)
		if err := loop.SavePollerHeartbeat(cfg, loop.PollerHeartbeat{
			AgentID:         agentID,
			Method:          "poller-run",
			Host:            localHostname(),
			PID:             os.Getpid(),
			IntervalSeconds: loop.DefaultPollerIntervalSeconds,
			LastSeen:        oldSeen,
		}); err != nil {
			t.Fatal(err)
		}

		// Force computeSelfPollResult to error: the inbox dir exists (so
		// needsBoot is false) but is unreadable, so the listing read fails
		// with a non-ErrInboxMissing error.
		inboxDir := cfg.AgentInboxDir(agentID)
		if err := os.Chmod(inboxDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(inboxDir, 0o700) })

		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     root,
		}
		err := pollerTick(cfg, params, nil, time.Now().UTC())
		if err == nil {
			t.Fatal("pollerTick with unreadable inbox returned nil; want a poll-computation error")
		}
	})

	hb, loadErr := loop.LoadPollerHeartbeat(cfg, agentID)
	if loadErr != nil {
		t.Fatalf("load heartbeat: %v", loadErr)
	}
	// The heartbeat must NOT have been refreshed to ~now: its age stays old.
	age := time.Now().UTC().Sub(hb.LastSeen.UTC())
	if age < 6*time.Minute {
		t.Fatalf("heartbeat was refreshed on poll error (age=%s); want it to keep aging (>= ~6m)", age.Round(time.Second))
	}
	// The failure must be recorded so liveness can distinguish "beating but
	// failing" from "healthy".
	if strings.TrimSpace(hb.LastError) == "" {
		t.Errorf("LastError not recorded on poll-computation failure: %+v", hb)
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
