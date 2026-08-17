package op

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// A5 at seam level: the preflight is a separate, non-mutating call precisely so
// a caller can run it BEFORE it has a body — locally that is what keeps a piped
// stdin untouched on a doomed send. It must refuse without having written
// anything, and without manufacturing state for the recipient (B3 §4 risk:
// taking the recipient's lock would create state/<typo>/ for any --to typo).
func TestSendPreflightRefusesBeforeAnyMutation(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")

	err := SendPreflight(cfg, Context{ActorID: "claude-code"}, "nobody")
	if !errors.Is(err, ErrRecipientUnknown) {
		t.Fatalf("err = %v, want ErrRecipientUnknown", err)
	}
	if _, serr := os.Stat(cfg.AgentStateDir("nobody")); !os.IsNotExist(serr) {
		t.Fatalf("preflight manufactured state for an unknown recipient: %v", serr)
	}
	if _, serr := os.Stat(cfg.AgentInboxDir("nobody")); !os.IsNotExist(serr) {
		t.Fatalf("preflight manufactured an inbox for an unknown recipient: %v", serr)
	}
}

func TestSendPreflightRefusesUnregisteredSender(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "codex")

	err := SendPreflight(cfg, Context{ActorID: "claude-code"}, "codex")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

func TestSendPreflightRefusesUnreadableRecipient(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	if err := os.WriteFile(cfg.AgentRegistrationPath("codex"), []byte("not a registration"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := SendPreflight(cfg, Context{ActorID: "claude-code"}, "codex")
	if !errors.Is(err, ErrRecipientUnreadable) {
		t.Fatalf("err = %v, want ErrRecipientUnreadable", err)
	}
}

// C29(b): the preflight arm. The sentinel says WHICH raise site produced it and
// the wrapped cause carries the fields the operator-facing text prints.
func TestSendPreflightStaleArmCarriesReachabilityFields(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	backdate(t, cfg, "codex", time.Now().UTC().Add(-72*time.Hour))

	err := SendPreflight(cfg, Context{ActorID: "claude-code"}, "codex")
	if !errors.Is(err, ErrRecipientStale) {
		t.Fatalf("err = %v, want ErrRecipientStale", err)
	}
	if errors.Is(err, ErrRecipientRacing) {
		t.Fatal("a preflight refusal must not classify as the under-lock racing arm")
	}
	var stale *loop.ErrRecipientStale
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want a *loop.ErrRecipientStale cause", err)
	}
	if stale.To != "codex" || stale.LastSeen.IsZero() || stale.Threshold == 0 {
		t.Fatalf("cause = %+v, want the reachability fields the C29 text renders", stale)
	}
}

// C29(c): reached ONLY after the op's own preflight already found `to` fresh, so
// a stale verdict from delivery is the racing case by construction. The CLI
// renders two different texts off this distinction.
func TestSendClassifiesUnderLockStaleAsRacing(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")

	original := SendTsMessageWithCommit
	SendTsMessageWithCommit = func(*loop.Config, string, string, []byte, string) (loop.TsID, bool, error) {
		return loop.TsID{}, false, &loop.ErrRecipientStale{To: "codex", Age: 90 * time.Second, Threshold: time.Hour}
	}
	defer func() { SendTsMessageWithCommit = original }()

	resp, err := Send(cfg, Context{ActorID: "claude-code"}, SendReq{To: "codex", Content: []byte("body")})
	if !errors.Is(err, ErrRecipientRacing) {
		t.Fatalf("err = %v, want ErrRecipientRacing", err)
	}
	if errors.Is(err, ErrRecipientStale) {
		t.Fatal("the under-lock arm must not classify as the preflight arm")
	}
	if resp.Filename != "" || resp.Committed {
		t.Fatalf("resp = %+v, want the zero response on a failed delivery", resp)
	}
	var stale *loop.ErrRecipientStale
	if !errors.As(err, &stale) {
		t.Fatal("the racing arm dropped the cause the C29 renderer needs")
	}
}

// Both Context constructors (C1): identity arrives out of band, so an op driven
// with a pinned id must have the same state effect as one driven with an id the
// caller resolved from its own environment. Same pool, same assertions.
func TestSendIsIdenticalUnderBothContextConstructors(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(t *testing.T) Context
	}{
		{"pinned", func(*testing.T) Context { return Context{ActorID: "claude-code"} }},
		{"resolved", func(t *testing.T) Context {
			t.Setenv("AGENTCHUTE_AGENT_ID", "claude-code")
			return Context{ActorID: os.Getenv("AGENTCHUTE_AGENT_ID")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newPool(t)
			enroll(t, cfg, "claude-code")
			enroll(t, cfg, "codex")

			resp, err := Send(cfg, tc.ctx(t), SendReq{
				To:      "codex",
				Content: loop.ComposeMessage("claude-code", "", "hello"),
				Ask:     true,
			})
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if resp.Filename == "" || resp.Ref == "" || !resp.Committed {
				t.Fatalf("resp = %+v, want a committed delivery", resp)
			}
			if resp.DurabilityNote != "" {
				t.Fatalf("unexpected durability note %q", resp.DurabilityNote)
			}
			if n := countFiles(t, cfg.AgentInboxDir("codex")); n != 1 {
				t.Fatalf("recipient inbox = %d files, want 1", n)
			}
			// --ask records the ASKER-owned obligation; an ask without one is a
			// silent leak.
			owed, err := loop.LoadOwedLedger(cfg, "claude-code")
			if err != nil {
				t.Fatal(err)
			}
			if len(owed.Owed) != 1 || owed.Owed[0].To != "codex" {
				t.Fatalf("owed ledger = %+v, want one obligation against codex", owed.Owed)
			}
		})
	}
}

// --reply-by overrides the default deadline; the op is what applies it, so the
// interval is asserted here rather than inferred from the CLI's flag parsing.
func TestSendReplyByOverridesTheOwedDeadline(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")

	if _, err := Send(cfg, Context{ActorID: "claude-code"}, SendReq{
		To:      "codex",
		Content: loop.ComposeMessage("claude-code", "", "hi"),
		Ask:     true,
		ReplyBy: 45 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	owed, err := loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if got := owed.Owed[0].By.Sub(owed.Owed[0].RecordedAt); got != 45*time.Minute {
		t.Fatalf("reply-by interval = %s, want 45m", got)
	}
}

// Linked-but-sync-failed is a PARTIAL SUCCESS: the message is in the inbox and
// must never be resent. The response is populated and the note carries the
// reason — the shape the CLI's "Do NOT resend" warning renders from.
func TestSendPartialSuccessReportsDurabilityNote(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")

	original := SendTsMessageWithCommit
	SendTsMessageWithCommit = func(cfg *loop.Config, from, to string, content []byte, token string) (loop.TsID, bool, error) {
		id, committed, err := original(cfg, from, to, content, token)
		if err != nil {
			return id, committed, err
		}
		return id, true, errors.New("forced post-link sync failure")
	}
	defer func() { SendTsMessageWithCommit = original }()

	resp, err := Send(cfg, Context{ActorID: "claude-code"}, SendReq{To: "codex", Content: []byte("body")})
	if err != nil {
		t.Fatalf("partial success returned an error: %v", err)
	}
	if !resp.Committed || resp.Filename == "" {
		t.Fatalf("resp = %+v, want a committed delivery", resp)
	}
	if resp.DurabilityNote != "forced post-link sync failure" {
		t.Fatalf("durability note = %q, want the sync failure verbatim", resp.DurabilityNote)
	}
	if n := countFiles(t, cfg.AgentInboxDir("codex")); n != 1 {
		t.Fatalf("recipient inbox = %d files, want the linked message to be there", n)
	}
}

// The third return shape: delivered, but the owed bookkeeping failed. The
// response is populated (so the caller knows NOT to resend) and the error is
// RecordOwed's own, so the warning text stays verbatim.
func TestSendReportsOwedBookkeepingFailureBesideACommittedResponse(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")

	// A directory where the ledger file belongs makes the ledger write fail
	// without touching delivery.
	if err := os.MkdirAll(cfg.AgentStateDir("claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ownedLedgerPath(cfg, "claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}

	resp, err := Send(cfg, Context{ActorID: "claude-code"}, SendReq{
		To:      "codex",
		Content: loop.ComposeMessage("claude-code", "", "hi"),
		Ask:     true,
	})
	if err == nil {
		t.Fatal("an ask whose obligation was never recorded must not report success")
	}
	if resp.Filename == "" || !resp.Committed {
		t.Fatalf("resp = %+v, want the committed delivery reported alongside the error", resp)
	}
	if n := countFiles(t, cfg.AgentInboxDir("codex")); n != 1 {
		t.Fatalf("recipient inbox = %d files, want the message delivered", n)
	}
}

func ownedLedgerPath(cfg *loop.Config, agentID string) string {
	return cfg.AgentStateDir(agentID) + string(os.PathSeparator) + "owed.json"
}
