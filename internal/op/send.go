package op

import (
	"errors"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// SendTsMessageWithCommit is the delivery seam (F1): mint the send stamp under
// the write-ahead floor, then link into the recipient's inbox under their lock
// (fresh-suffix EEXIST retry included). EXPORTED deliberately — two existing
// package-`cli` tests reassign it to force a post-link sync failure and a
// fresh-but-racing recipient, and an unexported var here would leave them
// nothing to patch. One production call site: Send, below.
var SendTsMessageWithCommit = loop.SendTsMessageWithCommit

// SendReq is a fully composed message plus its delivery options.
//
// Content is the WHOLE message (frontmatter + body): composition — the ASK
// heading, reply_required splicing, loop.ComposeMessage — stays client-side so
// the hub never rewrites bodies (§4.5.1). There is NO From field: the sender is
// Context.ActorID, so a request can never assert a second sender (§3.1).
type SendReq struct {
	To         string        `json:"to"`
	Content    []byte        `json:"content"`
	Ask        bool          `json:"ask,omitempty"`
	ReplyBy    time.Duration `json:"reply_by,omitempty"`
	ServeToken string        `json:"serve_token,omitempty"`
}

// SendResp is a committed delivery.
//
// Two independent non-fatal outcomes can ride alongside a commit, and they are
// SEPARATE fields because they are separate facts with separate operator text —
// and because both can happen on the same send:
//
//   - DurabilityNote: linked-but-dir-sync-failed. The message IS in the
//     recipient's inbox and must never be resent.
//   - OwedNote: delivery committed but the ASKER-side reply obligation could
//     not be recorded. An ask without a recorded obligation is a silent leak,
//     so it is reported loudly — but it is not a delivery failure, and nothing
//     may treat it as one.
//
// Both are always present on the wire (empty when they did not occur), like
// tick-ok's warnings.
type SendResp struct {
	Filename       string `json:"filename"`
	Ref            string `json:"ref"`
	Committed      bool   `json:"committed"`
	DurabilityNote string `json:"durability_note"`
	OwedNote       string `json:"owed_note"`
}

// SendPreflight is Send's lock-free, non-mutating validation: the sender is
// enrolled at all, and the recipient is registered and fresh.
//
// It is exported because it is also the CLI's A5 fast-fail: cmdSend runs it
// BEFORE reading stdin so a piped body stays untouched on a doomed send. Send
// runs it again as the authoritative check — the recheck is two lock-free
// reads, and on the wire there is no earlier point to run it at (§4.5.1).
// Deliberately lock-free: WithAgentLock's ensurePrivateDir side effect would
// otherwise manufacture state/<to>/ for an arbitrary --to typo (B3 §4 risk).
func SendPreflight(cfg *loop.Config, ctx Context, to string) error {
	// B1: this confirms the sender is enrolled at all; it never refreshes
	// liveness (only serve's lease-gated heartbeat does).
	if err := requireRegistered(cfg, ctx.ActorID); err != nil {
		return err
	}

	rr, err := loop.CheckRecipientReachability(cfg, to, time.Now().UTC())
	if err != nil {
		// ErrRecipientUnknown / ErrRecipientUnreadable are re-exported
		// sentinels, so these already satisfy errors.Is against the op set.
		return err
	}
	if !rr.Fresh {
		return staleAt(ErrRecipientStale, &loop.ErrRecipientStale{
			To:        to,
			LastSeen:  rr.LastSeen,
			Age:       rr.Age,
			Threshold: rr.Threshold,
		})
	}
	return nil
}

// Send validates and delivers one composed message, then — when Ask — records
// the ASKER-OWNED reply obligation.
//
// Return contract, exactly TWO shapes — once delivery commits, the error is
// always nil:
//
//   - (zero SendResp, err): nothing was delivered. err classifies why:
//     ErrNotRegistered, ErrRecipientUnknown/Unreadable/Stale from the
//     preflight, ErrRecipientRacing or ErrFenced from delivery, or a raw I/O
//     error. The caller still holds the body and must spool it.
//   - (populated SendResp, nil): delivered. DurabilityNote and/or OwedNote
//     report anything non-fatal that happened alongside the commit.
//
// **Committed is the discriminator**, not a non-empty Filename — Filename is
// only an incidental proxy for the same fact. Committed is the field DESIGN
// §4.4.1 makes mandatory on every send-ok and the one the never-auto-replay
// rule is written against, so the local decision and the wire decision are the
// same predicate (opus-xhigh, PR #148 gate).
//
// **Why the owed failure is a FIELD and not an error beside a populated
// response** (codex, PR #148 gate; authorized by claude-code): a remote send
// terminates with either send-ok or error (§§4.4/4.5.3). An error frame loses
// the committed response and drives spool/failure handling — a duplicate-send
// hazard on an operation that already committed — while the send-ok frame had
// nowhere to carry the warning. So "exit 0, delivered, do NOT resend" was not
// expressible on the wire at all. A seam whose local behavior cannot survive
// its own transport is a seam defect, so the fact moved onto the response.
func Send(cfg *loop.Config, ctx Context, req SendReq) (SendResp, error) {
	now := time.Now().UTC()

	if err := SendPreflight(cfg, ctx, req.To); err != nil {
		return SendResp{}, err
	}

	// serveToken fences the write: a send from a child launched under
	// `agentchute serve` carries the runner's active serve-lease epoch, so a
	// write from a fenced (reclaimed) agent fails closed. Empty token (no
	// serve lease) => intentionally unfenced.
	id, committed, sendErr := SendTsMessageWithCommit(cfg, ctx.ActorID, req.To, req.Content, req.ServeToken)
	if sendErr != nil && !committed {
		return SendResp{}, classifyDeliveryFailure(sendErr)
	}

	// The committed identity is (to,from,stamp,suffix); the suffix may differ
	// from the one first proposed if a link collision forced a fresh-suffix
	// retry (C4).
	resp := SendResp{Filename: id.Filename(), Ref: id.RefString(), Committed: true}
	if sendErr != nil {
		resp.DurabilityNote = sendErr.Error()
	}

	if req.Ask {
		deadline := now.Add(loop.ReplyOwedDeadline)
		if req.ReplyBy > 0 {
			deadline = now.Add(req.ReplyBy)
		}
		if err := loop.RecordOwed(cfg, ctx.ActorID, id, deadline, now); err != nil {
			// Not an error: the message is delivered and resending it would
			// duplicate it. The caller renders this verbatim.
			resp.OwedNote = err.Error()
		}
	}
	return resp, nil
}

// classifyDeliveryFailure records WHICH raise site produced a stale recipient.
// Reached only after Send's own preflight already found `to` fresh, so a stale
// verdict here is the racing case (C29c) by construction — the row went stale
// in the gap between the preflight and the recipient lock. Everything else
// passes through unchanged: the loop sentinels are re-exports, so they already
// satisfy errors.Is against the op set.
func classifyDeliveryFailure(err error) error {
	var stale *loop.ErrRecipientStale
	if errors.As(err, &stale) {
		return staleAt(ErrRecipientRacing, stale)
	}
	return err
}
