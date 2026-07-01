package cli

import (
	"errors"
	"os"
	"os/exec"
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

// WI-E2 (removed): a poller tick used to re-prove the agent's own wake target and
// record the cached reachability fact, so a polling (non-runner) lane self-healed
// off-turn. Gate 6a (pull-only): TestPollerTick_RecordsReachabilityFact was
// removed. pollerTick no longer reproves or records a reachability fact (the
// own-wake reprove call was deleted), so there is nothing to assert.

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

func TestPollerEnsureNoopsWhenRunnerLive(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		// Resolve the config exactly as the poller command does (cwd-relative,
		// symlink-normalized).
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}

		// Gate 6c (pull-only): a poller is not required when the agent is already
		// live; presence is `.live` freshness, not a runner-socket ping.
		mustWriteLiveAt(t, cfg, "codex", time.Now().UTC())

		now := time.Now().UTC()
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), &loop.Registration{
			AgentID:     "codex",
			Vendor:      "openai",
			ControlRepo: cfg.ControlRepo,
			WorkingRepos: []string{
				cfg.ControlRepo,
			},
			Host:     localHostname(),
			LastSeen: now,
			Status:   loop.StatusActive,
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
		content := loop.ComposeMessage("claude-code", "", "body")
		mustWriteSeqInbox(t, cfg.AgentInboxDir(agentID), "claude-code", 1, content)

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

// seedPollerWork registers the agent and drops a pending inbox message so a
// poller tick computes ShouldWake==true.
func seedPollerWork(t *testing.T, cfg *loop.Config, agentID string) {
	t.Helper()
	if err := cmdRegister([]string{"--as", agentID, "--vendor", "openai"}); err != nil {
		t.Fatal(err)
	}
	content := loop.ComposeMessage("claude-code", "", "body")
	mustWriteSeqInbox(t, cfg.AgentInboxDir(agentID), "claude-code", 1, content)
}

// seedOldHeartbeat writes a known-old launch-enabled heartbeat so a tick that
// (incorrectly) refreshes can be detected by a shrunk age.
func seedOldHeartbeat(t *testing.T, cfg *loop.Config, agentID string, age time.Duration) {
	t.Helper()
	if err := loop.SavePollerHeartbeat(cfg, loop.PollerHeartbeat{
		AgentID:         agentID,
		Method:          "poller-run",
		Host:            localHostname(),
		PID:             os.Getpid(),
		IntervalSeconds: loop.DefaultPollerIntervalSeconds,
		LaunchEnabled:   true,
		LastSeen:        time.Now().UTC().Add(-age),
	}); err != nil {
		t.Fatal(err)
	}
}

// WI-4 follow-up: a launch-enabled tick whose wrapper fails to START must NOT
// advance last_seen — it must record last_error and keep aging. Before the fix
// the heartbeat was stamped fresh before the launch, leaving a false-fresh
// liveness signal even though no mail was consumed.
func TestPollerTick_LaunchStartFailureDoesNotRefreshHeartbeat(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"

	withCwd(t, root, func() {
		seedPollerWork(t, cfg, agentID)
		seedOldHeartbeat(t, cfg, agentID, 7*time.Minute)

		// Force cmd.Start() to fail: a nonexistent working dir makes the child
		// chdir fail before exec. The inbox/compute is unaffected (it reads the
		// loop dir, not Repo), so ShouldWake stays true.
		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Command:  "true",
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     filepath.Join(root, "does-not-exist-launch-dir"),
			Launch:   true,
		}
		err := pollerTick(cfg, params, nil, time.Now().UTC())
		if err == nil {
			t.Fatal("pollerTick with un-startable wrapper returned nil; want a launch error")
		}
	})

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		t.Fatalf("load heartbeat: %v", err)
	}
	age := time.Now().UTC().Sub(hb.LastSeen.UTC())
	if age < 6*time.Minute {
		t.Fatalf("heartbeat was refreshed on launch-start failure (age=%s); want it to keep aging (>= ~6m)", age.Round(time.Second))
	}
	if strings.TrimSpace(hb.LastError) == "" {
		t.Errorf("LastError not recorded on launch-start failure: %+v", hb)
	}
}

// WI-4 follow-up: a launch-enabled tick that successfully launches (and, for a
// synchronous rt==nil tick, completes) refreshes the heartbeat fresh.
func TestPollerTick_LaunchSuccessRefreshesHeartbeat(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"

	withCwd(t, root, func() {
		seedPollerWork(t, cfg, agentID)
		seedOldHeartbeat(t, cfg, agentID, 7*time.Minute)

		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Command:  "true",
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     root,
			Launch:   true,
		}
		if err := pollerTick(cfg, params, nil, time.Now().UTC()); err != nil {
			t.Fatalf("pollerTick = %v, want nil on successful launch", err)
		}
	})

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		t.Fatalf("load heartbeat: %v", err)
	}
	age := time.Now().UTC().Sub(hb.LastSeen.UTC())
	if age > time.Minute {
		t.Fatalf("heartbeat not refreshed on successful launch (age=%s); want fresh", age.Round(time.Second))
	}
	if strings.TrimSpace(hb.LastError) != "" {
		t.Errorf("LastError set on successful launch: %q", hb.LastError)
	}
}

// WI-4 follow-up: a no-work tick (ShouldWake==false) — and a non-launch tick —
// attempt no launch, so a successful compute refreshes the heartbeat.
func TestPollerTick_NoWorkOrNonLaunchRefreshesHeartbeat(t *testing.T) {
	root, cfg := setupSendFixture(t)

	t.Run("no work, launch-enabled", func(t *testing.T) {
		agentID := "codex-agentchute-nowork"
		withCwd(t, root, func() {
			if err := cmdRegister([]string{"--as", agentID, "--vendor", "openai"}); err != nil {
				t.Fatal(err)
			}
			seedOldHeartbeat(t, cfg, agentID, 7*time.Minute)
			params := serviceParams{
				AgentID:  agentID,
				Vendor:   "openai",
				Command:  "false", // would fail if launched; it must not be
				Interval: loop.DefaultPollerIntervalSeconds,
				Repo:     root,
				Launch:   true,
			}
			if err := pollerTick(cfg, params, nil, time.Now().UTC()); err != nil {
				t.Fatalf("pollerTick (no work) = %v, want nil", err)
			}
		})
		hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
		if err != nil {
			t.Fatalf("load heartbeat: %v", err)
		}
		if age := time.Now().UTC().Sub(hb.LastSeen.UTC()); age > time.Minute {
			t.Fatalf("no-work tick did not refresh heartbeat (age=%s)", age.Round(time.Second))
		}
	})

	t.Run("work pending, non-launch", func(t *testing.T) {
		agentID := "codex-agentchute-nonlaunch"
		withCwd(t, root, func() {
			seedPollerWork(t, cfg, agentID)
			seedOldHeartbeat(t, cfg, agentID, 7*time.Minute)
			params := serviceParams{
				AgentID:  agentID,
				Vendor:   "openai",
				Command:  "false",
				Interval: loop.DefaultPollerIntervalSeconds,
				Repo:     root,
				Launch:   false,
			}
			if err := pollerTick(cfg, params, nil, time.Now().UTC()); err != nil {
				t.Fatalf("pollerTick (non-launch) = %v, want nil", err)
			}
		})
		hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
		if err != nil {
			t.Fatalf("load heartbeat: %v", err)
		}
		if age := time.Now().UTC().Sub(hb.LastSeen.UTC()); age > time.Minute {
			t.Fatalf("non-launch tick did not refresh heartbeat (age=%s)", age.Round(time.Second))
		}
		if hb.LaunchEnabled {
			t.Error("LaunchEnabled = true for non-launch tick; want false")
		}
	})
}

// WI-4 follow-up: when an async wrapper from a previous tick is still running,
// the launch is healthy — refresh without trying to relaunch.
func TestPollerTick_ExistingRunningWrapperRefreshes(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"

	withCwd(t, root, func() {
		seedPollerWork(t, cfg, agentID)
		seedOldHeartbeat(t, cfg, agentID, 7*time.Minute)

		// A long-lived child stands in for a wrapper still consuming mail.
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-done
		})
		rt := &pollerRuntime{running: cmd, done: done}

		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Command:  "false", // must not be launched while the existing one runs
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     root,
			Launch:   true,
		}
		if err := pollerTick(cfg, params, rt, time.Now().UTC()); err != nil {
			t.Fatalf("pollerTick with running wrapper = %v, want nil", err)
		}
		if rt.running != cmd {
			t.Fatal("existing running wrapper was replaced or reaped while alive")
		}
	})

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		t.Fatalf("load heartbeat: %v", err)
	}
	if age := time.Now().UTC().Sub(hb.LastSeen.UTC()); age > time.Minute {
		t.Fatalf("running-wrapper tick did not refresh heartbeat (age=%s)", age.Round(time.Second))
	}
}

// WI-4 follow-up: an async wrapper that exited WITH an error must not be
// silently discarded. If a subsequent tick reaps the crash and then cannot
// relaunch, the failure surfaces in LastError and the heartbeat does NOT go
// fresh. Here the relaunch is forced to fail (un-startable Repo) so the reaped
// crash is the only liveness evidence.
func TestPollerTick_ReapedCrashedWrapperSurfacesError(t *testing.T) {
	root, cfg := setupSendFixture(t)
	agentID := "codex-agentchute-2"

	withCwd(t, root, func() {
		seedPollerWork(t, cfg, agentID)
		seedOldHeartbeat(t, cfg, agentID, 7*time.Minute)

		// Simulate a finished async wrapper that exited with an error.
		failed := exec.Command("sh", "-c", "exit 3")
		if err := failed.Start(); err != nil {
			t.Fatal(err)
		}
		exitErr := failed.Wait()
		if exitErr == nil {
			t.Fatal("expected the simulated wrapper to exit non-zero")
		}
		done := make(chan error, 1)
		done <- exitErr
		rt := &pollerRuntime{running: failed, done: done}

		// Relaunch is forced to fail so the reaped crash is what surfaces.
		params := serviceParams{
			AgentID:  agentID,
			Vendor:   "openai",
			Command:  "true",
			Interval: loop.DefaultPollerIntervalSeconds,
			Repo:     filepath.Join(root, "does-not-exist-relaunch-dir"),
			Launch:   true,
		}
		err := pollerTick(cfg, params, rt, time.Now().UTC())
		if err == nil {
			t.Fatal("pollerTick after reaped crash + failed relaunch returned nil; want error")
		}
	})

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		t.Fatalf("load heartbeat: %v", err)
	}
	if age := time.Now().UTC().Sub(hb.LastSeen.UTC()); age < 6*time.Minute {
		t.Fatalf("heartbeat refreshed after reaped crash + failed relaunch (age=%s); want aging", age.Round(time.Second))
	}
	if strings.TrimSpace(hb.LastError) == "" {
		t.Errorf("reaped crashed wrapper error not surfaced in LastError: %+v", hb)
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
		content := loop.ComposeMessage("claude-code", "", "body")
		mustWriteSeqInbox(t, cfg.AgentInboxDir(agentID), "claude-code", 1, content)

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
