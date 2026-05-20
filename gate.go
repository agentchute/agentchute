package main

import (
	"encoding/json"
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
// Mirrors the watchdog default in check.go (cooperativeStaleThreshold uses
// the same 5m for peer activity, but a registration is "stale" at a longer
// 30m grace per spec rev3 §A.3).
const StaleRegThreshold = 30 * time.Minute

// Lifecycle phases recognized by `gate --before <phase>`. Order matters
// only for grouping: each later phase implies the earlier phase's checks.
const (
	gatePhaseConsensus = "consensus"
	gatePhaseCommit    = "commit"
	gatePhaseRelease   = "release"
	gatePhaseFinish    = "finish"
)

// cmdGate is the lifecycle gate. Read-only: never refreshes registration,
// never archives, never pokes peers. Reports whether the agent is clear to
// proceed past <phase>; exits 2 (the canonical "blocked" signal honored by
// codex Stop hooks and gemini emergency-brake surfaces) when an obligation
// remains.
//
// Spec: v0.1.1 rev3 §A.3.
func cmdGate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, before, controlRepo, loopDir, codexHook string
	var jsonOut, requireConfirm, ackStaleReg bool
	fs.StringVar(&agentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&before, "before", "", "lifecycle phase: consensus|commit|release|finish")
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

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID == "" {
		return fmt.Errorf("missing agent identity; pass --as or set AGENTCHUTE_AGENT_ID")
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}

	phase := strings.TrimSpace(before)
	if phase == "" {
		return gateUsage(fmt.Errorf("--before <phase> is required"))
	}
	if !isValidGatePhase(phase) {
		return gateUsage(fmt.Errorf("unknown phase %q (valid: consensus|commit|release|finish)", phase))
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

	now := time.Now().UTC()

	// Inbox peek — same path boot/pending use, no side effects. `skipped`
	// is the §11 protocol-violation surface: files that look like inbox
	// messages but fail the §6.1.2 reference filename encoding. They block
	// consensus/finish because the agent owes a quarantine + corrective
	// notify, which `check` runs.
	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		return fmt.Errorf("list inbox: %w", err)
	}

	// Pending-reply ledger — entries owed a reply.
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		return fmt.Errorf("load pending-reply ledger: %w", err)
	}
	pendingReplies := ledger.PendingEntries()

	// Stale-registration check — only run for phases that need it. A missing
	// registration is "stale" by definition: an unenrolled agent should not
	// commit/release. We track missing separately from age-exceeded so the
	// reason text can name the actual condition (codex review on c17e310).
	staleReg := false
	missingReg := false
	regAge := time.Duration(0)
	if phaseChecksStaleReg(phase) {
		regPath := cfg.AgentRegistrationPath(agentID)
		reg, regErr := loop.ReadRegistration(regPath)
		if regErr != nil {
			if os.IsNotExist(regErr) {
				staleReg = true
				missingReg = true
			} else {
				return fmt.Errorf("read own registration: %w", regErr)
			}
		} else {
			regAge = now.Sub(reg.LastSeen.UTC())
			if regAge > StaleRegThreshold {
				staleReg = true
			}
		}
	}

	// Wake-stale — release-phase warn surface (per spec rev3 §A.3).
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
		Agent:          agentID,
		Phase:          phase,
		UnreadCount:    len(msgs),
		MalformedCount: len(skipped),
		RepliesPending: len(pendingReplies),
		StaleReg:       staleReg,
		MissingReg:     missingReg,
		StaleRegAge:    regAge.String(),
		WakeStale:      wakeStaleCount > 0,
		WakeStaleCount: wakeStaleCount,
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
	Agent          string   `json:"agent"`
	Phase          string   `json:"phase"`
	UnreadCount    int      `json:"unread_count"`
	MalformedCount int      `json:"malformed_count"`
	RepliesPending int      `json:"replies_pending"`
	StaleReg       bool     `json:"stale_reg"`
	MissingReg     bool     `json:"missing_reg,omitempty"` // own registration absent (subset of StaleReg)
	StaleRegAge    string   `json:"stale_reg_age,omitempty"`
	WakeStale      bool     `json:"wake_stale"`
	WakeStaleCount int      `json:"wake_stale_count,omitempty"`
	Blocked        bool     `json:"blocked"`
	Reasons        []string `json:"reasons,omitempty"`
	Warnings       []string `json:"warnings,omitempty"` // non-blocking signals (e.g., wake_stale on release)
}

func isValidGatePhase(phase string) bool {
	switch phase {
	case gatePhaseConsensus, gatePhaseCommit, gatePhaseRelease, gatePhaseFinish:
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
		reasons = append(reasons, fmt.Sprintf("%d malformed inbox file(s); run `agentchute check` to quarantine + notify (§11)", s.MalformedCount))
	}
	if s.RepliesPending > 0 {
		reasons = append(reasons, fmt.Sprintf("%d pending reply obligation(s) in ledger", s.RepliesPending))
	}

	// commit + release additionally block on a stale registration unless the
	// caller explicitly acknowledged. The acknowledgment only counts when
	// --require-confirm is set (the request was "double-check me on this");
	// otherwise stale-reg always blocks per the spec default.
	if phaseChecksStaleReg(phase) && s.StaleReg {
		if !(requireConfirm && ackStaleReg) {
			if s.MissingReg {
				reasons = append(reasons, "no registration on disk (run `agentchute boot` or `agentchute register` first)")
			} else {
				reasons = append(reasons, fmt.Sprintf("registration is stale (last_seen age %s > %s)", s.StaleRegAge, StaleRegThreshold))
			}
		}
	}

	// release warns on wake_stale but does not block (per spec rev3 §A.3).
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
Usage: agentchute gate --as <id> --before <phase> [flags]

Lifecycle gate. Reports whether the agent is clear to proceed past the
named phase. Read-only: never refreshes registration, never archives,
never pokes peers.

Phases:
  consensus  blocks on unread direct mail OR pending required-replies
  commit     same as consensus + flags stale registration (> 30m)
  release    same as commit + warns on wake_stale peer registrations
  finish     blocks on unread direct mail OR pending required-replies
             (strongest gate; for end-of-turn use)

Exit codes:
  0  clear to proceed
  2  blocked (message explains why; honored by codex Stop and gemini)
  1  command failure (binary error, filesystem error, etc.)

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --before <phase>      consensus|commit|release|finish (required)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output
  --codex-hook <event>  codex hook JSON shape (Stop)
  --require-confirm     refuse unless warn-level conditions are acknowledged
  --ack-stale-reg       acknowledge stale registration (for --require-confirm)
`)
}
