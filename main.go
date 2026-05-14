// agentchute — inbox-based agent coordination via markdown files + pluggable
// wake adapters. The v0.1 reference adapter is tmux (see AGENTCHUTE.md §8).
//
// See AGENTCHUTE.md (at repo root) for the full spec. This binary is the
// reference implementation of the optional CLI sketched in the spec. The
// protocol itself does not require this CLI; two agents can coordinate
// using nothing more than `mv` and a wake poke if they follow the spec.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

const usage = `agentchute — inbox-based agent coordination via markdown files + pluggable wake adapters.

Usage:
  agentchute <command> [flags]

Commands:
  init           scaffold a project for agentchute (writes AGENTCHUTE.md, loop dirs, enrollment blocks)
  prepare-pool   prepare one or more folders as pool participants (writes pointer file + enrollment blocks)
  register       create or update a live registration for an agent
  send           send a message from one agent to another
  check          consume and archive messages addressed to me
  status         print registry overview, inbox depths, and last_seen freshness
  watchdog       run liveness daemon (§10.1); pokes peers with stale inboxes

Run 'agentchute <command> --help' for command-specific flags.
See AGENTCHUTE.md for the full spec.
`

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "prepare-pool":
		err = cmdPreparePool(args)
	case "register":
		err = cmdRegister(args)
	case "send":
		err = cmdSend(args)
	case "check":
		err = cmdCheck(args)
	case "status":
		err = cmdStatus(args)
	case "watchdog":
		err = cmdWatchdog(args)
	case "-v", "--version", "version":
		fmt.Printf("agentchute %s\n", version)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "agentchute: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
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
		fmt.Fprintf(os.Stderr, "agentchute %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
