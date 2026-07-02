package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// StaleRegThreshold is the age beyond which `gate --before commit` (and
// stronger phases that wrap it) flag the agent's own registration as stale.
const StaleRegThreshold = 30 * time.Minute

// Lifecycle phases recognized by `gate --before <phase>`. Order matters
// only for grouping: each later phase implies the earlier phase's checks.
//
// The `continue` phase (v0.2) is a sibling of `finish` optimized for
// in-session catchup: "should the wrapper immediately continue into another
// turn?" Identical blocking predicate as `finish` (unread / malformed) —
// diverges only when a wrapper-specific hook envelope is requested.
const (
	gatePhaseConsensus = "consensus"
	gatePhaseCommit    = "commit"
	gatePhaseRelease   = "release"
	gatePhaseFinish    = "finish"
	gatePhaseContinue  = "continue"
)

// cmdGate is the lifecycle gate. Read-only: never refreshes registration,
// never archives, never pokes peers. Reports whether the agent is clear to
// proceed past <phase>; exits 2 in text/--json modes (the canonical "blocked"
// signal) when an obligation remains. Wrapper-specific hook-envelope modes
// return exit 0 and signal block/allow in their JSON payload.
//
// Spec: AGENTCHUTE.md §6 (messaging obligations).
func cmdGate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, before, controlRepo, loopDir, codexHook string
	var jsonOut, requireConfirm, ackStaleReg bool
	fs.StringVar(&agentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&before, "before", "", "lifecycle phase: consensus|commit|release|finish|continue")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.StringVar(&codexHook, "codex-hook", "", "codex hook JSON shape for the named event (Stop)")
	fs.BoolVar(&requireConfirm, "require-confirm", false, "refuse unless warn-level conditions are explicitly acknowledged")
	fs.BoolVar(&ackStaleReg, "ack-stale-reg", false, "acknowledge that the registration is stale (for --require-confirm)")

	if err := fs.Parse(args); err != nil {
		return gateUsage(err)
	}
	if fs.NArg() != 0 {
		return gateUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return err
	}

	agentID, err = resolveAgentID(agentID, vendor, cfg)
	if err != nil {
		return err
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}

	phase := strings.TrimSpace(before)
	if phase == "" {
		return gateUsage(fmt.Errorf("--before <phase> is required"))
	}
	if !isValidGatePhase(phase) {
		return gateUsage(fmt.Errorf("unknown phase %q (valid: consensus|commit|release|finish|continue)", phase))
	}

	now := time.Now().UTC()
	status, err := evaluateGate(cfg, agentID, phase, requireConfirm, ackStaleReg, now)
	if err != nil {
		return err
	}

	// Emit + exit.
	switch {
	case codexHook == "Stop":
		// On clear: no output, exit 0. On block: emit {"decision":"block",...}
		// JSON, exit 0 (codex's preferred shape; main.go won't see errBlocked).
		return emitGateCodexStop(status)
	case jsonOut:
		if err := emitGateJSON(status); err != nil {
			return err
		}
	default:
		emitGateText(status)
	}

	if status.Blocked {
		return errBlocked
	}
	return nil
}

// evaluateGate performs the full read-only gate evaluation for `phase` and
// returns the populated status (Reasons/Warnings/Blocked all set). It is the
// SINGLE source of the gate's blocking decision: cmdGate emits its result, and
// finishGateClear (used by `ack`) reuses it for the finish phase, so the two can
// never diverge. Read-only: never refreshes registration, archives, or pokes
// peers. `now` is injected so callers share one clock for liveness/expiry.
func evaluateGate(cfg *loop.Config, agentID, phase string, requireConfirm, ackStaleReg bool, now time.Time) (gateStatus, error) {
	// Inbox peek — same path boot/pending use, no side effects. `skipped`
	// is the §11 protocol-violation surface: files that look like inbox
	// messages but fail the §6.1 reference filename encoding. They block
	// consensus/finish because the agent owes a quarantine + corrective
	// notify, which `check` runs.
	//
	// ErrInboxMissing is treated as "this agent never booted on this
	// host" — folded into the missing-registration path below so the
	// reason text is unified ("not registered; run boot first").
	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	inboxMissing := false
	if err != nil {
		if errors.Is(err, loop.ErrInboxMissing) {
			inboxMissing = true
			msgs, skipped = nil, nil
		} else {
			return gateStatus{}, fmt.Errorf("list inbox: %w", err)
		}
	}

	// Asker-owned `.owed` ledger (protocol-v2) — the SOLE reply-obligation
	// surface (v0.9.0). NON-BLOCKING by design: reply obligations are asker-owned
	// only, so the recipient's finish gate is NEVER blocked by a reply_required
	// message (best-effort pull delivery, no forcing function once delivered).
	// The asker's own outstanding / expired obligations are surfaced here as
	// warnings (dead-recipient detection — an expired entry means the recipient
	// may be dead). A corrupt `.owed` is a warning too, never fatal: gate stays
	// read-only and must never deadlock.
	owedOutstanding, owedExpired := 0, 0
	owedCorrupt := false
	if owed, oerr := loop.LoadOwedLedger(cfg, agentID); oerr != nil {
		owedCorrupt = true
	} else {
		owedOutstanding = len(owed.OutstandingOwed())
		owedExpired = len(owed.ExpiredOwed(now))
	}

	// Uncommitted two-phase-consume residue: messages CLAIMED by `check` but not
	// yet COMMITTED by `ack`. NON-BLOCKING (gate is read-only; `ack` commits);
	// surfaced so the operator knows work is mid-flight. A read error is ignored
	// (best-effort, like the owed read).
	claimedResidue := 0
	if residue, rerr := loop.ListClaimedMessages(cfg.AgentClaimedDir(agentID)); rerr == nil {
		claimedResidue = len(residue)
	}

	// Registration check — v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md
	// §5.3): every phase blocks on missing registration; only commit/release
	// additionally blocks on age-stale registration. An inbox dir that
	// doesn't exist implies a missing registration too — the boot/register
	// path creates both atomically.
	staleReg := false
	missingReg := inboxMissing
	staleRegAge := ""
	regPath := cfg.AgentRegistrationPath(agentID)
	// The registration read still gates missing-vs-present (and surfaces a
	// corrupt own-registration as a hard error); only the FRESHNESS source
	// moves to `.live`.
	_, regErr := loop.ReadRegistration(regPath)
	if regErr != nil {
		if os.IsNotExist(regErr) {
			missingReg = true
		} else {
			return gateStatus{}, fmt.Errorf("read own registration: %w", regErr)
		}
	} else if phaseChecksStaleReg(phase) {
		// GATE 3: presence/freshness comes from `.live`, NOT registration
		// last_seen. A fresh `.live` => not stale; a `.live` older than the
		// threshold => stale; an absent `.live` for a registered agent (never
		// published, or expired) => stale, same as an old registration would be.
		// StaleRegThreshold and the staleReg/StaleRegAge JSON shape are kept.
		liveSeen, present := loop.LiveLastSeen(cfg, agentID)
		if !present {
			staleReg = true // .live absent => stale (StaleRegAge stays empty).
		} else {
			age := now.Sub(liveSeen)
			if age < 0 {
				age = 0 // future-dated (clock skew) reads as fresh.
			}
			staleRegAge = age.String()
			if age > StaleRegThreshold {
				staleReg = true
			}
		}
	}
	status := gateStatus{
		Agent:           agentID,
		Phase:           phase,
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

	// Apply the phase predicates to build the blocking-reasons list and
	// any non-blocking warnings.
	status.Reasons, status.Warnings = evaluateGatePhase(phase, status, requireConfirm, ackStaleReg)
	// Asker-owned `.owed` obligations (protocol-v2): NON-BLOCKING dead-recipient
	// signal. This is the sole reply-obligation surface (v0.9.0); the recipient
	// is never blocked at finish by a reply_required message.
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
	// Uncommitted claimed residue (two-phase consume): NON-BLOCKING — `ack`
	// commits it (gate is read-only).
	if status.ClaimedResidue > 0 {
		status.Warnings = append(status.Warnings, fmt.Sprintf("%d claimed-but-unacked message(s) in .claimed; run `agentchute ack --as %s` to commit", status.ClaimedResidue, status.Agent))
	}
	status.Blocked = len(status.Reasons) > 0
	return status, nil
}

// finishGateClear reports whether `agentchute gate --before finish` would clear,
// using the EXACT SAME predicate (evaluateGate over the finish phase): unread
// inbox / malformed inbox / missing registration. Read-only.
//
// By construction NON-BLOCKING here (so `ack` can always commit its OWN
// just-claimed mail once real blockers clear): uncommitted `.claimed` residue and
// outstanding/expired `.owed` obligations are gate WARNINGS, not Reasons, so they
// never make this return clear=false. `ack` is the caller; gate.go drives its own
// finish decision through the same evaluateGate path, so the two cannot diverge.
func finishGateClear(cfg *loop.Config, agentID string, now time.Time) (clear bool, reasons []string, err error) {
	status, err := evaluateGate(cfg, agentID, gatePhaseFinish, false, false, now)
	if err != nil {
		return false, nil, err
	}
	return !status.Blocked, status.Reasons, nil
}

// gateStatus is the cross-format result of a gate evaluation.
type gateStatus struct {
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

func isValidGatePhase(phase string) bool {
	switch phase {
	case gatePhaseConsensus, gatePhaseCommit, gatePhaseRelease, gatePhaseFinish, gatePhaseContinue:
		return true
	}
	return false
}

// phaseChecksStaleReg reports whether the given phase consults the agent's
// own registration freshness. consensus and finish skip the check because
// the relevant question for those phases is "do you still owe inbox /
// reply work?" — they don't require fresh enrollment metadata.
func phaseChecksStaleReg(phase string) bool {
	return phase == gatePhaseCommit || phase == gatePhaseRelease
}

// evaluateGatePhase returns the list of blocking reasons and the list of
// non-blocking warnings for the agent under the named phase. Empty reasons
// = clear.
func evaluateGatePhase(phase string, s gateStatus, requireConfirm, ackStaleReg bool) ([]string, []string) {
	var reasons, warnings []string

	// Every phase blocks on unread direct mail and malformed inbox files.
	// Malformed files signal a §11 quarantine obligation that gate refuses to
	// clear past — `check` is what discharges it. Reply obligations are
	// asker-owned only (v0.9.0): a reply_required message NEVER blocks the
	// recipient's finish gate (best-effort pull delivery).
	if s.UnreadCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unread direct message(s) in inbox", s.UnreadCount))
	}
	if s.MalformedCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d malformed inbox file(s); run `agentchute check --as %s` to quarantine + notify (§11)", s.MalformedCount, s.Agent))
	}

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): every phase blocks
	// on missing self-registration. An unenrolled agent has not declared
	// itself to the pool; it can neither commit, finish, nor continue.
	if s.MissingReg {
		reasons = append(reasons, "not registered (run `agentchute boot --as <id> --vendor <vendor>` first; §5.3)")
	}

	// commit + release additionally block on age-stale registration unless
	// the caller explicitly acknowledged. The acknowledgment only counts
	// when --require-confirm is set (the request was "double-check me on
	// this"); otherwise stale-reg always blocks per the spec default.
	if phaseChecksStaleReg(phase) && s.StaleReg && !s.MissingReg {
		if !(requireConfirm && ackStaleReg) {
			if s.StaleRegAge == "" {
				// `.live` absent for a registered agent: no presence ever
				// published (or it expired). Cite that distinctly rather than
				// leaking a misleading "age 0s > threshold".
				reasons = append(reasons, fmt.Sprintf("registration is stale (no recent presence; run `agentchute boot --as %s`)", s.Agent))
			} else {
				reasons = append(reasons, fmt.Sprintf("registration is stale (last_seen age %s > %s)", s.StaleRegAge, StaleRegThreshold))
			}
		}
	}

	return reasons, warnings
}

func emitGateText(s gateStatus) {
	if !s.Blocked {
		fmt.Printf("gate %s (%s): clear\n", s.Phase, s.Agent)
		for _, w := range s.Warnings {
			fmt.Printf("  warning: %s\n", w)
		}
		return
	}
	fmt.Printf("gate %s (%s): blocked\n", s.Phase, s.Agent)
	for _, r := range s.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	for _, w := range s.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func emitGateJSON(s gateStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// emitGateCodexStop emits codex's Stop-hook shape. On block: `{"decision":
// "block","reason":"..."}` to stdout, exit 0 (returned nil) — codex sees
// the decision and continues the turn. On clear: no stdout (codex stops
// normally). Differs from the boot --codex-hook SessionStart wrapper
// because Stop's contract is "block/continue", not "inject context".
func emitGateCodexStop(s gateStatus) error {
	if !s.Blocked {
		return nil
	}
	reason := fmt.Sprintf("agentchute gate --before %s: %s", s.Phase, strings.Join(s.Reasons, "; "))
	out := map[string]any{
		"decision": "block",
		"reason":   reason,
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

func gateUsage(err error) error {
	if err == flag.ErrHelp {
		return gateHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, gateHelp())
}

func gateHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, gateHelp())
}

func gateHelp() string {
	return strings.TrimSpace(`
Usage: agentchute gate [--vendor <vendor>] [--as <id>] --before <phase> [flags]

Lifecycle gate. Reports whether the agent is clear to proceed past the
named phase. Read-only: never refreshes registration, never archives,
never pokes peers.

Phases:
  consensus  blocks on outstanding work
  commit     same as consensus + flags stale registration (> 30m)
  release    same as commit
  finish     blocks on outstanding work
             (strongest gate; for end-of-turn use)
  continue   same predicate as finish; for in-session decision hooks
             that ask "continue the turn?"

Outstanding work / trust blockers (all phases):
  - unread direct mail in the inbox
  - malformed inbox files that require check quarantine + corrective notice
  - missing self-registration

Reply obligations are asker-owned only (v0.9.0): a reply_required message
never blocks the recipient's finish gate. The asker's own outstanding/expired
.owed obligations surface here as non-blocking warnings.

All phases block if this agent is not registered.

Exit codes:
  0  clear to proceed; also used by hook-envelope modes whose JSON is the signal
     (--codex-hook Stop)
  2  blocked in text/--json modes (including shipped Claude/Gemini finish hooks)
  1  command failure (binary error, filesystem error, etc.)

Flags:
  --before <phase>      consensus|commit|release|finish|continue (required)
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin (anthropic, openai, google, xai)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output (blocked still exits 2)
  --codex-hook <event>  codex hook JSON shape (Stop)
  --require-confirm     refuse unless warn-level conditions are acknowledged
  --ack-stale-reg       acknowledge stale registration (for --require-confirm)
`)
}
