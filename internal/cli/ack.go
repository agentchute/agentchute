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

// cmdAck is phase 2 of the two-phase consume (Gate 5): the COMMIT. It archives
// every message that `check` CLAIMED (moved into inbox/<id>/.claimed) but did not
// yet commit. Archiving is the single commit point; a crash before ack leaves the
// residue in .claimed so the next `check` RE-DELIVERS it (at-least-once).
//
// UNCONDITIONAL COMMIT (v0.10.1, replaces the old CLEAR-THEN-COMMIT contract):
// ack ALWAYS archives .claimed, regardless of finish-gate status. Archiving is
// the caller's OWN state (mail it already claimed and acted on); it is not pool
// hygiene, and gating it on the finish gate meant an UNRELATED blocker — e.g. a
// third party dropping a new malformed/unread file into this agent's inbox
// between `check` and `ack` — delayed committing mail this agent had already
// correctly handled. The finish gate still independently blocks `finish`: ack
// evaluates it AFTER committing (so reported reasons reflect the post-commit
// state) purely to REPORT whether other obligations remain, never to withhold
// the archive.
//
// EXIT CODE distinguishes the two outcomes so scripts never mistake "committed"
// for "done" — in DEFAULT mode only: gate clear after commit => nil (exit 0).
// Gate still blocked after commit (unrelated obligations remain) => errBlocked
// (exit 2), same sentinel `gate --before finish` returns, so `ack`'s exit code
// alone tells a caller whether it's safe to treat the turn as finished. A
// blocked exit is reported via text/JSON exactly like an already-committed-
// and-still-blocked state; it is NOT a command failure (exit 1) — the commit
// itself always succeeds or `ack` returns a real error.
//
// --quiet is HOOK MODE, not just "less output": it suppresses BOTH the
// text/JSON report AND the exit-2 signal (always returns nil once the commit
// itself succeeds). Its one documented consumer is the Stop-hook commit step,
// where `ack --quiet` runs as its OWN hook entry immediately before
// `gate --before finish --json` in the same hook list. A Stop hook that exits
// 2 is itself a block signal to the harness; if `ack --quiet` also returned
// errBlocked, a blocked turn would raise it TWICE — once from ack with NO
// reason (quiet swallowed the text), once from gate with the real reasons —
// a confusing, reason-less duplicate block. In hook mode `gate` is the sole
// authoritative block signal; `ack --quiet`'s job is only to commit silently.
// Callers that want the committed-vs-done distinction use the DEFAULT
// (non-quiet) mode and read the exit code / `gate_clear` JSON field.
//
// Idempotent: an already-archived message (e.g. a partial prior ack) is treated
// as success, and an empty .claimed is a no-op. Flags mirror `check`
// (--as/--vendor/--json) plus --quiet for the Stop-hook commit step.
func cmdAck(args []string) error {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, controlRepo, loopDir string
	var jsonOut, quiet bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.BoolVar(&quiet, "quiet", false, "suppress non-error output")

	if err := fs.Parse(args); err != nil {
		return ackUsage(err)
	}
	if fs.NArg() != 0 {
		return ackUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
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

	now := time.Now().UTC()

	claimedDir := cfg.AgentClaimedDir(agentID)
	residue, err := loop.ListClaimedMessages(claimedDir)
	if err != nil {
		return fmt.Errorf("list claimed residue: %w", err)
	}

	// UNCONDITIONAL COMMIT (F7): archive every claimed message regardless of
	// finish-gate status — see the doc comment above cmdAck.
	acked := make([]ackItem, 0, len(residue))
	for _, msg := range residue {
		dest, err := loop.ArchiveMessage(msg, cfg.ArchiveDir(), agentID, now)
		if err != nil {
			return fmt.Errorf("ack (archive) %s: %w", msg.Filename, err)
		}
		acked = append(acked, ackItem{Filename: msg.Filename, ArchivePath: dest})
	}

	// Evaluate the finish gate AFTER committing, purely to REPORT whether other
	// obligations remain. Read-only; reuses gate.go's exact blocking predicate,
	// so ack and `gate --before finish` can never disagree about whether finish
	// would block. Post-commit means the reported reasons never stale-cite the
	// batch we just archived (gate.go's own ClaimedResidue warning drops to 0).
	clear, reasons, err := finishGateClear(cfg, agentID, now)
	if err != nil {
		return fmt.Errorf("evaluate finish gate: %w", err)
	}

	result := ackResult{Agent: agentID, Count: len(acked), Acked: acked, GateClear: clear}
	if !clear {
		result.BlockReasons = reasons
	}

	if quiet {
		// Hook mode: the commit already happened above; suppress both the
		// report and the exit-2 signal so a paired `gate --before finish` hook
		// entry remains the sole authoritative block signal (see doc comment).
		return nil
	}

	if jsonOut {
		if err := emitAckJSON(result); err != nil {
			return err
		}
	} else {
		emitAckText(result)
	}

	// Exit code carries the distinction JSON/text already reported: clear => the
	// commit is fully done; blocked => committed, but do not treat this turn as
	// finished (same sentinel/exit-2 contract as `gate --before finish`).
	if !clear {
		return errBlocked
	}
	return nil
}

// emitAckText prints the human-readable ack outcome: the committed items (or
// "(nothing to ack)"), followed by any remaining finish-gate blockers.
func emitAckText(r ackResult) {
	if len(r.Acked) == 0 {
		fmt.Println("(nothing to ack)")
	} else {
		for _, a := range r.Acked {
			fmt.Printf("acked %s -> %s\n", a.Filename, a.ArchivePath)
		}
	}
	if !r.GateClear {
		fmt.Println("finish gate still blocked after commit:")
		for _, reason := range r.BlockReasons {
			fmt.Printf("  - %s\n", reason)
		}
	}
}

type ackItem struct {
	Filename    string `json:"filename"`
	ArchivePath string `json:"archive_path"`
}

type ackResult struct {
	Agent        string    `json:"agent"`
	Count        int       `json:"count"`
	Acked        []ackItem `json:"acked"`
	GateClear    bool      `json:"gate_clear"`
	BlockReasons []string  `json:"block_reasons,omitempty"` // remaining finish-gate reasons; set only when gate_clear=false (committing is unconditional, so these are no longer about THIS ack's own residue)
}

func emitAckJSON(r ackResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func ackUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute ack [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--json] [--quiet]\n  ack COMMITS messages that `check` claimed: archives inbox/<id>/.claimed residue, unconditionally.\n  Default mode: exit 0 if the finish gate is then clear; exit 2 (same sentinel as `gate --before\n  finish`) if other obligations still block finish — the commit itself already happened either way.\n  --quiet is hook mode: suppresses output AND always exits 0 once the commit succeeds, so a paired\n  `gate --before finish` hook entry remains the sole authoritative block signal.", err)
}
