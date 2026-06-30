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

	acked := make([]ackItem, 0, len(residue))
	for _, msg := range residue {
		dest, err := loop.ArchiveMessage(msg, cfg.ArchiveDir(), agentID, now)
		if err != nil {
			return fmt.Errorf("ack (archive) %s: %w", msg.Filename, err)
		}
		acked = append(acked, ackItem{Filename: msg.Filename, ArchivePath: dest})
	}

	if jsonOut {
		return emitAckJSON(ackResult{Agent: agentID, Count: len(acked), Acked: acked})
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

type ackItem struct {
	Filename    string `json:"filename"`
	ArchivePath string `json:"archive_path"`
}

type ackResult struct {
	Agent string    `json:"agent"`
	Count int       `json:"count"`
	Acked []ackItem `json:"acked"`
}

func emitAckJSON(r ackResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func ackUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute ack [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--json] [--quiet]\n  ack COMMITS messages that `check` claimed: archives inbox/<id>/.claimed residue.", err)
}
