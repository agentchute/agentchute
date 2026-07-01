package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/agentchute/agentchute/internal/loop"
)

func cmdIdentity(args []string) error {
	fs := flag.NewFlagSet("identity", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, wrapper, controlRepo, loopDir string
	fs.StringVar(&agentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&wrapper, "wrapper", "", "wrapper command/key (claude-code, codex, gemini-cli, grok)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path")

	if err := fs.Parse(args); err != nil {
		return identityUsage(err)
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
	// If discovery fails, we still try to resolve the ID without cfg (no conflict check)
	if err != nil {
		cfg = nil
	}

	if vendor == "" {
		vendor = wrapper
	}
	id, err := resolveAgentID(agentID, vendor, cfg)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func identityUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute identity [--as <id>] [--vendor <v> | --wrapper <name>] [--control-repo <path>] [--loop-dir <path>]", err)
}
