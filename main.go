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
  boot           session-start ritual: register + peek inbox + pending-reply summary (use in SessionStart hooks)
  gate           lifecycle gate: block declaring done while inbox/replies are outstanding
  defer          explicitly defer a pending-reply obligation (clears the gate; notifies sender)
  send           send a message from one agent to another
  check          consume and archive messages addressed to me
  pending        peek unread messages (read-only; safe for lifecycle hooks)
  self-check     refresh own registration/last_seen and reconcile wake target
  self-poll      "should I wake the wrapper?" — side-effect-free helper for schedulers and launch prompts
  status         print registry overview, inbox depths, and last_seen freshness
  doctor         diagnostic aggregator: scaffold, hook content, registration, ledger, wake target
  watch          recipient-side persistent watcher: fire OS notification / print / exec on new mail
  watchdog       run liveness daemon (§10.1); pokes peers with stale inboxes
  hooks          install canonical hook templates into .claude/ / .codex/ / .gemini/ (v0.2.1)

Run 'agentchute <command> --help' for command-specific flags.
See AGENTCHUTE.md for the full spec.
`

var version = "dev"

// errBlocked is the canonical "lifecycle gate blocked" sentinel for v0.1.1.
// Returned by `boot` (interactive mode, when unread mail or pending replies
// exist) and `gate` (when --before <phase> finds an obligation). Mapped to
// exit code 2 by main, matching codex Stop-hook and gemini emergency-brake
// conventions. Distinct from errFailIfAny which is `pending`-specific.
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
	case "init":
		err = cmdInit(args)
	case "prepare-pool":
		err = cmdPreparePool(args)
	case "register":
		err = cmdRegister(args)
	case "boot":
		err = cmdBoot(args)
	case "gate":
		err = cmdGate(args)
	case "defer":
		err = cmdDefer(args)
	case "send":
		err = cmdSend(args)
	case "check":
		err = cmdCheck(args)
	case "pending":
		err = cmdPending(args)
	case "self-check":
		err = cmdSelfCheck(args)
	case "self-poll":
		err = cmdSelfPoll(args)
	case "status":
		err = cmdStatus(args)
	case "doctor":
		err = cmdDoctor(args)
	case "watch":
		err = cmdWatch(args)
	case "watchdog":
		err = cmdWatchdog(args)
	case "hooks":
		err = cmdHooks(args)
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
		// Exit code 2 for the lifecycle-gate sentinels (canonical "blocked"
		// signal honored by codex Stop hooks and gemini blocking surfaces).
		// Exit code 1 reserved for actual command failures.
		if err == errFailIfAny || err == errBlocked {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "agentchute %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
