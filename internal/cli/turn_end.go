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
//  1. archive `.claimed` ONLY when the CURRENT session holds this agent's own
//     guard latch (C23). A dead/foreign latch, or no latch at all (guard
//     disabled, or nothing claimed under this session yet), leaves the
//     residue untouched — it survives for `check`'s own redelivery banner.
//     `ack` remains the archiving path for humans and unguarded lanes.
//  2. clear the own-session guard latch (no-op if none/foreign).
//  3. evaluate + emit the finish gate via the EXACT SAME contracts as
//     `agentchute gate --before finish` (gate.go): default text, --json, and
//     --codex-hook Stop (silent on clear, block-JSON exit-0 on block).
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

	opts := registerOpts{Host: host, Bio: bio}
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

	// STEP 0.
	agentID, _, err = selfRepairRegistration(cfg, &opts, agentID, vendor, "turn-end", now)
	if err != nil {
		return err
	}

	// STEP 1 + 2: resolve whether THIS session currently holds agentID's own
	// guard latch. Resolved once and reused for both steps so a latch that
	// disappears mid-function (it can't — we hold no lock across steps, but
	// the read is authoritative at the moment we act) can't desync archive
	// from clear.
	session := resolveGuardSession()
	ownLatch := false
	if session != "" {
		if latch, lerr := loop.ReadGuardLatch(cfg, agentID); lerr == nil && latch.Session == session {
			ownLatch = true
		}
	}

	var acked []ackItem
	if ownLatch {
		acked, err = archiveAllClaimed(cfg, agentID, now)
		if err != nil {
			return err
		}
	}

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
