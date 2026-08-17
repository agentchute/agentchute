package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// StaleRegThreshold and the phase names are the seam's, re-exported here so
// every existing caller (doctor.go, turn_end.go, the tests) keeps naming the
// same value. The gate's evaluation itself lives in internal/op.
const StaleRegThreshold = op.StaleRegThreshold

const (
	gatePhaseConsensus = op.GatePhaseConsensus
	gatePhaseCommit    = op.GatePhaseCommit
	gatePhaseRelease   = op.GatePhaseRelease
	gatePhaseFinish    = op.GatePhaseFinish
	gatePhaseContinue  = op.GatePhaseContinue
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

	agentID, err = resolveAgentID(agentID, cfg)
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

	req := op.GateReq{
		Phase:          phase,
		RequireConfirm: requireConfirm,
		AckStaleReg:    ackStaleReg,
	}
	var status op.GateResp
	if cfg.Remote != nil {
		session, openErr := openRemoteOneShot(cfg, agentID)
		if openErr != nil {
			return openErr
		}
		status, err = session.Gate(req)
	} else {
		status, err = op.Gate(cfg, op.Context{ActorID: agentID}, req)
	}
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

	emitGateBlockedStderr(status)
	if status.Blocked {
		return errBlocked
	}
	return nil
}

// gateStatus is the cross-format result of a gate evaluation. An ALIAS, not a
// defined type: the struct now lives in internal/op (which the hub session
// serializes verbatim), and every existing decode site still names this.
type gateStatus = op.GateResp

func isValidGatePhase(phase string) bool {
	switch phase {
	case gatePhaseConsensus, gatePhaseCommit, gatePhaseRelease, gatePhaseFinish, gatePhaseContinue:
		return true
	}
	return false
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

// gateBlockedReasonLine is the one-line block verdict shared by every channel
// that carries the reason to a model: the codex Stop envelope's "reason" field
// and the stderr mirror below.
func gateBlockedReasonLine(s gateStatus) string {
	return fmt.Sprintf("agentchute gate --before %s: %s", s.Phase, strings.Join(s.Reasons, "; "))
}

// emitGateBlockedStderr mirrors a blocked verdict to stderr in the
// non-envelope modes (text and --json). Claude Code's Stop hook surfaces only
// stderr for an exit-2 hook — its stdout is reserved for that harness's own
// decision-JSON schema, which the plain --json shape is not — so a
// stdout-only block re-prompted the session with "No stderr output" and no
// actionable content, an empty-feedback wake loop (aws-demo report,
// 2026-08-11). Envelope modes (--codex-hook) keep their single channel.
func emitGateBlockedStderr(s gateStatus) {
	if !s.Blocked {
		return
	}
	fmt.Fprintln(os.Stderr, gateBlockedReasonLine(s))
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
	out := map[string]any{
		"decision": "block",
		"reason":   gateBlockedReasonLine(s),
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
  - malformed inbox files that require check quarantine
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
