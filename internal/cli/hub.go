package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func cmdHub(args []string) error {
	if len(args) == 0 {
		return hubUsage(fmt.Errorf("expected hub subcommand: join, authorize, or session"))
	}
	switch args[0] {
	case "join":
		return cmdHubJoin(args[1:])
	case "authorize":
		return cmdHubAuthorize(args[1:])
	case "session":
		return cmdHubSession(args[1:])
	case "-h", "--help", "help":
		return hubUsage(flag.ErrHelp)
	default:
		return hubUsage(fmt.Errorf("unknown hub subcommand %q", args[0]))
	}
}

func cmdHubSession(args []string) error {
	fs := flag.NewFlagSet("hub session", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var agent, pool, poolID string
	fs.StringVar(&agent, "agent", "", "agent id pinned by the authorized key")
	fs.StringVar(&pool, "pool", "", "absolute hub control-repo path")
	fs.StringVar(&poolID, "pool-id", "", "pool identity pinned by the authorized key")
	if err := fs.Parse(args); err != nil {
		return hubSessionUsage(err)
	}
	if fs.NArg() != 0 {
		return hubSessionUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if agent == "" || pool == "" || poolID == "" {
		return hubSessionUsage(fmt.Errorf("--agent, --pool, and --pool-id are required"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	transport := &stdioHubTransport{in: os.Stdin, out: os.Stdout}
	return serveHubSession(ctx, transport, hubSessionOptions{
		Agent:  agent,
		Pool:   pool,
		PoolID: poolID,
		HubBin: version,
	})
}

func hubUsage(err error) error {
	return fmt.Errorf("%w\n\nUsage:\n  agentchute hub join ssh://[user@]host[:port]/abs/path/to/pool (--name <local-name> | --as <agent-id>)\n  agentchute hub authorize [flags]\n  agentchute hub session --agent <id> --pool <absolute-path> --pool-id <12-hex>", err)
}

func hubSessionUsage(err error) error {
	return fmt.Errorf("%w\n\nUsage:\n  agentchute hub session --agent <id> --pool <absolute-path> --pool-id <12-hex>", err)
}
