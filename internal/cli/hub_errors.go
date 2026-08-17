package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

func hubVersionError(hubVersion, clientVersion int) error {
	message := fmt.Sprintf("hub: hub speaks agentchute-hub v%d; this CLI needs v%d. The hub always upgrades first — run `agentchute ", hubVersion, clientVersion) + "update` on the hub, then retry. This machine is not misconfigured and re-running `agentchute hub join` will NOT fix it: wait for the hub upgrade."
	return &hubwire.ProtocolError{Code: hubwire.CodeVersion, Msg: message}
}

func hubIdentityError(authorized, acting string) error {
	return &hubwire.ProtocolError{Code: hubwire.CodeIdentity, Msg: fmt.Sprintf("hub: this key is authorized as %q but you are acting as %q. Fix --as/AGENTCHUTE_AGENT_ID, or join this machine as %s: agentchute hub join <url> --as %s", authorized, acting, acting, acting)}
}

func hubPoolNotFoundError(pool, agentID string) error {
	return &hubwire.ProtocolError{Code: hubwire.CodePoolNotFound, Msg: fmt.Sprintf("hub: the authorized pool path %s no longer resolves on the hub. The hub operator should re-run hub authorize from the pool's current location (agentchute hub authorize --agent %s --replace-key --pool <new-path> --key \"<key>\").", pool, agentID)}
}

func hubPoolMismatchError(pool, expected, actual, agentID string) error {
	return &hubwire.ProtocolError{Code: hubwire.CodePoolMismatch, Msg: fmt.Sprintf("hub: this key is authorized for pool id %s, but %s on the hub reports pool id %s (or has no state/pool.id at all). The authorized_keys line's --pool was edited without its --pool-id, or the pool directory was replaced. On the hub, re-run: agentchute hub authorize --agent %s --replace-key --pool <the pool this key should serve> --key \"<key>\".", expected, pool, actual, agentID)}
}

func hubClientPoolMismatchMessage(hubPool, hubID, joinedID, joinedPool, agentID string) string {
	return fmt.Sprintf("hub: this key now serves pool %s (id %s) on the hub, but this machine joined pool id %s (%s). The key line was re-pointed or the hub moved the pool. Re-join if the move is intended (agentchute hub join <url> --as %s), or re-authorize the key with the right --pool on the hub.", hubPool, hubID, joinedID, joinedPool, agentID)
}

func hubChannelLostMessage(argv []string) string {
	return fmt.Sprintf("hub: channel to the hub was lost; the wrapper was stopped (fenced). Relaunch with: %s. (This lane was started with --relaunch=false; the default relaunches automatically, §6.7.)", strings.Join(argv, " "))
}

func hubOrderError() error {
	return &hubwire.ProtocolError{Code: "E_ORDER", Msg: "hub: protocol order violation (tick before register on this channel). This is a client bug, not an operator problem — please report it."}
}

func hubIOError(cause error) error {
	return &hubwire.ProtocolError{Code: "E_HUB_IO", Msg: fmt.Sprintf("hub: the hub could not write pool state: %v. This is a hub-side problem; nothing was partially delivered unless the message text says otherwise.", cause)}
}

func hubFencedError(agentID string) error {
	return &hubwire.ProtocolError{Code: "E_FENCED", Msg: fmt.Sprintf("serve: this lane was fenced out (lease reclaimed — likely a newer serve for %s, or a hub update). Restart this lane: ac serve %s", agentID, agentID)}
}

func hubLeaseHeldError(cfg *loop.Config, agentID string, now time.Time) error {
	claim, err := loop.ReadServeClaim(cfg, agentID)
	if err != nil {
		return &hubwire.ProtocolError{Code: "E_LEASE_HELD", Msg: fmt.Sprintf("runner for %s is already active (serve lease held). A second machine serving the same id must pick a distinct --as; if a connection just dropped, this clears within ~20s.", agentID)}
	}
	age := now.UTC().Sub(claim.LastSeen)
	if age < 0 {
		age = 0
	}
	if loop.ClaimIsStale(claim, now.UTC()) {
		claimPath := filepath.Join(cfg.AgentStateDir(agentID), "serve.claim")
		return &hubwire.ProtocolError{Code: "E_LEASE_HELD", Msg: fmt.Sprintf("runner for %s looks DEAD (lease stale %s) but hub pid %d still reads alive on the same boot as this claim — either that process is frozen but real, or this is OS pid reuse within one boot. On the hub: inspect `ps -p %d`; if it is unrelated, delete %s and relaunch.", agentID, compactHubAge(age), claim.PID, claim.PID, claimPath)}
	}
	return &hubwire.ProtocolError{Code: "E_LEASE_HELD", Msg: fmt.Sprintf("runner for %s is already active (serve lease held by hub pid %d, fresh %s ago). A second machine serving the same id must pick a distinct --as; if a connection just dropped, this clears within ~20s.", agentID, claim.PID, compactHubAge(age))}
}

func compactHubAge(age time.Duration) string {
	age = age.Round(time.Second)
	if age >= time.Hour && age%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	if age >= time.Minute && age%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
	return age.String()
}

func hubSessionCatalogError(cfg *loop.Config, agentID string, err error) error {
	switch {
	case errors.Is(err, op.ErrLeaseHeld) && cfg != nil:
		return hubLeaseHeldError(cfg, agentID, time.Now().UTC())
	case errors.Is(err, op.ErrOrder):
		return hubOrderError()
	case errors.Is(err, op.ErrFenced):
		return hubFencedError(agentID)
	case hubwire.CodeFor(err) == "E_HUB_IO":
		mapped := hubIOError(err)
		var source, target *hubwire.ProtocolError
		if errors.As(err, &source) && errors.As(mapped, &target) {
			target.Retriable = source.Retriable
			target.ClaimedHeld = source.ClaimedHeld
		}
		return mapped
	default:
		return err
	}
}

func hubLocalNameWarning(localName, agentID string) string {
	return fmt.Sprintf("warning: AGENTCHUTE_AGENT_ID=%s is a local name on this machine; every command resolves it to %q (this hub's names map). Unset it, or export the full id, if that is not what you want.", localName, agentID)
}

func hubJoinNameError(name string) error {
	return fmt.Errorf("hub join: --name %s does not name a wrapper this machine can launch (known: claude, codex, gemini, grok). --name is the LOCAL name you launch the lane with (ac serve <name>), so it must be a wrapper token — a lane named %q would have no launch form at all. For an arbitrary pool id, use --as instead and launch with an explicit wrapper: agentchute hub join <url> --as work-tiny, then ac --as work-tiny serve claude.", name, name)
}

func hubJoinBusyError(lockPath string) error {
	return fmt.Errorf("hub join: another agentchute hub join/rotate is already running for this hub (lock %s). Wait for it to finish and re-run.", displayHomePath(lockPath))
}

func hubJoinKeyVersionError(name string) error {
	return fmt.Errorf("hub join: keys/%s is not a valid key version (expected .v<N>, N a positive decimal integer). Move it out of the keys directory and re-run.", name)
}

func hubTurnEndConnectError() error {
	return errors.New("turn-end: could not reach the hub to commit claimed mail (connect failed after 5s). Nothing is lost: the claim is held on the hub and the guard latch stays armed; turn-end retries at the next turn boundary. If this persists, check the network and run agentchute doctor.")
}

func hubSenderNotRegisteredError(agentID string) error {
	return fmt.Errorf("sender %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", agentID, agentID)
}

func hubAgentNotRegisteredError(agentID string) error {
	return fmt.Errorf("agent %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", agentID, agentID)
}

func checkHubAuthorizedKeysAudit() doctorCheck {
	var output strings.Builder
	err := listHubAuthorizedKeys(&output)
	message := strings.TrimSpace(output.String())
	if message == "no agentchute-authorized keys" && err == nil {
		return doctorCheck{Name: "hub_authorized_keys", Severity: severitySkip, Message: message}
	}
	if err != nil {
		if message != "" {
			message += "; "
		}
		message += err.Error()
		return doctorCheck{Name: "hub_authorized_keys", Severity: severityBlocker, Message: message}
	}
	return doctorCheck{Name: "hub_authorized_keys", Severity: severityOK, Message: message}
}
