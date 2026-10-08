package hubclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// Review 2026-10-08 S11: a migration decided "same hub" from an ssh-keyscan
// fingerprint (which proves nothing about who holds the key) and then probed
// the new URL with accept-new, trusting any key the new host presented. The
// pinned probe instead makes the new host PROVE possession of a key already
// pinned for the old one: strict checking, the pinned keys only, and no mux
// master that could have been authenticated under accept-new.
func TestPinnedInvocationRequiresAPinnedHostKey(t *testing.T) {
	remote := &loop.RemoteConfig{Host: "hub-alias.example", Port: 22, HubID: "0123456789ab", HubDir: "/tmp/hubdir"}
	got, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: "codex", PinnedKnownHosts: "/tmp/pinned"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got.Args, " ")
	for _, want := range []string{
		"StrictHostKeyChecking=yes", "UserKnownHostsFile=/tmp/pinned", "GlobalKnownHostsFile=/dev/null",
		"HostKeyAlias=" + pinnedHostAlias, "ControlMaster=no", "ControlPath=none",
		// A user ssh_config must not add keys behind the pin's back.
		"VerifyHostKeyDNS=no", "KnownHostsCommand=none",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("pinned argv is missing %q: %v", want, got.Args)
		}
	}
	if strings.Contains(joined, "accept-new") {
		t.Fatalf("pinned argv still accepts a new host key: %v", got.Args)
	}
}

func TestPinnedKnownHostsKeepsOnlyUsableKeyLines(t *testing.T) {
	src := filepath.Join(t.TempDir(), "known_hosts")
	content := strings.Join([]string{
		"# a comment",
		"hub.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOne",
		"|1|c2FsdA==|aGFzaA== ecdsa-sha2-nistp256 AAAAE2VjZHNhTwo",
		"@revoked hub.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIRevoked",
		"@cert-authority *.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICa",
		"",
	}, "\n")
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pinnedHostKeyLines(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		pinnedHostAlias + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOne",
		pinnedHostAlias + " ecdsa-sha2-nistp256 AAAAE2VjZHNhTwo",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pinned lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := pinnedHostKeyLines(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("no known_hosts file is no pin: want an error, not an empty pin set")
	}
}

// PR #216 gate (codex P1): dropping every @-marked line dropped @revoked too,
// while keeping the ordinary entry for the SAME key — so the pinned probe
// accepted a key the pin file explicitly revokes. A revoked key leaves the
// positive set, and a set with nothing left fails closed.
func TestPinnedKnownHostsHonoursRevocation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	onlyRevoked := write("revoked", "hub.example ssh-ed25519 AAAAkeyA\n@revoked hub.example ssh-ed25519 AAAAkeyA\n")
	if got, err := pinnedHostKeyLines(onlyRevoked); err == nil {
		t.Fatalf("a pin file whose only key is revoked pinned %v; want fail-closed", got)
	}
	mixed := write("mixed", "hub.example ssh-ed25519 AAAAkeyA\nhub.example ssh-ed25519 AAAAkeyB\n@revoked * ssh-ed25519 AAAAkeyA\n")
	got, err := pinnedHostKeyLines(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != pinnedHostAlias+" ssh-ed25519 AAAAkeyB" {
		t.Fatalf("pinned = %v, want only the unrevoked key B", got)
	}
	// CarryHostKeyPin reads the same set: a revoked key is never carried to a new name.
	if err := CarryHostKeyPin(mixed, "hub-alias.example"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(mixed)
	if strings.Contains(string(data), "hub-alias.example ssh-ed25519 AAAAkeyA") || !strings.Contains(string(data), "hub-alias.example ssh-ed25519 AAAAkeyB") {
		t.Fatalf("carried pins =\n%s\nwant B carried and revoked A not", data)
	}
}
