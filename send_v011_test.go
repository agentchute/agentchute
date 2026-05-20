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
// and returns the loop config. Both agents become each other's reachable
// peers so the wake receipt has something to report on.
func setupSendFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMUX_PANE", "%2")
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

// Spec rev3 §A.4 + Test 11: --ask sets reply_required: true frontmatter
// AND prepends ## ASK to the body.
func TestSendAskSetsReplyRequiredAndHeading(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "review", "--ask", "--body", "look at the diff",
			"--no-wake"}); err != nil {
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
			"--task", "review", "--ask", "--body", "## ASK\n\nalready has heading",
			"--no-wake"}); err != nil {
			t.Fatal(err)
		}
	})
	body := readMostRecentInboxMessage(t, cfg, "codex")
	count := strings.Count(body, "## ASK")
	if count != 1 {
		t.Errorf("## ASK appears %d times in body; expected 1 (idempotent):\n%s", count, body)
	}
}

// Spec rev3 §A.4: --reply-to clears a matching pending-reply ledger entry.
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
			"--body", "my reply", "--no-wake"}); err != nil {
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
			"--body", "different recipient", "--no-wake"}); err != nil {
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
			"--body", "test body", "--no-wake"}); err != nil {
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

// --reply-to with no matching ledger entry is silent OK (the reference is
// just a threading hint, not necessarily a reply_required discharge).
func TestSendReplyToWithoutMatchingEntryIsSilent(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "x", "--reply-to", "2026-01-01T00:00:00.000000Z",
			"--body", "b", "--no-wake"})
		if err != nil {
			t.Errorf("err = %v, want nil (unmatched --reply-to is a benign threading hint)", err)
		}
	})
}

// Spec rev3 §A.10: send output emits wake_method, wake_attempted, wake_result.
func TestSendEmitsWakeReceipt(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex",
				"--task", "x", "--body", "b", "--no-wake"})
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"wake_method:", "wake_attempted:", "wake_result:"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if !strings.Contains(out, "skipped (--no-wake)") {
			t.Errorf("output missing --no-wake skip indication:\n%s", out)
		}
	})
}

func TestSendJSONShape(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex",
				"--task", "x", "--body", "b", "--no-wake", "--json"})
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
		if got.WakeMethod != "none" {
			t.Errorf("WakeMethod = %q, want none (--no-wake)", got.WakeMethod)
		}
		if got.WakeAttempted {
			t.Error("WakeAttempted = true under --no-wake")
		}
		if got.WakeResult != "skipped (--no-wake)" {
			t.Errorf("WakeResult = %q, want \"skipped (--no-wake)\"", got.WakeResult)
		}
	})
}

// Spec rev3 §A.4 [REV2]: warn if the sender's pending-reply ledger has
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
			"--task", "unrelated", "--body", "no reply-to", "--no-wake"}); err != nil {
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
// §6.4.3, replies should default reply_required: false — a warning here
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
			"--task", "self-test", "--ask", "--body", "self-obligation",
			"--no-wake"}); err != nil {
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

// Same shape WITHOUT --ask must not warn — self-sends are sometimes a
// legitimate scratch-note pattern; only the --ask combination is the
// loop hazard.
func TestSendDoesNotWarnOnSelfSendWithoutAsk(t *testing.T) {
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
			"--task", "self-note", "--body", "scratch", "--no-wake"}); err != nil {
			t.Fatal(err)
		}
	})
	_ = w.Close()
	<-done

	if got := buf.String(); strings.Contains(got, "self-send") {
		t.Errorf("self-send without --ask should not warn; got:\n%s", got)
	}
}

// Codex pre-merge ask: assert --reply-to does NOT implicitly set
// reply_required. The two flags are orthogonal per AGENTCHUTE.md §6.4.3:
// reply_required MUST NOT be inferred or propagated from in_reply_to.
func TestSendReplyToWithoutAskDoesNotSetReplyRequired(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex",
			"--task", "ack", "--reply-to", "2026-05-19T00:00:00.000000Z",
			"--body", "thanks", "--no-wake"}); err != nil {
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

// Sanity: --no-wake suppresses the poke without affecting delivery.
func TestSendNoWakeSkipsPoke(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex",
				"--task", "x", "--body", "b", "--no-wake"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "skipped (--no-wake)") {
			t.Errorf("--no-wake didn't suppress poke:\n%s", out)
		}
	})
}
