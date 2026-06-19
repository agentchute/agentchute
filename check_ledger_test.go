package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// writeReplyRequiredInbox drops a valid §6.1-named, reply_required message
// into recipient's inbox and returns its filename + path.
func writeReplyRequiredInbox(t *testing.T, cfg *loop.Config, recipient, messageID string) (string, string) {
	t.Helper()
	return writeReplyRequiredInboxNamed(t, cfg, recipient, messageID,
		"2026-05-19T22-02-00-000000Z_from-codex_msg-abcd.md")
}

// writeReplyRequiredInboxNamed is writeReplyRequiredInbox with an explicit
// inbox filename, so tests can deliver two reply_required messages that share a
// message_id but have distinct (recipient-trusted) filenames.
func writeReplyRequiredInboxNamed(t *testing.T, cfg *loop.Config, recipient, messageID, filename string) (string, string) {
	t.Helper()
	inbox := cfg.AgentInboxDir(recipient)
	body := "---\n" +
		"message_id: " + messageID + "\n" +
		"from: codex\n" +
		"to: " + recipient + "\n" +
		"reply_required: true\n" +
		"task: please reply\n" +
		"---\n" +
		"\nplease reply\n"
	path := filepath.Join(inbox, filename)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename, path
}

// TestCheck_DuplicateMessageIDDoesNotWedge: a peer cannot poison the consume
// loop by sending two reply_required messages with the SAME message_id but
// different filenames. `check` must consume BOTH without error and record BOTH
// obligations (filename-keyed). Pre-WI-2, the second delivery tripped the fatal
// ErrLedgerEntryCollision wedge in RecordPendingReply and `check` returned a
// non-nil error, stranding the message in the inbox.
func TestCheck_DuplicateMessageIDDoesNotWedge(t *testing.T) {
	root, cfg := setupSendFixture(t)
	f1, p1 := writeReplyRequiredInboxNamed(t, cfg, "claude-code", "poison-id",
		"2026-05-19T22-02-00-000000Z_from-codex_msg-aaaa.md")
	f2, p2 := writeReplyRequiredInboxNamed(t, cfg, "claude-code", "poison-id",
		"2026-05-19T22-02-01-000000Z_from-codex_msg-bbbb.md")

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatalf("check wedged on duplicate message_id: %v", err)
		}
	})

	// Both messages consumed (archived out of the inbox).
	if _, err := os.Stat(p1); !os.IsNotExist(err) {
		t.Fatalf("%s still in inbox (stat err=%v); check did not consume it", f1, err)
	}
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Fatalf("%s still in inbox (stat err=%v); check did not consume it", f2, err)
	}

	// Both obligations recorded, keyed by filename.
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 2 {
		t.Fatalf("ledger has %d entries; want 2 (both poison deliveries recorded)", len(ledger.Pending))
	}
	files := map[string]bool{}
	for _, e := range ledger.Pending {
		files[e.OriginalFilename] = true
		if e.MessageID != "poison-id" {
			t.Errorf("MessageID = %q, want poison-id", e.MessageID)
		}
	}
	if !files[f1] || !files[f2] {
		t.Fatalf("ledger filenames = %v, want both %q and %q", files, f1, f2)
	}
}

// TestCheck_RecordFailureLeavesMessageInInbox: when recording the reply
// obligation fails (ledger error or lock timeout), check must NOT archive the
// message — it stays in the inbox so the next check re-processes it. Fix C
// (gemini finding): archiving before a failed record silently drops the
// obligation.
func TestCheck_RecordFailureLeavesMessageInInbox(t *testing.T) {
	root, cfg := setupSendFixture(t)
	filename, inboxPath := writeReplyRequiredInbox(t, cfg, "claude-code", "rec-fail-1")

	// Inject a record failure (simulating a ledger write error or lock timeout).
	injected := errors.New("injected record/lock failure")
	orig := recordReplyObligationFn
	recordReplyObligationFn = func(_ *loop.Config, _ string, _ loop.Message, _ string, _ []byte, _ time.Time) error {
		return injected
	}
	t.Cleanup(func() { recordReplyObligationFn = orig })

	var cmdErr error
	withCwd(t, root, func() {
		_, cmdErr = captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		})
	})
	if cmdErr == nil {
		t.Fatal("cmdCheck returned nil; want the injected record failure surfaced")
	}

	// The message must STILL be in the inbox (not archived).
	if _, err := os.Stat(inboxPath); err != nil {
		t.Fatalf("message was removed from inbox despite record failure: %v", err)
	}

	// And it must NOT be in the archive dir.
	archiveEntries, _ := os.ReadDir(cfg.ArchiveDir())
	for _, e := range archiveEntries {
		if strings.Contains(e.Name(), filename) {
			t.Fatalf("message archived %q despite record failure (obligation would be silently dropped)", e.Name())
		}
	}

	// No ledger entry either (record failed).
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 0 {
		t.Fatalf("ledger has %d entries; want 0 after a failed record", len(ledger.Pending))
	}

	// Re-processable: clear the injected failure and re-check; now it archives
	// and records exactly once.
	recordReplyObligationFn = orig
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatalf("re-check after clearing failure: %v", err)
		}
	})
	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Fatalf("message still in inbox after successful re-check (stat err=%v)", err)
	}
	ledger, err = loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 1 {
		t.Fatalf("ledger has %d entries after re-check; want exactly 1", len(ledger.Pending))
	}
}

// TestCheck_RecordThenArchive_IdempotentOnReRecord: record succeeds, but then
// archiving the file is simulated to fail; the message stays in the inbox and
// is re-checked next cycle. The second check re-records the SAME message_id +
// original_filename — which must be idempotent (single ledger entry, no error)
// — and then archives. Confirms record-before-archive plus filename-keyed
// idempotency (Fix C).
func TestCheck_RecordThenArchive_IdempotentOnReRecord(t *testing.T) {
	root, cfg := setupSendFixture(t)
	filename, inboxPath := writeReplyRequiredInbox(t, cfg, "claude-code", "idem-1")

	// First check: record succeeds (real recorder) but archiving "fails" — we
	// simulate the archive-fail-after-record-success window by recording the
	// obligation in the seam and then returning an error so check does NOT
	// archive (the real flow records first, then archives; an archive error
	// would also leave the message in the inbox, but the ledger entry persists).
	recorded := false
	orig := recordReplyObligationFn
	recordReplyObligationFn = func(c *loop.Config, agentID string, msg loop.Message, archivePath string, content []byte, now time.Time) error {
		if err := orig(c, agentID, msg, archivePath, content, now); err != nil {
			return err
		}
		recorded = true
		// Simulate an archive failure happening AFTER the record succeeded by
		// returning an error from the record seam on the first pass only.
		return errors.New("simulated archive-fail after record-success")
	}

	var cmdErr error
	withCwd(t, root, func() {
		_, cmdErr = captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		})
	})
	if cmdErr == nil {
		t.Fatal("first check: want surfaced error from the simulated archive-fail")
	}
	if !recorded {
		t.Fatal("first check: obligation was not recorded before the failure")
	}
	// Message stays in inbox (not archived because the step errored).
	if _, err := os.Stat(inboxPath); err != nil {
		t.Fatalf("message removed from inbox after record-success/archive-fail: %v", err)
	}
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 1 {
		t.Fatalf("after first check: ledger has %d entries; want 1", len(ledger.Pending))
	}

	// Second check: restore the real recorder; it re-records the SAME
	// message_id + original_filename, which must be idempotent (no duplicate,
	// no error), then archives.
	recordReplyObligationFn = orig
	t.Cleanup(func() { recordReplyObligationFn = orig })
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatalf("second check (re-record same filename) errored: %v", err)
		}
	})

	ledger, err = loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 1 {
		t.Fatalf("after re-record: ledger has %d entries; want exactly 1 (idempotent on filename)", len(ledger.Pending))
	}
	if ledger.Pending[0].OriginalFilename != filename {
		t.Fatalf("ledger OriginalFilename = %q, want %q", ledger.Pending[0].OriginalFilename, filename)
	}
	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Fatalf("message still in inbox after second check (stat err=%v)", err)
	}
}

// Spec rev3 §A.9 lifecycle: `check` archives a message with frontmatter
// `reply_required: true` AND records a pending entry in the recipient's
// ledger. This is the missing piece of Test 1's end-to-end flow.
func TestCheckRecordsPendingReplyOnArchiveOfReplyRequiredMessage(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		// codex sends claude-code a reply_required message via --ask.
		if err := cmdSend([]string{"--from", "codex", "--to", "claude-code",
			"--task", "review", "--ask", "--body", "look at the diff",
			"--no-wake"}); err != nil {
			t.Fatal(err)
		}

		// claude-code runs check → message archived AND ledger entry recorded.
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 {
		t.Fatalf("PendingEntries = %d, want 1 after check archives reply_required message", len(pending))
	}
	got := pending[0]
	if got.From != "codex" {
		t.Errorf("From = %q, want codex", got.From)
	}
	if got.To != "claude-code" {
		t.Errorf("To = %q, want claude-code", got.To)
	}
	if got.Task != "review" {
		t.Errorf("Task = %q, want review", got.Task)
	}
	if !strings.Contains(got.ArchivePath, ".examplecorp/loop/archive") {
		t.Errorf("ArchivePath %q should point into the archive dir", got.ArchivePath)
	}
}

// Codex final-pass review (5320c08): hand-protocol messages with
// whitespace-tolerant frontmatter delimiters (per §6.4) must still
// land a ledger entry on archive. The validator accepts trimmed `---`
// lines; the ledger parser must use the same lenient semantics or a
// legal-but-whitespacey reply_required message becomes a silent
// obligation leak.
func TestCheckRecordsLedgerForWhitespaceTolerantFrontmatter(t *testing.T) {
	root, cfg := setupSendFixture(t)

	// Drop a hand-protocol-shaped message into claude-code's inbox: valid
	// §6.4 frontmatter with trailing whitespace on both delimiter lines.
	inbox := cfg.AgentInboxDir("claude-code")
	filename := "2026-05-19T22-02-00-000000Z_from-codex_msg-abcd.md"
	body := "---   \n" +
		"message_id: ws-test\n" +
		"from: codex\n" +
		"to: claude-code\n" +
		"reply_required: true\n" +
		"task: whitespace delimiter\n" +
		"   ---   \n" +
		"\n" +
		"please reply\n"
	if err := os.WriteFile(filepath.Join(inbox, filename), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 1 {
		t.Fatalf("ledger has %d entries; want 1 (whitespace delimiters must be tolerated)", len(ledger.Pending))
	}
	if ledger.Pending[0].MessageID != "ws-test" {
		t.Errorf("MessageID = %q, want ws-test", ledger.Pending[0].MessageID)
	}
	if ledger.Pending[0].Task != "whitespace delimiter" {
		t.Errorf("Task = %q, want \"whitespace delimiter\"", ledger.Pending[0].Task)
	}
}

// Messages WITHOUT reply_required must NOT create a ledger entry —
// the ledger is for explicit obligations only.
func TestCheckDoesNotRecordLedgerForNonReplyRequiredMessages(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "codex", "--to", "claude-code",
			"--task", "info", "--body", "fyi only", "--no-wake"}); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Pending) != 0 {
		t.Errorf("ledger has %d entries; want 0 for non-reply-required message", len(ledger.Pending))
	}
}

// End-to-end Test 1 (spec rev3 Part 4): the msg-43b6 reproduction.
//
//  1. codex sends claude-code a reply_required + ## ASK message
//  2. claude-code runs `check` → message archived AND ledger entry created
//     with status: pending
//  3. claude-code's `gate --before consensus` exits 2 (blocked)
//  4. claude-code's `send --reply-to <msg-id>` to codex transitions the
//     entry to status: replied
//  5. `gate --before consensus` now exits 0
func TestEndToEndMsg43b6Reproduction(t *testing.T) {
	root, cfg := setupSendFixture(t)

	var sentMessageID string
	withCwd(t, root, func() {
		// Step 1: send reply_required message
		if err := cmdSend([]string{"--from", "codex", "--to", "claude-code",
			"--task", "review", "--ask", "--body", "look at the diff",
			"--no-wake"}); err != nil {
			t.Fatal(err)
		}
	})

	// Read codex's archived send so we can extract the message_id.
	inboxDir := cfg.AgentInboxDir("claude-code")
	body := readMostRecentFile(t, inboxDir)
	sentMessageID = extractMessageIDFromBody(t, body)

	withCwd(t, root, func() {
		// Step 2: check (archives + records ledger entry)
		if _, err := captureStdout(t, func() error {
			return cmdCheck([]string{"--as", "claude-code"})
		}); err != nil {
			t.Fatal(err)
		}
		mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")

		// Step 3: gate --before consensus blocks
		_, gerr := captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if gerr == nil {
			t.Fatal("gate consensus passed before reply; want errBlocked")
		}

		// Step 4: reply via send --reply-to
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "review-reply", "--reply-to", sentMessageID,
			"--body", "looks good", "--no-wake"}); err != nil {
			t.Fatal(err)
		}

		// Step 5: gate --before consensus passes
		_, gerr = captureStdout(t, func() error { return cmdGate(gateArgs("consensus")) })
		if gerr != nil {
			t.Fatalf("gate consensus after reply err = %v; want nil", gerr)
		}
	})

	// Verify the ledger entry transitioned to replied.
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ledger.FindByMessageID(sentMessageID)
	if !ok {
		t.Fatal("ledger entry vanished")
	}
	if got.Status != loop.PendingReplyStatusReplied {
		t.Errorf("Status = %q, want replied (end-to-end flow)", got.Status)
	}
}

// readMostRecentFile returns the contents of the most recently modified
// non-dotfile in dir.
func readMostRecentFile(t *testing.T, dir string) string {
	t.Helper()
	return readMostRecentInboxMessageDir(t, dir)
}

func readMostRecentInboxMessageDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().After(newestMod) {
			newest = filepath.Join(dir, e.Name())
			newestMod = info.ModTime()
		}
	}
	if newest == "" {
		t.Fatalf("no files in %s", dir)
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// extractMessageIDFromBody pulls the message_id frontmatter value from
// a composed message's body. Simple grep — production code uses the
// readFrontmatter helper but tests can be small.
func extractMessageIDFromBody(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "message_id:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "message_id:"))
			return strings.Trim(val, `"'`)
		}
	}
	t.Fatalf("message_id not found in:\n%s", body)
	return ""
}
