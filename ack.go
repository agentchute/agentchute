package main

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
// CLEAR-THEN-COMMIT (the load-bearing contract): ack archives ONLY IF the finish
// gate is clear (finishGateClear: unread / malformed / pending-reply / corrupt
// ledger / missing registration). If finish would BLOCK, ack archives NOTHING and
// leaves .claimed intact, so a gate-blocked turn still RE-DELIVERS its claimed
// mail next turn (at-least-once is preserved across the block). This is why the
// Stop/BeforeAgent hooks can run `ack` alongside `gate --before finish` in any
// order: ack self-gates, so claimed mail is never committed past a blocked finish.
// The agent's OWN claimed/owed state is NON-BLOCKING in the finish gate, so ack
// can always commit its just-claimed mail once unrelated blockers clear.
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

	// CLEAR-THEN-COMMIT: evaluate the finish gate FIRST. If it is not clear,
	// archive NOTHING and leave the residue uncommitted (re-delivered next turn).
	// Read-only and reuses gate.go's exact blocking predicate, so ack and
	// `gate --before finish` can never disagree.
	clear, reasons, err := finishGateClear(cfg, agentID, now)
	if err != nil {
		return fmt.Errorf("evaluate finish gate: %w", err)
	}
	if !clear {
		return emitAckNotClear(agentID, len(residue), reasons, jsonOut, quiet)
	}

	acked := make([]ackItem, 0, len(residue))
	for _, msg := range residue {
		dest, err := loop.ArchiveMessage(msg, cfg.ArchiveDir(), agentID, now)
		if err != nil {
			return fmt.Errorf("ack (archive) %s: %w", msg.Filename, err)
		}
		acked = append(acked, ackItem{Filename: msg.Filename, ArchivePath: dest})
	}

	if jsonOut {
		return emitAckJSON(ackResult{Agent: agentID, Count: len(acked), Acked: acked, GateClear: true})
	}
	if quiet {
		return nil
	}
	if len(acked) == 0 {
		fmt.Println("(nothing to ack)")
		return nil
	}
	for _, a := range acked {
		fmt.Printf("acked %s -> %s\n", a.Filename, a.ArchivePath)
	}
	return nil
}

// emitAckNotClear reports a CLEAR-THEN-COMMIT abort: the finish gate is not clear,
// so ack archived nothing and the claimed residue stays for redelivery. It is NOT
// an error and returns nil (exit 0): the paired `gate --before finish` hook step
// emits the authoritative block signal (exit 2 / decision JSON); ack's only job
// here is to refuse to commit early — erroring would break the hook chain.
func emitAckNotClear(agentID string, claimedCount int, reasons []string, jsonOut, quiet bool) error {
	if jsonOut {
		return emitAckJSON(ackResult{
			Agent:        agentID,
			Count:        0,
			Acked:        []ackItem{},
			GateClear:    false,
			NotAcked:     claimedCount,
			BlockReasons: reasons,
		})
	}
	if quiet {
		return nil
	}
	fmt.Printf("finish gate not clear; %d claimed message(s) left uncommitted\n", claimedCount)
	for _, r := range reasons {
		fmt.Printf("  - %s\n", r)
	}
	return nil
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
	NotAcked     int       `json:"not_acked,omitempty"`     // claimed residue left uncommitted because finish was not clear
	BlockReasons []string  `json:"block_reasons,omitempty"` // the finish-gate blocking reasons (when gate_clear=false)
}

func emitAckJSON(r ackResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func ackUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute ack [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--json] [--quiet]\n  ack COMMITS messages that `check` claimed: archives inbox/<id>/.claimed residue.", err)
}
