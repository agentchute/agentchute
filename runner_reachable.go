package main

import (
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// runnerReachableForRecipient reports whether a registration's runner wake
// socket answers the ping/ack protocol, WITHOUT ever dialing a socket the
// recipient does not legitimately own.
//
// Recipient-binding before dial: a peer-controlled registration can name any
// path (e.g. unix:/tmp/evil.sock). Probing reachability by dialing that path
// would connect this process to an attacker-controlled socket. So for the
// runner wake method we first require cfg.RunnerWakeTargetOwnedBy to pass; an
// unowned target is reported UNREACHABLE without a dial. For a legitimate self
// registration the owned-check passes and the dial proceeds as before; if the
// reg was tampered the check is protective.
//
// This is the recipient-bound counterpart of loop.RunnerSocketReachable and is
// the only path registration-driven reachability probes should use. Non-runner
// wake methods are out of scope for this socket-specific helper and report
// unreachable here (callers handle other methods separately).
//
// The owned-check-before-dial logic now lives behind the WakeAdapter interface
// (runnerWakeAdapter.Reachable). This helper delegates to the dispatcher so
// there is ONE place the rule lives; the WakeMethod guard preserves the prior
// "non-runner ⇒ unreachable here" contract that this socket-specific helper
// promised its callers (the dispatcher itself would otherwise route a tmux/herdr
// reg to its own adapter).
func runnerReachableForRecipient(cfg *loop.Config, reg *loop.Registration, timeout time.Duration) bool {
	if reg == nil || reg.WakeMethod != loop.RunnerWakeMethod {
		return false
	}
	return loop.RegistrationReachable(cfg, reg, timeout)
}

func herdrAgentPaneID(name string) (string, bool) {
	info, found := herdrAgentByName(name)
	if !found {
		return "", false
	}
	return info.PaneID, true
}

// init wires the concrete tmux/herdr probes (which live in this root package and
// shell out via package-level binary vars) into the loop package's wake
// dispatcher. The loop-package tmux/herdr adapters cannot call these directly
// without an import cycle (loop must not import main), so they call the injected
// hooks. The runner adapter needs no hook — its owned-check and dial both
// already live in loop.
func init() {
	loop.SetTmuxReachableHook(tmuxTargetReachableWithin)
	loop.SetHerdrReachableHook(herdrAgentReachableWithin)
	loop.SetHerdrPaneResolverHook(herdrAgentPaneID)
}
