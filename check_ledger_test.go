package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

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
// whitespace-tolerant frontmatter delimiters (per §6.4.2) must still
// land a ledger entry on archive. The validator accepts trimmed `---`
// lines; the ledger parser must use the same lenient semantics or a
// legal-but-whitespacey reply_required message becomes a silent
// obligation leak.
func TestCheckRecordsLedgerForWhitespaceTolerantFrontmatter(t *testing.T) {
	root, cfg := setupSendFixture(t)

	// Drop a hand-protocol-shaped message into claude-code's inbox: valid
	// §6.4.2 frontmatter with trailing whitespace on both delimiter lines.
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
