package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const usage = `agentchute — pull-only, inbox-based agent coordination via markdown files (senders write inboxes; nobody pokes).

Usage:
  agentchute <command> [flags]

Commands:
  setup          one-command control-repo setup; installs the runner wake path (the only supported path)
  init           scaffold a project for agentchute (writes AGENTCHUTE.md, loop dirs, enrollment blocks)
  serve          launch a wrapper under the PTY runner (serve lease + inbox polling + check-inbox injection; pull-only, no wake socket)
  send           send a message from one agent to another
  check          claim + display messages addressed to me (at-least-once; run ack to commit)
  ack            commit messages claimed by check (archive the .claimed residue)
  status         print registry overview, inbox depths, and .live presence freshness
  doctor         diagnostic aggregator: scaffold, hook content, registration, inbox, .live presence

Advanced / internal (mostly hook- or setup-driven; run 'agentchute <cmd> --help' for any):
  boot · register · gate · pending · poller · self-check · hooks · shims · prepare-pool · identity · update

Run 'agentchute <command> --help' for command-specific flags.
See AGENTCHUTE.md for the full spec.
`

// version is the binary version. Root main.go injects the ldflag-set value via
// Assets.Version at startup; it defaults to "dev" for `go test` and local runs.
var version = "dev"

// errBlocked is the canonical "lifecycle gate blocked" sentinel for v0.1.1.
// Returned by `boot` (interactive mode, when unread mail exists) and `gate`
// (when --before <phase> finds an obligation). Mapped to exit code 2 by Main,
// matching codex Stop-hook and gemini emergency-brake conventions. Distinct
// from errFailIfAny which is `pending`-specific.
var errBlocked = fmt.Errorf("agentchute: lifecycle gate blocked")

// Assets carries the build-time embedded content the CLI depends on. The
// embeds live in the root main package because //go:embed cannot reference a
// parent directory, so root main.go injects them here at startup. The values
// back the package-level vars the command handlers read (embeddedSpecContent,
// enrollmentWrapperTemplate, enrollmentAgentsTemplate, hooksFS, version).
type Assets struct {
	Version         string
	Spec            string // AGENTCHUTE.md
	WrapperTemplate string // templates/enrollment/wrapper.md
	AgentsTemplate  string // templates/enrollment/agents.md
	Hooks           fs.FS  // //go:embed all:examples/hooks (root layout: examples/hooks/...)
}

// Main is the CLI entrypoint. Root main.go injects the embedded assets plus the
// process argv (os.Args[1:], so args[0] is the subcommand) and exits with the
// returned code. Behavior is identical to the historical root main(); the only
// change is returning an int exit code instead of calling os.Exit directly.
func Main(a Assets, args []string) int {
	// Fail loudly if the build-time assets were not injected: writing an empty
	// spec/template or a nil hooks FS would silently corrupt `init`/`hooks`.
	if a.Spec == "" || a.WrapperTemplate == "" || a.AgentsTemplate == "" || a.Hooks == nil {
		fmt.Fprintln(os.Stderr, "agentchute: internal error: build-time assets not injected (spec/templates/hooks missing); refusing to run")
		return 1
	}
	version = a.Version
	embeddedSpecContent = a.Spec
	enrollmentWrapperTemplate = a.WrapperTemplate
	enrollmentAgentsTemplate = a.AgentsTemplate
	hooksFS = a.Hooks

	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd := args[0]
	cmdArgs := args[1:]

	var err error
	switch cmd {
	case "-v", "--version", "version":
		fmt.Printf("agentchute %s\n", version)
		return 0
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "ac", "dispatch":
		// The `ac` launcher/dispatcher front door (Gate 1). The installed `ac`
		// script execs `agentchute dispatch -- "$@"`.
		err = cmdDispatch(cmdArgs)
	default:
		if h, ok := commandHandlers[cmd]; ok {
			err = h(cmdArgs)
		} else {
			fmt.Fprintf(os.Stderr, "agentchute: unknown command %q\n\n%s", cmd, usage)
			return 2
		}
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Subcommand --help: print the usage portion (skip the wrapping
			// "flag: help requested" prefix) and exit 0, not 1.
			msg := err.Error()
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[i+1:]
			}
			fmt.Println(msg)
			return 0
		}
		// Exit code 2 for lifecycle-gate sentinels in ordinary text/--json
		// modes. Hook-envelope modes such as --codex-hook Stop return nil and
		// carry block/allow in their JSON payload. Exit code 1 is reserved for
		// actual command failures.
		if err == errFailIfAny || err == errBlocked {
			return 2
		}
		fmt.Fprintf(os.Stderr, "agentchute %s: %v\n", cmd, err)
		return 1
	}
	return 0
}
