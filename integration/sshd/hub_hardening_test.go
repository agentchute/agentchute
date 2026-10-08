//go:build sshd_integration

package sshd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

// Review 2026-10-08 S11 / PR #216 gate (codex P1 and look-for 6), against a real
// sshd: the pinned migration probe passes when the server holds a pinned key,
// fails for a key the pin file revokes — exactly as plain ssh refuses it — and
// fails for a key the file does not pin at all.
func TestSSHDPinnedProbeAcceptsThePinAndRefusesARevocation(t *testing.T) {
	h := newSSHDHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pinned, err := os.ReadFile(h.knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hubclient.ProbeWithPinnedHostKey(ctx, h.remote, "codex", "sshd-row", h.keys["codex"], h.knownHosts); err != nil {
		t.Fatalf("pinned probe against the server's own pinned key = %v, want hello", err)
	}

	revokedFile := filepath.Join(h.root, "revoked_known_hosts")
	if err := os.WriteFile(revokedFile, append(append([]byte{}, pinned...), append([]byte("@revoked "), pinned...)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hubclient.ProbeWithPinnedHostKey(ctx, h.remote, "codex", "sshd-row", h.keys["codex"], revokedFile); err == nil {
		t.Fatal("the pinned probe accepted a host key its pin file revokes")
	}

	otherKey := filepath.Join(h.root, "other_host_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-f", otherKey)
	otherPub, err := os.ReadFile(otherKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	wrongFile := filepath.Join(h.root, "wrong_known_hosts")
	if err := os.WriteFile(wrongFile, []byte("hub.example "+strings.TrimSpace(string(otherPub))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hubclient.ProbeWithPinnedHostKey(ctx, h.remote, "codex", "sshd-row", h.keys["codex"], wrongFile); err == nil {
		t.Fatal("the pinned probe accepted a server whose host key the file does not pin")
	}
}

// Agent and X11 forwarding stay off on the hub ssh even when the user's
// ssh_config turns them on: real ssh resolves the effective configuration.
func TestSSHDHubSSHHasForwardingOffWhateverTheUserConfigSays(t *testing.T) {
	h := newSSHDHarness(t)
	cfg := filepath.Join(h.root, "forwarding_ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n  ForwardAgent yes\n  ForwardX11 yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation, err := hubclient.BuildSSHInvocation(hubclient.SSHBuildOptions{Remote: h.remote, AgentID: "codex", KeyPath: h.keys["codex"], Channel: true})
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-F", cfg, "-G"}, invocation.Args[:len(invocation.Args)-1]...)
	out, err := exec.Command(h.ssh, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, out)
	}
	for _, want := range []string{"forwardagent no", "forwardx11 no"} {
		if !strings.Contains(strings.ToLower(string(out)), want) {
			t.Fatalf("effective ssh config lacks %q under a ForwardAgent/ForwardX11 yes user config:\n%s", want, out)
		}
	}
}

// hub join will not repoint a checkout that points at another hub unless
// --replace — through the real binary, before any key or connection — and
// with --replace it joins.
func TestSSHDJoinRefusesToRepointWithoutReplace(t *testing.T) {
	h := newSSHDHarness(t)
	checkout := h.newCheckout()
	other := "ssh://alex@other-hub.example/home/alex/pool"
	pointer := filepath.Join(checkout, loop.PointerFileName)
	if err := os.WriteFile(pointer, []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex")
	if err == nil || !strings.Contains(stdout+stderr, "--replace") {
		t.Fatalf("join over another hub's pointer = %v\nstdout:\n%s\nstderr:\n%s\nwant a refusal naming --replace", err, stdout, stderr)
	}
	if got, _ := os.ReadFile(pointer); strings.TrimSpace(string(got)) != other {
		t.Fatalf("pointer = %q, want it untouched", got)
	}
	if stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex", "--replace"); err != nil {
		t.Fatalf("join --replace: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got, _ := os.ReadFile(pointer); strings.TrimSpace(string(got)) != h.remote.URL {
		t.Fatalf("pointer after --replace = %q, want %s", got, h.remote.URL)
	}
}
