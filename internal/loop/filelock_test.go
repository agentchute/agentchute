package loop

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newLockTestConfig builds a Config rooted at a tempdir so AgentStateDir and
// AgentRegistrationPath resolve under test-owned storage.
func newLockTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
}

// writeTestRegistration lays down a minimal valid registration for agentID so
// concurrent mutations have a file to read-modify-write.
func writeTestRegistration(t *testing.T, cfg *Config, agentID string) {
	t.Helper()
	reg := &Registration{
		AgentID:     agentID,
		Vendor:      "agentchute",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC),
	}
	if err := WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatalf("seed registration: %v", err)
	}
}

// TestWithAgentLock_SerializesConcurrentLedgerAppends drives 50 goroutines, each
// recording a DISTINCT `.owed` obligation (distinct seq) for the same asker.
// Without a per-agent lock the load->append->save sequence races and loses
// updates; with the lock all 50 entries must survive. RecordOwed uses the same
// withAgentLock path the removed pending-reply ledger did.
func TestWithAgentLock_SerializesConcurrentLedgerAppends(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"
	const n = 50
	// #175 sightings 1/2/4: this row failed on ubuntu CI at 5.19s and 5.01s —
	// the package's own agentLockTimeout, to two decimal places. It is not a
	// lost-update bug and not scheduler noise.
	//
	// MEASURED (macOS, GOMAXPROCS=2, -race, 25 runs): this test takes 1.32s,
	// stable to ±0.03s. 50 contenders times agentLockRetryInterval's 25ms poll
	// is 1.25s, so the runtime IS the poll cadence; the critical section barely
	// registers. That leaves under 4x headroom against a 5s bound, and a
	// poll-based lock is unfair — a goroutine can lose round after round — so a
	// slower, loaded runner reaches the bound and RecordOwed returns the
	// timeout error this test then reports as a failure.
	//
	// The property under test is that no update is LOST under concurrency, not
	// how long the lock waits. So the bound is lifted here rather than the
	// contention lowered: dropping n would weaken the thing being proven, and
	// leaving it would keep a correctness row hostage to how busy a runner is.
	withGenerousAgentLockTimeout(t)

	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	by := now.Add(30 * time.Minute)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := MsgID{To: "codex", From: agentID, Seq: uint64(i + 1)}
			if err := RecordOwed(cfg, agentID, key, by, now); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("RecordOwed: %v", err)
	}

	ledger, err := LoadOwedLedger(cfg, agentID)
	if err != nil {
		t.Fatalf("LoadOwedLedger: %v", err)
	}
	if len(ledger.Owed) != n {
		t.Fatalf("ledger has %d entries, want %d (lost update under concurrency)", len(ledger.Owed), n)
	}
	seen := make(map[uint64]bool, n)
	for _, e := range ledger.Owed {
		seen[e.Seq] = true
	}
	for i := 0; i < n; i++ {
		if !seen[uint64(i+1)] {
			t.Fatalf("missing owed entry for seq %d", i+1)
		}
	}
}

// TestWithAgentLock_MutualExclusionNoOverlap asserts the lock is real mutual
// exclusion: across many concurrent holders, no two critical sections ever
// overlap. This is the correctness property the Windows implementation must
// satisfy — the prior O_CREATE|O_EXCL + age-based stale-reclaim could be stolen
// from a live holder whose section ran long, double-holding the lock. The test
// is portable (runs against whichever build-tagged impl is compiled) and uses
// short critical sections well under any stale window.
func TestWithAgentLock_MutualExclusionNoOverlap(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"
	const goroutines = 16
	const itersEach = 8

	var (
		mu       sync.Mutex
		inside   int
		maxObs   int
		overlaps int
	)

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*itersEach)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < itersEach; i++ {
				if err := withAgentLock(cfg, agentID, func() error {
					mu.Lock()
					inside++
					if inside > maxObs {
						maxObs = inside
					}
					if inside > 1 {
						overlaps++
					}
					mu.Unlock()

					// Tiny critical section to widen the overlap window.
					time.Sleep(time.Millisecond)

					mu.Lock()
					inside--
					mu.Unlock()
					return nil
				}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("withAgentLock: %v", err)
	}
	if overlaps != 0 || maxObs != 1 {
		t.Fatalf("lock did NOT provide mutual exclusion: max concurrent holders=%d overlaps=%d (want 1, 0)", maxObs, overlaps)
	}
}

// TestWithAgentLock_BoundedWaitDoesNotDeadlock holds the lock past the bounded
// wait window in one goroutine and verifies a competing acquisition returns a
// timeout error instead of blocking forever.
func TestWithAgentLock_BoundedWaitDoesNotDeadlock(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"

	// Lower the bounded-wait window so the competing acquisition gives up in
	// ~100ms instead of the 5s production default — the assertion (a contended
	// acquire returns a timeout error rather than blocking forever) is
	// unchanged, it just no longer costs ~5s per test run. Restored after.
	oldTimeout := agentLockTimeout
	agentLockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agentLockTimeout = oldTimeout })

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withAgentLock(cfg, agentID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- withAgentLock(cfg, agentID, func() error { return nil })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected bounded-wait timeout error while lock is held, got nil")
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("withAgentLock took %s, exceeds bounded wait", time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("withAgentLock blocked forever; bounded wait did not fire")
	}
}

// TestHeartbeatRegistration_NoLostUpdateUnderConcurrency runs concurrent
// HeartbeatRegistration calls alongside a concurrent Body mutation (standing
// in for another command's read-modify-write, e.g. a re-register updating
// the bio) and asserts the registration is never torn (always parses) and
// survives the race. B1: replaces the retired UpdateLastSeen version of this
// test — the heartbeat is now lease-gated, so it needs a real serve lease
// token. v2.5 plan B5: Status is gone, so the concurrent mutation now targets
// Body — the field HeartbeatRegistration's merge is documented to preserve.
func TestHeartbeatRegistration_NoLostUpdateUnderConcurrency(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"
	writeTestRegistration(t, cfg, agentID)
	lease, err := AcquireServeLease(cfg, agentID)
	if err != nil {
		t.Fatal(err)
	}
	template := Registration{
		AgentID:     agentID,
		Vendor:      "agentchute",
		ControlRepo: cfg.ControlRepo,
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := HeartbeatRegistration(cfg, template, lease.Token); err != nil {
				errs <- err
			}
		}(i)
	}
	// One concurrent Body mutation through the same lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := withAgentLock(cfg, agentID, func() error {
			reg, err := ReadRegistration(cfg.AgentRegistrationPath(agentID))
			if err != nil {
				return err
			}
			reg.Body = "updated bio"
			return WriteRegistration(cfg.AgentRegistrationPath(agentID), reg)
		}); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent mutation: %v", err)
	}

	reg, err := ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if err != nil {
		t.Fatalf("registration torn / unreadable after concurrency: %v", err)
	}
	if reg.AgentID != agentID {
		t.Fatalf("agent_id = %q, want %q", reg.AgentID, agentID)
	}
}

// TestAtomicWrite_SyncDirFailureAfterRenameNotReportedAsWriteFail asserts that
// once os.Rename succeeds, the new content is durably present at the path. The
// real concern is the cleanup flag flipping false immediately after rename so a
// later syncDir failure cannot also delete the just-published temp/target. We
// cannot easily fault-inject syncDir on a real fs, so we assert the post-rename
// content is the new content and the temp file is gone (cleanup did not fire on
// the published file). Limitation documented: the syncDir-error branch itself is
// not fault-injected here.
func TestAtomicWrite_SyncDirFailureAfterRenameNotReportedAsWriteFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")

	if err := atomicWriteFile(path, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := atomicWriteFile(path, []byte("second")); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}

	// No leftover temp files (cleanup logic correct, not racing the published file).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp_") {
			t.Fatalf("leftover temp file %q after atomic write", e.Name())
		}
	}
}

// withGenerousAgentLockTimeout lifts the per-agent lock's bounded wait for the
// duration of one test. agentLockTimeout is already a package var for exactly
// this reason — the comment on it says tests lower it to keep the bounded-wait
// row fast; these two need the opposite, and for the same reason: neither is
// testing the bound.
func withGenerousAgentLockTimeout(t *testing.T) {
	t.Helper()
	original := agentLockTimeout
	agentLockTimeout = 60 * time.Second
	t.Cleanup(func() { agentLockTimeout = original })
}

// The mechanism, pinned rather than left in an issue comment: with the bound
// set below what the poll cadence costs, contention produces the timeout error
// — which is the failure CI was reporting.
func TestAgentLockBoundIsWhatFiftyContendersHit(t *testing.T) {
	cfg := newLockTestConfig(t)
	original := agentLockTimeout
	agentLockTimeout = 40 * time.Millisecond
	t.Cleanup(func() { agentLockTimeout = original })

	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	by := now.Add(30 * time.Minute)
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := MsgID{To: "codex", From: "claude-code", Seq: uint64(i + 1)}
			if err := RecordOwed(cfg, "claude-code", key, by, now); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	failed := 0
	for range errs {
		failed++
	}
	if failed == 0 {
		t.Skip("40ms was enough on this machine; the cadence-versus-bound relationship is what the row documents")
	}
	// The point: the failure is the BOUND, not corruption. Whatever did get
	// recorded is intact and distinct — a timed-out acquirer writes nothing.
	ledger, err := LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Owed)+failed != n {
		t.Fatalf("%d recorded + %d timed out != %d; a timed-out acquire lost or corrupted an update", len(ledger.Owed), failed, n)
	}
}
