package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestRunInjectsPromptOnSocketWake(t *testing.T) {
	root := setupShortRunFixture(t)
	script := filepath.Join(root, "fake-wrapper.sh")
	mustWrite(t, script, []byte("#!/bin/sh\nprintf 'READY\\n'\nIFS= read line\nprintf 'GOT:%s\\n' \"$line\"\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	withCwd(t, root, func() {
		go func() {
			errCh <- cmdRun([]string{
				"--as", "runner-test",
				"--vendor", "test",
				"--control-repo", root,
				"--loop-dir", filepath.Join(root, ".examplecorp", "loop"),
				"--interval", "5",
				"--idle-grace", "100ms",
				"--prompt", "check inbox",
				"--", script,
			})
		}()

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		target := loop.RunnerWakeTarget(cfg.RunnerSocketPath("runner-test"))
		waitForRunnerSocket(t, target, errCh)
		if err := loop.PokeWakeTarget(loop.RunnerWakeMethod, target); err != nil {
			t.Fatalf("poke runner: %v", err)
		}
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("cmdRun err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not inject prompt and exit fake wrapper")
	}
}

func TestPromptInjectionBytesDefaultUsesCarriageReturn(t *testing.T) {
	got := string(promptInjectionBytes(runnerOptions{
		AgentID:     "runner-test",
		Vendor:      "test",
		Prompt:      "check inbox",
		WrapperArgs: []string{"/tmp/fake-wrapper"},
	}))
	want := "check inbox\r"
	if got != want {
		t.Fatalf("promptInjectionBytes = %q, want %q", got, want)
	}
}

func TestPromptInjectionBytesCodexUsesBracketedPasteAndEnhancedEnter(t *testing.T) {
	got := string(promptInjectionBytes(runnerOptions{
		AgentID:     "codex",
		Vendor:      "openai",
		Prompt:      "check inbox",
		WrapperArgs: []string{"/usr/local/bin/codex"},
	}))
	want := bracketedPasteStart + "check inbox" + bracketedPasteEnd + codexEnhancedEnter
	if got != want {
		t.Fatalf("promptInjectionBytes = %q, want %q", got, want)
	}
}

func TestPromptInjectionBytesCodexWrapperUsesEnhancedEnter(t *testing.T) {
	got := string(promptInjectionBytes(runnerOptions{
		AgentID:     "custom-codex",
		Vendor:      "openai",
		Prompt:      "check inbox",
		WrapperArgs: []string{"/usr/local/bin/codex"},
	}))
	want := bracketedPasteStart + "check inbox" + bracketedPasteEnd + codexEnhancedEnter
	if got != want {
		t.Fatalf("promptInjectionBytes = %q, want %q", got, want)
	}
}

func TestRunnerMakeRawNoopsForNonTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	restore, enabled, err := runnerMakeRaw(f)
	if err != nil {
		t.Fatalf("runnerMakeRaw err = %v", err)
	}
	if enabled {
		t.Fatal("runnerMakeRaw enabled raw mode for non-terminal")
	}
	if restore == nil {
		t.Fatal("restore func is nil")
	}
	if err := restore(); err != nil {
		t.Fatalf("restore err = %v", err)
	}
}

func TestRunRefusesLiveRunnerCollision(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir("codex")); err != nil {
		t.Fatal(err)
	}
	socketPath := cfg.RunnerSocketPath("codex")
	startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{
		OK:        true,
		RunnerPID: os.Getpid(),
		Status:    "active",
	})

	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:    "codex",
		RunnerPID:  os.Getpid(),
		SocketPath: socketPath,
		StartedAt:  time.Now().UTC(),
		Status:     "active",
	}); err != nil {
		t.Fatal(err)
	}

	err = refuseLiveRunnerCollision(cfg, "codex")
	if err == nil {
		t.Fatal("expected live runner collision")
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestRunnerSocketReachableRequiresPingAck(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	socketPath := cfg.RunnerSocketPath("codex")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	target := loop.RunnerWakeTarget(socketPath)
	if loop.RunnerSocketReachable(target, 100*time.Millisecond) {
		t.Fatal("bare socket accepted as reachable runner; want ping/ack failure")
	}
}

func TestRunShutdownSocketCleansUpRunner(t *testing.T) {
	root := setupShortRunFixture(t)
	marker := filepath.Join(root, "terminated")
	ready := filepath.Join(root, "ready")
	script := filepath.Join(root, "fake-wrapper.sh")
	mustWrite(t, script, []byte("#!/bin/sh\ntrap 'echo stopped > "+marker+"; exit 0' TERM HUP INT\nprintf 'READY\\n'\necho ready > "+ready+"\nwhile :; do sleep 1; done\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	withCwd(t, root, func() {
		go func() {
			errCh <- cmdRun([]string{
				"--as", "codex",
				"--vendor", "openai",
				"--control-repo", root,
				"--loop-dir", filepath.Join(root, ".examplecorp", "loop"),
				"--interval", "5",
				"--idle-grace", "100ms",
				"--", script,
			})
		}()

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		socketPath := cfg.RunnerSocketPath("codex")
		waitForRunnerSocket(t, loop.RunnerWakeTarget(socketPath), errCh)
		waitForFile(t, ready)
		if err := runnerSocketOp(socketPath, "shutdown"); err != nil {
			t.Fatalf("shutdown runner: %v", err)
		}
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("cmdRun err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not exit after shutdown")
	}

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Status != loop.StatusOffline {
		t.Fatalf("registration status = %s, want offline", reg.Status)
	}
	if _, err := os.Stat(cfg.RunnerSocketPath("codex")); !os.IsNotExist(err) {
		t.Fatalf("socket stat err = %v, want missing socket", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("wrapper did not receive shutdown signal: %v", err)
	}
}

func TestRunnerPingReportsState(t *testing.T) {
	root := setupShortRunFixture(t)
	ready := filepath.Join(root, "ready")
	script := filepath.Join(root, "fake-wrapper.sh")
	mustWrite(t, script, []byte("#!/bin/sh\ntrap 'exit 0' TERM HUP INT\nprintf 'READY\\n'\necho ready > "+shellQuote(ready)+"\nwhile :; do sleep 1; done\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	withCwd(t, root, func() {
		go func() {
			errCh <- cmdRun([]string{
				"--as", "codex",
				"--vendor", "openai",
				"--control-repo", root,
				"--loop-dir", filepath.Join(root, ".examplecorp", "loop"),
				"--interval", "5",
				"--idle-grace", "100ms",
				"--", script,
			})
		}()

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		socketPath := cfg.RunnerSocketPath("codex")
		target := loop.RunnerWakeTarget(socketPath)
		waitForRunnerSocket(t, target, errCh)
		waitForFile(t, ready)
		resp, err := loop.PingRunner(target, time.Second)
		if err != nil {
			t.Fatalf("ping runner: %v", err)
		}
		if !resp.OK {
			t.Fatal("ping response OK = false")
		}
		if resp.RunnerPID != os.Getpid() {
			t.Fatalf("RunnerPID = %d, want %d", resp.RunnerPID, os.Getpid())
		}
		if resp.ChildPID <= 0 {
			t.Fatalf("ChildPID = %d, want positive pid", resp.ChildPID)
		}
		if resp.Status != "active" {
			t.Fatalf("Status = %q, want active", resp.Status)
		}
		if err := runnerSocketOp(socketPath, "shutdown"); err != nil {
			t.Fatalf("shutdown runner: %v", err)
		}
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("cmdRun err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not exit after shutdown")
	}
}

func TestRunExportsRunnerPIDToWrapper(t *testing.T) {
	root := setupShortRunFixture(t)
	envPath := filepath.Join(root, "runner-env.txt")
	script := filepath.Join(root, "fake-wrapper.sh")
	mustWrite(t, script, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$AGENTCHUTE_RUNNER\" \"$AGENTCHUTE_RUNNER_PID\" > "+shellQuote(envPath)+"\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		if err := cmdRun([]string{
			"--as", "codex",
			"--vendor", "openai",
			"--control-repo", root,
			"--loop-dir", filepath.Join(root, ".examplecorp", "loop"),
			"--interval", "5",
			"--idle-grace", "100ms",
			"--", script,
		}); err != nil {
			t.Fatalf("cmdRun err = %v", err)
		}
	})

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 {
		t.Fatalf("runner env lines = %q, want 2 lines", lines)
	}
	if lines[0] != "1" {
		t.Fatalf("AGENTCHUTE_RUNNER = %q, want 1", lines[0])
	}
	if lines[1] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("AGENTCHUTE_RUNNER_PID = %q, want %d", lines[1], os.Getpid())
	}
}

func TestRunnerWakeSatisfiesRecipientLiveness(t *testing.T) {
	cfg := newDoctorCfg(t)
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir("codex")); err != nil {
		t.Fatal(err)
	}
	socketPath := cfg.RunnerSocketPath("codex")
	startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{
		OK:        true,
		RunnerPID: os.Getpid(),
		Status:    "active",
	})

	reg := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: cfg.ControlRepo,
		Host:        localHostname(),
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  loop.RunnerWakeTarget(socketPath),
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), reg); err != nil {
		t.Fatal(err)
	}
	liveness := evaluateRecipientLiveness(cfg, "codex", time.Now().UTC())
	if !liveness.OK {
		t.Fatalf("liveness OK = false, message=%q", liveness.Message)
	}
	if liveness.Via != "wake" {
		t.Fatalf("liveness Via = %q, want wake", liveness.Via)
	}
}

func TestClearStaleRunnerWakeTargetsDoesNotClearTransientPingMiss(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	peerID := "peer-runner"
	socketPath := cfg.RunnerSocketPath(peerID)
	target := loop.RunnerWakeTarget(socketPath)
	reachableAt := time.Now().UTC()
	lastSeen := reachableAt.Add(-time.Minute)
	reg := &loop.Registration{
		AgentID:            peerID,
		Vendor:             "test",
		ControlRepo:        cfg.ControlRepo,
		Host:               localHostname(),
		WakeMethod:         loop.RunnerWakeMethod,
		WakeTarget:         target,
		LastSeen:           lastSeen,
		Status:             loop.StatusActive,
		ReachableAt:        &reachableAt,
		ReachabilityMethod: loop.RunnerWakeMethod,
		ReachabilityTarget: target,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(peerID), reg); err != nil {
		t.Fatal(err)
	}
	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:    peerID,
		Host:       localHostname(),
		RunnerPID:  os.Getpid(),
		SocketPath: socketPath,
		Status:     "active",
	}); err != nil {
		t.Fatal(err)
	}

	cleared, err := clearStaleRunnerWakeTargets(cfg, "self-runner")
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Fatalf("cleared = %v, want none for a live process with a transient ping miss", cleared)
	}
	got, err := loop.ReadRegistration(cfg.AgentRegistrationPath(peerID))
	if err != nil {
		t.Fatal(err)
	}
	if got.WakeMethod != loop.RunnerWakeMethod || got.WakeTarget != target {
		t.Fatalf("wake = %s/%s, want preserved %s/%s", got.WakeMethod, got.WakeTarget, loop.RunnerWakeMethod, target)
	}
	if got.Status != loop.StatusActive {
		t.Fatalf("status = %s, want active", got.Status)
	}
	if got.ReachableAt == nil {
		t.Fatal("ReachableAt cleared on transient miss; want preserved")
	}
}

func TestClearStaleRunnerWakeTargetsClearsOfflineStateWithoutOffliningPeer(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	peerID := "peer-runner"
	socketPath := cfg.RunnerSocketPath(peerID)
	target := loop.RunnerWakeTarget(socketPath)
	reachableAt := time.Now().UTC()
	reg := &loop.Registration{
		AgentID:            peerID,
		Vendor:             "test",
		ControlRepo:        cfg.ControlRepo,
		Host:               localHostname(),
		WakeMethod:         loop.RunnerWakeMethod,
		WakeTarget:         target,
		LastSeen:           reachableAt.Add(-time.Hour),
		Status:             loop.StatusActive,
		ReachableAt:        &reachableAt,
		ReachabilityMethod: loop.RunnerWakeMethod,
		ReachabilityTarget: target,
		ReachabilityError:  "old success",
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(peerID), reg); err != nil {
		t.Fatal(err)
	}
	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:    peerID,
		Host:       localHostname(),
		RunnerPID:  os.Getpid(),
		SocketPath: socketPath,
		Status:     "offline",
	}); err != nil {
		t.Fatal(err)
	}

	cleared, err := clearStaleRunnerWakeTargets(cfg, "self-runner")
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 1 || cleared[0] != peerID {
		t.Fatalf("cleared = %v, want [%s]", cleared, peerID)
	}
	got, err := loop.ReadRegistration(cfg.AgentRegistrationPath(peerID))
	if err != nil {
		t.Fatal(err)
	}
	if got.WakeMethod != "" || got.WakeTarget != "" {
		t.Fatalf("wake = %s/%s, want cleared", got.WakeMethod, got.WakeTarget)
	}
	if got.Status != loop.StatusActive {
		t.Fatalf("status = %s, want preserved active (startup stale-clear must not offline peers)", got.Status)
	}
	if got.ReachableAt != nil || got.ReachabilityMethod != "" || got.ReachabilityTarget != "" || got.ReachabilityError != "" {
		t.Fatalf("reachability cache not cleared: at=%v method=%q target=%q err=%q", got.ReachableAt, got.ReachabilityMethod, got.ReachabilityTarget, got.ReachabilityError)
	}
}

func TestMarkRunnerOfflineClearsReachabilityCache(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	agentID := "runner-test"
	target := loop.RunnerWakeTarget(cfg.RunnerSocketPath(agentID))
	reachableAt := time.Now().UTC()
	reg := &loop.Registration{
		AgentID:            agentID,
		Vendor:             "test",
		ControlRepo:        cfg.ControlRepo,
		Host:               localHostname(),
		WakeMethod:         loop.RunnerWakeMethod,
		WakeTarget:         target,
		LastSeen:           reachableAt.Add(-time.Minute),
		Status:             loop.StatusActive,
		ReachableAt:        &reachableAt,
		ReachabilityMethod: loop.RunnerWakeMethod,
		ReachabilityTarget: target,
		ReachabilityError:  "old success",
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}

	if err := markRunnerOffline(cfg, agentID); err != nil {
		t.Fatal(err)
	}
	got, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != loop.StatusOffline {
		t.Fatalf("status = %s, want offline", got.Status)
	}
	if got.ReachableAt != nil || got.ReachabilityMethod != "" || got.ReachabilityTarget != "" || got.ReachabilityError != "" {
		t.Fatalf("reachability cache not cleared: at=%v method=%q target=%q err=%q", got.ReachableAt, got.ReachabilityMethod, got.ReachabilityTarget, got.ReachabilityError)
	}
}

// newPollTestRuntime builds a minimal runnerRuntime sufficient to exercise
// pollOnce in isolation (no PTY, no socket). It registers the agent so the
// pollOnce UpdateLastSeen call has a registration to read, and seeds the
// runner state dir so saveState can write.
func newPollTestRuntime(t *testing.T, cfg *loop.Config, agentID string) *runnerRuntime {
	t.Helper()
	if err := loop.EnsurePrivateDir(cfg.AgentInboxDir(agentID)); err != nil {
		t.Fatal(err)
	}
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "test",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}
	rt := &runnerRuntime{
		cfg:     cfg,
		opts:    runnerOptions{AgentID: agentID, Vendor: "test", IntervalSeconds: 5},
		started: time.Now().UTC(),
		wakeCh:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}
	return rt
}

func (r *runnerRuntime) drainWake() bool {
	r.mu.Lock()
	pending := r.pendingWake
	r.pendingWake = false
	r.mu.Unlock()
	select {
	case <-r.wakeCh:
	default:
	}
	return pending
}

// A malformed/skipped inbox file (parse failure) must still wake the runner:
// gate blocks until `check` quarantines it, so the repair turn needs a poke.
func TestRunnerPoll_WakesOnMalformedFile(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")

	// First poll seeds the seen-set with an empty inbox (no wake).
	rt.pollOnce(false)
	if rt.drainWake() {
		t.Fatal("unexpected wake on seeding poll of empty inbox")
	}

	// A hand-written file with an invalid nonce — parses as skipped, not a Message.
	malformed := filepath.Join(cfg.AgentInboxDir("runner-test"), "2026-05-09T16-32-00-123456Z_from-codex_msg-zz.md")
	mustWrite(t, malformed, []byte("body"))

	rt.pollOnce(true)
	if !rt.drainWake() {
		t.Fatal("runner did not wake on a malformed (skipped) inbox file")
	}
}

// A valid new file whose sender-encoded filename timestamp sorts BEFORE the
// last-observed filename must still wake the runner. The old lexicographic-
// newest tracking would miss back-dated mail.
func TestRunnerPoll_WakesOnBackdatedFilename(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	inbox := cfg.AgentInboxDir("runner-test")

	// A "newer" message is already present and observed on the seeding poll.
	newer := filepath.Join(inbox, "2026-05-09T16-32-00-123456Z_from-codex_msg-abcd.md")
	mustWrite(t, newer, []byte("newer"))
	rt.pollOnce(false)
	if rt.drainWake() {
		t.Fatal("unexpected wake on seeding poll")
	}

	// A valid message whose filename timestamp sorts BEFORE the observed newest.
	backdated := filepath.Join(inbox, "2026-05-09T16-30-00-000000Z_from-codex_msg-bcde.md")
	mustWrite(t, backdated, []byte("back-dated"))

	rt.pollOnce(true)
	if !rt.drainWake() {
		t.Fatal("runner did not wake on a valid back-dated inbox file")
	}
}

// WI-E2: the runner's off-turn poll loop re-proves its OWN wake target each tick
// and records a cached reachability fact (ReachableAt) in its registration.
func TestRunnerPollLoop_WritesReachableAt(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	agentID := "runner-test"
	if err := loop.EnsurePrivateDir(cfg.AgentInboxDir(agentID)); err != nil {
		t.Fatal(err)
	}
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir(agentID)); err != nil {
		t.Fatal(err)
	}

	// A live runner socket the recipient owns (so the owned-check passes and the
	// dispatcher dials it).
	socketPath := cfg.RunnerSocketPath(agentID)
	startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{AgentID: agentID})
	target := loop.RunnerWakeTarget(socketPath)

	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "test",
		ControlRepo: cfg.ControlRepo,
		Host:        localHostname(),
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  target,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}

	rt := &runnerRuntime{
		cfg:     cfg,
		opts:    runnerOptions{AgentID: agentID, Vendor: "test", IntervalSeconds: 5},
		started: time.Now().UTC(),
		socket:  socketPath,
		wakeCh:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}

	rt.pollOnce(false)

	got, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if err != nil {
		t.Fatal(err)
	}
	if got.ReachableAt == nil {
		t.Fatal("pollOnce did not write ReachableAt for a live owned runner socket")
	}
	if got.ReachabilityMethod != loop.RunnerWakeMethod || got.ReachabilityTarget != target {
		t.Fatalf("reachability endpoint = %s/%s, want %s/%s", got.ReachabilityMethod, got.ReachabilityTarget, loop.RunnerWakeMethod, target)
	}
}

func runnerSocketOp(path, op string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	return json.NewEncoder(conn).Encode(map[string]string{"op": op})
}

func startFakeRunnerPingSocket(t *testing.T, path string, resp loop.RunnerPingResponse) net.Listener {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		resp.OK = true
	}
	if resp.Status == "" {
		resp.Status = "active"
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var req struct {
					Op string `json:"op"`
				}
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					return
				}
				switch req.Op {
				case "ping", "wake", "status", "shutdown":
					_ = json.NewEncoder(conn).Encode(resp)
				}
			}(conn)
		}
	}()
	return listener
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}

func setupShortRunFixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "agentchute-run-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
	return root
}

func waitForRunnerSocket(t *testing.T, target string, errCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if loop.RunnerSocketReachable(target, 100*time.Millisecond) {
			return
		}
		select {
		case err := <-errCh:
			t.Fatalf("runner exited before socket was reachable: %v", err)
		default:
		}
		_, last = loop.ParseRunnerWakeTarget(target)
		time.Sleep(50 * time.Millisecond)
	}
	if last != nil && !errors.Is(last, os.ErrNotExist) {
		t.Fatalf("runner socket never became reachable: %v", last)
	}
	t.Fatal("runner socket never became reachable")
}
