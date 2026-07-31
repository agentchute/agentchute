package cli

import (
	"flag"
	"fmt"
	"io"
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

	id, err := resolveAgentID(agentID)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func identityUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute identity [--as <id>] [--vendor <v> | --wrapper <name>] [--control-repo <path>] [--loop-dir <path>]", err)
}
