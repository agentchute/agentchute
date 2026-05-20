package loop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLedgerTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
}

func newPendingEntry(messageID, from, to, task string) PendingReplyEntry {
	return PendingReplyEntry{
		MessageID:        messageID,
		From:             from,
		To:               to,
		Task:             task,
		OriginalFilename: messageID + "_from-" + from + "_msg-aaaa.md",
		ArchivePath:      ".rehumanlabs/loop/archive/example.md",
	}
}

func TestLoadPendingLedgerMissingFileReturnsEmpty(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	ledger, err := LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatalf("LoadPendingLedger: %v", err)
	}
	if ledger == nil {
		t.Fatal("LoadPendingLedger returned nil ledger")
	}
	if len(ledger.Pending) != 0 {
		t.Fatalf("Pending = %d, want 0", len(ledger.Pending))
	}
}

func TestLoadPendingLedgerInvalidAgentID(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	if _, err := LoadPendingLedger(cfg, "BAD ID"); err == nil {
		t.Fatal("expected agent_id validation error")
	}
}

func TestRecordPendingReplyCreatesEntry(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)

	entry := newPendingEntry("2026-05-19T17:53:59.561894Z", "codex", "claude-code", "R1 protocol")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatalf("RecordPendingReply: %v", err)
	}

	ledger, err := LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 1 {
		t.Fatalf("Pending = %d, want 1", len(ledger.Pending))
	}
	got := ledger.Pending[0]
	if got.MessageID != entry.MessageID {
		t.Errorf("MessageID = %q, want %q", got.MessageID, entry.MessageID)
	}
	if got.Status != PendingReplyStatusPending {
		t.Errorf("Status = %q, want pending", got.Status)
	}
	if got.RecordedAt != "2026-05-19T17:54:30Z" {
		t.Errorf("RecordedAt = %q, want 2026-05-19T17:54:30Z", got.RecordedAt)
	}
	if got.ReplySentAt != nil {
		t.Errorf("ReplySentAt = %v, want nil", got.ReplySentAt)
	}
	if got.DeferredAt != nil {
		t.Errorf("DeferredAt = %v, want nil", got.DeferredAt)
	}
}

func TestRecordPendingReplyIsIdempotentByMessageID(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")

	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	// Second record with the same message_id is a no-op.
	entry2 := entry
	entry2.Task = "updated task"
	entry2.ArchivePath = "different/path.md"
	if err := RecordPendingReply(cfg, "claude-code", entry2, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	ledger, err := LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 1 {
		t.Fatalf("Pending = %d, want 1 (idempotent on message_id)", len(ledger.Pending))
	}
	if ledger.Pending[0].Task != "review" {
		t.Errorf("Task = %q, want %q (first observation wins)", ledger.Pending[0].Task, "review")
	}
}

func TestRecordPendingReplyRequiresFields(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	cases := []struct {
		name  string
		entry PendingReplyEntry
	}{
		{"no message_id", PendingReplyEntry{From: "a", To: "b", OriginalFilename: "x.md"}},
		{"no from", PendingReplyEntry{MessageID: "m", To: "b", OriginalFilename: "x.md"}},
		{"no to", PendingReplyEntry{MessageID: "m", From: "a", OriginalFilename: "x.md"}},
		{"no filename", PendingReplyEntry{MessageID: "m", From: "a", To: "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := RecordPendingReply(cfg, "claude-code", tc.entry, now); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// Test 9 from spec rev3 Part 4: ledger identity (no collision) on same sender + same task.
func TestRecordPendingReplyDistinguishesByMessageID(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)

	entryA := newPendingEntry("2026-05-19T17:53:00.000001Z", "codex", "claude-code", "review")
	entryA.OriginalFilename = "2026-05-19T17-53-00-000001Z_from-codex_msg-aaaa.md"
	entryB := newPendingEntry("2026-05-19T17:53:00.000002Z", "codex", "claude-code", "review")
	entryB.OriginalFilename = "2026-05-19T17-53-00-000002Z_from-codex_msg-bbbb.md"

	if err := RecordPendingReply(cfg, "claude-code", entryA, now); err != nil {
		t.Fatal(err)
	}
	if err := RecordPendingReply(cfg, "claude-code", entryB, now); err != nil {
		t.Fatal(err)
	}

	ledger, err := LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 2 {
		t.Fatalf("Pending = %d, want 2 (distinct message_ids)", len(ledger.Pending))
	}

	// Reply to one; the other must still block.
	if err := MarkPendingReplied(cfg, "claude-code", entryA.MessageID, "reply-msg-id", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ledger, _ = LoadPendingLedger(cfg, "claude-code")
	pending := ledger.PendingEntries()
	if len(pending) != 1 {
		t.Fatalf("PendingEntries = %d, want 1 after replying to one", len(pending))
	}
	if pending[0].MessageID != entryB.MessageID {
		t.Errorf("remaining pending = %q, want %q", pending[0].MessageID, entryB.MessageID)
	}
}

func TestMarkPendingRepliedTransition(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	recordedAt := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	repliedAt := time.Date(2026, 5, 19, 18, 0, 0, 0, time.UTC)

	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, recordedAt); err != nil {
		t.Fatal(err)
	}

	if err := MarkPendingReplied(cfg, "claude-code", "msg-1", "reply-msg-99", repliedAt); err != nil {
		t.Fatal(err)
	}

	ledger, err := LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ledger.FindByMessageID("msg-1")
	if !ok {
		t.Fatal("entry vanished after MarkPendingReplied")
	}
	if got.Status != PendingReplyStatusReplied {
		t.Errorf("Status = %q, want replied", got.Status)
	}
	if got.ReplySentAt == nil || *got.ReplySentAt != "2026-05-19T18:00:00Z" {
		t.Errorf("ReplySentAt = %v, want 2026-05-19T18:00:00Z", got.ReplySentAt)
	}
	if got.ReplyMessageID == nil || *got.ReplyMessageID != "reply-msg-99" {
		t.Errorf("ReplyMessageID = %v, want reply-msg-99", got.ReplyMessageID)
	}
	if len(ledger.PendingEntries()) != 0 {
		t.Errorf("PendingEntries non-empty after reply: %d", len(ledger.PendingEntries()))
	}
}

func TestMarkPendingRepliedNotFound(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	err := MarkPendingReplied(cfg, "claude-code", "missing", "reply-id", time.Now().UTC())
	if !errors.Is(err, ErrLedgerEntryNotFound) {
		t.Fatalf("err = %v, want ErrLedgerEntryNotFound", err)
	}
}

func TestMarkPendingRepliedNotPending(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReplied(cfg, "claude-code", "msg-1", "reply-99", now); err != nil {
		t.Fatal(err)
	}
	// Second reply against a non-pending entry must surface a typed error.
	err := MarkPendingReplied(cfg, "claude-code", "msg-1", "reply-100", now)
	if !errors.Is(err, ErrLedgerEntryNotPending) {
		t.Fatalf("err = %v, want ErrLedgerEntryNotPending", err)
	}
}

func TestMarkPendingDeferredTransition(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	deferAt := time.Date(2026, 5, 19, 18, 30, 0, 0, time.UTC)

	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingDeferred(cfg, "claude-code", "msg-1", "needs research", "2026-05-26T00:00:00Z", deferAt); err != nil {
		t.Fatal(err)
	}

	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	got, _ := ledger.FindByMessageID("msg-1")
	if got.Status != PendingReplyStatusDeferred {
		t.Errorf("Status = %q, want deferred", got.Status)
	}
	if got.DeferredAt == nil || *got.DeferredAt != "2026-05-19T18:30:00Z" {
		t.Errorf("DeferredAt = %v, want 2026-05-19T18:30:00Z", got.DeferredAt)
	}
	if got.DeferredReason == nil || *got.DeferredReason != "needs research" {
		t.Errorf("DeferredReason = %v, want \"needs research\"", got.DeferredReason)
	}
	if got.DeferredUntil == nil || *got.DeferredUntil != "2026-05-26T00:00:00Z" {
		t.Errorf("DeferredUntil = %v, want 2026-05-26T00:00:00Z", got.DeferredUntil)
	}
	// Deferred entries do not block the finish gate.
	if len(ledger.PendingEntries()) != 0 {
		t.Errorf("PendingEntries = %d after defer, want 0", len(ledger.PendingEntries()))
	}
}

func TestMarkPendingDeferredRequiresReason(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingDeferred(cfg, "claude-code", "msg-1", "", "", now); err == nil {
		t.Fatal("expected reason-required error")
	}
}

func TestMarkPendingDeferredOptionalUntil(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingDeferred(cfg, "claude-code", "msg-1", "no eta", "", now); err != nil {
		t.Fatal(err)
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	got, _ := ledger.FindByMessageID("msg-1")
	if got.DeferredUntil != nil {
		t.Errorf("DeferredUntil = %v, want nil for empty --until", got.DeferredUntil)
	}
}

// Atomic write semantics: the saved file is mode 0600 and round-trips cleanly.
func TestSavePendingLedgerAtomicWriteAndPerms(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}

	path := cfg.PendingRepliesPath("claude-code")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("ledger file mode = %o, want 600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("state dir mode = %o, want 700", mode)
	}
}

// On-disk JSON matches the spec rev3 A.9 shape: nullable fields encode as
// literal `null`, status is one of the canonical strings, recorded_at is
// RFC3339 UTC.
func TestPendingLedgerJSONShape(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	entry := newPendingEntry("2026-05-19T17:53:59.561894Z", "codex", "claude-code", "R1 protocol improvements")
	entry.OriginalFilename = "2026-05-19T17-53-59-561894Z_from-codex_msg-8cbd.md"
	entry.ArchivePath = ".rehumanlabs/loop/archive/2026-05-19T17-54-30Z_to-claude-code_2026-05-19T17-53-59-561894Z_from-codex_msg-8cbd.md"

	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(cfg.PendingRepliesPath("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`"message_id": "2026-05-19T17:53:59.561894Z"`,
		`"from": "codex"`,
		`"to": "claude-code"`,
		`"status": "pending"`,
		`"recorded_at": "2026-05-19T17:54:30Z"`,
		`"reply_sent_at": null`,
		`"reply_message_id": null`,
		`"deferred_at": null`,
		`"deferred_until": null`,
		`"deferred_reason": null`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("JSON missing %s\nfull file:\n%s", want, text)
		}
	}

	// Parse-back round-trip preserves everything.
	var parsed PendingLedger
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Pending) != 1 {
		t.Fatalf("parsed Pending = %d, want 1", len(parsed.Pending))
	}
}

func TestLoadPendingLedgerRejectsCorruptJSON(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	path := cfg.PendingRepliesPath("claude-code")
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPendingLedger(cfg, "claude-code"); err == nil {
		t.Fatal("expected parse error for corrupt JSON")
	}
}

func TestLoadPendingLedgerRejectsOversize(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	path := cfg.PendingRepliesPath("claude-code")
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, MaxPendingLedgerBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPendingLedger(cfg, "claude-code"); err == nil {
		t.Fatal("expected oversize error")
	}
}

// Codex review (4d34826): empty reply_message_id must not transition a
// pending entry, since the schema records this field for traceability.
func TestMarkPendingRepliedRequiresReplyMessageID(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReplied(cfg, "claude-code", "msg-1", "  ", now); err == nil {
		t.Fatal("expected error on empty reply_message_id")
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	got, _ := ledger.FindByMessageID("msg-1")
	if got.Status != PendingReplyStatusPending {
		t.Errorf("Status = %q, want pending (no transition on validation failure)", got.Status)
	}
	if got.ReplyMessageID != nil {
		t.Errorf("ReplyMessageID = %v, want nil after rejected transition", got.ReplyMessageID)
	}
}

// Codex review (4d34826): archive_path is a required schema field; empty
// must reject before persistence.
func TestRecordPendingReplyRequiresArchivePath(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := PendingReplyEntry{
		MessageID:        "msg-1",
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "msg-1_from-codex_msg-aaaa.md",
		// ArchivePath intentionally empty
	}
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err == nil {
		t.Fatal("expected error on empty archive_path")
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	if len(ledger.Pending) != 0 {
		t.Errorf("Pending = %d, want 0 (rejected entry must not be persisted)", len(ledger.Pending))
	}
}

// Codex review (4d34826): same message_id with different original_filename
// must NOT silently no-op — that would drop a delivered obligation. Same
// filename remains idempotent (re-archive replay safety); different filename
// surfaces as a typed collision error.
func TestRecordPendingReplyCollisionOnDifferentFilename(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()

	first := newPendingEntry("shared-msg-id", "codex", "claude-code", "review")
	first.OriginalFilename = "first_from-codex_msg-aaaa.md"
	if err := RecordPendingReply(cfg, "claude-code", first, now); err != nil {
		t.Fatal(err)
	}

	// Same message_id, DIFFERENT original_filename: must error, not silently no-op.
	second := newPendingEntry("shared-msg-id", "codex", "claude-code", "review")
	second.OriginalFilename = "second_from-codex_msg-bbbb.md"
	err := RecordPendingReply(cfg, "claude-code", second, now.Add(time.Second))
	if err == nil {
		t.Fatal("expected collision error on same message_id with different original_filename")
	}
	if !errors.Is(err, ErrLedgerEntryCollision) {
		t.Errorf("err = %v, want ErrLedgerEntryCollision", err)
	}

	// Original-filename match still idempotent.
	dup := newPendingEntry("shared-msg-id", "codex", "claude-code", "review")
	dup.OriginalFilename = "first_from-codex_msg-aaaa.md"
	if err := RecordPendingReply(cfg, "claude-code", dup, now.Add(2*time.Second)); err != nil {
		t.Fatalf("idempotent replay should not error: %v", err)
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	if len(ledger.Pending) != 1 {
		t.Errorf("Pending = %d, want 1 (idempotent replay must not duplicate)", len(ledger.Pending))
	}
}

// Codex review (4d34826): unknown/corrupt status values must be flagged on
// load so callers can refuse to operate against a damaged ledger.
func TestLoadPendingLedgerRejectsUnknownStatus(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	path := cfg.PendingRepliesPath("claude-code")
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	corrupt := `{"pending":[{"message_id":"m","from":"x","to":"y","original_filename":"f","archive_path":"a","recorded_at":"2026-01-01T00:00:00Z","status":"weird","reply_sent_at":null,"reply_message_id":null,"deferred_at":null,"deferred_until":null,"deferred_reason":null}]}`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPendingLedger(cfg, "claude-code"); err == nil {
		t.Fatal("expected error on unknown status value")
	}
}

// Codex review (4d34826): defense-in-depth — even if Load is bypassed, the
// in-memory PendingEntries must treat anything outside the canonical
// replied/deferred terminal states as still-blocking.
func TestPendingEntriesConservativeOnUnknownStatus(t *testing.T) {
	ledger := &PendingLedger{Pending: []PendingReplyEntry{
		{MessageID: "a", Status: PendingReplyStatusPending},
		{MessageID: "b", Status: PendingReplyStatusReplied},
		{MessageID: "c", Status: PendingReplyStatusDeferred},
		{MessageID: "d", Status: PendingReplyStatus("unknown-future-value")},
		{MessageID: "e", Status: ""},
	}}
	pending := ledger.PendingEntries()
	wantIDs := map[string]bool{"a": true, "d": true, "e": true}
	if len(pending) != len(wantIDs) {
		t.Fatalf("PendingEntries = %d, want %d (pending + unknown + empty all block)", len(pending), len(wantIDs))
	}
	for _, e := range pending {
		if !wantIDs[e.MessageID] {
			t.Errorf("unexpected entry in PendingEntries: %s status=%q", e.MessageID, e.Status)
		}
	}
}

// Codex review (4d34826): strict A.9 shape — task field always emitted,
// even when empty, so the JSON shape is fixed across all entries.
func TestPendingLedgerJSONShapeIncludesTaskFieldWhenEmpty(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Date(2026, 5, 19, 17, 54, 30, 0, time.UTC)
	entry := PendingReplyEntry{
		MessageID:        "m",
		From:             "codex",
		To:               "claude-code",
		Task:             "",
		OriginalFilename: "m_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/m.md",
	}
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.PendingRepliesPath("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"task": ""`) {
		t.Errorf("expected `\"task\": \"\"` in JSON for empty task; got:\n%s", data)
	}
}

// Codex review (eb58443): RecordPendingReply must validate entry.From and
// entry.To as agent_ids before persisting. A bad value would later be used
// as a filesystem path component by defer / pending and could escape the
// intended inbox namespace.
func TestRecordPendingReplyRejectsInvalidAgentIDFields(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	cases := []struct {
		name string
		mut  func(*PendingReplyEntry)
	}{
		{"bad From slash", func(e *PendingReplyEntry) { e.From = "evil/agent" }},
		{"bad From uppercase", func(e *PendingReplyEntry) { e.From = "Evil" }},
		{"bad To dotdot", func(e *PendingReplyEntry) { e.To = ".." }},
		{"bad To space", func(e *PendingReplyEntry) { e.To = "bad id" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := newPendingEntry("msg-1", "codex", "claude-code", "review")
			tc.mut(&entry)
			if err := RecordPendingReply(cfg, "claude-code", entry, now); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// Hand-written or peer-corrupted ledger state with an invalid From/To
// must be rejected on load, defense-in-depth on top of the Record-time
// validation.
func TestLoadPendingLedgerRejectsInvalidAgentIDFields(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	path := cfg.PendingRepliesPath("claude-code")
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	corrupt := `{"pending":[{"message_id":"m","from":"evil/agent","to":"claude-code","task":"","original_filename":"f","archive_path":"a","recorded_at":"2026-01-01T00:00:00Z","status":"pending","reply_sent_at":null,"reply_message_id":null,"deferred_at":null,"deferred_until":null,"deferred_reason":null}]}`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPendingLedger(cfg, "claude-code"); err == nil {
		t.Fatal("expected error on invalid from agent_id in ledger")
	}
}

func TestFindByMessageIDMiss(t *testing.T) {
	ledger := &PendingLedger{Pending: []PendingReplyEntry{
		{MessageID: "exists", Status: PendingReplyStatusPending},
	}}
	if _, ok := ledger.FindByMessageID("missing"); ok {
		t.Fatal("FindByMessageID returned ok=true for missing id")
	}
	got, ok := ledger.FindByMessageID("exists")
	if !ok {
		t.Fatal("FindByMessageID returned ok=false for present id")
	}
	if got.MessageID != "exists" {
		t.Errorf("MessageID = %q, want exists", got.MessageID)
	}
}
