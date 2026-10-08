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
