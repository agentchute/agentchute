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
	if registrationHasReachableWake(reg) {
		return recipientLiveness{
			OK:      true,
			Via:     "wake",
			Message: fmt.Sprintf("reachable wake target: %s/%s", reg.WakeMethod, reg.WakeTarget),
		}
	}

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
		// If session exists but is dead, we'll fall through but keep the reason
		// in case we need to surface it in stalePollerLiveness.
		_ = reason
	}

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err == nil {
		fresh, age, threshold := loop.PollerFreshness(hb, now)
		if fresh {
			if !hb.LaunchEnabled {
				return stalePollerLiveness(agentID, reg.Vendor, fmt.Sprintf("heartbeat-only poller is fresh but cannot consume mail: method=%s host=%s age=%s threshold=%s", hb.Method, hb.Host, age.Round(time.Second), threshold), reg.WakeMethod)
			}
			return recipientLiveness{
				OK:      true,
				Via:     "poller",
				Message: fmt.Sprintf("fresh poller heartbeat: method=%s host=%s age=%s threshold=%s", hb.Method, hb.Host, age.Round(time.Second), threshold),
			}
		}
		return stalePollerLiveness(agentID, reg.Vendor, fmt.Sprintf("poller heartbeat stale: age=%s threshold=%s", age.Round(time.Second), threshold), reg.WakeMethod)
	}
	if !os.IsNotExist(err) {
		return stalePollerLiveness(agentID, reg.Vendor, fmt.Sprintf("poller heartbeat unreadable: %v", err), reg.WakeMethod)
	}

	// Final fallback: if we have an unreachable wake method, report that
	// specifically.
	if reg.WakeMethod != "" && reg.WakeTarget != "" {
		return stalePollerLiveness(agentID, reg.Vendor, fmt.Sprintf("unreachable wake target: %s/%s", reg.WakeMethod, reg.WakeTarget), reg.WakeMethod)
	}

	return stalePollerLiveness(agentID, reg.Vendor, "missing poller heartbeat", reg.WakeMethod)
}

func registrationHasReachableWake(reg *loop.Registration) bool {
	if reg == nil {
		return false
	}
	method := strings.TrimSpace(reg.WakeMethod)
	target := strings.TrimSpace(reg.WakeTarget)
	if target == "" {
		return false
	}
	localHost, _ := os.Hostname()
	if strings.TrimSpace(reg.Host) != "" && strings.TrimSpace(localHost) != "" && reg.Host != localHost {
		return false
	}
	switch method {
	case "tmux":
		return tmuxTargetReachable(target)
	case loop.RunnerWakeMethod:
		return loop.RunnerSocketReachable(target, time.Second)
	default:
		return false
	}
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
