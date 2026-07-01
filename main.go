// agentchute — pull-only, inbox-based agent coordination via markdown files.
// Senders only write a recipient's inbox; nobody pokes a recipient. A loopless
// wrapper is supervised by the runner (`agentchute serve`), a per-agent PTY
// supervisor that polls the agent's own inbox and injects a `check inbox` cue
// (see AGENTCHUTE.md §8).
//
// See AGENTCHUTE.md (at repo root) for the full spec. This binary is the
// reference implementation of the optional CLI sketched in the spec. The
// protocol itself does not require this CLI; two agents can coordinate using
// nothing more than `ln`/`mv` over a shared inbox if they follow the spec.
package main

import (
	"errors"
	"flag"
	"fmt"
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

var version = "dev"

// errBlocked is the canonical "lifecycle gate blocked" sentinel for v0.1.1.
// Returned by `boot` (interactive mode, when unread mail exists) and `gate`
// (when --before <phase> finds an obligation). Mapped to exit code 2 by main,
// matching codex Stop-hook and gemini emergency-brake conventions. Distinct
// from errFailIfAny which is `pending`-specific.
var errBlocked = fmt.Errorf("agentchute: lifecycle gate blocked")

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "-v", "--version", "version":
		fmt.Printf("agentchute %s\n", version)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	case "ac", "dispatch":
		// The `ac` launcher/dispatcher front door (Gate 1). The installed `ac`
		// script execs `agentchute dispatch -- "$@"`.
		err = cmdDispatch(args)
	default:
		if h, ok := commandHandlers[cmd]; ok {
			err = h(args)
		} else {
			fmt.Fprintf(os.Stderr, "agentchute: unknown command %q\n\n%s", cmd, usage)
			os.Exit(2)
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
			return
		}
		// Exit code 2 for lifecycle-gate sentinels in ordinary text/--json
		// modes. Hook-envelope modes such as --codex-hook Stop return nil and
		// carry block/allow in their JSON payload. Exit code 1 is reserved for
		// actual command failures.
		if err == errFailIfAny || err == errBlocked {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "agentchute %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
