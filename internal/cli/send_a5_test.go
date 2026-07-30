package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestSendPreflightRunsBeforeStdin(t *testing.T) {
	root, _ := setupSendFixture(t)
	stdinPath := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(stdinPath, []byte("must remain unread"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()

	originalStdin := sendStdin
	sendStdin = stdin
	defer func() { sendStdin = originalStdin }()

	withCwd(t, root, func() {
		err = cmdSend([]string{"--from", "claude-code", "--to", "unknown"})
	})
	if err == nil {
		t.Fatal("expected unknown-recipient preflight error")
	}
	if offset, seekErr := stdin.Seek(0, 1); seekErr != nil {
		t.Fatal(seekErr)
	} else if offset != 0 {
		t.Fatalf("stdin offset = %d, want 0 (unread)", offset)
	}
}

func TestSendSpoolsBodyOnPostStdinFailure(t *testing.T) {
	root, cfg := setupSendFixture(t)
	body := "plain body from stdin\n"

	var sendErr error
	withCwd(t, root, func() {
		sendErr = withRecipientRemovedAfterPreflight(t, cfg, "codex", func() error {
			return withSendStdin(t, body, func() error {
				return cmdSend([]string{"--from", "claude-code", "--to", "codex"})
			})
		})
	})
	if sendErr == nil {
		t.Fatal("expected pre-link send failure")
	}
	spoolPath := onlySendSpool(t, cfg, "claude-code")
	assertSendSpool(t, spoolPath, body)
	reportedSpoolPath := canonicalTestPath(t, spoolPath)
	wantRetry := "retry with: agentchute send --to codex --from claude-code < " + shellQuote(reportedSpoolPath)
	if !strings.Contains(sendErr.Error(), "body preserved at "+reportedSpoolPath) ||
		!strings.Contains(sendErr.Error(), wantRetry) {
		t.Fatalf("error missing spool path/retry:\n%v", sendErr)
	}
	if strings.Contains(sendErr.Error(), "register") {
		t.Fatalf("error coaches recipient registration:\n%v", sendErr)
	}

	if err := os.MkdirAll(cfg.AgentInboxDir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := withSpoolStdin(t, spoolPath, func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex"})
		}); err != nil {
			t.Fatalf("plain spool retry: %v", err)
		}
	})
	delivered := readMostRecentInboxMessage(t, cfg, "codex")
	if !strings.HasSuffix(delivered, "\n\nplain body from stdin\n") {
		t.Fatalf("retried message did not preserve body:\n%s", delivered)
	}
}

func TestSendSpoolRetryPreservesAskSemantics(t *testing.T) {
	root, cfg := setupSendFixture(t)
	body := "review this"

	var sendErr error
	withCwd(t, root, func() {
		sendErr = withRecipientRemovedAfterPreflight(t, cfg, "codex", func() error {
			return cmdSend([]string{
				"--from", "claude-code", "--to", "codex",
				"--ask", "--reply-by=45m", "--body", body,
			})
		})
	})
	if sendErr == nil {
		t.Fatal("expected pre-link send failure")
	}
	spoolPath := onlySendSpool(t, cfg, "claude-code")
	assertSendSpool(t, spoolPath, body)
	reportedSpoolPath := canonicalTestPath(t, spoolPath)
	if !strings.Contains(sendErr.Error(), "--ask --reply-by '45m' < "+shellQuote(reportedSpoolPath)) {
		t.Fatalf("retry line lost ask semantics:\n%v", sendErr)
	}

	if err := os.MkdirAll(cfg.AgentInboxDir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := withSpoolStdin(t, spoolPath, func() error {
			return cmdSend([]string{
				"--from", "claude-code", "--to", "codex",
				"--ask", "--reply-by", "45m",
			})
		}); err != nil {
			t.Fatalf("ask spool retry: %v", err)
		}
	})

	delivered := readMostRecentInboxMessage(t, cfg, "codex")
	if !strings.Contains(delivered, "reply_required: true") ||
		!strings.Contains(delivered, "## ASK\n\nreview this") {
		t.Fatalf("retried ask lost wire semantics:\n%s", delivered)
	}
	owed, err := loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 1 {
		t.Fatalf("owed entries = %d, want 1", len(owed.Owed))
	}
	if got := owed.Owed[0].By.Sub(owed.Owed[0].RecordedAt); got != 45*time.Minute {
		t.Fatalf("reply-by interval = %s, want 45m", got)
	}
}

func TestSendSpoolRetryPreservesReplyTo(t *testing.T) {
	root, cfg := setupSendFixture(t)
	const ref = "to-claude-code_from-codex_seq-00000000000000000001"
	body := "reply body"

	var sendErr error
	withCwd(t, root, func() {
		sendErr = withRecipientRemovedAfterPreflight(t, cfg, "codex", func() error {
			return cmdSend([]string{
				"--from", "claude-code", "--to", "codex",
				"--reply-to", ref, "--body", body,
			})
		})
	})
	if sendErr == nil {
		t.Fatal("expected pre-link send failure")
	}
	spoolPath := onlySendSpool(t, cfg, "claude-code")
	assertSendSpool(t, spoolPath, body)
	reportedSpoolPath := canonicalTestPath(t, spoolPath)
	if !strings.Contains(sendErr.Error(), "--reply-to "+shellQuote(ref)+" < "+shellQuote(reportedSpoolPath)) {
		t.Fatalf("retry line lost reply-to semantics:\n%v", sendErr)
	}

	if err := os.MkdirAll(cfg.AgentInboxDir("codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := withSpoolStdin(t, spoolPath, func() error {
			return cmdSend([]string{
				"--from", "claude-code", "--to", "codex",
				"--reply-to", ref,
			})
		}); err != nil {
			t.Fatalf("reply spool retry: %v", err)
		}
	})
	delivered := readMostRecentInboxMessage(t, cfg, "codex")
	if !strings.Contains(delivered, "in_reply_to: "+ref) ||
		!strings.HasSuffix(delivered, "\n\nreply body\n") {
		t.Fatalf("retried reply lost original intent:\n%s", delivered)
	}
}

func TestSendPartialSuccessOnOwedFailure(t *testing.T) {
	root, cfg := setupSendFixture(t)
	if err := os.MkdirAll(filepath.Join(cfg.AgentStateDir("claude-code"), "owed.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout string
	var sendErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			stdout, sendErr = captureStdout(t, func() error {
				return cmdSend([]string{
					"--from", "claude-code", "--to", "codex",
					"--ask", "--body", "bookkeeping failure",
				})
			})
		})
	})
	if sendErr != nil {
		t.Fatalf("post-link owed failure returned nonzero: %v", sendErr)
	}
	if !strings.Contains(stdout, "Sent from-claude-code_seq-") ||
		!strings.Contains(stderr, "WARNING: reply-obligation bookkeeping failed:") ||
		!strings.Contains(stderr, "Do NOT resend.") {
		t.Fatalf("partial-success output mismatch:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if countSendSpools(t, cfg, "claude-code") != 0 {
		t.Fatal("post-link owed failure created a spool file")
	}
	messages, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("delivered messages = %d, want 1", len(messages))
	}
}

func TestSendPostLinkSyncFailureIsPartialSuccess(t *testing.T) {
	root, cfg := setupSendFixture(t)
	originalSend := sendSeqMessageWithCommit
	sendSeqMessageWithCommit = func(cfg *loop.Config, from, to string, content []byte, idempotencyKey, serveToken string) (loop.MsgID, bool, error) {
		id, committed, err := originalSend(cfg, from, to, content, idempotencyKey, serveToken)
		if err != nil {
			return id, committed, err
		}
		return id, true, errors.New("forced post-link sync failure")
	}
	defer func() { sendSeqMessageWithCommit = originalSend }()

	var stdout string
	var sendErr error
	stderr := captureStderr(t, func() {
		withCwd(t, root, func() {
			stdout, sendErr = captureStdout(t, func() error {
				return cmdSend([]string{
					"--from", "claude-code", "--to", "codex",
					"--body", "linked before sync failed",
				})
			})
		})
	})
	if sendErr != nil {
		t.Fatalf("post-link sync failure returned nonzero: %v", sendErr)
	}
	if !strings.Contains(stdout, "Sent from-claude-code_seq-") ||
		!strings.Contains(stderr, "WARNING: message delivered but inbox durability sync failed: forced post-link sync failure.") ||
		!strings.Contains(stderr, "Do NOT resend.") {
		t.Fatalf("partial-success output mismatch:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if countSendSpools(t, cfg, "claude-code") != 0 {
		t.Fatal("post-link sync failure created a spool file")
	}
	delivered := readMostRecentInboxMessage(t, cfg, "codex")
	if !strings.Contains(delivered, "linked before sync failed") {
		t.Fatalf("linked message missing after partial success:\n%s", delivered)
	}
}

func withRecipientRemovedAfterPreflight(t *testing.T, cfg *loop.Config, to string, fn func() error) error {
	t.Helper()
	originalHook := afterSendPreflightHook
	afterSendPreflightHook = func() {
		if err := os.RemoveAll(cfg.AgentInboxDir(to)); err != nil {
			panic(err)
		}
	}
	defer func() { afterSendPreflightHook = originalHook }()
	return fn()
}

func withSendStdin(t *testing.T, body string, fn func() error) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return withSpoolStdin(t, path, fn)
}

func withSpoolStdin(t *testing.T, path string, fn func() error) error {
	t.Helper()
	stdin, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	originalStdin := sendStdin
	sendStdin = stdin
	defer func() { sendStdin = originalStdin }()
	return fn()
}

func onlySendSpool(t *testing.T, cfg *loop.Config, from string) string {
	t.Helper()
	dir := filepath.Join(cfg.AgentStateDir(from), "spool")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spool entries = %d, want 1", len(entries))
	}
	return filepath.Join(dir, entries[0].Name())
}

func assertSendSpool(t *testing.T, path, wantBody string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("spool mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != wantBody {
		t.Fatalf("spool body = %q, want %q", got, wantBody)
	}
}

func countSendSpools(t *testing.T, cfg *loop.Config, from string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.AgentStateDir(from), "spool"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
