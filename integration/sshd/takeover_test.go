//go:build sshd_integration

package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review 2026-10-08 S10, through the real binary: binding a key to an id that
// already runs locally on the hub needs --takeover from a terminal. runCLI gives
// the child os/exec's default stdin, /dev/null — a character device, which is
// what the first version of the gate tested for, so it passed.
func TestSSHDTakeoverWithoutATerminalIsRefused(t *testing.T) {
	h := newSSHDHarness(t)
	if stdout, stderr, err := h.runCLI(h.pool, "register", "--as", "local-lane", "--vendor", "openai"); err != nil {
		t.Fatalf("register a hub-local lane: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	attackerKey := filepath.Join(h.root, "attacker_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "attacker", "-f", attackerKey)
	pub, err := os.ReadFile(attackerKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.authorized)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := h.runCLI(h.pool, "hub", "authorize", "--agent", "local-lane", "--pool", h.pool, "--key", strings.TrimSpace(string(pub)), "--takeover")
	if err == nil || !strings.Contains(stdout+stderr, "interactive terminal") {
		t.Fatalf("--takeover with stdin=/dev/null = %v\nstdout:\n%s\nstderr:\n%s\nwant a refusal", err, stdout, stderr)
	}
	if after, _ := os.ReadFile(h.authorized); string(after) != string(before) {
		t.Fatal("a refused --takeover changed authorized_keys")
	}
}
