package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// cmdTurnEnd is the ordered end-of-turn handler (v2.5 plan A7, C24): ONE
// process replaces the separate `self-check` + `ack --quiet` +
// `gate --before finish` end-of-turn hook entries. Codex's docs say same-event
// hooks run CONCURRENTLY, so "self-check entry stays first" was never
// enforceable as template ordering on every vendor — folding all four steps
// into one process is the only way to guarantee the order.
//
// Strict in-process order, steps 0 and 3 run regardless of step 1's
// condition:
//
//  0. registration self-repair — identical logic to `self-check`
//     (selfRepairRegistration), so the row exists and last_seen/.live are
//     fresh before the gate evaluates.
//  1. archive `.claimed` UNLESS a DIFFERENT session's latch says otherwise
//     (C23). Only a latch that both (a) reads back successfully and (b)
//     names a foreign/dead session withholds the commit — that residue is
//     left for `check`'s own redelivery banner. No latch at all (guard
//     disabled, hand-run/unguarded session, or nothing claimed yet) always
//     archives, matching `ack`'s pre-A7 unconditional-commit contract; a
//     latch that fails to read (absent OR corrupt) is treated the same way,
//     never as a reason to withhold the commit.
//  2. clear the own-session guard latch (no-op if none/foreign).
//  3. evaluate + emit the finish gate via the EXACT SAME contracts as
//     `agentchute gate --before finish` (gate.go): default text, --json, and
//     --codex-hook Stop (silent on clear, block-JSON exit-0 on block).
//
// Recovery property (and its known limit): turn-end has NO self-denial of
// its own (unlike check/ack) — its only possible denial is the PreToolUse
// guard hook itself. When NEITHER that hook NOR the Stop hook is firing at
// all (e.g. a hook-trust rollout window on a vendor that gates project-local
// hook changes per-command), a lane armed by `check` is still recoverable:
// the guard that would deny a direct `turn-end` invocation also isn't
// running, so nothing stops it (check/ack's own self-denial error text names
// it as the fix for exactly this reason — TestGuardArmedWithoutHooksEverFiringStillRecoversViaTurnEnd).
//
// KNOWN GAP (codex review, PR #89 round 3, finding #1 — NOT fixed): a MIXED
// state where the PreToolUse guard is active but Stop is independently
// disabled/failing is not recoverable this way — the active guard denies a
// model's own attempt to run `turn-end` (it is deliberately deny-listed, so a
// same-turn instruction can't clear its own latch and disarm the rest of the
// deny list for the remainder of the turn). Removing turn-end from the deny
// list would close this gap but reopen that exact bypass, which several
// review rounds have independently protected; kept deny-listed on the
// judgment that a same-turn security bypass is worse than a narrow, human-
// recoverable (delete state/<id>/guard.latch) hook-rollout edge. Flagged for
// Alex/reviewers as an open design question, not silently accepted.
func cmdTurnEnd(args []string) error {
	fs := flag.NewFlagSet("turn-end", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, bio, controlRepo, loopDir, codexHook string
	var jsonOut bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai, local)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
	fs.StringVar(&bio, "bio", "", "short self-description for the registration body")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.StringVar(&codexHook, "codex-hook", "", "codex hook JSON shape for the named event (Stop)")

	if err := fs.Parse(args); err != nil {
		return turnEndUsage(err)
	}
	if fs.NArg() != 0 {
		return turnEndUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	opts := registerOpts{Host: host, Bio: bio, ServeToken: os.Getenv("AGENTCHUTE_SERVE_TOKEN")}
	// WI-E3 provenance: turn-end is a lifecycle hook enroll, same as
	// self-check. Under the runner (AGENTCHUTE_RUNNER=1) it records `runner`
	// so the runner lane is not demoted.
	opts.LaunchedBy, opts.HookEvent = hookLaunchProvenance("turn-end")
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			opts.HostProvided = true
		case "bio":
			opts.BioProvided = true
		}
	})

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

	// STEP 0. Best-effort: a registration WRITE failure (e.g. --vendor was
	// never passed — C26 ships turn-end's hook entries env-identity-only —
	// and this id doesn't prefix-match a canonical wrapper base closely
	// enough for resolveAgentVendor to backfill one from an existing row)
	// must not itself abort steps 1-3, which still need to commit THIS
	// session's own claimed mail and clear its latch regardless (claude-code
	// review, PR #89: the live roster id "sonnet" is exactly such an id, and
	// the old unconditional-abort-on-error fully wedged it). Only a genuine
	// identity-resolution failure — no id could be determined at all, so
	// resolvedID comes back empty — leaves nothing usable to proceed with.
	resolvedID, _, repairErr := selfRepairRegistration(cfg, &opts, agentID, vendor, "turn-end", now)
	if resolvedID == "" {
		return repairErr
	}
	agentID = resolvedID
	if repairErr != nil {
		fmt.Fprintf(os.Stderr, "warning: registration self-repair failed (continuing so this session's own claimed mail still commits): %v\n", repairErr)
	}

	// STEP 1: archive .claimed UNLESS THIS invocation is itself guard-armed
	// (session != "") AND a latch exists, reads back successfully, AND
	// belongs to a DIFFERENT session (the gemini crash / dead-latch case:
	// preserve that residue for check's own redelivery banner). Guard
	// disabled for this process (no serve token, or a hand-run session the
	// hooks never armed) always archives UNCONDITIONALLY, regardless of any
	// latch a PAST guarded session may have left behind — codex review, PR
	// #89 round 3: comparing a stale latch's session against session=="" made
	// EVERY unguarded/no-token turn-end call after a crashed guarded run
	// treat that stale latch as "foreign", withholding the commit forever
	// (step 2 also never clears it when session==""), reintroducing exactly
	// the same-vs-hand-run divergence A7 promised never to have. A latch
	// that fails to read at all (absent OR corrupt) is likewise never a
	// reason to withhold the commit within the armed branch (findings #1 and
	// #4 from round 2).
	session := resolveGuardSession()
	archive := true
	if session != "" {
		if latch, lerr := loop.ReadGuardLatch(cfg, agentID); lerr == nil && latch.Session != session {
			archive = false
		}
	}

	var acked []ackItem
	if archive {
		acked, err = archiveAllClaimed(cfg, agentID, now)
		if err != nil {
			return err
		}
	}

	// STEP 2: clear own-session latch. No-op if none/foreign/corrupt
	// (ClearGuardLatch fails open on a read it cannot make sense of).
	if session != "" {
		if err := loop.ClearGuardLatch(cfg, agentID, session); err != nil {
			return fmt.Errorf("clear guard latch: %w", err)
		}
	}

	// STEP 3.
	status, err := evaluateGate(cfg, agentID, gatePhaseFinish, false, false, now)
	if err != nil {
		return err
	}

	if codexHook == "Stop" {
		// Identical contract to gate.go's own codex Stop path: silent + exit 0
		// on clear, block-JSON + exit 0 on block (codex reads the JSON, not
		// the exit code, for this event).
		return emitGateCodexStop(status)
	}
	if jsonOut {
		if err := emitTurnEndJSON(status, acked); err != nil {
			return err
		}
	} else {
		emitTurnEndText(status, acked)
	}

	if status.Blocked {
		return errBlocked
	}
	return nil
}

// turnEndJSON is turn-end's --json shape: the same gateStatus fields
// `agentchute gate --json` emits, plus the archive commit this call made.
type turnEndJSON struct {
	gateStatus
	Archived []ackItem `json:"archived,omitempty"`
}

func emitTurnEndJSON(status gateStatus, acked []ackItem) error {
	out := turnEndJSON{gateStatus: status, Archived: acked}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func emitTurnEndText(status gateStatus, acked []ackItem) {
	if len(acked) == 0 {
		fmt.Println("(nothing to archive)")
	} else {
		for _, a := range acked {
			fmt.Printf("acked %s -> %s\n", a.Filename, a.ArchivePath)
		}
	}
	emitGateText(status)
}

func turnEndUsage(err error) error {
	if err == flag.ErrHelp {
		return turnEndHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, turnEndHelp())
}

func turnEndHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, turnEndHelp())
}

func turnEndHelp() string {
	return strings.TrimSpace(`
Usage: agentchute turn-end --as <id> --vendor <vendor> [flags]

Ordered end-of-turn handler (v2.5 plan A7/C24): registration self-repair, then
archives THIS session's own claimed mail (only if its guard latch is set),
clears that latch, then evaluates + emits the finish gate — replacing the
separate self-check + ack --quiet + gate --before finish hook entries (whose
relative order codex's concurrent hook execution made unenforceable).

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin (anthropic, openai, google, xai, local)
  --host <name>         host (defaults to OS hostname)
  --bio <text>          short self-description
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output
  --codex-hook <event>  codex hook JSON shape (Stop)
`)
}
