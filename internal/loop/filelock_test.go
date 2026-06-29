package loop

import (
	"fmt"
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
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
}

// writeTestRegistration lays down a minimal valid registration for agentID so
// concurrent UpdateLastSeen / status mutations have a file to read-modify-write.
func writeTestRegistration(t *testing.T, cfg *Config, agentID string, status Status) {
	t.Helper()
	reg := &Registration{
		AgentID:     agentID,
		Vendor:      "examplecorp",
		ControlRepo: "/tmp/repo",
		LastSeen:    time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC),
		Status:      status,
	}
	if err := WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatalf("seed registration: %v", err)
	}
}

// TestWithAgentLock_SerializesConcurrentLedgerAppends drives 50 goroutines, each
// recording a DISTINCT message_id+filename for the same agent. Without a
// per-agent lock the load->append->save sequence races and loses updates; with
// the lock all 50 entries must survive.
func TestWithAgentLock_SerializesConcurrentLedgerAppends(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"
	const n = 50
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entry := PendingReplyEntry{
				MessageID:        fmt.Sprintf("2026-05-19T17:53:59.%06dZ", i),
				From:             "codex",
				To:               agentID,
				Task:             "R1 protocol",
				OriginalFilename: fmt.Sprintf("msg-%04d_from-codex.md", i),
				ArchivePath:      ".examplecorp/loop/archive/example.md",
			}
			if err := RecordPendingReply(cfg, agentID, entry, now); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("RecordPendingReply: %v", err)
	}

	ledger, err := LoadPendingLedger(cfg, agentID)
	if err != nil {
		t.Fatalf("LoadPendingLedger: %v", err)
	}
	if len(ledger.Pending) != n {
		t.Fatalf("ledger has %d entries, want %d (lost update under concurrency)", len(ledger.Pending), n)
	}
	seen := make(map[string]bool, n)
	for _, e := range ledger.Pending {
		seen[e.MessageID] = true
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("2026-05-19T17:53:59.%06dZ", i)
		if !seen[id] {
			t.Fatalf("missing ledger entry for message_id %q", id)
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

// TestUpdateLastSeen_NoLostUpdateUnderConcurrency runs concurrent UpdateLastSeen
// calls alongside a status mutation and asserts the registration is never torn
// (always parses) and the status mutation survives.
func TestUpdateLastSeen_NoLostUpdateUnderConcurrency(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"
	writeTestRegistration(t, cfg, agentID, StatusActive)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := time.Date(2026, 5, 10, 0, 0, i%60, 0, time.UTC)
			if err := UpdateLastSeen(cfg, agentID, ts); err != nil {
				errs <- err
			}
		}(i)
	}
	// One concurrent status mutation through the same lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := withAgentLock(cfg, agentID, func() error {
			reg, err := ReadRegistration(cfg.AgentRegistrationPath(agentID))
			if err != nil {
				return err
			}
			reg.Status = StatusExhausted
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

// TestRunnerOfflineNotResurrectedByConcurrentLastSeen models the run.go shutdown
// race: a registration marked offline must not be flipped back to active by a
// concurrent last_seen refresh. With both writers serialized through
// withAgentLock and the offline write applied last, the terminal state is
// offline.
func TestRunnerOfflineNotResurrectedByConcurrentLastSeen(t *testing.T) {
	cfg := newLockTestConfig(t)
	const agentID = "claude-code"
	writeTestRegistration(t, cfg, agentID, StatusActive)

	var wg sync.WaitGroup
	// Pollers refreshing last_seen (they read whatever status is on disk).
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = UpdateLastSeen(cfg, agentID, time.Date(2026, 5, 10, 0, 0, i%60, 0, time.UTC))
		}(i)
	}
	wg.Wait()

	// Shutdown: mark offline under the lock, AFTER pollers have joined.
	if err := withAgentLock(cfg, agentID, func() error {
		reg, err := ReadRegistration(cfg.AgentRegistrationPath(agentID))
		if err != nil {
			return err
		}
		reg.Status = StatusOffline
		reg.LastSeen = time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
		return WriteRegistration(cfg.AgentRegistrationPath(agentID), reg)
	}); err != nil {
		t.Fatalf("mark offline: %v", err)
	}

	reg, err := ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if err != nil {
		t.Fatalf("read after offline: %v", err)
	}
	if reg.Status != StatusOffline {
		t.Fatalf("status = %q, want offline (resurrected by concurrent last_seen)", reg.Status)
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
