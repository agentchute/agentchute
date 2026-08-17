package op

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// StaleRegThreshold is the age beyond which `gate --before commit` (and
// stronger phases that wrap it) flag the agent's own registration as stale.
const StaleRegThreshold = 30 * time.Minute

// Lifecycle phases recognized by `gate --before <phase>`. Order matters only
// for grouping: each later phase implies the earlier phase's checks.
//
// The `continue` phase is a sibling of `finish` optimized for in-session
// catchup ("should the wrapper immediately continue into another turn?") —
// identical blocking predicate, diverging only in the caller's envelope.
const (
	GatePhaseConsensus = "consensus"
	GatePhaseCommit    = "commit"
	GatePhaseRelease   = "release"
	GatePhaseFinish    = "finish"
	GatePhaseContinue  = "continue"
)

// GateReq is evaluateGate's inputs minus the ones that arrive out of band: the
// pool is cfg, the actor is Context.ActorID, and `now` is minted by whoever
// runs the op (the hub's clock on a remote lane, §2) and is never a wire field.
type GateReq struct {
	Phase          string `json:"phase"`
	RequireConfirm bool   `json:"require_confirm,omitempty"`
	AckStaleReg    bool   `json:"ack_stale_reg,omitempty"`
}

// GateResp is the cross-format result of a gate evaluation — the shape
// `gate --json` and `turn-end --json` have always emitted, field names and
// JSON tags verbatim. `internal/cli` keeps a one-line type ALIAS to it, so
// every existing decode site still names the same struct.
type GateResp struct {
	Agent           string   `json:"agent"`
	Phase           string   `json:"phase"`
	UnreadCount     int      `json:"unread_count"`
	MalformedCount  int      `json:"malformed_count"`
	StaleReg        bool     `json:"stale_reg"`
	MissingReg      bool     `json:"missing_reg,omitempty"` // own registration absent (subset of StaleReg)
	StaleRegAge     string   `json:"stale_reg_age,omitempty"`
	OwedOutstanding int      `json:"owed_outstanding,omitempty"` // asker-owned obligations awaiting a reply (non-blocking)
	OwedExpired     int      `json:"owed_expired,omitempty"`     // subset past deadline (dead-recipient signal; non-blocking)
	OwedCorrupt     bool     `json:"owed_corrupt,omitempty"`     // .owed ledger unreadable (non-blocking warning)
	ClaimedResidue  int      `json:"claimed_residue,omitempty"`  // messages claimed by check, not yet acked (non-blocking)
	Blocked         bool     `json:"blocked"`
	Reasons         []string `json:"reasons,omitempty"`
	Warnings        []string `json:"warnings,omitempty"` // non-blocking signals
}

// Gate performs the full read-only gate evaluation for a phase. It is the
// SINGLE source of the gate's blocking decision: `gate` emits its result, and
// FinishGateClear (used by Ack and turn-end) reuses it for the finish phase, so
// the two can never diverge. Read-only: never refreshes registration, archives,
// or pokes peers.
func Gate(cfg *loop.Config, ctx Context, req GateReq) (GateResp, error) {
	return evaluateGate(cfg, ctx.ActorID, req, time.Now().UTC())
}

// FinishGateClear reports whether `gate --before finish` would clear, using the
// EXACT SAME predicate. Read-only.
//
// By construction NON-BLOCKING here (so `ack` can always commit its OWN
// just-claimed mail once real blockers clear): uncommitted `.claimed` residue
// and outstanding/expired `.owed` obligations are gate WARNINGS, not Reasons.
func FinishGateClear(cfg *loop.Config, ctx Context, now time.Time) (clear bool, reasons []string, err error) {
	status, err := evaluateGate(cfg, ctx.ActorID, GateReq{Phase: GatePhaseFinish}, now)
	if err != nil {
		return false, nil, err
	}
	return !status.Blocked, status.Reasons, nil
}

func evaluateGate(cfg *loop.Config, agentID string, req GateReq, now time.Time) (GateResp, error) {
	// Inbox peek — same path boot/pending use, no side effects. `skipped` is
	// the §11 protocol-violation surface: files that look like inbox messages
	// but fail the §6.1 reference filename encoding. They block
	// consensus/finish because the agent owes a quarantine + corrective
	// notify, which `check` runs.
	//
	// ErrInboxMissing is treated as "this agent never booted on this host" —
	// folded into the missing-registration path below so the reason text is
	// unified ("not registered; run boot first").
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir(agentID))
	inboxMissing := false
	if err != nil {
		if errors.Is(err, loop.ErrInboxMissing) {
			inboxMissing = true
			msgs, skipped = nil, nil
		} else {
			return GateResp{}, fmt.Errorf("list inbox: %w", err)
		}
	}

	// Asker-owned `.owed` ledger — the SOLE reply-obligation surface.
	// NON-BLOCKING by design: obligations are asker-owned, so the recipient's
	// finish gate is NEVER blocked by a reply_required message. The asker's own
	// outstanding/expired obligations surface as warnings (dead-recipient
	// detection). A corrupt `.owed` is a warning too, never fatal: gate stays
	// read-only and must never deadlock.
	owedOutstanding, owedExpired := 0, 0
	owedCorrupt := false
	if owed, oerr := loop.LoadOwedLedger(cfg, agentID); oerr != nil {
		owedCorrupt = true
	} else {
		owedOutstanding = len(owed.OutstandingOwed())
		owedExpired = len(owed.ExpiredOwed(now))
	}

	// Uncommitted two-phase-consume residue: claimed by `check`, not yet
	// committed by `ack`. NON-BLOCKING; surfaced so the operator knows work is
	// mid-flight. A read error is ignored (best-effort, like the owed read).
	claimedResidue := 0
	if residue, rerr := loop.ListClaimedMessages(cfg.AgentClaimedDir(agentID)); rerr == nil {
		claimedResidue = len(residue)
	}

	// Registration check (§5.3): every phase blocks on missing registration;
	// only commit/release additionally blocks on age-stale registration. An
	// inbox dir that doesn't exist implies a missing registration too — the
	// boot/register path creates both atomically.
	staleReg := false
	missingReg := inboxMissing
	staleRegAge := ""
	reg, regErr := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if regErr != nil {
		if os.IsNotExist(regErr) {
			missingReg = true
		} else {
			return GateResp{}, fmt.Errorf("read own registration: %w", regErr)
		}
	} else if phaseChecksStaleReg(req.Phase) {
		// No zero-LastSeen special case: Validate (called by ReadRegistration
		// before success) already rejects a zero LastSeen, so a successfully
		// read row is structurally non-zero and age-vs-threshold covers every
		// parsed row uniformly.
		age := now.Sub(reg.LastSeen)
		if age < 0 {
			age = 0 // future-dated (clock skew) reads as fresh.
		}
		staleRegAge = age.String()
		if age > StaleRegThreshold {
			staleReg = true
		}
	}

	status := GateResp{
		Agent:           agentID,
		Phase:           req.Phase,
		UnreadCount:     len(msgs),
		MalformedCount:  len(skipped),
		StaleReg:        staleReg,
		MissingReg:      missingReg,
		StaleRegAge:     staleRegAge,
		OwedOutstanding: owedOutstanding,
		OwedExpired:     owedExpired,
		OwedCorrupt:     owedCorrupt,
		ClaimedResidue:  claimedResidue,
	}

	status.Reasons, status.Warnings = evaluateGatePhase(req.Phase, status, req.RequireConfirm, req.AckStaleReg)
	if status.OwedOutstanding > 0 {
		w := fmt.Sprintf("%d outstanding owed reply obligation(s) awaiting a reply", status.OwedOutstanding)
		if status.OwedExpired > 0 {
			w += fmt.Sprintf(" (%d past deadline — recipient may be dead)", status.OwedExpired)
		}
		status.Warnings = append(status.Warnings, w)
	}
	if status.OwedCorrupt {
		status.Warnings = append(status.Warnings, fmt.Sprintf("owed-reply ledger is corrupt or unreadable; inspect `state/%s/owed.json`", status.Agent))
	}
	if status.ClaimedResidue > 0 {
		status.Warnings = append(status.Warnings, fmt.Sprintf("%d claimed-but-unacked message(s) in .claimed; run `agentchute ack --as %s` to commit", status.ClaimedResidue, status.Agent))
	}
	status.Blocked = len(status.Reasons) > 0
	return status, nil
}

// phaseChecksStaleReg reports whether the phase consults the agent's own
// registration freshness. consensus and finish skip it: the relevant question
// there is "do you still owe inbox work?", not "is your enrollment metadata
// fresh?".
func phaseChecksStaleReg(phase string) bool {
	return phase == GatePhaseCommit || phase == GatePhaseRelease
}

// evaluateGatePhase returns the blocking reasons and the non-blocking warnings
// for the agent under the named phase. Empty reasons = clear.
func evaluateGatePhase(phase string, s GateResp, requireConfirm, ackStaleReg bool) ([]string, []string) {
	var reasons, warnings []string

	// Every phase blocks on unread direct mail and malformed inbox files.
	// Malformed files signal a §11 quarantine obligation that gate refuses to
	// clear past — `check` is what discharges it. Reply obligations are
	// asker-owned only: a reply_required message NEVER blocks the recipient.
	if s.UnreadCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unread direct message(s) in inbox", s.UnreadCount))
	}
	if s.MalformedCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d malformed inbox file(s); run `agentchute check --as %s` to quarantine (§11)", s.MalformedCount, s.Agent))
	}

	// §5.3: every phase blocks on missing self-registration. An unenrolled
	// agent has not declared itself to the pool; it can neither commit,
	// finish, nor continue.
	if s.MissingReg {
		reasons = append(reasons, "not registered (run `agentchute boot --as <id> --vendor <vendor>` first; §5.3)")
	}

	// commit + release additionally block on age-stale registration unless the
	// caller explicitly acknowledged. The acknowledgment counts only when
	// --require-confirm is set (the request was "double-check me on this");
	// otherwise stale-reg always blocks per the spec default.
	if phaseChecksStaleReg(phase) && s.StaleReg && !s.MissingReg {
		if !(requireConfirm && ackStaleReg) {
			reasons = append(reasons, fmt.Sprintf("registration is stale (last_seen age %s > %s)", s.StaleRegAge, StaleRegThreshold))
		}
	}

	return reasons, warnings
}
