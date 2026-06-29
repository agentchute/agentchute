package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

type recipientLiveness struct {
	OK      bool
	Via     string
	Message string
}

func evaluateRecipientLiveness(cfg *loop.Config, agentID string, now time.Time) recipientLiveness {
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
	if err != nil {
		return recipientLiveness{OK: false, Via: "skip", Message: "registration unreadable"}
	}
	// WI-E2 advisory cache fast-path: a VALID cached reachability fact
	// (endpoint-bound, within TTL) proves liveness without a live probe — this is
	// what lets a self-healed lane read as live even if a transient live probe
	// would currently miss. On a cache MISS (absent / expired / endpoint changed,
	// including EVERY pre-upgrade registration with no ReachableAt) we FALL
	// THROUGH to the live wake / session / poller checks below; a miss is NEVER
	// treated as "unreachable" (codex backward-compat guardrail).
	if reg.IsReachable(now, recipientReachabilityTTL) {
		age := now.UTC().Sub(reg.ReachableAt.UTC())
		if age < 0 {
			age = 0
		}
		return recipientLiveness{
			OK:      true,
			Via:     "reachable-cache",
			Message: fmt.Sprintf("cached reachability fact fresh: %s/%s (age %s, ttl %s)", reg.ReachabilityMethod, reg.ReachabilityTarget, age.Round(time.Second), recipientReachabilityTTL),
		}
	}
	if registrationHasReachableWake(cfg, reg) {
		return recipientLiveness{
			OK:      true,
			Via:     "wake",
			Message: fmt.Sprintf("reachable wake target: %s/%s", reg.WakeMethod, reg.WakeTarget),
		}
	}

	// WI-4 Fix 5: when a session exists but is dead, keep its reason so the
	// eventual stale-liveness message explains WHY the session did not count
	// (e.g. "process dead/recycled and heartbeat stale ..."), instead of
	// silently discarding it. deadSessionDetail is prefixed onto whatever
	// downstream stale detail we end up reporting.
	deadSessionDetail := ""
	session, sessionErr := loop.LoadActiveSession(cfg, agentID)
	if sessionErr == nil {
		alive, reason := activeSessionAliveAtWithReason(session, now)
		if alive {
			age := now.UTC().Sub(session.LastSeen.UTC())
			if age < 0 {
				age = 0
			}
			return recipientLiveness{
				OK:      true,
				Via:     "session",
				Message: fmt.Sprintf("active wrapper session: pid=%d host=%s age=%s", session.PID, session.Host, age.Round(time.Second)),
			}
		}
		deadSessionDetail = fmt.Sprintf("active session present but not alive (%s)", reason)
	}

	// withSession threads the dead-session reason into a stale detail so the
	// operator sees both the session failure and the poller/wake failure.
	withSession := func(detail string) string {
		if deadSessionDetail == "" {
			return detail
		}
		return deadSessionDetail + "; " + detail
	}

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err == nil {
		fresh, age, threshold := loop.PollerFreshness(hb, now)
		if fresh {
			if !hb.LaunchEnabled {
				return stalePollerLiveness(agentID, reg.Vendor, withSession(fmt.Sprintf("heartbeat-only poller is fresh but cannot consume mail: method=%s host=%s age=%s threshold=%s", hb.Method, hb.Host, age.Round(time.Second), threshold)), reg.WakeMethod)
			}
			return recipientLiveness{
				OK:      true,
				Via:     "poller",
				Message: fmt.Sprintf("fresh poller heartbeat: method=%s host=%s age=%s threshold=%s", hb.Method, hb.Host, age.Round(time.Second), threshold),
			}
		}
		// Surface a recorded poll-computation failure (WI-4 Fix 2) so a
		// "beating but failing" poller is distinguishable from a silent one.
		staleDetail := fmt.Sprintf("poller heartbeat stale: age=%s threshold=%s", age.Round(time.Second), threshold)
		if e := strings.TrimSpace(hb.LastError); e != "" {
			staleDetail += fmt.Sprintf(" (last poll error: %s)", e)
		}
		return stalePollerLiveness(agentID, reg.Vendor, withSession(staleDetail), reg.WakeMethod)
	}
	if !os.IsNotExist(err) {
		return stalePollerLiveness(agentID, reg.Vendor, withSession(fmt.Sprintf("poller heartbeat unreadable: %v", err)), reg.WakeMethod)
	}

	// Final fallback: if we have an unreachable wake method, report that
	// specifically.
	if reg.WakeMethod != "" && reg.WakeTarget != "" {
		return stalePollerLiveness(agentID, reg.Vendor, withSession(fmt.Sprintf("unreachable wake target: %s/%s", reg.WakeMethod, reg.WakeTarget)), reg.WakeMethod)
	}

	return stalePollerLiveness(agentID, reg.Vendor, withSession("missing poller heartbeat"), reg.WakeMethod)
}

func registrationHasReachableWake(cfg *loop.Config, reg *loop.Registration) bool {
	if reg == nil {
		return false
	}
	if strings.TrimSpace(reg.WakeTarget) == "" {
		return false
	}
	localHost, _ := os.Hostname()
	if strings.TrimSpace(reg.Host) != "" && strings.TrimSpace(localHost) != "" && reg.Host != localHost {
		return false
	}
	// One dispatcher decides reachability per wake_method (tmux/herdr/runner/
	// unknown). The per-method switch that used to live here moved behind the
	// WakeAdapter.Reachable interface. The runner method still does its
	// recipient-bound owned-check BEFORE any dial (WI-3), now inside
	// runnerWakeAdapter.Reachable; an unknown method has no adapter and reports
	// unreachable, matching the old default arm exactly.
	return loop.RegistrationReachable(cfg, reg, time.Second)
}

func stalePollerLiveness(agentID, vendor, detail, wakeMethod string) recipientLiveness {
	if strings.TrimSpace(vendor) == "" {
		if preset, ok := vendorPresets[agentID]; ok {
			vendor = preset.Vendor
		} else {
			vendor = "<vendor>"
		}
	}
	var start string
	if wakeMethod == loop.RunnerWakeMethod {
		start = "start/restart the wrapper via its namespaced shim (ac-claude/ac-codex/ac-gemini/ac-grok) or `agentchute run --vendor <vendor> -- <wrapper>` to re-establish the runner wake"
	} else {
		start = fmt.Sprintf("agentchute poller ensure --as %s --vendor %s --launch (or `doctor --generate-service --as %s` for a scheduler)", agentID, vendor, agentID)
	}
	return recipientLiveness{
		OK:      false,
		Via:     "none",
		Message: fmt.Sprintf("no reachable wake target, active session, or launch-enabled poller heartbeat (%s); run `%s`", detail, start),
	}
}
