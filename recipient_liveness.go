package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

type recipientLiveness struct {
	OK        bool
	Via       string
	Message   string
	BlockText string
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

	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err == nil {
		fresh, age, threshold := loop.PollerFreshness(hb, now)
		if fresh {
			return recipientLiveness{
				OK:      true,
				Via:     "poller",
				Message: fmt.Sprintf("fresh poller heartbeat: method=%s host=%s age=%s threshold=%s", hb.Method, hb.Host, age.Round(time.Second), threshold),
			}
		}
		return stalePollerLiveness(agentID, reg.Vendor, fmt.Sprintf("poller heartbeat stale: age=%s threshold=%s", age.Round(time.Second), threshold))
	}
	if !os.IsNotExist(err) {
		return stalePollerLiveness(agentID, reg.Vendor, fmt.Sprintf("poller heartbeat unreadable: %v", err))
	}
	return stalePollerLiveness(agentID, reg.Vendor, "missing poller heartbeat")
}

func registrationHasReachableWake(reg *loop.Registration) bool {
	if reg == nil {
		return false
	}
	method := strings.TrimSpace(reg.WakeMethod)
	target := strings.TrimSpace(reg.WakeTarget)
	if method != "tmux" || target == "" {
		return false
	}
	localHost, _ := os.Hostname()
	if strings.TrimSpace(reg.Host) != "" && strings.TrimSpace(localHost) != "" && reg.Host != localHost {
		return false
	}
	return tmuxTargetReachable(target)
}

func stalePollerLiveness(agentID, vendor, detail string) recipientLiveness {
	if strings.TrimSpace(vendor) == "" {
		if preset, ok := vendorPresets[agentID]; ok {
			vendor = preset.Vendor
		} else {
			vendor = "<vendor>"
		}
	}
	start := fmt.Sprintf("agentchute poller ensure --as %s --vendor %s", agentID, vendor)
	return recipientLiveness{
		OK:        false,
		Via:       "none",
		Message:   fmt.Sprintf("no reachable wake target and no fresh poller heartbeat (%s); run `%s`", detail, start),
		BlockText: fmt.Sprintf("recipient liveness not proven: %s", detail),
	}
}
