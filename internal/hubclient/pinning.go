package hubclient

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
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
