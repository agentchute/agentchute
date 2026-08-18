//go:build sshd_integration

package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
)

func TestSSHDPoolOnlyKeyLineEditFailsBeforeHello(t *testing.T) {
	h := newSSHDHarness(t)
	other := filepath.Join(h.root, "other-pool")
	if err := os.MkdirAll(filepath.Join(other, ".agentchute", "loop", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "AGENTCHUTE.md"), []byte("# other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteAuthorizedCommand(t, h, "codex", h.pool, other, sshdPoolID, sshdPoolID)

	frame := rawHello(t, h, "codex", "codex", hubwire.Protocol, 1, 1)
	assertWireError(t, frame, hubwire.CodePoolMismatch)
}

func TestSSHDConsistentRepointFailsInClientArm(t *testing.T) {
	h := newSSHDHarness(t)
	checkout := h.newCheckout()
	stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--as", "work-tiny")
	if err != nil {
		t.Fatalf("hub join: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	otherID := "abcdef012345"
	other := filepath.Join(h.root, "other-pool")
	if err := os.MkdirAll(filepath.Join(other, ".agentchute", "loop", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "AGENTCHUTE.md"), []byte("# other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".agentchute", "loop", "state", "pool.id"), []byte(otherID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteAuthorizedCommand(t, h, "work-tiny", h.pool, other, sshdPoolID, otherID)

	stdout, stderr, err = h.runCLI(checkout, "status", "--as", "work-tiny")
	if err == nil || !strings.Contains(stderr, "but this machine joined pool id "+sshdPoolID) || !strings.Contains(stderr, otherID) {
		t.Fatalf("client-arm status = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}

func TestSSHDSymlinkPoolSpellingDoesNotMismatch(t *testing.T) {
	h := newSSHDHarness(t)
	alias := filepath.Join(h.root, "pool-alias")
	if err := os.Symlink(h.pool, alias); err != nil {
		t.Fatal(err)
	}
	rewriteAuthorizedCommand(t, h, "codex", h.pool, alias, sshdPoolID, sshdPoolID)
	hello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{})
	if hello.Pool12 != sshdPoolID {
		t.Fatalf("symlink hello = %+v", hello)
	}
}

func rewriteAuthorizedCommand(t *testing.T, h *sshdHarness, agent, oldPool, newPool, oldID, newID string) {
	t.Helper()
	data, err := os.ReadFile(h.authorized)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		if !strings.Contains(line, "agentchute:"+agent+":"+oldID) {
			continue
		}
		line = strings.Replace(line, "--pool "+oldPool+" ", "--pool "+newPool+" ", 1)
		line = strings.Replace(line, "--pool-id "+oldID+"", "--pool-id "+newID+"", 1)
		line = strings.Replace(line, "agentchute:"+agent+":"+oldID, "agentchute:"+agent+":"+newID, 1)
		lines[i] = line
		changed = true
	}
	if !changed {
		t.Fatalf("authorized line for %s not found", agent)
	}
	if err := os.WriteFile(h.authorized, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}
