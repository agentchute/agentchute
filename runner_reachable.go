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
func runnerReachableForRecipient(cfg *loop.Config, reg *loop.Registration, timeout time.Duration) bool {
	if reg == nil || reg.WakeMethod != loop.RunnerWakeMethod {
		return false
	}
	if cfg == nil {
		return false
	}
	if err := cfg.RunnerWakeTargetOwnedBy(reg.AgentID, reg.WakeTarget); err != nil {
		// Not a socket this recipient owns: never dial it.
		return false
	}
	return loop.RunnerSocketReachable(reg.WakeTarget, timeout)
}
