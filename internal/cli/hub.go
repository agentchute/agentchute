package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agentchute/agentchute/internal/hubwire"
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

	// The pinning check lives HERE, at the CLI seam, and deliberately not inside
	// serveHubSession. Both this path and the exported ServeHubSession
	// conformance entry call serveHubSession, and the M3/M6 in-process drivers
	// have no sshd and no SSH_ORIGINAL_COMMAND — an env read down there would
	// refuse them. Up here they are unaffected BY CONSTRUCTION rather than by a
	// conditional that could be got wrong.
	//
	// After the transport, not before it, so the refusal still writes a real
	// error frame instead of leaving a client with a dead pipe.
	if !reachedByForcedCommand() {
		return refuseUnpinnedHubSession(transport)
	}
	return serveHubSession(ctx, transport, hubSessionOptions{
		Agent:  agent,
		Pool:   pool,
		PoolID: poolID,
		HubBin: version,
	})
}

// hubSessionEnv is the environment reader, as a package var so rows can drive
// both arms without mutating the process environment.
var hubSessionEnv = os.LookupEnv

// reachedByForcedCommand is the entire hub-side predicate:
//
//	SSH_ORIGINAL_COMMAND present AND non-empty
//
// sshd sets it to the command the CLIENT asked for whenever an authorized_keys
// forced command overrides it. No forced command, no override, no variable — so
// its presence is the evidence that sshd pinned this session's --agent and
// --pool rather than the caller choosing them.
//
// Empty counts as absent, and that is not defensive: both real producers show
// EMPTY. An ssh-intercepting layer never consults authorized_keys at all, and an
// operator-key fallback authenticates on an unrestricted line; measured, both
// arrive with the variable set to "".
//
// It deliberately does NOT also check SSH_CONNECTION. A second term can only add
// false refusals — an sshd or interceptor that omits it — and cannot make a
// true-accept safer, because neither variable is a security boundary: a caller
// who can set the environment has already lost the pinning. Fewer terms, fewer
// ways to refuse a healthy hub.
//
// Accepted false-refuse: a client that opens a session with NO requested command
// gets the forced command with the variable unset, and is refused. Correct for
// agentchute — BuildSSHInvocation always appends the literal `agentchute-hub`,
// which AGENTCHUTE.md fixes as the carriage contract — and the refusal text says
// so, for anyone writing a third-party client.
func reachedByForcedCommand() bool {
	value, present := hubSessionEnv("SSH_ORIGINAL_COMMAND")
	return present && value != ""
}

// refuseUnpinnedHubSession writes the error frame AND a plain-language line to
// stderr, then returns non-nil so the process exits non-zero.
//
// That is a deliberate divergence from validateHubPool, which writes a frame and
// returns nil. On an unpinned host the caller is often not a hubwire client at
// all — an operator or a script that typed `agentchute hub session` over an
// intercepted login — and for them a frame on stdout is invisible while stderr
// and a non-zero exit are not. It costs a real client nothing: it has already
// read the error frame at hello time, so classifySSHFailure never runs and the
// exit status is never consulted.
func refuseUnpinnedHubSession(transport hubSessionTransport) error {
	defer func() { _ = transport.Close() }()
	frame := &hubwire.ProtocolError{Code: hubwire.CodeUnpinned, Msg: hubUnpinnedMessage()}
	_ = hubwire.NewWriter(transport).Write(hubwire.Error{
		ResponseBase: hubwire.ResponseBase{T: "error", Re: 0},
		Code:         frame.Code,
		Msg:          frame.Msg,
	}, nil)
	fmt.Fprintln(os.Stderr, "agentchute hub session: "+frame.Msg)
	return errors.New("hub session: refused, this hub did not apply an authorized_keys forced command")
}

func hubUsage(err error) error {
	return fmt.Errorf("%w\n\nUsage:\n  agentchute hub join ssh://[user@]host[:port]/abs/path/to/pool (--name <local-name> | --as <agent-id>)\n  agentchute hub authorize [flags]\n  agentchute hub session --agent <id> --pool <absolute-path> --pool-id <12-hex>", err)
}

func hubSessionUsage(err error) error {
	return fmt.Errorf("%w\n\nUsage:\n  agentchute hub session --agent <id> --pool <absolute-path> --pool-id <12-hex>", err)
}
