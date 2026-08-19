package hubclient

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// The pinning probes.
//
// The hub-side detector reads SSH_ORIGINAL_COMMAND. This side is deliberately
// BEHAVIOURAL instead: it asks the hub to run a command of the client's choosing
// and sees whether it runs. That tests the property the design actually depends
// on — a pinned host cannot run a command the client chose — so it cannot be
// fooled by an interceptor that happens to set the variable, and it needs no
// agreement with the hub about anything.
//
// Measured: with an authorized_keys forced command, a requested `echo NONCE`
// does NOT run (the forced command replaces it); without one, it does.

// pinningNonceBytes is 16 random bytes, hex-encoded to 32 characters. Hex is the
// point: no shell metacharacter can appear in it, so the probe is inert whatever
// login shell the hub runs.
const pinningNonceBytes = 16

// buildPinningProbe returns the real invocation with its trailing sentinel
// replaced by `echo <nonce>`, and nothing else changed.
//
// Everything before the command has to stay identical, or the probe answers a
// question about a different connection than the one that failed. Channel stays
// false so it reuses the mux master and usually costs no authentication.
func buildPinningProbe(opts SSHBuildOptions) (SSHInvocation, string, error) {
	invocation, err := BuildSSHInvocation(opts)
	if err != nil {
		return SSHInvocation{}, "", err
	}
	nonce, err := newPinningNonce()
	if err != nil {
		return SSHInvocation{}, "", err
	}
	invocation.Args[len(invocation.Args)-1] = "echo " + nonce
	return invocation, nonce, nil
}

func newPinningNonce() (string, error) {
	raw := make([]byte, pinningNonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// nonceEchoed reports whether the hub actually RAN the command we asked for.
//
// Whole line, not substring. A substring test would be satisfied by a hub that
// merely echoed the command back, or by any output that happened to contain the
// value — and the claim being made is that the hub executed a caller-chosen
// command, which is a much stronger thing than the bytes appearing somewhere.
//
// Everything else counts as pinned, INCLUDING exit 127: on a pinned host that
// means the hub binary is missing, which is a different problem and correctly
// not this check's business.
func nonceEchoed(stdout, nonce string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimRight(line, "\r") == nonce {
			return true
		}
	}
	return false
}

// pinningVerdict is what the probes concluded, and it deliberately has a third
// state. "Not producer 1" is not the same as "producer 2".
type pinningVerdict int

const (
	pinningPinned pinningVerdict = iota
	// pinningIntercepted: producer 1 — an ssh-intercepting layer. authorized_keys
	// is never consulted, so pinning is not in effect and revoking a key cuts off
	// nothing.
	pinningIntercepted
	// pinningOperatorFallback: producer 2 — the agentchute key was refused and ssh
	// fell back to another identity the hub accepts unrestricted. An authorization
	// problem wearing a "command not found".
	pinningOperatorFallback
	// pinningUnpinnedUnattributed: the host is unpinned, and the second probe could
	// not say which producer. NARROWS NOTHING and names both remedies.
	pinningUnpinnedUnattributed
)

// hubProbeRunner is the seam rows swap. Production runs ssh.
var hubProbeRunner = runProbeCommand

func runProbeCommand(args []string, timeout time.Duration) (string, string, error) {
	cmd := exec.Command("ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Stdin = nil
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		runErr = errProbeTimeout
	}
	return stdout.String(), stderr.String(), runErr
}

var errProbeTimeout = &Error{Code: "E_PROBE_TIMEOUT", Msg: "hub: the pinning probe did not return"}

// probeHubPinning runs 2b and, when 2b says unpinned, 2c.
//
// 2b decides PINNED vs NOT. 2c only ever NARROWS the message, and its third
// outcome is not optional: on a connect, proxy or timeout failure it must narrow
// NOTHING. -F /dev/null is the very thing ruled out of scope for the real path
// because it can make a ProxyJump-only hub unreachable, and that applies to a
// diagnostic just as much — so a second probe that fails to connect must never be
// the thing that decides an operator is at fault.
func probeHubPinning(opts SSHBuildOptions) pinningVerdict {
	invocation, nonce, err := buildPinningProbe(opts)
	if err != nil {
		return pinningPinned
	}
	stdout, _, _ := hubProbeRunner(invocation.Args, 15*time.Second)
	if !nonceEchoed(stdout, nonce) {
		return pinningPinned
	}
	return attributeUnpinnedHub(opts)
}

// attributeUnpinnedHub is 2c: probe on an identity that CANNOT be authorized.
//
// A freshly generated throwaway key, with the operator's config and agent both
// removed. If the nonce still comes back, the host admitted a key it has never
// seen — an intercepting layer that authorizes by something other than the key.
// An explicit publickey refusal means the opposite: the key mattered, so the
// earlier success came from some OTHER identity the operator's config supplied.
func attributeUnpinnedHub(opts SSHBuildOptions) pinningVerdict {
	dir, err := os.MkdirTemp("", "ac-pinning-probe-")
	if err != nil {
		return pinningUnpinnedUnattributed
	}
	defer func() { _ = os.RemoveAll(dir) }()
	key := filepath.Join(dir, "throwaway_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "agentchute-pinning-probe", "-f", key).CombinedOutput(); err != nil {
		_ = out
		return pinningUnpinnedUnattributed
	}
	throwaway := opts
	throwaway.KeyPath = key
	invocation, nonce, err := buildPinningProbe(throwaway)
	if err != nil {
		return pinningUnpinnedUnattributed
	}
	// For THIS probe only: no operator config, no agent. Both are what the probe
	// is trying to exclude.
	args := append([]string{"-F", "/dev/null", "-o", "IdentityAgent=none"}, invocation.Args...)
	stdout, stderr, runErr := hubProbeRunner(args, 15*time.Second)
	switch {
	case nonceEchoed(stdout, nonce):
		return pinningIntercepted
	case strings.Contains(strings.ToLower(stderr), "permission denied (publickey"):
		return pinningOperatorFallback
	default:
		// Connect failure, proxy failure, timeout, anything else. Narrow NOTHING.
		_ = runErr
		return pinningUnpinnedUnattributed
	}
}

func hubUnpinnedInterceptedMessage(remote *loop.RemoteConfig) string {
	host := "this hub"
	if remote != nil {
		host = remote.Host
	}
	return "hub: NOT PINNED — " + host + " ran a command this machine chose, which a forced command would have overridden. `authorized_keys` is not consulted on this host, so identity and pool pinning are not in effect and revoking a key here cuts off nothing. Any peer permitted to reach it can run any command as the hub user, including claiming another agent's id and another pool. Fix on the hub (Tailscale: `tailscale set --ssh=false`, or run a separate sshd for the hub), then re-run doctor."
}

func hubUnpinnedOperatorFallbackMessage(remote *loop.RemoteConfig, agentID string) string {
	host, pool := "this hub", "<pool>"
	if remote != nil {
		host, pool = remote.Host, remote.PoolPath
	}
	return "hub: " + host + " authenticated this machine, but NOT with the key agentchute pinned — no forced command was applied, so `agentchute-hub` reached a login shell instead of a hub session. The agentchute key was offered and refused; ssh then fell back to another identity from your ssh config or agent, which the hub accepts unrestricted. The binary on the hub is fine — this is an authorization problem wearing a \"command not found\". On the hub run: agentchute hub authorize --agent " + agentID + " --pool " + pool + " --key \"<this machine's agentchute public key>\", then retry."
}

func hubUnpinnedBothRemediesSuffix() string {
	return "The second probe could not tell the two apart, so this narrows nothing: it may instead be that the hub authenticated you with a different identity from your ssh config or agent, in which case the fix is `agentchute hub authorize` on the hub rather than any change to sshd. `agentchute doctor` re-runs this check."
}

// PinningVerdict is doctor's entry point: probe the host and return the
// operator-facing sentence plus whether it is pinned.
//
// Exported because doctor lives in internal/cli and the probes need
// BuildSSHInvocation. It reports the narrowed message when the second probe could
// narrow, and the both-remedies message when it could not.
func PinningVerdict(remote *loop.RemoteConfig, agentID string) (string, bool) {
	opts := SSHBuildOptions{Remote: remote, AgentID: agentID}
	if remote != nil {
		opts.StateDir = remote.HubDir
	}
	switch hubPinningProbe(opts) {
	case pinningIntercepted:
		return hubUnpinnedInterceptedMessage(remote), false
	case pinningOperatorFallback:
		return hubUnpinnedOperatorFallbackMessage(remote, agentID), false
	case pinningUnpinnedUnattributed:
		return hubUnpinnedInterceptedMessage(remote) + " " + hubUnpinnedBothRemediesSuffix(), false
	default:
		return "", true
	}
}
