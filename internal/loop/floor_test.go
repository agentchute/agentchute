package loop

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// floor_test.go — v2.5 plan B7: MintSendStamp's monotonic per-sender floor
// (C7/C8), replacing the deleted per-(from,to) seq allocator's fence tests
// and durability guarantees.

func TestMintSendStampFirstIsNow(t *testing.T) {
	cfg := newSeqTestConfig(t)
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)
	stamp, err := MintSendStamp(cfg, "alice", now, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := FormatStamp(now); stamp != want {
		t.Fatalf("first mint = %q, want %q (fresh sender mints at `now` directly)", stamp, want)
	}
}

// TestMintSendStampMonotonicAcrossRestart proves the write-ahead durability
// property (C7): the floor is persisted to disk BEFORE this call returns, so
// a fresh MintSendStamp call against the SAME cfg (standing in for a process
// restart — the floor file, not in-memory state, is the source of truth)
// never mints at or before the previous stamp, even when `now` regresses.
func TestMintSendStampMonotonicAcrossRestart(t *testing.T) {
	cfg := newSeqTestConfig(t)
	first, err := MintSendStamp(cfg, "alice", time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC), "")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: a fresh call, `now` EARLIER than the persisted
	// floor (clock regression, or simply a wall-clock read racing a
	// microsecond-precision floor already ahead of it).
	second, err := MintSendStamp(cfg, "alice", time.Date(2026, 5, 9, 16, 8, 30, 0, time.UTC), "")
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("second mint %q must be strictly greater than first %q", second, first)
	}
	floorTime, ok := ParseStamp(first)
	if !ok {
		t.Fatalf("parse first stamp %q", first)
	}
	if want := FormatStamp(floorTime.Add(time.Microsecond)); second != want {
		t.Fatalf("second mint = %q, want exactly floor+1us = %q", second, want)
	}
}

func TestMintSendStampClockRegressionClampsToFloorPlusMicrosecond(t *testing.T) {
	cfg := newSeqTestConfig(t)
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)
	first, err := MintSendStamp(cfg, "alice", now, "")
	if err != nil {
		t.Fatal(err)
	}
	// A `now` far in the past (simulating an NTP step-back) still clamps to
	// exactly floor+1us, never further behind.
	regressed := now.Add(-24 * time.Hour)
	second, err := MintSendStamp(cfg, "alice", regressed, "")
	if err != nil {
		t.Fatal(err)
	}
	floorTime, _ := ParseStamp(first)
	if want := FormatStamp(floorTime.Add(time.Microsecond)); second != want {
		t.Fatalf("regressed mint = %q, want floor+1us = %q", second, want)
	}
}

func TestMintSendStampAdvancesWithClock(t *testing.T) {
	cfg := newSeqTestConfig(t)
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)
	if _, err := MintSendStamp(cfg, "alice", now, ""); err != nil {
		t.Fatal(err)
	}
	// `now` clearly AHEAD of the floor mints at `now` directly (no clamping).
	ahead := now.Add(time.Hour)
	stamp, err := MintSendStamp(cfg, "alice", ahead, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := FormatStamp(ahead); stamp != want {
		t.Fatalf("mint clearly ahead of the floor = %q, want %q (no clamping needed)", stamp, want)
	}
}

// TestMintSendStampConcurrentMintsDistinct re-homes
// TestAllocateSeqConcurrentDistinct: concurrent mints from the SAME sender
// get DISTINCT, monotonically increasing stamps (the per-agent lock
// serializes minting, exactly like the deleted allocator's counter).
func TestMintSendStampConcurrentMintsDistinct(t *testing.T) {
	cfg := newSeqTestConfig(t)
	const n = 50
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)
	var (
		mu   sync.Mutex
		got  []string
		wg   sync.WaitGroup
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stamp, err := MintSendStamp(cfg, "alice", now, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			got = append(got, stamp)
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("MintSendStamp errors: %v", errs)
	}
	if len(got) != n {
		t.Fatalf("got %d stamps, want %d", len(got), n)
	}
	sort.Strings(got)
	seen := make(map[string]bool, n)
	for _, s := range got {
		if seen[s] {
			t.Fatalf("duplicate stamp minted: %s", s)
		}
		seen[s] = true
	}
}

// TestMintSendStampPerSenderScope: two DIFFERENT senders may legally mint the
// identical stamp — uniqueness needs nothing cross-sender, since each
// sender's messages land under its OWN from-<id>_... filename in the
// recipient's inbox (v2.5 plan B7 replaces the old per-(from,to) counter
// tower with ONE floor per sender).
func TestMintSendStampPerSenderScope(t *testing.T) {
	cfg := newSeqTestConfig(t)
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)
	a, err := MintSendStamp(cfg, "alice", now, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := MintSendStamp(cfg, "bob", now, "")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("independent senders at the identical `now` should mint the identical stamp (no cross-sender ordering constraint); alice=%q bob=%q", a, b)
	}
}

// TestMintStampInLockFenceClosesTOCTOU re-homes
// TestAllocateSeqInLockFenceClosesTOCTOU (v2.5 plan B7). The deleted
// AllocateSeq had TWO fence checks (an outer, lock-free fast-fail, then an
// in-lock re-verify) because a reclaim could land in the window BETWEEN
// them. MintSendStamp deliberately carries only ONE check, taken INSIDE
// withAgentLock(from) — there is no outer, lock-free check left to race,
// because a reclaimer (AcquireServeLease) takes the SAME lock
// (withAgentLock(id)==withAgentLock(from)) to write its new claim. The two
// critical sections can therefore never interleave: a reclaim either fully
// precedes or fully follows any given MintSendStamp call. This test proves
// the invariant that actually matters: a reclaim that completed before the
// call (however tightly before) is caught by the single in-lock check —
// there is no unprotected outer window a caller could slip through.
func TestMintStampInLockFenceClosesTOCTOU(t *testing.T) {
	cfg := newSeqTestConfig(t)

	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("AcquireServeLease: %v", err)
	}
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)

	// Reclaim completes (a new owner's AcquireServeLease overwrote the
	// claim's token) BEFORE the stale holder ever calls MintSendStamp.
	overwriteClaimToken(t, cfg, "alice", "RECLAIMED-BY-NEW-OWNER")

	// The stale holder's mint must fail closed: the in-lock check is the
	// ONLY check, and it sees the new token.
	if _, err := MintSendStamp(cfg, "alice", now, lease.Token); err != ErrFenced {
		t.Fatalf("MintSendStamp with reclaimed token err = %v, want ErrFenced", err)
	}

	// Fail-closed: the floor did NOT advance — the new owner's first mint is
	// still free to use `now` directly (nothing was persisted by the fenced
	// attempt).
	stamp, err := MintSendStamp(cfg, "alice", now, "RECLAIMED-BY-NEW-OWNER")
	if err != nil {
		t.Fatalf("MintSendStamp with new owner's token: %v", err)
	}
	if want := FormatStamp(now); stamp != want {
		t.Fatalf("stamp after fenced abort = %q, want %q (no advance)", stamp, want)
	}
}

// TestMintStampFenceMismatchAborts re-homes TestAllocateSeqFenceMismatchAborts
// (v2.5 plan B7): when a serve token is supplied, MintSendStamp VerifyFence's
// it BEFORE persisting the floor; a stale token (claim reclaimed) aborts with
// ErrFenced and does NOT advance the floor.
func TestMintStampFenceMismatchAborts(t *testing.T) {
	cfg := newSeqTestConfig(t)
	now := time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)

	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("AcquireServeLease: %v", err)
	}

	// A correct token mints fine.
	first, err := MintSendStamp(cfg, "alice", now, lease.Token)
	if err != nil {
		t.Fatalf("MintSendStamp with valid token: %v", err)
	}

	// Simulate reclaim: overwrite the claim with a different token.
	overwriteClaimToken(t, cfg, "alice", "STALE-OTHER-TOKEN")

	if _, err := MintSendStamp(cfg, "alice", now.Add(time.Hour), lease.Token); err != ErrFenced {
		t.Fatalf("MintSendStamp with stale token err = %v, want ErrFenced", err)
	}

	// The fenced attempt (which proposed a stamp an HOUR later) must NOT have
	// advanced the floor: the next valid mint (new token) at the SAME `now`
	// still clamps to first+1us, not to the fenced attempt's later value.
	next, err := MintSendStamp(cfg, "alice", now, "STALE-OTHER-TOKEN")
	if err != nil {
		t.Fatalf("MintSendStamp with new token: %v", err)
	}
	wantFloor, _ := ParseStamp(first)
	if want := FormatStamp(wantFloor.Add(time.Microsecond)); next != want {
		t.Fatalf("stamp after fenced abort = %q, want %q (floor unmoved by the fenced attempt)", next, want)
	}
}

// TestSendTsMessageWithCommitReportsPostLinkSyncFailure re-homes
// TestSendSeqMessageWithCommitReportsPostLinkSyncFailure (v2.5 plan B7): a
// failure in the post-link inbox-dir durability barrier is reported as
// committed=true (partial success), not as a safe-to-retry failure — the
// message already landed.
func TestSendTsMessageWithCommitReportsPostLinkSyncFailure(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")
	originalSync := syncSeqInboxDir
	syncSeqInboxDir = func(string) error {
		return errors.New("forced post-link sync failure")
	}
	defer func() { syncSeqInboxDir = originalSync }()

	id, committed, err := SendTsMessageWithCommit(cfg, "alice", "bob", []byte("landed"), "")
	if err == nil || err.Error() != "forced post-link sync failure" {
		t.Fatalf("err = %v, want forced sync failure", err)
	}
	if !committed {
		t.Fatal("committed = false after successful canonical link")
	}
	data, readErr := os.ReadFile(filepath.Join(cfg.AgentInboxDir("bob"), id.Filename()))
	if readErr != nil {
		t.Fatalf("committed file missing: %v", readErr)
	}
	if string(data) != "landed" {
		t.Fatalf("committed body = %q, want landed", data)
	}
}
