package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// send_b3_test.go — v2.5 plan B3: send re-derives reachability itself under
// the recipient's registration lock; three-way (C29) error text.

// backdateRegistration rewrites id's registration row's LastSeen, leaving
// every other field untouched.
func backdateRegistration(t *testing.T, cfg *loop.Config, id string, lastSeen time.Time) {
	t.Helper()
	path := cfg.AgentRegistrationPath(id)
	reg, err := loop.ReadRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	reg.LastSeen = lastSeen
	if err := loop.WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
}

// TestSendRefusesStaleRecipient: C29(b), caught by cmdSend's own lock-free
// preflight (no send attempted, no lock taken, no spool involved — the
// backdated row is stale before anything else runs).
func TestSendRefusesStaleRecipient(t *testing.T) {
	root, cfg := setupSendFixture(t)
	backdateRegistration(t, cfg, "codex", time.Now().UTC().Add(-2*time.Hour))

	var sendErr error
	withCwd(t, root, func() {
		sendErr = cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body", "hello"})
	})
	if sendErr == nil {
		t.Fatal("expected refusal for a stale recipient")
	}
	if !strings.Contains(sendErr.Error(), "was here, gone since") || !strings.Contains(sendErr.Error(), "not sending") {
		t.Fatalf("err = %v, want C29(b) stale text", sendErr)
	}
	if strings.Contains(sendErr.Error(), "retry once") {
		t.Fatalf("direct-stale (preflight) must not use the fresh-but-racing (c) wording: %v", sendErr)
	}
	entries, err := os.ReadDir(cfg.AgentInboxDir("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 delivered messages, got %d", len(entries))
	}
}

// TestSendFreshButRacingText: C29(c). cmdSend's own preflight already found
// `codex` fresh; the delivery stage (loop.DeliverUnderRecipientLock, reached
// via the sendSeqMessageWithCommit seam) reports the row went stale in the
// gap. classifySendFailure must render this as the racing wording, not the
// direct-stale wording — the two error TYPES already carry that distinction
// (internal/loop/send_delivery_test.go proves DeliverUnderRecipientLock
// itself catches the underlying race; this proves cmdSend renders it right).
func TestSendFreshButRacingText(t *testing.T) {
	root, cfg := setupSendFixture(t)
	originalSend := sendSeqMessageWithCommit
	sendSeqMessageWithCommit = func(cfg *loop.Config, from, to string, content []byte, idempotencyKey, serveToken string) (loop.MsgID, bool, error) {
		return loop.MsgID{}, false, &loop.ErrRecipientStale{
			To:        to,
			LastSeen:  time.Now().UTC().Add(-90 * time.Second),
			Age:       90 * time.Second,
			Threshold: time.Hour,
		}
	}
	defer func() { sendSeqMessageWithCommit = originalSend }()

	var sendErr error
	withCwd(t, root, func() {
		sendErr = withSendStdin(t, "body", func() error {
			return cmdSend([]string{"--from", "claude-code", "--to", "codex"})
		})
	})
	if sendErr == nil {
		t.Fatal("expected a racing-recipient failure")
	}
	if !strings.Contains(sendErr.Error(), "was here seconds ago") || !strings.Contains(sendErr.Error(), "retry once") {
		t.Fatalf("err = %v, want C29(c) fresh-but-racing text", sendErr)
	}
	if strings.Contains(sendErr.Error(), "gone since") {
		t.Fatalf("racing case must not reuse the direct-stale (b) wording: %v", sendErr)
	}
	// Post-stdin failure spools the body per A5/C28.
	if countSendSpools(t, cfg, "claude-code") != 1 {
		t.Fatalf("expected the racing failure to spool the body (stdin already drained)")
	}
}

// TestSendTakesNoLockForUnknownRecipient: the lock-free preflight must reject
// an unknown --to BEFORE any lock is taken — WithAgentLock's ensurePrivateDir
// side effect would otherwise manufacture state/<typo>/ for an arbitrary typo
// (v2.5 plan B3 §4 risk).
func TestSendTakesNoLockForUnknownRecipient(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "typo-nonexistent", "--body", "x"})
		if err == nil {
			t.Fatal("expected refusal for an unknown recipient")
		}
	})
	if _, err := os.Stat(cfg.AgentStateDir("typo-nonexistent")); !os.IsNotExist(err) {
		t.Fatalf("state dir for the typo'd recipient should not exist: stat err = %v", err)
	}
}

// TestSendSelfSendNoDeadlock is the B3 sentinel: mint (AllocateSeq, under
// WithAgentLock(from)) and deliver (DeliverUnderRecipientLock, under
// WithAgentLock(to)) are two SEPARATE, SEQUENTIAL lock acquisitions. For
// self-send (from==to) these two calls target the SAME non-reentrant flock;
// if they were ever held together this would deadlock. cmdSend is run in a
// goroutine so a real deadlock fails this test via timeout rather than
// hanging `go test` forever (withAgentLock's own 5s internal lock-timeout
// would otherwise surface as a slow error, not a true hang — either way,
// well past the 1s budget below).
func TestSendSelfSendNoDeadlock(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		done := make(chan error, 1)
		go func() {
			done <- cmdSend([]string{"--from", "claude-code", "--to", "claude-code", "--body", "self"})
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("self-send failed: %v", err)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("self-send did not complete within 1s (mint and delivery locks held together = deadlock)")
		}
	})
}
