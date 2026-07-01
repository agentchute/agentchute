package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// setupSendFixture registers claude-code + codex in a fresh control repo
// and returns the loop config. Pull-only: sends deliver by writing the inbox
// unconditionally.
func setupSendFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic", "--host", "peer-host"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

// readMostRecentInboxMessage returns the body of the most recently dropped
// non-archive message file for `agent`.
func readMostRecentInboxMessage(t *testing.T, cfg *loop.Config, agent string) string {
	t.Helper()
	inbox := cfg.AgentInboxDir(agent)
	entries, err := os.ReadDir(inbox)
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
			newest = filepath.Join(inbox, e.Name())
			newestMod = info.ModTime()
		}
	}
	if newest == "" {
		t.Fatalf("no inbox messages for %s", agent)
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Pull-only (Gate 6c): TestSendInfersSenderFromCurrentTmuxPane was removed.
// Sender inference from a tmux/herdr pane is gone (registrations carry no wake
// target to map a pane back to an id); --from comes from --as / $AGENTCHUTE_AGENT_ID.

// AGENTCHUTE.md §6.4: --ask sets reply_required: true frontmatter
// AND prepends ## ASK to the body.
func TestSendAskSetsReplyRequiredAndHeading(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "review", "--ask", "--body", "look at the diff"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	if !strings.Contains(body, "reply_required: true") {
		t.Errorf("frontmatter missing reply_required: true:\n%s", body)
	}
	if !strings.Contains(body, "## ASK") {
		t.Errorf("body missing ## ASK heading:\n%s", body)
	}
	if !strings.Contains(body, "look at the diff") {
		t.Errorf("body missing user content:\n%s", body)
	}
}

func TestSendAskPreservesExistingAskHeading(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "review", "--ask", "--body", "## ASK\n\nalready has heading"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	count := strings.Count(body, "## ASK")
	if count != 1 {
		t.Errorf("## ASK appears %d times in body; expected 1 (idempotent):\n%s", count, body)
	}
}

// AGENTCHUTE.md §6.4: --reply-to clears a matching pending-reply ledger entry.
func TestSendReplyToClearsPendingLedgerEntry(t *testing.T) {
	root, cfg := setupSendFixture(t)

	// Seed claude-code's ledger with a pending entry from codex.
	pendingMsgID := "2026-05-19T17:53:59.561894Z"
	entry := loop.PendingReplyEntry{
		MessageID:        pendingMsgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "review-reply", "--reply-to", pendingMsgID,
			"--body", "my reply"}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ledger.FindByMessageID(pendingMsgID)
	if !ok {
		t.Fatal("ledger entry vanished")
	}
	if got.Status != loop.PendingReplyStatusReplied {
		t.Errorf("Status = %q, want replied", got.Status)
	}
	if got.ReplySentAt == nil {
		t.Error("ReplySentAt is nil")
	}
	if got.ReplyMessageID == nil || *got.ReplyMessageID == "" {
		t.Error("ReplyMessageID not populated")
	}
}

// Codex review (89ad2d9): --reply-to ledger clearing must verify that the
// outbound recipient matches the ledger entry's original sender. Threading
// via a third party's msg-id while delivering to someone else must NOT
// clear the third party's obligation.
func TestSendReplyToMismatchedRecipientLeavesLedgerPending(t *testing.T) {
	root, cfg := setupSendFixture(t)
	// Register a third agent so we can deliver to a non-obligation-owner.
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%3")
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
	})

	// claude-code owes codex a reply.
	pendingMsgID := "2026-05-19T17:53:59.561894Z"
	entry := loop.PendingReplyEntry{
		MessageID:        pendingMsgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Send to gemini-cli (NOT codex) with --reply-to <codex's msg-id>.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "gemini-cli",
			"--task", "unrelated", "--reply-to", pendingMsgID,
			"--body", "different recipient"}); err != nil {
			t.Fatal(err)
		}
	})

	// The codex obligation must remain pending.
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ledger.FindByMessageID(pendingMsgID)
	if got.Status != loop.PendingReplyStatusPending {
		t.Errorf("Status = %q, want pending (mismatched --reply-to recipient must not clear the obligation)", got.Status)
	}
}

// Codex review (89ad2d9): applyReplyRequiredFrontmatter's idempotence check
// must be line/key scoped, not substring scoped. A task value containing
// "reply_required: audit" must not prevent --ask from adding the actual
// frontmatter field.
func TestSendAskWithMisleadingTaskValueStillSetsFrontmatter(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "reply_required: audit", "--ask",
			"--body", "test body"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")

	// The frontmatter must contain the real reply_required: true line,
	// not just the substring inside the task value.
	lines := strings.Split(body, "\n")
	foundActualField := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "reply_required: true" {
			foundActualField = true
			break
		}
	}
	if !foundActualField {
		t.Errorf("frontmatter missing top-level `reply_required: true` line (substring match on task value confused the splice):\n%s", body)
	}
}

// WI-2 follow-up (codex, 4-way review): the terminal-first short-circuit must
// not strand a still-pending duplicate. claude-code owes codex TWO obligations
// that share a message_id (filename-keyed — WI-2 Fix 1). The first is already
// terminal (replied); the second is still pending. A reply to that message_id
// must discharge the SECOND, not bail on the first being terminal.
func TestSend_ReplyToTerminalFirstStillDischargesPendingDuplicate(t *testing.T) {
	root, cfg := setupSendFixture(t)

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()

	// First obligation (will be pre-discharged to terminal "replied").
	first := loop.PendingReplyEntry{
		MessageID:        sharedMsgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/a.md",
	}
	// Second obligation, same message_id + sender, DISTINCT filename, pending.
	second := loop.PendingReplyEntry{
		MessageID:        sharedMsgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-bbbb.md",
		ArchivePath:      "archive/b.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", first, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", second, now); err != nil {
		t.Fatal(err)
	}
	// Pre-discharge ONLY the FIRST entry to terminal (hand-edit so the
	// sender-scoped MarkPendingReplied doesn't also discharge the second), so it
	// sorts ahead of the still-pending one in FindByMessageID's first-match.
	preLedger, _ := loop.LoadPendingLedger(cfg, "claude-code")
	for i := range preLedger.Pending {
		if preLedger.Pending[i].OriginalFilename == first.OriginalFilename {
			preLedger.Pending[i].Status = loop.PendingReplyStatusReplied
			rid := "earlier-reply"
			preLedger.Pending[i].ReplyMessageID = &rid
			sentAt := now.UTC().Format("2006-01-02T15:04:05Z")
			preLedger.Pending[i].ReplySentAt = &sentAt
		}
	}
	if err := loop.SavePendingLedger(cfg, "claude-code", preLedger); err != nil {
		t.Fatal(err)
	}

	// Sanity: exactly one obligation is still pending before we reply.
	pre, _ := loop.LoadPendingLedger(cfg, "claude-code")
	if len(pre.PendingEntries()) != 1 {
		t.Fatalf("setup: PendingEntries = %d, want 1 (second still pending)", len(pre.PendingEntries()))
	}

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "second-reply", "--reply-to", sharedMsgID,
			"--body", "discharging the duplicate"}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if pending := ledger.PendingEntries(); len(pending) != 0 {
		t.Errorf("PendingEntries = %d, want 0; the pending duplicate was stranded by the terminal-first short-circuit: %+v", len(pending), pending)
	}
}

// WI-2 follow-up (codex, 4-way review): a reply to one sender must NOT clear an
// obligation owed to a DIFFERENT sender that happens to reuse the same
// message_id. claude-code owes BOTH codex and gemini-cli a reply, both keyed on
// the same (reused) message_id. Replying to codex must leave gemini-cli's
// obligation pending.
func TestSend_ReplyToDoesNotClearOtherSendersObligation(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%3")
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
	})

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()

	fromCodex := loop.PendingReplyEntry{
		MessageID:        sharedMsgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/codex.md",
	}
	fromGemini := loop.PendingReplyEntry{
		MessageID:        sharedMsgID,
		From:             "gemini-cli",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-gemini-cli_msg-bbbb.md",
		ArchivePath:      "archive/gemini.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromCodex, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromGemini, now); err != nil {
		t.Fatal(err)
	}

	// Reply to codex with the shared message_id.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "codex-reply", "--reply-to", sharedMsgID,
			"--body", "to codex only"}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ledger.Pending {
		switch e.From {
		case "codex":
			if e.Status != loop.PendingReplyStatusReplied {
				t.Errorf("codex obligation status = %q, want replied", e.Status)
			}
		case "gemini-cli":
			if e.Status != loop.PendingReplyStatusPending {
				t.Errorf("gemini-cli obligation status = %q, want pending (a reply to codex must not clear gemini-cli's obligation)", e.Status)
			}
		}
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 || pending[0].From != "gemini-cli" {
		t.Fatalf("PendingEntries = %+v, want only gemini-cli still blocking", pending)
	}
}

// WI-2 follow-up rev2 (codex, 4-way review): the INVERSE-ordering case codex
// flagged as missing. The ledger holds an entry from senderA FIRST and a
// PENDING entry from senderB LATER, sharing a message_id. We reply --to senderB.
// Before rev2, the discharge decision keyed on FindByMessageID's first BARE row
// (senderA): `entry.From != toID` fired, warned, and left senderB's obligation
// pending. After rev2, the decision scopes by the KNOWN recipient (toID=senderB),
// so senderB's obligation is discharged and senderA's is left untouched.
func TestSend_ReplyToInverseOrder_DischargesIntendedSenderWhenNotFirstRow(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%3")
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
	})

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()

	// senderA == gemini-cli holds the FIRST row.
	fromGemini := loop.PendingReplyEntry{
		MessageID:        sharedMsgID,
		From:             "gemini-cli",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-gemini-cli_msg-aaaa.md",
		ArchivePath:      "archive/gemini.md",
	}
	// senderB == codex (the agent we reply to) holds a LATER pending row.
	fromCodex := loop.PendingReplyEntry{
		MessageID:        sharedMsgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-bbbb.md",
		ArchivePath:      "archive/codex.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromGemini, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromCodex, now); err != nil {
		t.Fatal(err)
	}

	// Reply to codex (the LATER row) with the shared message_id.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "codex-reply", "--reply-to", sharedMsgID,
			"--body", "to codex"}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ledger.Pending {
		switch e.From {
		case "codex":
			if e.Status != loop.PendingReplyStatusReplied {
				t.Errorf("codex obligation status = %q, want replied (the intended sender, even though it is the LATER/non-first row, must be discharged)", e.Status)
			}
		case "gemini-cli":
			if e.Status != loop.PendingReplyStatusPending {
				t.Errorf("gemini-cli obligation status = %q, want pending (replying to codex must not touch gemini-cli's obligation)", e.Status)
			}
		}
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 || pending[0].From != "gemini-cli" {
		t.Fatalf("PendingEntries = %+v, want only gemini-cli still blocking", pending)
	}
}

// WI-2 follow-up rev2: --reply-to names a message_id that exists ONLY from a
// third party (senderA), but we send --to senderB. No obligation owed to senderB
// under that message_id exists, so nothing is cleared and senderA's obligation
// is left pending (a warning is emitted).
func TestSend_ReplyToThirdPartyMessageIDLeavesPending(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%3")
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
	})

	msgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()
	// The message_id exists ONLY from gemini-cli (senderA).
	fromGemini := loop.PendingReplyEntry{
		MessageID:        msgID,
		From:             "gemini-cli",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-gemini-cli_msg-aaaa.md",
		ArchivePath:      "archive/gemini.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromGemini, now); err != nil {
		t.Fatal(err)
	}

	// Send --to codex (senderB) with --reply-to <gemini's msg-id>.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "unrelated", "--reply-to", msgID,
			"--body", "threading via gemini's id"}); err != nil {
			t.Fatal(err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ledger.FindByMessageID(msgID)
	if got.Status != loop.PendingReplyStatusPending {
		t.Errorf("gemini-cli obligation status = %q, want pending (sending to codex via gemini's msg-id must not clear gemini's obligation)", got.Status)
	}
}

// --reply-to with no matching ledger entry is silent OK (the reference is
// just a threading hint, not necessarily a reply_required discharge).
func TestSendReplyToWithoutMatchingEntryIsSilent(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "x", "--reply-to", "2026-01-01T00:00:00.000000Z",
			"--body", "b"})
		if err != nil {
			t.Errorf("err = %v, want nil (unmatched --reply-to is a benign threading hint)", err)
		}
	})
}

func TestSendJSONShape(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex",
				"--task", "x", "--body", "b", "--json"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var got sendResult
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.From != "claude-code" || got.To != "codex" {
			t.Errorf("from/to mismatch: %+v", got)
		}
		if got.MessageID == "" {
			t.Error("MessageID empty")
		}
	})
}

// AGENTCHUTE.md §6.4: warn if the sender's pending-reply ledger has
// entries from the recipient but --reply-to is not provided.
func TestSendWarnsOnUnclearedLedgerForRecipient(t *testing.T) {
	root, cfg := setupSendFixture(t)

	entry := loop.PendingReplyEntry{
		MessageID:        "msg-1",
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "msg-1_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// Redirect stderr to capture the warning.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()
	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		bs := make([]byte, 1024)
		for {
			n, _ := r.Read(bs)
			if n > 0 {
				buf.Write(bs[:n])
			}
			if n == 0 {
				close(done)
				return
			}
		}
	}()

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "unrelated", "--body", "no reply-to"}); err != nil {
			t.Fatal(err)
		}
	})
	_ = w.Close()
	<-done

	got := buf.String()
	if !strings.Contains(got, "pending reply obligation") {
		t.Errorf("stderr missing pending-obligation warning:\n%s", got)
	}
	if !strings.Contains(got, "--reply-to") {
		t.Errorf("stderr missing --reply-to suggestion:\n%s", got)
	}
}

// Real-bake follow-up: self-send with --ask is a loop hazard (claude-code
// owes claude-code a reply). Today the delivery succeeds but stderr must
// warn so the operator pauses on the unusual shape. Per AGENTCHUTE.md
// §6.4, replies should default reply_required: false — a warning here
// reinforces that convention at the CLI surface.
func TestSendWarnsOnSelfSendWithAsk(t *testing.T) {
	root, _ := setupSendFixture(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()
	done := make(chan struct{})
	var buf strings.Builder
	go func() {
		bs := make([]byte, 1024)
		for {
			n, _ := r.Read(bs)
			if n > 0 {
				buf.Write(bs[:n])
			}
			if n == 0 {
				close(done)
				return
			}
		}
	}()

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "claude-code",
			"--task", "self-test", "--ask", "--body", "self-obligation"}); err != nil {
			t.Fatal(err)
		}
	})
	_ = w.Close()
	<-done

	got := buf.String()
	if !strings.Contains(got, "self-send") || !strings.Contains(got, "--ask") {
		t.Errorf("stderr missing self-send + --ask warning:\n%s", got)
	}
}

// Codex pre-merge ask: assert --reply-to does NOT implicitly set
// reply_required. The two flags are orthogonal per AGENTCHUTE.md §6.4:
// reply_required MUST NOT be inferred or propagated from in_reply_to.
func TestSendReplyToWithoutAskDoesNotSetReplyRequired(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "ack", "--reply-to", "2026-05-19T00:00:00.000000Z",
			"--body", "thanks"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "reply_required:") {
			t.Errorf("--reply-to without --ask leaked reply_required line: %q (full:\n%s)", line, body)
		}
	}
}
