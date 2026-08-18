//go:build sshd_integration

package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A4 / §10.3 "join idempotence (G-B1)" — the half
// TestSSHDKeyRotationChangesMuxIdentityAndReauthenticates does not cover.
//
// That row proves rotation CHANGES the key and invalidates the mux. This one
// proves the complement, which is the property an operator actually relies on:
// running `hub join` again changes NOTHING. Re-running a setup command is the
// most ordinary thing a person does when unsure whether it worked, and the
// failure mode is silent — a second key authorized alongside the first leaves
// two live credentials for one agent, and revoking "the" key later revokes one
// of them.
//
// The authorized_keys LINE COUNT is the assertion that catches it. Same
// fingerprint alone would pass while a duplicate line accumulated, since both
// lines would carry that same key.
func TestSSHDJoinIsIdempotentAndRotationReplacesTheLine(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)

	firstPub := activePubKey(t, h, agentID)
	firstLines := authorizedLinesFor(t, h, agentID)
	if firstLines != 1 {
		t.Fatalf("after one join: %d authorized_keys lines for %s, want 1", firstLines, agentID)
	}

	// The second join. Same URL, same name — exactly what a person re-runs.
	if stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex"); err != nil {
		t.Fatalf("second join: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got := activePubKey(t, h, agentID); got != firstPub {
		t.Fatalf("second join replaced the key material:\n first=%s\n after=%s", firstPub, got)
	}
	if got := authorizedLinesFor(t, h, agentID); got != 1 {
		t.Fatalf("second join left %d authorized_keys lines for %s, want 1 — a duplicate line is a second live credential for one agent", got, agentID)
	}

	// Reap the masters opened by the joins above BEFORE rotating. The harness
	// discovers masters by scanning the hub dir's `.vN` key files, and rotation
	// retires the old one out of that naming — so a master opened with the
	// pre-rotation key would become undiscoverable and keep the daemon alive
	// past the test. This row is about authorized_keys, not mux lifetime; the
	// rotation row next door covers that.
	h.stopMuxMasters()

	// Rotation must REPLACE, not append. A rotation that adds a line leaves the
	// retired key authorized, which is the failure the whole rotation design
	// exists to prevent.
	if stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex", "--rotate-key"); err != nil {
		t.Fatalf("rotate: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	rotatedPub := activePubKey(t, h, agentID)
	if rotatedPub == firstPub {
		t.Fatalf("rotation kept the same key material %s", rotatedPub)
	}
	if got := authorizedLinesFor(t, h, agentID); got != 1 {
		t.Fatalf("rotation left %d authorized_keys lines for %s, want 1 — the retired key is still authorized", got, agentID)
	}
	// And the surviving line is the NEW key, not the old one left in place.
	if !authorizedContains(t, h, rotatedPub) {
		t.Fatalf("authorized_keys does not carry the rotated key %s", rotatedPub)
	}
	if authorizedContains(t, h, firstPub) {
		t.Fatalf("authorized_keys still carries the retired key %s", firstPub)
	}
}

// activePubKey returns the base64 body of the agent's current public key,
// resolved through the active symlink the way every caller resolves it.
func activePubKey(t *testing.T, h *sshdHarness, agentID string) string {
	t.Helper()
	remote := parseRemoteForHome(t, h)
	active := filepath.Join(remote.HubDir, "keys", agentID+"_ed25519")
	target, err := os.Readlink(active)
	if err != nil {
		t.Fatalf("read active key symlink: %v", err)
	}
	pub, err := os.ReadFile(filepath.Join(filepath.Dir(active), target+".pub"))
	if err != nil {
		t.Fatalf("read active pubkey: %v", err)
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		t.Fatalf("malformed pubkey %q", pub)
	}
	return fields[1]
}

// authorizedLinesFor counts authorized_keys lines whose forced command pins this
// agent. Counting lines rather than keys is the point: a duplicate line is how a
// second credential survives unnoticed.
func authorizedLinesFor(t *testing.T, h *sshdHarness, agentID string) int {
	t.Helper()
	data, err := os.ReadFile(h.authorized)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "--agent "+agentID+" ") {
			n++
		}
	}
	return n
}

func authorizedContains(t *testing.T, h *sshdHarness, pubBody string) bool {
	t.Helper()
	data, err := os.ReadFile(h.authorized)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	return strings.Contains(string(data), pubBody)
}
