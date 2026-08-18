package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/agentchute/agentchute/internal/hubclient"
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

	var cfg *loop.Config
	if cwd, err := os.Getwd(); err == nil {
		cfg, _ = discoverConfig(loop.DiscoverOpts{
			ControlRepoFlag: controlRepo,
			LoopDirFlag:     loopDir,
			Cwd:             cwd,
			EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
			EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
		})
	}
	id, err := resolveAgentID(agentID, cfg)
	if err != nil {
		return err
	}
	if cfg == nil || cfg.Remote == nil {
		fmt.Println(id)
		return nil
	}
	hubCfg, err := hubclient.ReadHubConfig(cfg.Remote.HubID)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(hubCfg.Names))
	for name := range hubCfg.Names {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s -> %s\n", name, hubCfg.Names[name])
	}
	fmt.Printf("resolved: %s\n", id)
	return nil
}

func identityUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute identity [--as <id>] [--vendor <v> | --wrapper <name>] [--control-repo <path>] [--loop-dir <path>]", err)
}
