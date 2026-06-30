package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// reproveProbeTimeout bounds the live reachability probe inside a reprove. Short
// so an off-turn poll tick never stalls on a black-hole wake target.
const reproveProbeTimeout = time.Second

// maxReachabilityErrorLen caps the diagnostic stored in ReachabilityError so a
// pathological probe error can never bloat the (1 MiB-capped) registration file.
const maxReachabilityErrorLen = 400

// recipientReachabilityTTL is how long a cached reachability fact (ReachableAt)
// is trusted by recipient-liveness as a VALID hit before it falls back to a live
// probe. Comfortably larger than the runner/poller poll cadence so a healthy
// recipient that re-proves every tick keeps a fresh fact, but short enough that a
// dead lane's cache ages out quickly.
const recipientReachabilityTTL = 2 * time.Minute

// reproveAndRebindOwnWake re-proves — and, when it has the binding context,
// re-binds or reselects — the agent's OWN wake target, then records the result as
// a cached reachability fact (ReachableAt / ReachabilityMethod /
// ReachabilityTarget / ReachabilityError) in the agent's own registration under
// the per-agent lock.
//
// This is the root-cause fix for the herdr circular deadlock: the recipient's
// OFF-TURN loop heals its OWN binding each tick, so repair no longer depends on
// an inbound wake (the very wake that is failing).
//
// Rebind capability is gated by binding context per wake method:
//   - herdr: requires HERDR_PANE_ID; resolves the stable name via
//     `herdr agent list` (match on the `name` field — NOT `agent get <name>`,
//     since the herdr handle can differ from the bound name). If the name no
//     longer maps to OUR pane and we have the pane, it re-binds (herdr rename).
//   - tmux: requires $TMUX_PANE; re-detects our current pane and re-binds the
//     wake target if it moved.
//   - agentchute-run (runner) / other: PROBE-ONLY via the recipient-bound
//     dispatcher (the runner owned-check runs first); a reachable runner always
//     remains primary.
//
// If the current primary is unreachable, the same truth-selection used by
// register/boot/self-check is allowed to select a different proven-live primary
// (runner > herdr > tmux). That models a live process moving panes without
// requiring sender-side multi-wake escalation.
//
// A context-LESS caller (no HERDR_PANE_ID/$TMUX_PANE) PROBES and records the
// fact but never re-binds (codex guardrail). The cache is ADVISORY: this writes
// it, but senders/liveness must fall back to live behavior on a miss, and it
// never suppresses delivery or the structural poke.
//
// On a SUCCESS, ReachableAt is advanced and ReachabilityError cleared. On a
// FAILURE, ReachableAt is cleared (so IsReachable falls back to a live probe
// rather than asserting a stale "reachable") and the diagnostic is recorded. A
// non-pokable registration (no wake strings) and a missing registration are
// both no-ops.
func reproveAndRebindOwnWake(cfg *loop.Config, agentID string) (rebound bool, err error) {
	if cfg == nil {
		return false, fmt.Errorf("reproveAndRebindOwnWake: nil config")
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return false, err
	}
	now := time.Now().UTC()

	lockErr := loop.WithAgentLock(cfg, agentID, func() error {
		regPath := cfg.AgentRegistrationPath(agentID)
		reg, rerr := loop.ReadRegistration(regPath)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return nil // not registered yet: nothing to reprove
			}
			return rerr
		}
		method := strings.TrimSpace(reg.WakeMethod)
		target := strings.TrimSpace(reg.WakeTarget)
		if method == "" || target == "" {
			// Non-pokable: no wake binding to reprove/rebind. Leave the reg as-is.
			return nil
		}

		var (
			reachable bool
			probeErr  string
			newTarget = target
		)
		switch method {
		case "herdr":
			reachable, rebound, newTarget, probeErr = reproveHerdrWake(reg, agentID)
		case "tmux":
			reachable, rebound, newTarget, probeErr = reproveTmuxWake(reg)
		default:
			// agentchute-run and any other adapter: probe-only via the
			// recipient-bound dispatcher (the runner adapter's owned-check runs
			// before any dial). No rebind — the endpoint is deterministic.
			reachable = loop.RegistrationReachable(cfg, reg, reproveProbeTimeout)
			if !reachable {
				probeErr = fmt.Sprintf("wake target %s/%s not currently reachable", method, target)
			}
		}

		if !reachable {
			selectedMethod, selectedTarget, selected, _ := selectTruthfulPrimary(cfg, registerOpts{
				AgentID:            agentID,
				Vendor:             reg.Vendor,
				ClearStaleTmuxWake: true,
			}, reg)
			if selected && (selectedMethod != method || selectedTarget != target) {
				method = selectedMethod
				target = selectedTarget
				newTarget = selectedTarget
				reg.WakeMethod = selectedMethod
				reg.WakeTarget = selectedTarget
				reachable = true
				rebound = true
				probeErr = ""
			}
		}

		// Apply a rebind that changed the wake target (tmux pane move), or a
		// primary reselect that changed method/target. herdr rebinds the pane to
		// the stable NAME, which is the target itself, so the target string is
		// unchanged there.
		if newTarget != target {
			reg.WakeTarget = newTarget
			target = newTarget
		}

		// Endpoint-bound cache: the cached fact always names the CURRENT wake
		// endpoint, so a later wake-target change invalidates it via IsReachable.
		reg.ReachabilityMethod = method
		reg.ReachabilityTarget = target
		if reachable {
			reg.ReachableAt = &now
			reg.ReachabilityError = ""
		} else {
			// Clear the proven-reachable stamp so IsReachable returns false and
			// consumers fall back to a live probe (never assert a stale truth).
			reg.ReachableAt = nil
			reg.ReachabilityError = truncateReachabilityError(probeErr)
		}
		return loop.WriteRegistration(regPath, reg)
	})
	if lockErr != nil {
		return rebound, lockErr
	}
	return rebound, nil
}

// reproveHerdrWake re-resolves a herdr wake by its bound NAME (the wake target)
// via `herdr agent list` and, when it has HERDR_PANE_ID context and the name no
// longer maps to our pane, re-binds it (herdr rename pane → agentID). It returns
// whether the binding is now reachable-as-us, whether a rebind happened, the
// (stable-name) target, and a diagnostic on failure.
func reproveHerdrWake(reg *loop.Registration, agentID string) (reachable, rebound bool, newTarget, probeErr string) {
	target := strings.TrimSpace(reg.WakeTarget) // the stable herdr name
	newTarget = target
	pane := currentHerdrPane()

	info, found := herdrAgentByName(target)
	if found {
		// The name resolves to a live pane. With pane context, require it to be
		// OURS; a context-less probe accepts any live binding as reachable.
		if pane == "" || info.PaneID == pane {
			return true, false, newTarget, ""
		}
		// Bound to a DIFFERENT pane than ours — stale; fall through to rebind.
	}

	if pane == "" {
		// No binding context: cannot rebind. Probe-only verdict is unreachable.
		return false, false, newTarget,
			fmt.Sprintf("herdr name %q does not map to a live pane and no HERDR_PANE_ID to rebind", target)
	}
	if !herdrAvailable() {
		return false, false, newTarget, fmt.Sprintf("herdr binary unavailable; cannot rebind %q", target)
	}
	// Re-bind our pane to the stable name. WI-E2: rebind to agentID (the canonical
	// stable name); the wake target IS that name, so reflect it.
	if err := renameCurrentHerdrAgent(agentID); err != nil {
		return false, false, newTarget, fmt.Sprintf("herdr rebind failed: %v", err)
	}
	return true, true, strings.TrimSpace(agentID), ""
}

// reproveTmuxWake re-detects our current tmux pane ($TMUX_PANE) and re-binds the
// wake target if it moved, otherwise probes the recorded pane. With no $TMUX_PANE
// context it is probe-only (no rebind).
func reproveTmuxWake(reg *loop.Registration) (reachable, rebound bool, newTarget, probeErr string) {
	target := strings.TrimSpace(reg.WakeTarget)
	newTarget = target
	pane := currentTmuxPane()

	if pane != "" && pane != target {
		// We are in a DIFFERENT pane than the registration records: re-bind.
		newTarget = pane
		if tmuxTargetReachable(pane) {
			return true, true, newTarget, ""
		}
		return false, true, newTarget, fmt.Sprintf("re-detected tmux pane %q not reachable", pane)
	}

	if tmuxTargetReachable(target) {
		return true, false, newTarget, ""
	}
	if pane == "" {
		return false, false, newTarget, fmt.Sprintf("tmux pane %q not reachable and no TMUX_PANE to rebind", target)
	}
	return false, false, newTarget, fmt.Sprintf("tmux pane %q not reachable", target)
}

// registrationHasReachableWake reports whether reg declares a wake target that
// is reachable from THIS host. Relocated here from the deleted recipient_liveness.go
// (Gate 1) because cmdPollerEnsure still consults it; it rides the same
// recipient-bound loop.RegistrationReachable dispatcher (runner does its
// owned-check before any dial; an unknown method has no adapter and reports
// unreachable). Cross-host registrations are short-circuited unreachable.
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
	return loop.RegistrationReachable(cfg, reg, time.Second)
}

// truncateReachabilityError bounds the diagnostic stored in the registration.
func truncateReachabilityError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxReachabilityErrorLen {
		return s
	}
	return s[:maxReachabilityErrorLen-1] + "…"
}
