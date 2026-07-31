package loop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// send_delivery_test.go — v2.5 plan B3/B7: DeliverUnderRecipientLock is
// send's single point of TRUTH for reachability. These tests prove the
// under-lock re-check genuinely closes the race a lock-free preflight leaves
// open — not just that the error TYPES exist, but that a real mutation
// racing the lock (via afterRecipientLockHook) is actually caught.

// testTsID builds a valid timestamp identity for delivery tests (v2.5 plan
// B7). now is fixed rather than time.Now() so tests stay deterministic.
func testTsID(t *testing.T, to, from string) TsID {
	t.Helper()
	suffix, err := rand128hex()
	if err != nil {
		t.Fatal(err)
	}
	return TsID{To: to, From: from, Stamp: FormatStamp(time.Date(2026, 5, 9, 16, 8, 36, 0, time.UTC)), Suffix: suffix}
}

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

	id := testTsID(t, "bob", "alice")
	_, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("x"), "")
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

	id := testTsID(t, "bob", "alice")
	_, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("x"), "")
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

	id := testTsID(t, "bob", "alice")
	_, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("x"), "")
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

	id := testTsID(t, "bob", "alice")
	committedID, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("x"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true for a fresh recipient")
	}
	data, rerr := os.ReadFile(cfg.AgentInboxDir("bob") + "/" + committedID.Filename())
	if rerr != nil {
		t.Fatalf("delivered file missing: %v", rerr)
	}
	if string(data) != "x" {
		t.Fatalf("delivered body = %q, want x", data)
	}
}

// TestDeliverUnderRecipientLockRetriesOnCollisionWithFreshSuffix proves C4:
// on link EEXIST, DeliverUnderRecipientLock retries with a FRESH suffix
// (id.To/From/Stamp held fixed) and the retried delivery lands under a
// DIFFERENT filename than the collision — never treating EEXIST as success
// (unlike the deleted seq allocator's alreadyLanded semantics).
func TestDeliverUnderRecipientLockRetriesOnCollisionWithFreshSuffix(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")

	id := testTsID(t, "bob", "alice")
	// Pre-create a file at id's exact canonical path, simulating an existing
	// collision (astronomically unlikely with a real 128-bit suffix; forced
	// here via the rand128hex seam).
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("bob"), id.Filename()), []byte("already here"))

	const freshSuffix = "22222222222222222222222222222222"
	oldRand := rand128hex
	rand128hex = func() (string, error) { return freshSuffix, nil }
	t.Cleanup(func() { rand128hex = oldRand })

	committedID, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("new content"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true after a fresh-suffix retry")
	}
	if committedID.Suffix != freshSuffix {
		t.Fatalf("committed suffix = %q, want the fresh retry suffix %q", committedID.Suffix, freshSuffix)
	}
	if committedID.Filename() == id.Filename() {
		t.Fatal("committed filename must differ from the collided original")
	}
	original, rerr := os.ReadFile(filepath.Join(cfg.AgentInboxDir("bob"), id.Filename()))
	if rerr != nil || string(original) != "already here" {
		t.Fatalf("original collided file was modified: %v %q", rerr, original)
	}
	delivered, derr := os.ReadFile(filepath.Join(cfg.AgentInboxDir("bob"), committedID.Filename()))
	if derr != nil || string(delivered) != "new content" {
		t.Fatalf("retried delivery missing/wrong content: %v %q", derr, delivered)
	}
}

// TestDeliverUnderRecipientLockExhaustsCollisionRetries proves the other half
// of C4: if EVERY attempt collides (pathological), DeliverUnderRecipientLock
// hard-errors after maxLinkCollisionRetries attempts and commits NOTHING.
func TestDeliverUnderRecipientLockExhaustsCollisionRetries(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")

	id := testTsID(t, "bob", "alice")
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("bob"), id.Filename()), []byte("collision"))

	// Every retry ALSO collides — same suffix as the original, every time.
	oldRand := rand128hex
	rand128hex = func() (string, error) { return id.Suffix, nil }
	t.Cleanup(func() { rand128hex = oldRand })

	_, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("never lands"), "")
	if committed {
		t.Fatal("committed = true, want false after exhausting collision retries")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want wrapping os.ErrExist", err)
	}
	data, rerr := os.ReadFile(filepath.Join(cfg.AgentInboxDir("bob"), id.Filename()))
	if rerr != nil || string(data) != "collision" {
		t.Fatalf("pre-existing collision file was modified: %v %q", rerr, data)
	}
}

// TestDeliverUnderRecipientLockRejectsMalformedIdentity is codex PR #99
// round 2, finding 2: a caller-supplied TsID whose Stamp/Suffix don't
// conform to the C3 grammar must be rejected BEFORE any filepath.Join or
// filesystem write — an unvalidated Stamp/Suffix could otherwise escape the
// inbox directory once joined into a path. Covers both a merely malformed
// identity and a deliberately path-escaping one.
//
// The path-escaping case is constructed to land at a PREDICTABLE, EXISTING
// directory (cfg.LoopDir, created by mkFreshRecipient's mkInbox call) rather
// than a nonexistent one — landing on a nonexistent parent would make
// os.Link fail regardless of validation (ENOENT), silently passing even
// without the fix. Suffix "../../../../escaped" contains four raw ".."
// tokens; the first is absorbed canceling its own glued-on push (there is no
// "/" between "_r" and the first ".." in the rendered filename), so only the
// remaining three actually pop path components: the first re-cancels that
// same glued push (net zero), and the next two pop "bob" and "inbox" off
// cfg.AgentInboxDir("bob") (= LoopDir/inbox/bob), landing the write at
// exactly LoopDir/escaped.md — verified to NOT exist after the call.
func TestDeliverUnderRecipientLockRejectsMalformedIdentity(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")
	escapedPath := filepath.Join(cfg.LoopDir, "escaped.md")

	cases := []struct {
		name string
		id   TsID
	}{
		{
			name: "malformed stamp and suffix",
			id:   TsID{To: "bob", From: "alice", Stamp: "not-a-stamp", Suffix: "not-hex"},
		},
		{
			name: "path-escaping suffix reaches an existing directory",
			id:   TsID{To: "bob", From: "alice", Stamp: FormatStamp(time.Now()), Suffix: "../../../../escaped"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := os.ReadDir(cfg.AgentInboxDir("bob"))
			if err != nil {
				t.Fatal(err)
			}
			_, committed, err := DeliverUnderRecipientLock(cfg, c.id, []byte("must not land"), "")
			if err == nil {
				t.Fatal("expected a validation error for a malformed identity")
			}
			if committed {
				t.Fatal("committed = true, want false for a rejected identity")
			}
			if _, statErr := os.Stat(escapedPath); !os.IsNotExist(statErr) {
				t.Fatalf("identity escaped the inbox directory: %s exists (stat err = %v)", escapedPath, statErr)
			}
			after, err := os.ReadDir(cfg.AgentInboxDir("bob"))
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("inbox entry count changed (%d -> %d); a malformed identity must create NOTHING", len(before), len(after))
			}
		})
	}
}

// TestDeliverUnderRecipientLockRecipientIsSoleSourceOfTruth is codex PR #99
// round 3: DeliverUnderRecipientLock used to take a separate `to` parameter
// ALONGSIDE id.To, each validated in isolation and never checked against the
// other — a caller bug could commit a well-formed identity naming one agent
// into a DIFFERENT agent's inbox, while the returned identity (and whatever
// RefString/owed key a caller records from it) still named the original.
// Fixed by DELETING the redundant parameter rather than adding a runtime
// equality check: id.To is now the ONLY recipient source, so the two facts
// cannot even be expressed as different values — this test confirms that
// invariant holds end-to-end (destination and returned identity never
// diverge) rather than asserting a mismatch is "rejected", since a mismatch
// is no longer expressible at all.
func TestDeliverUnderRecipientLockRecipientIsSoleSourceOfTruth(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkFreshRecipient(t, cfg, "bob")
	mkFreshRecipient(t, cfg, "carol")

	id := testTsID(t, "carol", "alice")
	committedID, committed, err := DeliverUnderRecipientLock(cfg, id, []byte("for carol only"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !committed {
		t.Fatal("committed = false, want true")
	}
	if committedID.To != "carol" {
		t.Fatalf("committedID.To = %q, want carol (must match the identity's own To, not drift)", committedID.To)
	}

	bobEntries, err := os.ReadDir(cfg.AgentInboxDir("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bobEntries) != 0 {
		t.Fatalf("bob's inbox has %d entries, want 0 — delivery must never touch a recipient other than id.To", len(bobEntries))
	}
	data, rerr := os.ReadFile(filepath.Join(cfg.AgentInboxDir("carol"), committedID.Filename()))
	if rerr != nil || string(data) != "for carol only" {
		t.Fatalf("carol's inbox missing/wrong delivered content: %v %q", rerr, data)
	}

	// The obligation key a caller would record from the returned identity
	// names the SAME recipient the file actually landed under — no reply-
	// tracking black hole is possible (the B3 lesson, one field over).
	if got := committedID.OwedKey().To; got != "carol" {
		t.Fatalf("committedID.OwedKey().To = %q, want carol", got)
	}
}
