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
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
}

func newPendingEntry(messageID, from, to, task string) PendingReplyEntry {
	return PendingReplyEntry{
		MessageID:        messageID,
		From:             from,
		To:               to,
		Task:             task,
		OriginalFilename: messageID + "_from-" + from + "_msg-aaaa.md",
		ArchivePath:      ".examplecorp/loop/archive/example.md",
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
	if err := MarkPendingReplied(cfg, "claude-code", entryA.MessageID, "codex", "reply-msg-id", now.Add(time.Minute)); err != nil {
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

	if err := MarkPendingReplied(cfg, "claude-code", "msg-1", "codex", "reply-msg-99", repliedAt); err != nil {
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
	err := MarkPendingReplied(cfg, "claude-code", "missing", "codex", "reply-id", time.Now().UTC())
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
	if err := MarkPendingReplied(cfg, "claude-code", "msg-1", "codex", "reply-99", now); err != nil {
		t.Fatal(err)
	}
	// Second reply against a non-pending entry must surface a typed error.
	err := MarkPendingReplied(cfg, "claude-code", "msg-1", "codex", "reply-100", now)
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
	if err := MarkPendingDeferred(cfg, "claude-code", "msg-1", "codex", "needs research", "2026-05-26T00:00:00Z", deferAt); err != nil {
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
	if err := MarkPendingDeferred(cfg, "claude-code", "msg-1", "codex", "", "", now); err == nil {
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
	if err := MarkPendingDeferred(cfg, "claude-code", "msg-1", "codex", "no eta", "", now); err != nil {
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
	entry.ArchivePath = ".examplecorp/loop/archive/2026-05-19T17-54-30Z_to-claude-code_2026-05-19T17-53-59-561894Z_from-codex_msg-8cbd.md"

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
	if err := MarkPendingReplied(cfg, "claude-code", "msg-1", "codex", "  ", now); err == nil {
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

// WI-2 Fix 1: the obligation is keyed on the recipient-trusted OriginalFilename
// (the inbox filename), NOT the sender-controlled message_id. Two deliveries
// that share a message_id but land under different filenames are two REAL
// obligations and must BOTH be recorded — no fatal collision wedge (a peer can
// no longer poison the consume loop by reusing a message_id). message_id stays
// as informational metadata.
func TestRecordPendingReply_DuplicateMessageIDDifferentFilenameBothRecorded(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()

	first := newPendingEntry("shared-msg-id", "codex", "claude-code", "review")
	first.OriginalFilename = "first_from-codex_msg-aaaa.md"
	if err := RecordPendingReply(cfg, "claude-code", first, now); err != nil {
		t.Fatalf("record first: %v", err)
	}

	// Same message_id, DIFFERENT original_filename: a distinct obligation, no error.
	second := newPendingEntry("shared-msg-id", "codex", "claude-code", "review")
	second.OriginalFilename = "second_from-codex_msg-bbbb.md"
	if err := RecordPendingReply(cfg, "claude-code", second, now.Add(time.Second)); err != nil {
		t.Fatalf("record second (different filename) must not error: %v", err)
	}

	ledger, err := LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 2 {
		t.Fatalf("Pending = %d, want 2 (same message_id, distinct filenames = distinct obligations)", len(ledger.Pending))
	}
	gotFiles := map[string]bool{}
	for _, e := range ledger.Pending {
		gotFiles[e.OriginalFilename] = true
		if e.MessageID != "shared-msg-id" {
			t.Errorf("MessageID = %q, want shared-msg-id (metadata preserved)", e.MessageID)
		}
	}
	for _, want := range []string{"first_from-codex_msg-aaaa.md", "second_from-codex_msg-bbbb.md"} {
		if !gotFiles[want] {
			t.Errorf("missing obligation for filename %q", want)
		}
	}
}

// WI-2 Fix 1: re-recording the SAME OriginalFilename is idempotent (re-archive
// replay safety) — single entry, nil error, first observation wins.
func TestRecordPendingReply_SameFilenameIdempotent(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()

	first := newPendingEntry("msg-1", "codex", "claude-code", "review")
	first.OriginalFilename = "only_from-codex_msg-aaaa.md"
	if err := RecordPendingReply(cfg, "claude-code", first, now); err != nil {
		t.Fatal(err)
	}

	// Same filename again, even with a *different* message_id and task, must
	// no-op (the filename is the primary key; first observation wins).
	dup := newPendingEntry("msg-2-rewritten", "codex", "claude-code", "rewritten task")
	dup.OriginalFilename = "only_from-codex_msg-aaaa.md"
	if err := RecordPendingReply(cfg, "claude-code", dup, now.Add(time.Second)); err != nil {
		t.Fatalf("idempotent replay on same filename should not error: %v", err)
	}

	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	if len(ledger.Pending) != 1 {
		t.Fatalf("Pending = %d, want 1 (idempotent on filename)", len(ledger.Pending))
	}
	if ledger.Pending[0].MessageID != "msg-1" {
		t.Errorf("MessageID = %q, want msg-1 (first observation wins)", ledger.Pending[0].MessageID)
	}
	if ledger.Pending[0].Task != "review" {
		t.Errorf("Task = %q, want review (first observation wins)", ledger.Pending[0].Task)
	}
}

// WI-2 Fix 1: discharging a thread discharges every pending obligation that
// shares the thread's message_id. With the poison case (two filenames, one
// message_id), marking ALL matching entries replied is correct and cannot leave
// an obligation un-discharged. The normal single-match case is covered too.
func TestMarkPendingReplied_DischargesAllMatchingMessageID(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()

	// Two obligations, shared message_id, distinct filenames.
	a := newPendingEntry("thread-1", "codex", "claude-code", "review")
	a.OriginalFilename = "a_from-codex_msg-aaaa.md"
	b := newPendingEntry("thread-1", "codex", "claude-code", "review")
	b.OriginalFilename = "b_from-codex_msg-bbbb.md"
	// A third, UNRELATED obligation that must stay pending.
	c := newPendingEntry("thread-2", "codex", "claude-code", "other")
	c.OriginalFilename = "c_from-codex_msg-cccc.md"
	for _, e := range []PendingReplyEntry{a, b, c} {
		if err := RecordPendingReply(cfg, "claude-code", e, now); err != nil {
			t.Fatal(err)
		}
	}

	if err := MarkPendingReplied(cfg, "claude-code", "thread-1", "codex", "reply-msg", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkPendingReplied: %v", err)
	}

	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	for _, e := range ledger.Pending {
		switch e.MessageID {
		case "thread-1":
			if e.Status != PendingReplyStatusReplied {
				t.Errorf("thread-1 entry %q status = %q, want replied", e.OriginalFilename, e.Status)
			}
			if e.ReplyMessageID == nil || *e.ReplyMessageID != "reply-msg" {
				t.Errorf("thread-1 entry %q reply_message_id = %v, want reply-msg", e.OriginalFilename, e.ReplyMessageID)
			}
		case "thread-2":
			if e.Status != PendingReplyStatusPending {
				t.Errorf("unrelated thread-2 status = %q, want pending (untouched)", e.Status)
			}
		}
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 || pending[0].MessageID != "thread-2" {
		t.Fatalf("PendingEntries = %+v, want only thread-2 still blocking", pending)
	}
}

// Single-match normal case: one obligation, MarkPendingReplied discharges it.
func TestMarkPendingReplied_SingleMatchDischarges(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("only-msg", "codex", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	if err := MarkPendingReplied(cfg, "claude-code", "only-msg", "codex", "reply-1", now); err != nil {
		t.Fatalf("MarkPendingReplied: %v", err)
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	if len(ledger.PendingEntries()) != 0 {
		t.Errorf("PendingEntries = %d, want 0 after single-match discharge", len(ledger.PendingEntries()))
	}
}

// MarkPendingDeferred also discharges ALL pending entries sharing the message_id.
func TestMarkPendingDeferred_DischargesAllMatchingMessageID(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	a := newPendingEntry("thread-1", "codex", "claude-code", "review")
	a.OriginalFilename = "a_from-codex_msg-aaaa.md"
	b := newPendingEntry("thread-1", "codex", "claude-code", "review")
	b.OriginalFilename = "b_from-codex_msg-bbbb.md"
	for _, e := range []PendingReplyEntry{a, b} {
		if err := RecordPendingReply(cfg, "claude-code", e, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := MarkPendingDeferred(cfg, "claude-code", "thread-1", "codex", "no eta", "", now); err != nil {
		t.Fatalf("MarkPendingDeferred: %v", err)
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	if len(ledger.PendingEntries()) != 0 {
		t.Errorf("PendingEntries = %d, want 0 after deferring all matches", len(ledger.PendingEntries()))
	}
	for _, e := range ledger.Pending {
		if e.Status != PendingReplyStatusDeferred {
			t.Errorf("entry %q status = %q, want deferred", e.OriginalFilename, e.Status)
		}
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

// WI-2 follow-up: MarkPendingReplied is SENDER-SCOPED. message_id is
// sender-controlled and reusable, so two senders can land entries with the same
// message_id. Replying to one sender's obligation must transition ONLY that
// sender's pending entries and leave the other sender's untouched.
func TestMarkPendingReplied_SenderScopedOnly(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()

	// Two obligations, SAME message_id, DIFFERENT senders.
	fromA := newPendingEntry("shared-id", "peer-a", "claude-code", "review")
	fromA.OriginalFilename = "a_from-peer-a_msg-aaaa.md"
	fromB := newPendingEntry("shared-id", "peer-b", "claude-code", "review")
	fromB.OriginalFilename = "b_from-peer-b_msg-bbbb.md"
	for _, e := range []PendingReplyEntry{fromA, fromB} {
		if err := RecordPendingReply(cfg, "claude-code", e, now); err != nil {
			t.Fatal(err)
		}
	}

	// Reply to peer-a's obligation only.
	if err := MarkPendingReplied(cfg, "claude-code", "shared-id", "peer-a", "reply-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkPendingReplied: %v", err)
	}

	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	for _, e := range ledger.Pending {
		switch e.From {
		case "peer-a":
			if e.Status != PendingReplyStatusReplied {
				t.Errorf("peer-a entry status = %q, want replied", e.Status)
			}
		case "peer-b":
			if e.Status != PendingReplyStatusPending {
				t.Errorf("peer-b entry status = %q, want pending (other sender's obligation must NOT be cleared)", e.Status)
			}
		}
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 || pending[0].From != "peer-b" {
		t.Fatalf("PendingEntries = %+v, want only peer-b still blocking", pending)
	}
}

// WI-2 follow-up: a (message_id, fromSender) with NO match returns NotFound
// even if the same message_id exists under a DIFFERENT sender. The scope is the
// (message_id, sender) pair, not the bare message_id.
func TestMarkPendingReplied_OtherSenderOnlyIsNotFound(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	entry := newPendingEntry("shared-id", "peer-a", "claude-code", "review")
	if err := RecordPendingReply(cfg, "claude-code", entry, now); err != nil {
		t.Fatal(err)
	}
	// Discharge scoped to peer-b: no (shared-id, peer-b) row exists.
	err := MarkPendingReplied(cfg, "claude-code", "shared-id", "peer-b", "reply-1", now)
	if !errors.Is(err, ErrLedgerEntryNotFound) {
		t.Fatalf("err = %v, want ErrLedgerEntryNotFound (no row for the scoped sender)", err)
	}
	// peer-a's obligation stays pending.
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	if len(ledger.PendingEntries()) != 1 {
		t.Errorf("PendingEntries = %d, want 1 (other-sender discharge must not touch peer-a)", len(ledger.PendingEntries()))
	}
}

// WI-2 follow-up: MarkPendingDeferred is SENDER-SCOPED too — deferring one
// sender's obligation leaves a same-message_id obligation from another sender
// pending.
func TestMarkPendingDeferred_SenderScopedOnly(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()
	fromA := newPendingEntry("shared-id", "peer-a", "claude-code", "review")
	fromA.OriginalFilename = "a_from-peer-a_msg-aaaa.md"
	fromB := newPendingEntry("shared-id", "peer-b", "claude-code", "review")
	fromB.OriginalFilename = "b_from-peer-b_msg-bbbb.md"
	for _, e := range []PendingReplyEntry{fromA, fromB} {
		if err := RecordPendingReply(cfg, "claude-code", e, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := MarkPendingDeferred(cfg, "claude-code", "shared-id", "peer-a", "no eta", "", now); err != nil {
		t.Fatalf("MarkPendingDeferred: %v", err)
	}
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	for _, e := range ledger.Pending {
		switch e.From {
		case "peer-a":
			if e.Status != PendingReplyStatusDeferred {
				t.Errorf("peer-a entry status = %q, want deferred", e.Status)
			}
		case "peer-b":
			if e.Status != PendingReplyStatusPending {
				t.Errorf("peer-b entry status = %q, want pending (other sender's obligation must NOT be cleared)", e.Status)
			}
		}
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 || pending[0].From != "peer-b" {
		t.Fatalf("PendingEntries = %+v, want only peer-b still blocking", pending)
	}
}

// WI-2 follow-up: the terminal-first short-circuit is gone. Two entries, same
// sender, same message_id: first already terminal (replied), second still
// pending. MarkPendingReplied must discharge the second (skip the terminal one,
// never block on it). RED before the fix only at the send/defer layer; this
// asserts the ledger primitive handles the mixed-status set directly.
func TestMarkPendingReplied_TerminalFirstDoesNotStrandPending(t *testing.T) {
	cfg := newLedgerTestConfig(t)
	now := time.Now().UTC()

	first := newPendingEntry("shared-id", "peer-a", "claude-code", "review")
	first.OriginalFilename = "first_from-peer-a_msg-aaaa.md"
	second := newPendingEntry("shared-id", "peer-a", "claude-code", "review")
	second.OriginalFilename = "second_from-peer-a_msg-bbbb.md"
	for _, e := range []PendingReplyEntry{first, second} {
		if err := RecordPendingReply(cfg, "claude-code", e, now); err != nil {
			t.Fatal(err)
		}
	}
	// Discharge the FIRST filename's obligation directly via the ledger by
	// hand-marking it replied, simulating a prior partial discharge.
	ledger, _ := LoadPendingLedger(cfg, "claude-code")
	ledger.Pending[0].Status = PendingReplyStatusReplied
	rid := "earlier-reply"
	ledger.Pending[0].ReplyMessageID = &rid
	sentAt := formatLedgerTimestamp(now)
	ledger.Pending[0].ReplySentAt = &sentAt
	if err := SavePendingLedger(cfg, "claude-code", ledger); err != nil {
		t.Fatal(err)
	}

	// Now discharge the message_id again: the still-pending second entry must
	// transition; the already-terminal first must not block it.
	if err := MarkPendingReplied(cfg, "claude-code", "shared-id", "peer-a", "reply-2", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkPendingReplied: %v (terminal-first entry must not short-circuit the pending one)", err)
	}
	ledger, _ = LoadPendingLedger(cfg, "claude-code")
	if len(ledger.PendingEntries()) != 0 {
		t.Errorf("PendingEntries = %d, want 0 (the pending duplicate must not be stranded)", len(ledger.PendingEntries()))
	}
}

func TestPendingByMessageIDFrom(t *testing.T) {
	ledger := &PendingLedger{Pending: []PendingReplyEntry{
		{MessageID: "m", From: "peer-a", To: "claude-code", Status: PendingReplyStatusPending},
		{MessageID: "m", From: "peer-a", To: "claude-code", Status: PendingReplyStatusReplied},
		{MessageID: "m", From: "peer-b", To: "claude-code", Status: PendingReplyStatusPending},
		{MessageID: "other", From: "peer-a", To: "claude-code", Status: PendingReplyStatusPending},
	}}
	got := ledger.PendingByMessageIDFrom("m", "peer-a")
	if len(got) != 1 {
		t.Fatalf("PendingByMessageIDFrom(m, peer-a) = %d, want 1 (only the pending peer-a row)", len(got))
	}
	if got[0].Status != PendingReplyStatusPending || got[0].From != "peer-a" {
		t.Errorf("got %+v, want a pending peer-a row", got[0])
	}
	if n := len(ledger.PendingByMessageIDFrom("m", "peer-c")); n != 0 {
		t.Errorf("PendingByMessageIDFrom(m, peer-c) = %d, want 0", n)
	}
}

func TestEntriesByMessageIDFrom(t *testing.T) {
	ledger := &PendingLedger{Pending: []PendingReplyEntry{
		{MessageID: "m", From: "peer-a", To: "claude-code", Status: PendingReplyStatusPending},
		{MessageID: "m", From: "peer-a", To: "claude-code", Status: PendingReplyStatusReplied},
		{MessageID: "m", From: "peer-b", To: "claude-code", Status: PendingReplyStatusPending},
		{MessageID: "other", From: "peer-a", To: "claude-code", Status: PendingReplyStatusPending},
	}}
	// ALL entries (any status) matching message_id AND from.
	got := ledger.EntriesByMessageIDFrom("m", "peer-a")
	if len(got) != 2 {
		t.Fatalf("EntriesByMessageIDFrom(m, peer-a) = %d, want 2 (pending + replied peer-a rows)", len(got))
	}
	for _, e := range got {
		if e.From != "peer-a" || e.MessageID != "m" {
			t.Errorf("got %+v, want a peer-a/m row", e)
		}
	}
	// A sender with no entry at all returns empty (distinguishes
	// "exists-but-terminal" from "no-such-sender-entry" for send).
	if n := len(ledger.EntriesByMessageIDFrom("m", "peer-c")); n != 0 {
		t.Errorf("EntriesByMessageIDFrom(m, peer-c) = %d, want 0", n)
	}
}

func TestFirstPendingByMessageID(t *testing.T) {
	ledger := &PendingLedger{Pending: []PendingReplyEntry{
		// First bare row is TERMINAL — must be skipped.
		{MessageID: "m", From: "peer-a", To: "claude-code", Status: PendingReplyStatusReplied},
		// Later row is PENDING — must be the one returned.
		{MessageID: "m", From: "peer-b", To: "claude-code", Status: PendingReplyStatusPending},
	}}
	got, ok := ledger.FirstPendingByMessageID("m")
	if !ok {
		t.Fatal("FirstPendingByMessageID(m) ok=false, want true (a later pending row exists)")
	}
	if got.From != "peer-b" || got.Status != PendingReplyStatusPending {
		t.Errorf("got %+v, want the pending peer-b row (terminal first row must be skipped)", got)
	}
	// All-terminal ⇒ not ok.
	allTerminal := &PendingLedger{Pending: []PendingReplyEntry{
		{MessageID: "m", From: "peer-a", To: "claude-code", Status: PendingReplyStatusReplied},
		{MessageID: "m", From: "peer-b", To: "claude-code", Status: PendingReplyStatusDeferred},
	}}
	if _, ok := allTerminal.FirstPendingByMessageID("m"); ok {
		t.Error("FirstPendingByMessageID(m) ok=true on all-terminal rows, want false")
	}
	// No such message_id ⇒ not ok.
	if _, ok := ledger.FirstPendingByMessageID("nope"); ok {
		t.Error("FirstPendingByMessageID(nope) ok=true for missing id, want false")
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
