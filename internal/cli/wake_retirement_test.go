package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// wake_retirement_test.go — AGENTCHUTE.md §8 "wake retirement" (decision
// package 2026-09-17, item C). codex-tui 0.154 repaints its composer every
// 150 ms, so serve's idle heuristic (2 s of PTY silence) never fires and the
// queued wake used to park inside waitForInjectionWindow for the lane's whole
// life — pending_wake true, last_injection never set — and was NOT retired
// when the inbox drained. These tests pin the fix: an observed-empty inbox
// cancels the waiting attempt (the waiter actually stops), at most one
// attempt is outstanding per pending period, and an old attempt can neither
// inject into nor clear the state of a newer period.

// busyChild fakes a wrapper that emits PTY output every 100 ms (a repainting
// TUI) — never idle under a 2 s IdleGrace. The returned func stops it.
func busyChild(t *testing.T, rt *runnerRuntime) (stop func()) {
	t.Helper()
	rt.opts.IdleGrace = 2 * time.Second
	rt.lastOutputUnixNano.Store(time.Now().UnixNano())
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				rt.lastOutputUnixNano.Store(time.Now().UnixNano())
			}
		}
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(done)
		<-finished
	}
}

// waitUntil polls cond every 20 ms until it holds or the deadline passes.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// attachPTYPipe gives rt a write end to inject into and returns the read end;
// the caller reads it after closePTY.
func attachPTYPipe(t *testing.T, rt *runnerRuntime) *os.File {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pr.Close() })
	rt.ptmx = pw
	return pr
}

func readPTYAfterClose(t *testing.T, rt *runnerRuntime, pr *os.File) string {
	t.Helper()
	rt.closePTY()
	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// newRetirementRuntime builds a runtime with diagnostics and the wake
// machinery wired, ready for injectLoop.
func newRetirementRuntime(t *testing.T) (*loop.Config, *runnerRuntime) {
	t.Helper()
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.opts.Prompt = defaultRunnerPrompt // newPollTestRuntime leaves it empty; cue counting needs the real text
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	t.Cleanup(rt.diag.close)
	return cfg, rt
}

func startInjectLoop(t *testing.T, rt *runnerRuntime) {
	t.Helper()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		rt.injectLoop()
	}()
	t.Cleanup(func() {
		rt.stopLoops()
		<-loopDone
	})
}

// TestWakeRetiredWhenInboxDrainsWhileBusy is the load-bearing scenario: a
// continuously busy child, mail arrives (wake queued and taken by injectLoop,
// which parks waiting for idle), the inbox drains while it waits. The next
// poll must retire the attempt — pending_wake false, the waiter actually gone
// (proved by injectLoop being free to take the NEXT period's attempt while the
// child is still busy) — and nothing is ever written for the drained mail.
// The new period then cues exactly once, recue=false, when the child idles.
func TestWakeRetiredWhenInboxDrainsWhileBusy(t *testing.T) {
	cfg, rt := newRetirementRuntime(t)
	stopBusy := busyChild(t, rt)
	defer stopBusy()
	pr := attachPTYPipe(t, rt)
	startInjectLoop(t, rt)

	inbox := cfg.AgentInboxDir("runner-test")
	first := filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 1}.Filename())
	mustWrite(t, first, []byte("---\nfrom: peer\nto: runner-test\n---\n\nhi\n"))

	rt.pollOnce()
	a1 := rt.outstandingWake()
	if a1 == nil || a1.recue {
		t.Fatalf("first poll: attempt=%+v, want an outstanding recue=false attempt", a1)
	}
	waitUntil(t, 2*time.Second, func() bool { return len(rt.wakeCh) == 0 },
		"injectLoop never took the queued attempt")
	// Many polls with the mail still pending and the child still busy: the
	// same single attempt stays outstanding — nothing queues behind it.
	for i := 0; i < 3; i++ {
		rt.pollOnce()
	}
	if got := rt.outstandingWake(); got != a1 {
		t.Fatalf("after repeated polls the outstanding attempt changed: got %+v, want the original %+v", got, a1)
	}

	// The inbox drains (the agent's own check claimed it, or a peer withdrew
	// it) while the attempt is still waiting for an idle window that never
	// comes.
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	rt.pollOnce()
	if got := rt.outstandingWake(); got != nil {
		t.Fatalf("observed-empty poll left attempt %d outstanding", got.id)
	}
	st, err := loop.LoadRunnerState(cfg, "runner-test")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingWake || st.WakeAttempt != 0 {
		t.Fatalf("runner.json after retirement: pending_wake=%v wake_attempt=%d, want false/0", st.PendingWake, st.WakeAttempt)
	}
	if !st.LastInjection.IsZero() {
		t.Fatal("runner.json records an injection for mail that was never cued")
	}
	waitUntil(t, 2*time.Second, func() bool {
		return strings.Contains(readRunnerLog(t, cfg, "runner-test"), "wake attempt 1 wait cancelled")
	}, "the waiter did not observe its own retirement")

	// A NEW period: mail again, child still busy. injectLoop must be free to
	// take this attempt — had the old waiter still been parked, wakeCh would
	// stay full behind it.
	second := filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 2}.Filename())
	mustWrite(t, second, []byte("---\nfrom: peer\nto: runner-test\n---\n\nagain\n"))
	rt.pollOnce()
	a2 := rt.outstandingWake()
	if a2 == nil || a2 == a1 || a2.recue {
		t.Fatalf("new period: attempt=%+v, want a fresh recue=false attempt", a2)
	}
	waitUntil(t, 2*time.Second, func() bool { return len(rt.wakeCh) == 0 },
		"injectLoop did not take the new period's attempt — the retired waiter is still parked")

	// The child goes idle: exactly one cue is written, for the new period.
	stopBusy()
	rt.lastOutputUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	waitUntil(t, 3*time.Second, func() bool { return rt.outstandingWake() == nil },
		"the new period's attempt never completed once the child idled")
	got := readPTYAfterClose(t, rt, pr)
	if n := strings.Count(got, rt.opts.Prompt); n != 1 {
		t.Fatalf("PTY received %d cues (%q), want exactly 1 for the new period and none for the drained one", n, got)
	}
	rt.mu.Lock()
	injected, last := rt.injectedThisPeriod, rt.lastInjection
	rt.mu.Unlock()
	if !injected || last.IsZero() {
		t.Fatal("the delivered cue did not mark the new period as injected")
	}
}

// TestWakeDrainAndNewMailBetweenPollsStaysOnePeriod: the inbox drains and
// refills between two polls, so no poll ever observes it empty. The runner
// has no period boundary to act on: the one outstanding attempt stays, and
// when the child idles it delivers exactly one cue (which covers the new
// mail). No second attempt queues behind it.
func TestWakeDrainAndNewMailBetweenPollsStaysOnePeriod(t *testing.T) {
	cfg, rt := newRetirementRuntime(t)
	stopBusy := busyChild(t, rt)
	defer stopBusy()
	pr := attachPTYPipe(t, rt)
	startInjectLoop(t, rt)

	inbox := cfg.AgentInboxDir("runner-test")
	first := filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 1}.Filename())
	mustWrite(t, first, []byte("first"))
	rt.pollOnce()
	a1 := rt.outstandingWake()
	if a1 == nil {
		t.Fatal("first poll queued nothing")
	}
	waitUntil(t, 2*time.Second, func() bool { return len(rt.wakeCh) == 0 }, "injectLoop never took the attempt")

	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 2}.Filename()), []byte("second"))
	rt.pollOnce()
	if got := rt.outstandingWake(); got != a1 {
		t.Fatalf("a poll that never saw the inbox empty replaced the attempt: got %+v want %+v", got, a1)
	}

	stopBusy()
	rt.lastOutputUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	waitUntil(t, 3*time.Second, func() bool { return rt.outstandingWake() == nil }, "attempt never completed once idle")
	if n := strings.Count(readPTYAfterClose(t, rt, pr), rt.opts.Prompt); n != 1 {
		t.Fatalf("PTY received %d cues, want exactly 1", n)
	}
}

// TestStaleWakeAttemptCannotTouchNewerPeriod pins §8's ownership rule at
// both recheck points. a1 was retired (inbox drained) and a2 is the newer
// period's outstanding attempt. A still-running a1 waiter reaching
// injectIfPending must not inject (mail is pending, but not its mail); a1
// reaching injectPrompt's completion (the write already raced through) must
// not clear a2 or mark the new period injected; and retiring a1 again is a
// no-op on a2.
func TestStaleWakeAttemptCannotTouchNewerPeriod(t *testing.T) {
	cfg, rt := newRetirementRuntime(t)
	pr := attachPTYPipe(t, rt)
	inbox := cfg.AgentInboxDir("runner-test")

	first := filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 1}.Filename())
	mustWrite(t, first, []byte("first"))
	rt.pollOnce()
	a1 := rt.takeQueuedWake()
	if a1 == nil {
		t.Fatal("first poll queued nothing")
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	rt.pollOnce() // observed empty: a1 retired
	select {
	case <-a1.retired:
	default:
		t.Fatal("a1 was not retired by the observed-empty poll")
	}
	mustWrite(t, filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 2}.Filename()), []byte("second"))
	rt.pollOnce()
	a2 := rt.takeQueuedWake()
	if a2 == nil || a2 == a1 {
		t.Fatalf("new period queued %+v, want a fresh attempt", a2)
	}

	// Recheck point 1: the stale waiter reaches injectIfPending with mail
	// pending. It does not own the period — no injection, a2 untouched.
	rt.injectIfPending(a1)
	if got := rt.outstandingWake(); got != a2 {
		t.Fatalf("stale injectIfPending changed the outstanding attempt to %+v", got)
	}
	if !strings.Contains(readRunnerLog(t, cfg, "runner-test"), "wake attempt 1 is stale") {
		t.Fatal("stale attempt was not logged as stale")
	}

	// A retirement aimed at a1 must not touch a2.
	rt.mu.Lock()
	rt.retireWakeLocked(a1, "test")
	rt.mu.Unlock()
	if got := rt.outstandingWake(); got != a2 {
		t.Fatalf("retiring the stale attempt cleared the newer one: got %+v", got)
	}

	// Recheck point 2: the stale attempt's write already went through (the
	// unavoidable window between the ownership check and the PTY write).
	// Completion must leave a2 outstanding and the new period un-injected.
	rt.injectPrompt(a1)
	if got := rt.outstandingWake(); got != a2 {
		t.Fatalf("stale completion cleared the newer attempt: got %+v", got)
	}
	rt.mu.Lock()
	injected, last := rt.injectedThisPeriod, rt.lastInjection
	rt.mu.Unlock()
	if injected || !last.IsZero() {
		t.Fatalf("stale completion marked the newer period injected (injected=%v last=%v)", injected, last)
	}
	st, err := loop.LoadRunnerState(cfg, "runner-test")
	if err != nil {
		t.Fatal(err)
	}
	if !st.PendingWake || st.WakeAttempt != a2.id {
		t.Fatalf("runner.json lost the newer period: pending_wake=%v wake_attempt=%d want true/%d", st.PendingWake, st.WakeAttempt, a2.id)
	}

	// The legitimate owner still completes normally.
	rt.injectIfPending(a2)
	if rt.outstandingWake() != nil {
		t.Fatal("the owning attempt did not complete")
	}
	if n := strings.Count(readPTYAfterClose(t, rt, pr), rt.opts.Prompt); n != 2 {
		t.Fatalf("PTY received %d cues, want 2 (the raced stale write + the owner's)", n)
	}
}

// TestWakeNotRetiredOnMalformedOnlyInbox: a malformed-only inbox is still
// pending (check must quarantine it), so it neither retires a waiting attempt
// nor stops cueing.
func TestWakeNotRetiredOnMalformedOnlyInbox(t *testing.T) {
	cfg, rt := newRetirementRuntime(t)
	inbox := cfg.AgentInboxDir("runner-test")
	good := filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 1}.Filename())
	mustWrite(t, good, []byte("good"))
	mustWrite(t, filepath.Join(inbox, "not-a-seq-name.md"), []byte("body"))
	rt.pollOnce()
	a := rt.takeQueuedWake()
	if a == nil {
		t.Fatal("first poll queued nothing")
	}
	if err := os.Remove(good); err != nil {
		t.Fatal(err)
	}
	rt.pollOnce()
	if got := rt.outstandingWake(); got != a {
		t.Fatalf("a malformed-only inbox retired the attempt: got %+v", got)
	}
	select {
	case <-a.retired:
		t.Fatal("attempt cancelled although a malformed file still needs a cue")
	default:
	}
}

// TestWakeNotRetiredOnFailedListing: a listing failure is not evidence of an
// empty inbox. The tick reports Pending:1 (fail open), so the attempt stays.
func TestWakeNotRetiredOnFailedListing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfg, rt := newRetirementRuntime(t)
	inbox := cfg.AgentInboxDir("runner-test")
	mustWrite(t, filepath.Join(inbox, loop.MsgID{From: "peer", Seq: 1}.Filename()), []byte("hi"))
	rt.pollOnce()
	a := rt.takeQueuedWake()
	if a == nil {
		t.Fatal("first poll queued nothing")
	}
	if err := os.Chmod(inbox, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inbox, 0o700) })
	rt.pollOnce()
	if got := rt.outstandingWake(); got != a {
		t.Fatalf("a failed listing retired the attempt: got %+v", got)
	}
	select {
	case <-a.retired:
		t.Fatal("attempt cancelled on a listing failure")
	default:
	}
	if err := os.Chmod(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	// Once the listing works again and the inbox is genuinely empty, the
	// same attempt retires.
	if err := os.RemoveAll(inbox); err != nil {
		t.Fatal(err)
	}
	if err := loop.EnsurePrivateDir(inbox); err != nil {
		t.Fatal(err)
	}
	rt.pollOnce()
	if rt.outstandingWake() != nil {
		t.Fatal("attempt survived an observed-empty poll after the listing recovered")
	}
}

// TestWakeShutdownMidWaitExitsCleanly: shutdown while an attempt is waiting
// makes the waiter return false without injecting, and injectLoop exits.
func TestWakeShutdownMidWaitExitsCleanly(t *testing.T) {
	cfg, rt := newRetirementRuntime(t)
	stopBusy := busyChild(t, rt)
	defer stopBusy()
	pr := attachPTYPipe(t, rt)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		rt.injectLoop()
	}()

	mustWrite(t, filepath.Join(cfg.AgentInboxDir("runner-test"), loop.MsgID{From: "peer", Seq: 1}.Filename()), []byte("hi"))
	rt.pollOnce()
	waitUntil(t, 2*time.Second, func() bool { return len(rt.wakeCh) == 0 }, "injectLoop never took the attempt")

	rt.requestShutdown(0) // no child process: stopLoops + shutdownRequested only
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("injectLoop did not exit on shutdown while an attempt was waiting")
	}
	if got := readPTYAfterClose(t, rt, pr); got != "" {
		t.Fatalf("shutdown mid-wait wrote %q to the PTY", got)
	}
}

// TestRemoteTickEmptyRetiresWake: a remote lane has no raw inbox to list —
// the hub tick's pending/skipped result is the observation. Pending on one
// tick queues the attempt; an empty result on the next retires it. A tick
// ERROR (channel loss) is not an empty inbox: the attempt is left alone and
// the existing channel-loss shutdown path runs.
func TestRemoteTickEmptyRetiresWake(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Remote:      &loop.RemoteConfig{URL: "ssh://hub.example/pool", Host: "hub.example", Port: 22},
	}
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir("remote-test")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRemoteServeChannel{token: "remote-token"}
	pending := 1
	fake.tickFn = func() (op.TickResp, error) { return op.TickResp{Pending: pending}, nil }
	opts := runnerOptions{AgentID: "remote-test", Vendor: "test", IntervalSeconds: 5, InterruptPolicy: interruptAfterIdle, Prompt: defaultRunnerPrompt, IdleGrace: time.Hour}
	rt := newRunnerRuntime(cfg, opts, root, nil, loop.Registration{}, fake)
	rt.diag = newRunnerDiagnostics(cfg, "remote-test")
	defer rt.diag.close()

	rt.pollOnce()
	a := rt.takeQueuedWake()
	if a == nil {
		t.Fatal("pending tick queued nothing")
	}
	if !rt.hasPendingInboxMail() {
		t.Fatal("remote hasPendingInboxMail ignored the pending tick")
	}

	pending = 0
	rt.pollOnce()
	if rt.outstandingWake() != nil {
		t.Fatal("an empty hub tick did not retire the waiting attempt")
	}
	select {
	case <-a.retired:
	default:
		t.Fatal("attempt not cancelled on an empty hub tick")
	}
	if rt.hasPendingInboxMail() {
		t.Fatal("remote hasPendingInboxMail still true after an empty tick")
	}

	// New period, then channel loss: the attempt survives the error.
	pending = 1
	rt.pollOnce()
	b := rt.takeQueuedWake()
	if b == nil || b == a {
		t.Fatalf("new period queued %+v, want a fresh attempt", b)
	}
	fake.tickFn = func() (op.TickResp, error) { return op.TickResp{}, &hubTestErr{code: "E_CHANNEL_LOST"} }
	rt.pollOnce()
	if got := rt.outstandingWake(); got != b {
		t.Fatalf("channel loss retired the attempt: got %+v", got)
	}
	select {
	case <-b.retired:
		t.Fatal("attempt cancelled on channel loss")
	default:
	}
	select {
	case err := <-rt.channelErr:
		if err == nil {
			t.Fatal("channel loss reported a nil error")
		}
	default:
		t.Fatal("channel loss did not reach the supervisor")
	}
	if !rt.shutdownRequested.Load() {
		t.Fatal("channel loss did not request shutdown")
	}
}

// TestWakeNotRetiredOnLocalTickError: a local tick that fails outright (not
// fenced) observed nothing, so it neither retires the attempt nor resets the
// period — a zero TickResp is not an empty inbox.
func TestWakeNotRetiredOnLocalTickError(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeRemoteServeChannel{token: "local-token"}
	fake.tickFn = func() (op.TickResp, error) { return op.TickResp{Pending: 1}, nil }
	opts := runnerOptions{AgentID: "runner-test", Vendor: "test", IntervalSeconds: 5, Prompt: defaultRunnerPrompt, IdleGrace: time.Hour}
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir("runner-test")); err != nil {
		t.Fatal(err)
	}
	rt := newRunnerRuntime(cfg, opts, root, nil, loop.Registration{}, fake)
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()

	rt.pollOnce()
	a := rt.takeQueuedWake()
	if a == nil {
		t.Fatal("pending tick queued nothing")
	}
	rt.mu.Lock()
	rt.injectedThisPeriod = true
	rt.mu.Unlock()

	fake.tickFn = func() (op.TickResp, error) { return op.TickResp{}, &hubTestErr{code: "tick exploded"} }
	rt.pollOnce()
	if got := rt.outstandingWake(); got != a {
		t.Fatalf("a failed tick retired the attempt: got %+v", got)
	}
	rt.mu.Lock()
	injected := rt.injectedThisPeriod
	rt.mu.Unlock()
	if !injected {
		t.Fatal("a failed tick reset the pending period")
	}
	if rt.shutdownRequested.Load() {
		t.Fatal("a non-fenced local tick error requested shutdown")
	}
}

// hubTestErr is a minimal error for the remote tick-failure path.
type hubTestErr struct{ code string }

func (e *hubTestErr) Error() string { return e.code }
