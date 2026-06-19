package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// setupDeferFixture sets up a control repo with two registered agents
// (claude-code, codex) and seeds a pending-reply ledger entry on
// claude-code from codex. Returns the seeded entry's message_id and the
// loop config so individual tests can verify state.
func setupDeferFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	root := setupBootFixture(t)

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}

	// Register both agents (use cmdRegister directly so existing helpers compose).
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

	// Seed the ledger entry.
	msgID := "2026-05-19T17:53:59.561894Z"
	entry := loop.PendingReplyEntry{
		MessageID:        msgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DEFER_ROOT", root)
	return msgID, cfg
}

// Test 5 (spec rev3 Part 4): defer transitions the ledger entry to
// deferred, populates deferred_at + reason (+ optional deferred_until),
// and gate --before finish exits 0 afterwards. Also: an automatic
// deferral-ack message lands in the sender's inbox.
func TestDeferTransitionsLedgerAndAcksSender(t *testing.T) {
	msgID, cfg := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{
				"--as", "claude-code",
				"--message", msgID,
				"--reason", "needs research",
				"--until", "24h",
			})
		})
		if err != nil {
			t.Fatalf("cmdDefer: %v", err)
		}
	})

	// Ledger entry transitioned.
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ledger.FindByMessageID(msgID)
	if !ok {
		t.Fatal("ledger entry vanished")
	}
	if got.Status != loop.PendingReplyStatusDeferred {
		t.Errorf("Status = %q, want deferred", got.Status)
	}
	if got.DeferredReason == nil || *got.DeferredReason != "needs research" {
		t.Errorf("DeferredReason = %v, want \"needs research\"", got.DeferredReason)
	}
	if got.DeferredAt == nil {
		t.Error("DeferredAt is nil; want populated")
	}
	if got.DeferredUntil == nil || *got.DeferredUntil == "" {
		t.Errorf("DeferredUntil = %v, want populated", got.DeferredUntil)
	}

	// Deferral-ack landed in codex's inbox.
	codexInbox := cfg.AgentInboxDir("codex")
	entries, err := os.ReadDir(codexInbox)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(codexInbox, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		if strings.Contains(s, "task: deferred-reply") && strings.Contains(s, "needs research") {
			found = true
			if !strings.Contains(s, `in_reply_to: "`+msgID) {
				t.Errorf("ack missing in_reply_to=%s: %s", msgID, s)
			}
			break
		}
	}
	if !found {
		t.Errorf("deferral-ack not found in codex inbox; entries=%v", entries)
	}

	// Gate finish must now pass.
	mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		if err != nil {
			t.Errorf("gate finish after defer err = %v, want nil", err)
		}
	})
}

// captureStderr mirrors captureStdout for the deferral-ack poke warning, which
// is written to stderr (best-effort poke failures are non-fatal warnings).
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	cmdErr := fn()
	_ = w.Close()
	<-done
	os.Stderr = orig
	return buf.String(), cmdErr
}

// TestDeferAck_RefusesUnownedRunnerSocket: when the deferral-ack's recipient
// (the original sender) advertises a runner wake_target it does not own, the
// poke is REFUSED (recipient-binding) without dialing. The defer still succeeds
// and the ack still lands; only the poke is skipped, with a stderr warning
// whose "refused" wording proves the refusal short-circuited ahead of any dial.
func TestDeferAck_RefusesUnownedRunnerSocket(t *testing.T) {
	msgID, cfg := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")

	// Rewrite codex (the ack recipient) to advertise an unowned runner socket.
	evil := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: cfg.ControlRepo,
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  "unix:/tmp/evil.sock",
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), evil); err != nil {
		t.Fatal(err)
	}

	var stderr string
	withCwd(t, root, func() {
		var err error
		stderr, err = captureStderr(t, func() error {
			_, e := captureStdout(t, func() error {
				return cmdDefer([]string{"--as", "claude-code", "--message", msgID, "--reason", "needs research"})
			})
			return e
		})
		if err != nil {
			t.Fatalf("defer should succeed despite refused poke: %v", err)
		}
	})

	if !strings.Contains(stderr, "refused") {
		t.Fatalf("stderr should warn that the unowned runner poke was refused, got: %q", stderr)
	}

	// Ledger still transitioned and ack still delivered.
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ledger.FindByMessageID(msgID)
	if !ok || got.Status != loop.PendingReplyStatusDeferred {
		t.Fatalf("ledger entry not deferred despite refused poke: ok=%v status=%v", ok, got.Status)
	}
	entries, err := os.ReadDir(cfg.AgentInboxDir("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("deferral-ack should still land in codex inbox even when poke is refused")
	}
}

func TestDeferRejectsMissingMessageID(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", "nope", "--reason", "x"})
		})
		if err == nil {
			t.Fatal("expected error for missing ledger entry")
		}
		if errors.Is(err, errBlocked) {
			t.Errorf("missing-entry should be command failure, not errBlocked: %v", err)
		}
	})
}

func TestDeferRejectsAlreadyDeferred(t *testing.T) {
	msgID, _ := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", msgID, "--reason", "first"})
		}); err != nil {
			t.Fatal(err)
		}
		// Second defer must refuse.
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", msgID, "--reason", "second"})
		})
		if err == nil {
			t.Fatal("expected error on second defer")
		}
	})
}

// WI-2 follow-up (codex, 4-way review): defer must not strand a still-pending
// duplicate when FindByMessageID's first match is already terminal. Two
// obligations from the same sender share a message_id; the first is replied,
// the second pending. defer --message <id> must defer the second, not error out
// on the terminal first.
func TestDefer_TerminalFirstStillDefersPendingDuplicate(t *testing.T) {
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

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()
	first := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "codex", To: "claude-code", Task: "review",
		OriginalFilename: "a_from-codex_msg-aaaa.md", ArchivePath: "archive/a.md",
	}
	second := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "codex", To: "claude-code", Task: "review",
		OriginalFilename: "b_from-codex_msg-bbbb.md", ArchivePath: "archive/b.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", first, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", second, now); err != nil {
		t.Fatal(err)
	}
	// Pre-discharge ONLY the first entry to terminal "replied" (hand-edit so the
	// sender-scoped mark doesn't also discharge the second).
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

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", sharedMsgID, "--reason", "later"})
		}); err != nil {
			t.Fatalf("cmdDefer must defer the still-pending duplicate, got err = %v", err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if pending := ledger.PendingEntries(); len(pending) != 0 {
		t.Errorf("PendingEntries = %d, want 0; the pending duplicate was stranded: %+v", len(pending), pending)
	}
}

// WI-2 follow-up (codex, 4-way review): defer scopes to the sender of the
// matched entry. A same-message_id obligation owed to a DIFFERENT sender must
// stay pending (defer can never clear another sender's obligation).
func TestDefer_DoesNotClearOtherSendersObligation(t *testing.T) {
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
		t.Setenv("TMUX_PANE", "%3")
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()
	fromCodex := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "codex", To: "claude-code", Task: "review",
		OriginalFilename: "codex_from-codex_msg-aaaa.md", ArchivePath: "archive/codex.md",
	}
	fromGemini := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "gemini-cli", To: "claude-code", Task: "review",
		OriginalFilename: "gemini_from-gemini-cli_msg-bbbb.md", ArchivePath: "archive/gemini.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromCodex, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromGemini, now); err != nil {
		t.Fatal(err)
	}

	// FindByMessageID returns the FIRST match (codex, recorded first), so this
	// defers codex's obligation. gemini-cli's same-message_id obligation must
	// stay pending.
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", sharedMsgID, "--reason", "later"})
		}); err != nil {
			t.Fatalf("cmdDefer: %v", err)
		}
	})

	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ledger.Pending {
		switch e.From {
		case "codex":
			if e.Status != loop.PendingReplyStatusDeferred {
				t.Errorf("codex obligation status = %q, want deferred", e.Status)
			}
		case "gemini-cli":
			if e.Status != loop.PendingReplyStatusPending {
				t.Errorf("gemini-cli obligation status = %q, want pending (defer scoped to codex must not clear gemini-cli's obligation)", e.Status)
			}
		}
	}
	pending := ledger.PendingEntries()
	if len(pending) != 1 || pending[0].From != "gemini-cli" {
		t.Fatalf("PendingEntries = %+v, want only gemini-cli still blocking", pending)
	}
}

// WI-2 follow-up rev2 (codex, 4-way review): the terminal-first DIFFERENT-sender
// case codex flagged as missing. The FIRST bare row is TERMINAL (senderA); a
// LATER row is PENDING from a DIFFERENT sender (senderB), same message_id. Before
// rev2, defer scoped fromSender to FindByMessageID's first bare row (terminal
// senderA) → errored "already in status replied", and a retry hit the same first
// terminal row → senderB permanently unreachable. After rev2, defer picks a
// PENDING sender (senderB) → senderB is deferred (reachable), not an error.
func TestDefer_TerminalFirstRow_ReachesLaterPendingDifferentSender(t *testing.T) {
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
		t.Setenv("TMUX_PANE", "%3")
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()
	// senderA == codex holds the FIRST row (will be pre-discharged to terminal).
	fromCodex := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "codex", To: "claude-code", Task: "review",
		OriginalFilename: "codex_from-codex_msg-aaaa.md", ArchivePath: "archive/codex.md",
	}
	// senderB == gemini-cli holds a LATER PENDING row, DIFFERENT sender.
	fromGemini := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "gemini-cli", To: "claude-code", Task: "review",
		OriginalFilename: "gemini_from-gemini-cli_msg-bbbb.md", ArchivePath: "archive/gemini.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromCodex, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", fromGemini, now); err != nil {
		t.Fatal(err)
	}
	// Pre-discharge ONLY the first (codex) entry to terminal "replied".
	preLedger, _ := loop.LoadPendingLedger(cfg, "claude-code")
	for i := range preLedger.Pending {
		if preLedger.Pending[i].From == "codex" {
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

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", sharedMsgID, "--reason", "later"})
		}); err != nil {
			t.Fatalf("cmdDefer must reach the later pending DIFFERENT-sender row, got err = %v", err)
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
				t.Errorf("codex obligation status = %q, want replied (untouched terminal first row)", e.Status)
			}
		case "gemini-cli":
			if e.Status != loop.PendingReplyStatusDeferred {
				t.Errorf("gemini-cli obligation status = %q, want deferred (the later pending different-sender row must be reached)", e.Status)
			}
		}
	}
	if pending := ledger.PendingEntries(); len(pending) != 0 {
		t.Errorf("PendingEntries = %d, want 0 (later pending row was stranded): %+v", len(pending), pending)
	}
}

// WI-2 follow-up rev2: every row for the message_id is terminal ⇒ defer errors
// cleanly (no pending obligation to defer), never panics or scopes to a
// terminal sender.
func TestDefer_NoPendingForMessageID_Errors(t *testing.T) {
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

	sharedMsgID := "2026-05-19T17:53:59.561894Z"
	now := time.Now().UTC()
	first := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "codex", To: "claude-code", Task: "review",
		OriginalFilename: "a_from-codex_msg-aaaa.md", ArchivePath: "archive/a.md",
	}
	second := loop.PendingReplyEntry{
		MessageID: sharedMsgID, From: "codex", To: "claude-code", Task: "review",
		OriginalFilename: "b_from-codex_msg-bbbb.md", ArchivePath: "archive/b.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", first, now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", second, now); err != nil {
		t.Fatal(err)
	}
	// Hand-mark BOTH rows terminal so nothing is pending.
	preLedger, _ := loop.LoadPendingLedger(cfg, "claude-code")
	for i := range preLedger.Pending {
		preLedger.Pending[i].Status = loop.PendingReplyStatusReplied
		rid := "prior-reply"
		preLedger.Pending[i].ReplyMessageID = &rid
		sentAt := now.UTC().Format("2006-01-02T15:04:05Z")
		preLedger.Pending[i].ReplySentAt = &sentAt
	}
	if err := loop.SavePendingLedger(cfg, "claude-code", preLedger); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", sharedMsgID, "--reason", "later"})
		})
		if err == nil {
			t.Fatal("expected error deferring a message_id with no pending obligation")
		}
	})
}

func TestDeferRequiresReason(t *testing.T) {
	msgID, _ := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", msgID})
		})
		if err == nil {
			t.Fatal("expected error when --reason is omitted")
		}
	})
}

func TestNormalizeDeferUntil(t *testing.T) {
	pastRFC := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name    string
		input   string
		wantErr bool
		check   func(t *testing.T, got string)
	}{
		{"parses durations", "24h", false, func(t *testing.T, got string) {
			parsed, err := time.Parse(time.RFC3339, got)
			if err != nil {
				t.Fatalf("not RFC3339: %q (%v)", got, err)
			}
			if d := time.Until(parsed); d < 23*time.Hour || d > 25*time.Hour {
				t.Errorf("24h offset out of range: %s", got)
			}
		}},
		{"accepts RFC3339", "2099-05-26T00:00:00Z", false, func(t *testing.T, got string) {
			if got != "2099-05-26T00:00:00Z" {
				t.Errorf("got %q", got)
			}
		}},
		{"empty passes through", "", false, func(t *testing.T, got string) {
			if got != "" {
				t.Errorf("got %q, want empty", got)
			}
		}},
		{"rejects garbage", "tomorrow morning", true, nil},
		{"rejects past RFC3339", pastRFC, true, nil},
		{"rejects zero duration", "0s", true, nil},
		{"rejects negative duration", "-1h", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeDeferUntil(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.check != nil {
				c.check(t, got)
			}
		})
	}
}

// Codex review (eb58443): if the ledger contains an entry whose To field
// doesn't match the deferring agent's id, defer must refuse rather than
// silently route the ack from a mismatched namespace.
func TestDeferRejectsLedgerEntryWithMismatchedTo(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
	})

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	// Hand-write a ledger entry whose `to` is some other agent.
	if err := ensurePrivateDirHelper(cfg.PendingRepliesPath("claude-code")); err != nil {
		t.Fatal(err)
	}
	corrupt := `{"pending":[{"message_id":"mismatched","from":"codex","to":"someone-else","task":"x","original_filename":"f.md","archive_path":"a.md","recorded_at":"2026-01-01T00:00:00Z","status":"pending","reply_sent_at":null,"reply_message_id":null,"deferred_at":null,"deferred_until":null,"deferred_reason":null}]}`
	if err := os.WriteFile(cfg.PendingRepliesPath("claude-code"), []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", "mismatched", "--reason", "test"})
		})
		if err == nil {
			t.Fatal("expected error on ledger entry whose To doesn't match the deferring agent")
		}
	})
}

// Helper because defer_test is in package main and ensurePrivateDir lives
// in the internal/loop package. We need a 0700 dir for the ledger file.
func ensurePrivateDirHelper(filePath string) error {
	return os.MkdirAll(filepath.Dir(filePath), 0o700)
}
