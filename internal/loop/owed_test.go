package loop

import (
	"path/filepath"
	"testing"
	"time"
)

func newOwedTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
}

func TestLoadOwedLedgerMissingReturnsEmpty(t *testing.T) {
	cfg := newOwedTestConfig(t)
	ledger, err := LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatalf("LoadOwedLedger: %v", err)
	}
	if ledger == nil || len(ledger.Owed) != 0 {
		t.Fatalf("want empty ledger, got %+v", ledger)
	}
}

func TestRecordThenClearOnMatchingReply(t *testing.T) {
	cfg := newOwedTestConfig(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	key := MsgID{To: "bob", From: "alice", Seq: 1}

	if err := RecordOwed(cfg, "alice", key, now.Add(time.Hour), now); err != nil {
		t.Fatalf("RecordOwed: %v", err)
	}
	ledger, _ := LoadOwedLedger(cfg, "alice")
	if len(ledger.OutstandingOwed()) != 1 {
		t.Fatalf("want 1 outstanding, got %d", len(ledger.OutstandingOwed()))
	}

	// Clear on a matching in_reply_to (same key) discharges the obligation.
	if err := ClearOwed(cfg, "alice", key); err != nil {
		t.Fatalf("ClearOwed: %v", err)
	}
	ledger, _ = LoadOwedLedger(cfg, "alice")
	if len(ledger.OutstandingOwed()) != 0 {
		t.Fatalf("want 0 after clear, got %d", len(ledger.OutstandingOwed()))
	}
}

func TestClearNonMatchingLeavesObligation(t *testing.T) {
	cfg := newOwedTestConfig(t)
	now := time.Now()
	key := MsgID{To: "bob", From: "alice", Seq: 1}
	if err := RecordOwed(cfg, "alice", key, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	// A non-matching in_reply_to (different seq) must NOT clear it.
	if err := ClearOwed(cfg, "alice", MsgID{To: "bob", From: "alice", Seq: 99}); err != nil {
		t.Fatal(err)
	}
	ledger, _ := LoadOwedLedger(cfg, "alice")
	if len(ledger.OutstandingOwed()) != 1 {
		t.Fatalf("non-matching clear must leave obligation; got %d", len(ledger.OutstandingOwed()))
	}
	// Different recipient (To) also must not clear.
	if err := ClearOwed(cfg, "alice", MsgID{To: "carol", From: "alice", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	ledger, _ = LoadOwedLedger(cfg, "alice")
	if len(ledger.OutstandingOwed()) != 1 {
		t.Fatalf("different-To clear must leave obligation; got %d", len(ledger.OutstandingOwed()))
	}
}

func TestExpiredOwedSurfacesDeadRecipient(t *testing.T) {
	cfg := newOwedTestConfig(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	// Deadline already in the past relative to "now+2h".
	key := MsgID{To: "bob", From: "alice", Seq: 1}
	if err := RecordOwed(cfg, "alice", key, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	ledger, _ := LoadOwedLedger(cfg, "alice")

	// Before deadline: not expired.
	if got := ledger.ExpiredOwed(now.Add(30 * time.Minute)); len(got) != 0 {
		t.Fatalf("pre-deadline expired = %d, want 0", len(got))
	}
	// Past deadline: surfaces as the asker's expired obligation (dead recipient).
	exp := ledger.ExpiredOwed(now.Add(2 * time.Hour))
	if len(exp) != 1 {
		t.Fatalf("post-deadline expired = %d, want 1", len(exp))
	}
	if !exp[0].Key().Equal(key) {
		t.Fatalf("expired key = %+v, want %+v", exp[0].Key(), key)
	}
}

func TestRecordOwedIdempotentOnSameKey(t *testing.T) {
	cfg := newOwedTestConfig(t)
	now := time.Now()
	key := MsgID{To: "bob", From: "alice", Seq: 1}
	for i := 0; i < 3; i++ {
		if err := RecordOwed(cfg, "alice", key, now.Add(time.Hour), now); err != nil {
			t.Fatalf("RecordOwed #%d: %v", i, err)
		}
	}
	ledger, _ := LoadOwedLedger(cfg, "alice")
	if len(ledger.OutstandingOwed()) != 1 {
		t.Fatalf("idempotent re-record: got %d entries, want 1", len(ledger.OutstandingOwed()))
	}
}

func TestSaveOwedLedgerRoundTrips(t *testing.T) {
	cfg := newOwedTestConfig(t)
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC).UTC()
	in := &OwedLedger{Owed: []OwedEntry{
		{To: "bob", From: "alice", Seq: 1, By: now.Add(time.Hour), RecordedAt: now},
		{To: "carol", From: "alice", Seq: 5, By: now.Add(2 * time.Hour), RecordedAt: now},
	}}
	if err := SaveOwedLedger(cfg, "alice", in); err != nil {
		t.Fatalf("SaveOwedLedger: %v", err)
	}
	out, err := LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatalf("LoadOwedLedger: %v", err)
	}
	if len(out.Owed) != 2 {
		t.Fatalf("round-trip count = %d, want 2", len(out.Owed))
	}
	if out.Owed[0].To != "bob" || out.Owed[0].Seq != 1 || !out.Owed[0].By.Equal(now.Add(time.Hour)) {
		t.Fatalf("entry 0 mismatch: %+v", out.Owed[0])
	}
	if out.Owed[1].To != "carol" || out.Owed[1].Seq != 5 {
		t.Fatalf("entry 1 mismatch: %+v", out.Owed[1])
	}
}

func TestLoadOwedLedgerRejectsCorrupt(t *testing.T) {
	cfg := newOwedTestConfig(t)
	// Not valid JSON.
	if err := atomicWriteFile(owedPath(cfg, "alice"), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOwedLedger(cfg, "alice"); err == nil {
		t.Fatal("corrupt JSON must be rejected on load")
	}
}

func TestLoadOwedLedgerRejectsNonCanonical(t *testing.T) {
	cfg := newOwedTestConfig(t)
	// Valid JSON but an invalid agent_id in `to` (path-escaping defense).
	bad := []byte(`{"owed":[{"to":"../evil","from":"alice","seq":1,"by":"2026-06-30T13:00:00Z","recorded_at":"2026-06-30T12:00:00Z"}]}`)
	if err := atomicWriteFile(owedPath(cfg, "alice"), bad); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOwedLedger(cfg, "alice"); err == nil {
		t.Fatal("non-canonical to must be rejected on load")
	}

	// Zero seq is also non-canonical.
	badSeq := []byte(`{"owed":[{"to":"bob","from":"alice","seq":0,"by":"2026-06-30T13:00:00Z","recorded_at":"2026-06-30T12:00:00Z"}]}`)
	if err := atomicWriteFile(owedPath(cfg, "alice"), badSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOwedLedger(cfg, "alice"); err == nil {
		t.Fatal("zero seq must be rejected on load")
	}
}

func TestRecordOwedRejectsForeignAsker(t *testing.T) {
	cfg := newOwedTestConfig(t)
	now := time.Now()
	// key.From must equal the ledger owner (asker).
	if err := RecordOwed(cfg, "alice", MsgID{To: "bob", From: "mallory", Seq: 1}, now.Add(time.Hour), now); err == nil {
		t.Fatal("RecordOwed must reject key.From != asker")
	}
}
