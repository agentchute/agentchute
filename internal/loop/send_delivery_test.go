package loop

import (
	"errors"
	"os"
	"testing"
	"time"
)

// send_delivery_test.go — v2.5 plan B3: DeliverUnderRecipientLock is send's
// single point of TRUTH for reachability. These tests prove the under-lock
// re-check genuinely closes the race a lock-free preflight leaves open — not
// just that the error TYPES exist, but that a real mutation racing the lock
// (via afterRecipientLockHook) is actually caught.

func TestCheckRecipientReachabilityUnknown(t *testing.T) {
	cfg := newSeqTestConfig(t)
	_, err := CheckRecipientReachability(cfg, "ghost", time.Now().UTC())
	if !errors.Is(err, ErrRecipientUnknown) {
		t.Fatalf("err = %v, want ErrRecipientUnknown", err)
	}
	// ErrRecipientUnknown wraps os.ErrNotExist via %w, so errors.Is (which
	// walks Unwrap chains) sees it — but the legacy os.IsNotExist does NOT:
	// it only unwraps *PathError/*LinkError/*SyscallError, not an arbitrary
	// fmt.Errorf %w chain. errors.Is is the correct check for callers.
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ErrRecipientUnknown must satisfy errors.Is(err, os.ErrNotExist), got %v", err)
	}
}

func TestCheckRecipientReachabilityFreshAndStale(t *testing.T) {
	cfg := newSeqTestConfig(t)
	now := time.Now().UTC()
	mkFreshRecipient(t, cfg, "fresh")

	rr, err := CheckRecipientReachability(cfg, "fresh", now)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Fresh {
		t.Fatalf("rr.Fresh = false for a just-registered row, age=%s threshold=%s", rr.Age, rr.Threshold)
	}

	stale := &Registration{AgentID: "stale", Vendor: "agentchute", ControlRepo: cfg.ControlRepo, LastSeen: now.Add(-2 * time.Hour)}
	if err := WriteRegistration(cfg.AgentRegistrationPath("stale"), stale); err != nil {
		t.Fatal(err)
	}
	rr, err = CheckRecipientReachability(cfg, "stale", now)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Fresh {
		t.Fatalf("rr.Fresh = true for a 2h-old row against the default stale_after")
	}
}

// TestDeliverUnderRecipientLockBacksOffWhenRecipientGoesStaleDuringLock
// reproduces the exact race the recipient lock exists to close: `to`'s row is
// fresh at scan/preflight time, but goes stale in the gap between that check
// and the per-delivery lock — afterRecipientLockHook fires INSIDE the lock,
// after acquisition but before the freshness re-read, so the divergence comes
// entirely from that window (mirrors sweep.go's afterSweepScanHook pattern).
func TestDeliverUnderRecipientLockBacksOffWhenRecipientGoesStaleDuringLock(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")

	t.Cleanup(func() { afterRecipientLockHook = nil })
	afterRecipientLockHook = func() {
		reg, err := ReadRegistration(cfg.AgentRegistrationPath("bob"))
		if err != nil {
			t.Fatalf("re-read bob during race window: %v", err)
		}
		reg.LastSeen = time.Now().UTC().Add(-2 * time.Hour)
		if err := WriteRegistration(cfg.AgentRegistrationPath("bob"), reg); err != nil {
			t.Fatalf("backdate bob during race window: %v", err)
		}
	}

	id := MsgID{To: "bob", From: "alice", Seq: 1}
	committed, err := DeliverUnderRecipientLock(cfg, "bob", id, []byte("x"), "")
	if committed {
		t.Fatal("committed = true, want false (a recipient that went stale mid-lock must not receive delivery)")
	}
	var staleErr *ErrRecipientStale
	if !errors.As(err, &staleErr) {
		t.Fatalf("err = %v, want *ErrRecipientStale", err)
	}
	if staleErr.To != "bob" {
		t.Fatalf("ErrRecipientStale.To = %q, want bob", staleErr.To)
	}
	entries, rerr := os.ReadDir(cfg.AgentInboxDir("bob"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 delivered messages, got %d", len(entries))
	}
}

// TestDeliverUnderRecipientLockBacksOffWhenRecipientVanishesDuringLock is the
// absent-row twin: `to`'s registration is removed entirely (e.g. swept) in
// the same race window.
func TestDeliverUnderRecipientLockBacksOffWhenRecipientVanishesDuringLock(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")

	t.Cleanup(func() { afterRecipientLockHook = nil })
	afterRecipientLockHook = func() {
		if err := os.Remove(cfg.AgentRegistrationPath("bob")); err != nil {
			t.Fatalf("remove bob during race window: %v", err)
		}
	}

	id := MsgID{To: "bob", From: "alice", Seq: 1}
	committed, err := DeliverUnderRecipientLock(cfg, "bob", id, []byte("x"), "")
	if committed {
		t.Fatal("committed = true, want false")
	}
	if !errors.Is(err, ErrRecipientUnknown) {
		t.Fatalf("err = %v, want ErrRecipientUnknown", err)
	}
}

// TestCheckRecipientReachabilityFailsClosedOnMalformedRow: a registration row
// that EXISTS but fails to parse must never read as reachable — a corrupt row
// proves nothing about whether the recipient is actually there. PR #95 P1
// (codex/claude-code): an earlier version fell back to the file's mtime here
// (mirroring sweep.go's registrationAge, C12), so a malformed row with a
// FRESH mtime read as Fresh=true, letting delivery commit to an agent that
// was never confirmed reachable — precisely the black-hole this slice exists
// to eliminate, reached through the corrupt-row door instead of the
// never-registered door. Sweep's mtime fallback exists for the OPPOSITE
// requirement (a corrupt row must age out, not be immortal against cleanup);
// send needs unparseable to mean "cannot prove reachable", not "assume
// fresh".
func TestCheckRecipientReachabilityFailsClosedOnMalformedRow(t *testing.T) {
	cfg := newSeqTestConfig(t)
	if err := ensurePrivateDir(cfg.AgentsDir()); err != nil {
		t.Fatal(err)
	}
	path := cfg.AgentRegistrationPath("bob")
	if err := atomicWriteFile(path, []byte("{not a valid registration")); err != nil {
		t.Fatal(err)
	}
	// Freshly written (mtime = now) — exactly the shape that fooled the old
	// mtime fallback.

	rr, err := CheckRecipientReachability(cfg, "bob", time.Now().UTC())
	if !errors.Is(err, ErrRecipientUnreadable) {
		t.Fatalf("err = %v, want ErrRecipientUnreadable", err)
	}
	if rr.Fresh {
		t.Fatal("rr.Fresh = true for a malformed row — must never read as reachable")
	}
}

// TestDeliverUnderRecipientLockBacksOffWhenRecipientBecomesMalformedDuringLock
// is the malformed-row twin of the goes-stale/vanishes races above: `to`'s row
// is fresh and well-formed at preflight time, but gets corrupted (e.g. a torn
// concurrent write) in the gap before the per-delivery lock.
func TestDeliverUnderRecipientLockBacksOffWhenRecipientBecomesMalformedDuringLock(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")

	t.Cleanup(func() { afterRecipientLockHook = nil })
	afterRecipientLockHook = func() {
		if err := atomicWriteFile(cfg.AgentRegistrationPath("bob"), []byte("{not a valid registration")); err != nil {
			t.Fatalf("corrupt bob during race window: %v", err)
		}
	}

	id := MsgID{To: "bob", From: "alice", Seq: 1}
	committed, err := DeliverUnderRecipientLock(cfg, "bob", id, []byte("x"), "")
	if committed {
		t.Fatal("committed = true, want false (a recipient that became malformed mid-lock must not receive delivery)")
	}
	if !errors.Is(err, ErrRecipientUnreadable) {
		t.Fatalf("err = %v, want ErrRecipientUnreadable", err)
	}
	entries, rerr := os.ReadDir(cfg.AgentInboxDir("bob"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 delivered messages, got %d", len(entries))
	}
}

// TestDeliverUnderRecipientLockSucceedsWhenStillFresh is the control: without
// the hook mutating anything, a genuinely fresh recipient's delivery lands
// normally. Guards against a broken re-check that backs off unconditionally.
func TestDeliverUnderRecipientLockSucceedsWhenStillFresh(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")

	id := MsgID{To: "bob", From: "alice", Seq: 1}
	committed, err := DeliverUnderRecipientLock(cfg, "bob", id, []byte("x"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true for a fresh recipient")
	}
	data, rerr := os.ReadFile(cfg.AgentInboxDir("bob") + "/" + id.Filename())
	if rerr != nil {
		t.Fatalf("delivered file missing: %v", rerr)
	}
	if string(data) != "x" {
		t.Fatalf("delivered body = %q, want x", data)
	}
}
