package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Simple-again Gate 6b (pull-only): the runner RECEIVE socket was removed.
// TestRunInjectsPromptOnSocketWake (socket poke -> inject), TestRunnerSocketReachableRequiresPingAck,
// TestRunShutdownSocketCleansUpRunner (socket "shutdown" op), TestRunnerPingReportsState,
// and the two TestClearStaleRunnerWakeTargets* tests were deleted with their
// subject. Injection-on-wake is now covered by the poll tests (pollOnce ->
// enqueueWake) below; id-uniqueness moved to the serve lease (see the collision +
// lease-lifecycle tests).

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

	// A FRESH valid serve lease owns "codex": a second runner must fail closed
	// (id-uniqueness now rides the lease, not a socket ping).
	lease, err := loop.AcquireServeLease(cfg, "codex")
	if err != nil {
		t.Fatalf("seed serve lease: %v", err)
	}
	if _, err := refuseLiveRunnerCollision(cfg, "codex"); err == nil {
		t.Fatal("expected live runner collision while lease held")
	} else if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("collision error = %v, want 'already active'", err)
	}

	// Once the held lease is released, a fresh runner may acquire it.
	if err := loop.ReleaseLease(lease); err != nil {
		t.Fatalf("release seed lease: %v", err)
	}
	got, err := refuseLiveRunnerCollision(cfg, "codex")
	if err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	}
	if got == nil {
		t.Fatal("nil lease returned on successful acquire")
	}
	_ = loop.ReleaseLease(got)
}

// The runner lifecycle acquires, renews (fence verify), and releases the serve
// lease. A renew while we still own the claim succeeds; a release removes it so
// a later acquire is unobstructed.
func TestRunnerLeaseAcquireRenewRelease(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := refuseLiveRunnerCollision(cfg, "runner-test")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := loop.RenewLease(lease); err != nil {
		t.Fatalf("renew held lease: %v", err)
	}
	if err := loop.ReleaseLease(lease); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Released => the claim is gone, so a new acquire wins immediately.
	again, err := refuseLiveRunnerCollision(cfg, "runner-test")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = loop.ReleaseLease(again)
}

// A runner whose lease was RECLAIMED (the live claim now carries a different
// serve_token) must detect the fence on its next poll tick and shut itself down
// cleanly instead of continuing to inject — the dup-writer guard.
func TestRunnerPollShutsDownWhenFenced(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	lease, err := loop.AcquireServeLease(cfg, "runner-test")
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	rt.lease = lease

	// Simulate a reclaim: overwrite the live claim with a different serve_token.
	reclaimed := loop.ServeClaim{
		ID:         "runner-test",
		Host:       "other-host",
		PID:        os.Getpid(),
		ServeToken: "ffffffffffffffffffffffffffffffff",
		StartedAt:  time.Now().UTC(),
		LastSeen:   time.Now().UTC(),
	}
	data, err := json.Marshal(reclaimed)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(cfg.AgentStateDir("runner-test"), "serve.claim"), data)

	rt.pollOnce(true)

	if !rt.shutdownRequested.Load() {
		t.Fatal("fenced runner did not request shutdown on the next tick")
	}
	if rt.drainWake() {
		t.Fatal("fenced runner enqueued a wake instead of shutting down")
	}
}

// The runner exports its active serve_token to the child via the environment so
// the child's sends are fenced (send.go reads AGENTCHUTE_SERVE_TOKEN).
func TestRunnerChildEnvCarriesServeToken(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	env := runnerChildEnv(cfg, runnerOptions{AgentID: "runner-test", Vendor: "test"}, "tok-abc123")
	found := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENTCHUTE_SERVE_TOKEN=") {
			found = strings.TrimPrefix(kv, "AGENTCHUTE_SERVE_TOKEN=")
		}
	}
	if found != "tok-abc123" {
		t.Fatalf("AGENTCHUTE_SERVE_TOKEN = %q, want tok-abc123", found)
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
			"--loop-dir", filepath.Join(root, ".agentchute", "loop"),
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

// Pull-only (Gate 6c): markRunnerOffline sets Status=offline. The reachability
// cache it used to clear no longer exists (registrations carry no wake state).
func TestMarkRunnerOfflineSetsOfflineStatus(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	agentID := "runner-test"
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "test",
		ControlRepo: cfg.ControlRepo,
		Host:        localHostname(),
		LastSeen:    time.Now().UTC().Add(-time.Minute),
		Status:      loop.StatusActive,
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
// gate blocks until `check` quarantines it, so the runner must still enqueue a
// wake (inject the check-inbox cue) to drive the repair turn.
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

	// A hand-written file with an unrecognized (non-seq) name — parses as
	// skipped, not a Message.
	malformed := filepath.Join(cfg.AgentInboxDir("runner-test"), "not-a-seq-name.md")
	mustWrite(t, malformed, []byte("body"))

	rt.pollOnce(true)
	if !rt.drainWake() {
		t.Fatal("runner did not wake on a malformed (skipped) inbox file")
	}
}

// A valid new file whose seq filename sorts BEFORE the last-observed filename
// (a lower seq landing after a higher one) must still wake the runner. The old
// lexicographic-newest tracking would miss such out-of-order mail.
func TestRunnerPoll_WakesOnBackdatedFilename(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	inbox := cfg.AgentInboxDir("runner-test")

	// A higher-seq message is already present and observed on the seeding poll.
	newer := filepath.Join(inbox, loop.MsgID{From: "codex", Seq: 5}.Filename())
	mustWrite(t, newer, []byte("newer"))
	rt.pollOnce(false)
	if rt.drainWake() {
		t.Fatal("unexpected wake on seeding poll")
	}

	// A valid message whose seq filename sorts BEFORE the observed newest (a
	// lower seq landing after a higher one).
	backdated := filepath.Join(inbox, loop.MsgID{From: "codex", Seq: 2}.Filename())
	mustWrite(t, backdated, []byte("back-dated"))

	rt.pollOnce(true)
	if !rt.drainWake() {
		t.Fatal("runner did not wake on a valid back-dated inbox file")
	}
}

// WI-E2 (removed): the runner's off-turn poll loop used to re-prove its OWN wake
// target each tick and record a cached reachability fact (ReachableAt) in its
// registration. Gate 6a (pull-only): TestRunnerPollLoop_WritesReachableAt was
// removed. pollOnce no longer reproves or records ReachableAt (the own-wake
// reprove call at run.go's poll tick was deleted), so there is nothing to assert.

func setupShortRunFixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "agentchute-run-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	return root
}
