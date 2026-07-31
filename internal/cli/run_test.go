package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

func TestRunnerDiagnosticsLogFileOnlyDuringRawWindow(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()

	stderr := captureStderr(t, func() {
		rt.injectPrompt()
	})
	if stderr != "" {
		t.Fatalf("raw-window diagnostic wrote stderr: %q", stderr)
	}
	log := readRunnerLog(t, cfg, "runner-test")
	if !strings.Contains(log, "agentchute serve: inject prompt:") {
		t.Fatalf("runner.log missing inject diagnostic:\n%s", log)
	}
	firstLine := strings.Split(strings.TrimSpace(log), "\n")[0]
	fields := strings.Fields(firstLine)
	if len(fields) < 2 {
		t.Fatalf("runner.log line missing timestamp/message fields: %q", firstLine)
	}
	if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
		t.Fatalf("runner.log timestamp = %q, want RFC3339: %v", fields[0], err)
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

	rt.pollOnce()

	if !rt.shutdownRequested.Load() {
		t.Fatal("fenced runner did not request shutdown on the next tick")
	}
	if rt.drainWake() {
		t.Fatal("fenced runner enqueued a wake instead of shutting down")
	}
}

func TestRunnerInvalidatedLeaseLogsC15NoticeAndBuffersFatal(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()
	lease, err := loop.AcquireServeLease(cfg, "runner-test")
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	rt.lease = lease

	invalidated, err := loop.InvalidateAllServeLeases(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated != 1 {
		t.Fatalf("invalidated = %d, want 1", invalidated)
	}

	stderr := captureStderr(t, func() {
		rt.pollOnce()
	})
	if stderr != "" {
		t.Fatalf("fenced shutdown wrote raw stderr: %q", stderr)
	}
	if !rt.shutdownRequested.Load() {
		t.Fatal("fenced runner did not request shutdown")
	}
	log := readRunnerLog(t, cfg, "runner-test")
	want := "serve: this agentchute binary was fenced out (update or identity reclaim). Restart this lane: ac serve <wrapper>"
	if !strings.Contains(log, want) {
		t.Fatalf("runner.log missing fenced fatal:\n%s", log)
	}

	var postRestore bytes.Buffer
	restoreRunnerTerminal(func() error { return nil }, rt.diag, &postRestore)
	if got := postRestore.String(); got != want+"\n" {
		t.Fatalf("post-restore fatal = %q, want %q", got, want+"\n")
	}
}

func TestRestoreRunnerTerminalBuffersRestoreFailure(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	diag := newRunnerDiagnostics(cfg, "runner-test")
	defer diag.close()

	var stderr bytes.Buffer
	restoreRunnerTerminal(func() error {
		return errors.New("raw mode still active")
	}, diag, &stderr)

	want := "agentchute serve: restore terminal: raw mode still active"
	if got := stderr.String(); got != want+"\n" {
		t.Fatalf("restore fatal = %q, want %q", got, want+"\n")
	}
	log := readRunnerLog(t, cfg, "runner-test")
	if !strings.Contains(log, want) {
		t.Fatalf("runner.log missing restore fatal:\n%s", log)
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

// TestRunnerChildEnvStripsInheritedGuardBit is the v2.5 A7/C22 guard-enablement
// contract (codex review, PR #89 finding #2): an unguarded wrapper (Guarded:
// false, e.g. grok) must never appear armed just because the PROCESS running
// serve itself inherited AGENTCHUTE_GUARD=1 from a guarded parent session
// (e.g. `ac serve grok` launched from inside a guarded claude-code session).
// runnerChildEnv must strip any inherited value before conditionally
// re-adding it, not merely rely on always overriding it (which only works
// when Guarded is true).
func TestRunnerChildEnvStripsInheritedGuardBit(t *testing.T) {
	t.Setenv("AGENTCHUTE_GUARD", "1") // simulates a guarded parent process's env
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	env := runnerChildEnv(cfg, runnerOptions{AgentID: "grok", Vendor: "xai", Guarded: false}, "tok-grok")
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENTCHUTE_GUARD=") {
			t.Fatalf("unguarded child (Guarded:false) carries AGENTCHUTE_GUARD from the parent's inherited env: %q\nfull env: %v", kv, env)
		}
	}
}

// A guarded wrapper still gets the bit even when the parent process happens
// to have it unset — the positive counterpart to the strip test above.
func TestRunnerChildEnvSetsGuardBitWhenGuarded(t *testing.T) {
	t.Setenv("AGENTCHUTE_GUARD", "")
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	env := runnerChildEnv(cfg, runnerOptions{AgentID: "claude-code", Vendor: "anthropic", Guarded: true}, "tok-claude")
	found := false
	for _, kv := range env {
		if kv == "AGENTCHUTE_GUARD=1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("guarded child (Guarded:true) missing AGENTCHUTE_GUARD=1: %v", env)
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
		if err := cmdServe([]string{
			"--as", "codex",
			"--vendor", "openai",
			"--control-repo", root,
			"--loop-dir", filepath.Join(root, ".agentchute", "loop"),
			"--interval", "5",
			"--idle-grace", "100ms",
			"--", script,
		}); err != nil {
			t.Fatalf("cmdServe err = %v", err)
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
		wakeCh:  make(chan bool, 1),
		stopCh:  make(chan struct{}),
	}
	return rt
}

func (r *runnerRuntime) drainWake() bool {
	_, had := r.drainWakeRecue()
	return had
}

// drainWakeRecue drains a queued wake (if any) and reports its recue value.
// had=false means no wake was queued (pendingWake was already false); recue
// is meaningless when had is false.
func (r *runnerRuntime) drainWakeRecue() (recue bool, had bool) {
	r.mu.Lock()
	had = r.pendingWake
	r.pendingWake = false
	r.mu.Unlock()
	select {
	case recue = <-r.wakeCh:
	default:
	}
	return recue, had
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	t.Cleanup(func() { _ = r.Close() })
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func readRunnerLog(t *testing.T, cfg *loop.Config, agentID string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.AgentStateDir(agentID), "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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

	// First poll: empty inbox, no wake.
	rt.pollOnce()
	if rt.drainWake() {
		t.Fatal("unexpected wake on poll of empty inbox")
	}

	// A hand-written file with an unrecognized (non-seq) name — parses as
	// skipped, not a Message.
	malformed := filepath.Join(cfg.AgentInboxDir("runner-test"), "not-a-seq-name.md")
	mustWrite(t, malformed, []byte("body"))

	rt.pollOnce()
	if !rt.drainWake() {
		t.Fatal("runner did not wake on a malformed (skipped) inbox file")
	}
}

// A2 (v2.5 plan): the runner's first ("seeding") poll no longer silently
// snapshots pre-existing mail into a seen-set before deciding whether to wake
// — startup mail cues immediately, same as mail arriving mid-session. This
// inverts the pre-A2 assertion (INVERT per the plan). The scenario this test
// used to guard against (a lexicographic-older seq filename landing after a
// newer one, defeating "newest observed" tracking) no longer applies: pollOnce
// has no per-filename seen-set left to fool, only a pending/not-pending
// (hasPendingInboxMail) check.
func TestRunnerPoll_WakesOnBackdatedFilename(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	inbox := cfg.AgentInboxDir("runner-test")

	// A message is already present before the runner ever polls.
	newer := filepath.Join(inbox, loop.MsgID{From: "codex", Seq: 5}.Filename())
	mustWrite(t, newer, []byte("newer"))

	rt.pollOnce()
	recue, had := rt.drainWakeRecue()
	if !had {
		t.Fatal("runner did not wake on pre-existing (startup) inbox mail")
	}
	if recue {
		t.Fatal("startup cue was recue=true, want the first-of-period recue=false")
	}
}

// WI-E2 (removed): the runner's off-turn poll loop used to re-prove its OWN wake
// target each tick and record a cached reachability fact (ReachableAt) in its
// registration. Gate 6a (pull-only): TestRunnerPollLoop_WritesReachableAt was
// removed. pollOnce no longer reproves or records ReachableAt (the own-wake
// reprove call at serve.go's poll tick was deleted), so there is nothing to assert.

// TestRunnerRecuesAfterInterval proves the C16 re-cue predicate end to end:
// the first poll on newly-pending mail cues immediately (recue=false); an
// immediate re-poll (interval not elapsed, mail still pending) does not
// re-queue; once recueInterval elapses with the mail still unread, the next
// poll re-cues with recue=true.
func TestRunnerRecuesAfterInterval(t *testing.T) {
	origInterval := recueInterval
	// 300ms (not 50ms, code review fix): the no-early-recue assertion below
	// needs real headroom against scheduler stalls on this repo's known-flaky
	// macOS CI runners — a >50ms stall between setting lastInjection and the
	// second pollOnce would fail it spuriously.
	recueInterval = 300 * time.Millisecond
	t.Cleanup(func() { recueInterval = origInterval })

	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("runner-test"), "not-a-seq-name.md"), []byte("body"))

	// First poll: period start, immediate cue, recue=false.
	rt.pollOnce()
	recue, had := rt.drainWakeRecue()
	if !had || recue {
		t.Fatalf("first poll: had=%v recue=%v, want had=true recue=false", had, recue)
	}

	// Simulate the cue having actually been injected, as injectPrompt would.
	rt.mu.Lock()
	rt.lastInjection = time.Now().UTC()
	rt.pendingWake = false
	rt.injectedThisPeriod = true
	rt.mu.Unlock()

	// Mail is still pending and the (shrunken) interval has not elapsed: no
	// re-cue yet.
	rt.pollOnce()
	if rt.drainWake() {
		t.Fatal("re-cued before recueInterval elapsed")
	}

	// After the interval elapses, the next poll re-cues, recue=true.
	time.Sleep(recueInterval + 100*time.Millisecond)
	rt.pollOnce()
	recue, had = rt.drainWakeRecue()
	if !had || !recue {
		t.Fatalf("after interval: had=%v recue=%v, want had=true recue=true", had, recue)
	}
}

// TestRunnerRecueWaitsForIdle proves C17: a re-cue (recue=true) must NEVER
// escalate via Ctrl-C, even under --interrupt-policy=always, while the
// wrapper is busy — it waits for idle unconditionally. The Ctrl-C escalation
// is reserved for the first injection of a pending period (recue=false).
func TestRunnerRecueWaitsForIdle(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.opts.InterruptPolicy = interruptAlways
	rt.opts.IdleGrace = time.Hour // never idle within this test's lifetime

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	rt.ptmx = pw
	rt.lastOutputUnixNano.Store(time.Now().UnixNano()) // force "busy"

	done := make(chan bool, 1)
	go func() { done <- rt.waitForInjectionWindow(true) }()

	select {
	case <-done:
		t.Fatal("waitForInjectionWindow(recue=true) returned while busy under always policy — must wait for idle, not escalate")
	case <-time.After(500 * time.Millisecond):
	}

	close(rt.stopCh)
	if got := <-done; got {
		t.Fatal("waitForInjectionWindow should return false on shutdown, not true")
	}

	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotBytes) != 0 {
		t.Fatalf("waitForInjectionWindow(recue=true) wrote %q, want no bytes (no Ctrl-C escalation on a re-cue)", gotBytes)
	}
}

// TestRunnerStartupMailCues proves the wiring, not just the pollOnce unit:
// pollLoop's own first (pre-ticker) call cues mail that was present before
// the runner ever started, with recue=false (the first-of-period treatment).
func TestRunnerStartupMailCues(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.opts.IntervalSeconds = 5 // ticker must not fire during this test
	inbox := cfg.AgentInboxDir("runner-test")
	mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\nto: runner-test\n---\n\nhi\n"))

	rt.pollWG.Add(1)
	go rt.pollLoop()
	t.Cleanup(func() {
		close(rt.stopCh)
		rt.pollWG.Wait()
	})

	select {
	case recue := <-rt.wakeCh:
		if recue {
			t.Fatal("startup cue was recue=true, want the first-of-period recue=false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop did not cue pre-existing (startup) mail")
	}
}

// TestInjectIfPendingSkipClearsPendingWake proves the stale-flag fix (A2):
// when injectIfPending's re-check finds the inbox already drained (the M4
// claim race), it must clear pendingWake — not leave runner.json reporting
// pending_wake:true against an empty inbox forever.
func TestInjectIfPendingSkipClearsPendingWake(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()

	inbox := cfg.AgentInboxDir("runner-test")
	mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\nto: runner-test\n---\n\nhi\n"))
	msgs, _, err := loop.ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("seed: got %d inbox messages, want 1", len(msgs))
	}
	// Simulate the claim race: `check` (from an earlier cue) claims the mail
	// before THIS wake gets to inject.
	if _, err := loop.ClaimMessage(msgs[0], cfg.AgentClaimedDir("runner-test")); err != nil {
		t.Fatal(err)
	}

	rt.mu.Lock()
	rt.pendingWake = true
	rt.mu.Unlock()
	if err := rt.saveState(); err != nil {
		t.Fatal(err)
	}

	rt.injectIfPending()

	rt.mu.Lock()
	stillPending := rt.pendingWake
	rt.mu.Unlock()
	if stillPending {
		t.Fatal("injectIfPending's skip path left pendingWake stuck true on an already-drained inbox")
	}

	got, err := loop.LoadRunnerState(cfg, "runner-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingWake {
		t.Fatal("runner.json still shows pending_wake:true after an inject attempt on a drained inbox")
	}
}

// TestInjectPromptFailureClearsPendingWake is a code-review-fix regression
// test: a failed injection attempt (e.g. a PTY I/O error while the child
// survives — rt.ptmx is nil here, which writePTY treats identically) must
// still clear pendingWake. Under A2's pendingWake gate in enqueueWake, a
// stuck-true flag here would permanently suppress every future wake for the
// runner's lifetime — including brand-new, unrelated mail arriving later —
// defeating the whole point of this slice. Pre-A2 code self-healed because
// enqueueWake's channel send was ungated; A2 introduced this failure mode.
func TestInjectPromptFailureClearsPendingWake(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()
	inbox := cfg.AgentInboxDir("runner-test")
	mustWrite(t, filepath.Join(inbox, "not-a-seq-name.md"), []byte("body"))

	// First poll: period start, queues a wake (recue=false). Mail stays
	// pending throughout this test (never claimed).
	rt.pollOnce()
	if _, had := rt.drainWakeRecue(); !had {
		t.Fatal("first poll did not queue a wake")
	}
	// The test helper's drain cleared pendingWake as a side effect of
	// inspecting the channel; restore it to simulate injectLoop's real
	// consume-then-wait sequence (wake received, pendingWake still set) right
	// before it calls injectIfPending -> injectPrompt.
	rt.mu.Lock()
	rt.pendingWake = true
	rt.mu.Unlock()

	rt.injectIfPending() // hasPendingInboxMail is true -> injectPrompt -> writePTY fails (ptmx is nil)

	rt.mu.Lock()
	stillLatched := rt.pendingWake
	rt.mu.Unlock()
	if stillLatched {
		t.Fatal("a failed injection left pendingWake stuck true — every future wake, including brand-new mail, would be silently suppressed forever")
	}

	// A later poll, with the mail still genuinely pending, must still be able
	// to queue a fresh wake — proving the runner is not permanently jammed.
	// It must be recue=FALSE (review fix, codex finding #1): a failed
	// attempt never delivered a first cue, so the retry still deserves the
	// configured --interrupt-policy escalation, not a silent recue=true —
	// otherwise a continuously busy wrapper under always/after-grace could
	// suppress the escalation retry indefinitely (recue=true always just
	// waits for idle, regardless of policy).
	rt.pollOnce()
	recue, had := rt.drainWakeRecue()
	if !had {
		t.Fatal("runner did not re-cue after a failed injection attempt")
	}
	if recue {
		t.Fatal("retry after a failed injection was queued recue=true — the escalation-eligible first-cue treatment was lost")
	}
}

// TestRunnerFailedInjectionStillEscalatesOnRetry is codex finding #1's most
// direct reproduction: under always-interrupt-policy with a continuously busy
// wrapper, a FAILED first injection attempt must not downgrade the period —
// the very next retry attempt must still send Ctrl-C (escalate) rather than
// silently switching to after-idle-only waiting, which could suppress the
// retry for as long as the wrapper stays busy.
func TestRunnerFailedInjectionStillEscalatesOnRetry(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()
	rt.opts.InterruptPolicy = interruptAlways
	rt.opts.IdleGrace = time.Hour // never idle within this test's lifetime
	rt.lastOutputUnixNano.Store(time.Now().UnixNano())

	inbox := cfg.AgentInboxDir("runner-test")
	mustWrite(t, filepath.Join(inbox, "not-a-seq-name.md"), []byte("body"))

	// First poll queues the first cue (recue=false); simulate the failed
	// injection attempt (ptmx is nil, so injectPrompt's writePTY fails).
	rt.pollOnce()
	if _, had := rt.drainWakeRecue(); !had {
		t.Fatal("first poll did not queue a wake")
	}
	rt.mu.Lock()
	rt.pendingWake = true
	rt.mu.Unlock()
	rt.injectIfPending()

	// Next poll's retry must still be recue=false...
	rt.pollOnce()
	recue, had := rt.drainWakeRecue()
	if !had || recue {
		t.Fatalf("retry after failed injection: had=%v recue=%v, want had=true recue=false", had, recue)
	}

	// ...and waitForInjectionWindow(false) under always-policy, while still
	// busy, must escalate (send Ctrl-C) rather than block waiting for idle.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	rt.ptmx = pw

	done := make(chan bool, 1)
	go func() { done <- rt.waitForInjectionWindow(recue) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitForInjectionWindow returned false unexpectedly")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForInjectionWindow(recue=false) under always-policy did not escalate while busy — the retry lost its first-cue treatment")
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotBytes), "\x03") {
		t.Fatalf("waitForInjectionWindow did not write Ctrl-C; got %q", gotBytes)
	}
}

// TestRunnerDrainedPeriodResetsThroughProductionPath is codex finding #2's
// production-path reproduction: injectIfPending's own drained-mail skip path
// (not a manually-set field) must reset the period, so mail arriving right
// after is treated as genuinely new (recue=false within one tick) instead of
// being misclassified as a continuation of the just-ended period.
func TestRunnerDrainedPeriodResetsThroughProductionPath(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()
	inbox := cfg.AgentInboxDir("runner-test")

	first := filepath.Join(inbox, loop.MsgID{From: "codex", Seq: 1}.Filename())
	mustWrite(t, first, []byte("first"))

	// Establish the precondition a real successful injection would leave:
	// the period has already delivered a cue this round.
	rt.mu.Lock()
	rt.injectedThisPeriod = true
	rt.pendingWake = true
	rt.mu.Unlock()

	// Simulate the message being claimed (drained) by the agent's own check,
	// then drive injectIfPending's ACTUAL skip path (production code, not a
	// manually-set field) to observe the now-empty inbox and reset the period.
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	rt.injectIfPending()
	rt.mu.Lock()
	stillTracking := rt.injectedThisPeriod
	rt.mu.Unlock()
	if stillTracking {
		t.Fatal("injectIfPending's drained-mail skip path did not reset injectedThisPeriod")
	}

	// A brand-new message arriving right after must cue within one tick,
	// recue=false — not wait out recueInterval as a misclassified continuation.
	second := filepath.Join(inbox, loop.MsgID{From: "codex", Seq: 2}.Filename())
	mustWrite(t, second, []byte("second"))
	rt.pollOnce()
	recue, had := rt.drainWakeRecue()
	if !had {
		t.Fatal("new mail after a drained period did not cue within one tick")
	}
	if recue {
		t.Fatal("new mail after a drained period was queued recue=true — the period reset did not take effect")
	}
}

// TestSaveStateSerializesSnapshotAndWrite is codex finding #3's reproduction:
// saveStateWithStatus's snapshot and its durable write must be one atomic
// critical section under r.mu. A second concurrent caller must be unable to
// even mutate shared state (let alone complete its own write) while the
// first is mid-write — otherwise a slow writer holding a stale snapshot
// could rename its file AFTER a faster, fresher concurrent write, silently
// resurrecting stale runner.json state.
func TestSaveStateSerializesSnapshotAndWrite(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")

	release := make(chan struct{})
	entered := make(chan struct{})
	afterSaveStateSnapshotHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { afterSaveStateSnapshotHook = nil })

	rt.mu.Lock()
	rt.pendingWake = true
	rt.mu.Unlock()

	firstDone := make(chan struct{})
	go func() {
		_ = rt.saveState() // snapshots pendingWake=true, then pauses mid-write (still holding r.mu)
		close(firstDone)
	}()
	<-entered

	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		close(secondStarted)
		rt.mu.Lock() // must block: the first call still holds r.mu
		rt.pendingWake = false
		rt.mu.Unlock()
		_ = rt.saveState()
		close(secondDone)
	}()
	<-secondStarted

	select {
	case <-secondDone:
		t.Fatal("second caller mutated state and wrote while the first still held the lock mid-write — snapshot+write is not one atomic critical section")
	case <-time.After(100 * time.Millisecond):
		// expected: still blocked on r.mu.
	}

	// Clear the hook before releasing: it already fired for the first
	// (in-flight) call, and must not fire again for the second caller's own
	// saveState call once the lock frees up (the hook closure isn't
	// reentrant-safe across two separate saveStateWithStatus invocations).
	afterSaveStateSnapshotHook = nil
	close(release)
	<-firstDone
	<-secondDone

	got, err := loop.LoadRunnerState(cfg, "runner-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.PendingWake {
		t.Fatal("the first (stale, pendingWake=true) write landed after the second (fresh, pendingWake=false) write — writes are not serialized in snapshot order")
	}
}

// TestRunnerNewPeriodCuesWithinOneTickDespiteRecentInjection pins the
// periodStart disjunct in pollOnce's cue predicate (an undisclosed-until-
// review third deviation from C16's literal text, now disclosed in the PR
// body): a genuinely NEW pending period must cue immediately, recue=false,
// even when lastInjection is recent from a PREVIOUS, already-resolved period.
// C16's literal predicate alone (lastInjection.IsZero() ||
// elapsed>=recueInterval) would wrongly delay this new arrival until
// recueInterval had passed since the unrelated prior period's injection,
// violating "mail arrives mid-session: cued within one tick." If periodStart
// is ever removed from the predicate, this test must fail.
func TestRunnerNewPeriodCuesWithinOneTickDespiteRecentInjection(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	inbox := cfg.AgentInboxDir("runner-test")

	// A previous, unrelated period was injected moments ago and has since
	// fully resolved (mail claimed elsewhere, injectedThisPeriod/pendingWake
	// clear).
	rt.mu.Lock()
	rt.lastInjection = time.Now().UTC()
	rt.injectedThisPeriod = false
	rt.pendingWake = false
	rt.mu.Unlock()

	// A brand-new message arrives, starting a genuinely new period.
	mustWrite(t, filepath.Join(inbox, "not-a-seq-name.md"), []byte("body"))

	rt.pollOnce()
	recue, had := rt.drainWakeRecue()
	if !had {
		t.Fatal("a new pending period did not cue within one tick despite a recent prior-period injection")
	}
	if recue {
		t.Fatal("a new period's first cue was recue=true, want recue=false")
	}
}

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
