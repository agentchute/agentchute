package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// turn?" Identical blocking predicate as `finish` (unread / malformed /
// pending-replies / corrupt ledger) — diverges only when a wrapper-specific
// hook envelope is requested.
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

	var agentID, vendor, before, controlRepo, loopDir, codexHook, geminiHook string
	var jsonOut, requireConfirm, ackStaleReg bool
	fs.StringVar(&agentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&before, "before", "", "lifecycle phase: consensus|commit|release|finish|continue")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.StringVar(&codexHook, "codex-hook", "", "codex hook JSON shape for the named event (Stop)")
	fs.StringVar(&geminiHook, "gemini-hook", "", "legacy gemini-cli decision JSON shape (AfterAgent); current Gemini hooks use BeforeAgent + --json")
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
			return fmt.Errorf("list inbox: %w", err)
		}
	}

	// Pending-reply ledger — entries owed a reply. A corrupt / oversized /
	// unparseable ledger must NOT be fatal: returning the parse error here would
	// brick EVERY gate phase until a human edits pending-replies.json. It must
	// also NOT be silently treated as "no obligations" — that would false-clear
	// finish past a ledger we can no longer trust. Instead we BLOCK with an
	// actionable, quarantine-style remediation (mirrors the malformed-inbox
	// surface below) so the agent can't falsely finish but other diagnostics
	// (unread mail, registration, liveness) still run.
	var pendingReplies []loop.PendingReplyEntry
	ledgerCorrupt := false
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		ledgerCorrupt = true
	} else {
		pendingReplies = ledger.PendingEntries()
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
			return fmt.Errorf("read own registration: %w", regErr)
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
	// Wake-stale — release-phase warn surface (per the watchdog liveness model, AGENTCHUTE.md §10).
	// Reads peer registrations and counts those that declare a wake_method
	// (pokable) but whose last_seen exceeds StaleRegThreshold. A non-zero
	// count populates the JSON shape and shows up in text output; it does
	// NOT block release.
	wakeStaleCount := 0
	if phase == gatePhaseRelease {
		c, err := countWakeStalePeers(cfg, agentID, now, StaleRegThreshold)
		if err != nil {
			return fmt.Errorf("scan peer registrations: %w", err)
		}
		wakeStaleCount = c
	}

	status := gateStatus{
		Agent:           agentID,
		Phase:           phase,
		UnreadCount:     len(msgs),
		MalformedCount:  len(skipped),
		RepliesPending:  len(pendingReplies),
		LedgerCorrupt:   ledgerCorrupt,
		StaleReg:        staleReg,
		MissingReg:      missingReg,
		StaleRegAge:     staleRegAge,
		WakeStale:       wakeStaleCount > 0,
		WakeStaleCount:  wakeStaleCount,
	}

	// Apply the phase predicates to build the blocking-reasons list and
	// any non-blocking warnings (e.g., wake_stale on release).
	status.Reasons, status.Warnings = evaluateGatePhase(phase, status, requireConfirm, ackStaleReg)
	status.Blocked = len(status.Reasons) > 0

	// Emit + exit.
	switch {
	case codexHook == "Stop":
		// On clear: no output, exit 0. On block: emit {"decision":"block",...}
		// JSON, exit 0 (codex's preferred shape; main.go won't see errBlocked).
		return emitGateCodexStop(status)
	case geminiHook == "AfterAgent":
		// Legacy/experimental gemini-cli AfterAgent decision envelope:
		// {"decision":"deny","reason":"..."} on block, {"decision":"allow"}
		// on clear. Always exits 0 (the JSON is the signal). Current shipped
		// Gemini hooks use BeforeAgent + --json exit-code blocking instead.
		return emitGateGeminiAfterAgent(status)
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

// gateStatus is the cross-format result of a gate evaluation.
type gateStatus struct {
	Agent           string   `json:"agent"`
	Phase           string   `json:"phase"`
	UnreadCount     int      `json:"unread_count"`
	MalformedCount  int      `json:"malformed_count"`
	RepliesPending  int      `json:"replies_pending"`
	LedgerCorrupt   bool     `json:"ledger_corrupt,omitempty"` // pending-reply ledger unparseable/oversized (blocks; quarantine remediation)
	StaleReg        bool     `json:"stale_reg"`
	MissingReg      bool     `json:"missing_reg,omitempty"` // own registration absent (subset of StaleReg)
	StaleRegAge     string   `json:"stale_reg_age,omitempty"`
	WakeStale       bool     `json:"wake_stale"`
	WakeStaleCount  int      `json:"wake_stale_count,omitempty"`
	Blocked         bool     `json:"blocked"`
	Reasons         []string `json:"reasons,omitempty"`
	Warnings        []string `json:"warnings,omitempty"` // non-blocking signals (e.g., wake_stale on release)
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

	// Every phase blocks on unread direct mail, malformed inbox files, and
	// pending replies. Malformed files signal a §11 quarantine obligation
	// that gate refuses to clear past — `check` is what discharges it.
	if s.UnreadCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unread direct message(s) in inbox", s.UnreadCount))
	}
	if s.MalformedCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d malformed inbox file(s); run `agentchute check --as %s` to quarantine + notify (§11)", s.MalformedCount, s.Agent))
	}
	if s.RepliesPending > 0 {
		reasons = append(reasons, fmt.Sprintf("%d pending reply obligation(s) in ledger", s.RepliesPending))
	}
	// A corrupt/unparseable pending-reply ledger blocks every phase: we cannot
	// trust it to be empty, so we refuse to clear past it rather than crash
	// (fatal) or false-clear (treat as 0 obligations). Quarantine-style
	// remediation: the operator inspects/repairs/moves the file, then re-runs.
	if s.LedgerCorrupt {
		reasons = append(reasons, fmt.Sprintf(
			"pending-reply ledger is corrupt or unreadable; inspect or quarantine the file and re-run (`agentchute pending --as %s`)",
			s.Agent))
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

	// release warns on wake_stale but does not block (per AGENTCHUTE.md §10).
	if phase == gatePhaseRelease && s.WakeStaleCount > 0 {
		warnings = append(warnings, fmt.Sprintf("%d peer registration(s) declare a wake_method but are stale (last_seen > %s); pokes may fail", s.WakeStaleCount, StaleRegThreshold))
	}

	return reasons, warnings
}

// countWakeStalePeers scans the registry and counts peers (excluding self)
// that are pokable but whose last_seen is older than threshold. Used by
// release to surface the WAKE_STALE warning. Best-effort: peers with
// unreadable registrations are silently skipped (they are status's concern,
// not gate's).
func countWakeStalePeers(cfg *loop.Config, selfID string, now time.Time, threshold time.Duration) (int, error) {
	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	stale := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") || name == "README.md" {
			continue
		}
		reg, err := loop.ReadRegistration(filepath.Join(cfg.AgentsDir(), name))
		if err != nil {
			continue
		}
		if reg.AgentID == selfID || !reg.IsPokable() {
			continue
		}
		if now.Sub(reg.LastSeen.UTC()) > threshold {
			stale++
		}
	}
	return stale, nil
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

// emitGateGeminiAfterAgent emits the legacy/experimental gemini-cli AfterAgent
// decision JSON. Current shipped Gemini hooks use BeforeAgent + --json.
// On block: `{"decision":"deny","reason":"..."}` forces another turn.
// On clear: `{"decision":"allow"}` lets the session end. Always exits 0
// (the JSON is the signal). v0.2 wake-method R&D: this is the in-session
// continuation surface for gemini-cli without an external scheduler;
// typically paired with `--before continue`.
func emitGateGeminiAfterAgent(s gateStatus) error {
	if !s.Blocked {
		out := map[string]any{"decision": "allow"}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(out)
	}
	reason := fmt.Sprintf("agentchute gate --before %s: %s", s.Phase, strings.Join(s.Reasons, "; "))
	out := map[string]any{
		"decision": "deny",
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
  release    same as commit + warns on wake_stale peer registrations
  finish     blocks on outstanding work
             (strongest gate; for end-of-turn use)
  continue   same predicate as finish; for in-session decision hooks
             that ask "continue the turn?"

Outstanding work / trust blockers (all phases):
  - unread direct mail in the inbox
  - malformed inbox files that require check quarantine + corrective notice
  - pending required-reply ledger entries
  - corrupt or unreadable pending-reply ledger state
  - missing self-registration

All phases block if this agent is not registered.

Exit codes:
  0  clear to proceed; also used by hook-envelope modes whose JSON is the signal
     (--codex-hook Stop, --gemini-hook AfterAgent)
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
  --gemini-hook <event> legacy gemini-cli decision JSON shape (AfterAgent);
                        current Gemini templates use BeforeAgent + --json
  --require-confirm     refuse unless warn-level conditions are acknowledged
  --ack-stale-reg       acknowledge stale registration (for --require-confirm)
`)
}
