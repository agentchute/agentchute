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

func TestSSHDRepointTakesEffectAfterLiveMasterIsReaped(t *testing.T) {
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

	before := h.authCount()
	stdout, stderr, err = h.runCLI(checkout, "status", "--as", "work-tiny")
	if err == nil || !strings.Contains(stderr, "not registered") || strings.Contains(stderr, "joined pool id") {
		t.Fatalf("live-master status did not retain its authorization snapshot: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got := h.authCount(); got != before {
		t.Fatalf("live-master repoint re-authenticated: auth count %d -> %d", before, got)
	}

	remote := parseRemoteForHome(t, h)
	activeKey := filepath.Join(remote.HubDir, "keys", "work-tiny_ed25519")
	if err := hubclient.ReapSSHMux(remote, "work-tiny", activeKey, remote.HubDir); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = h.runCLI(checkout, "status", "--as", "work-tiny")
	if err == nil || !strings.Contains(stderr, "but this machine joined pool id "+sshdPoolID) || !strings.Contains(stderr, otherID) {
		t.Fatalf("fresh-auth client-arm status = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got := h.authCount(); got != before+1 {
		t.Fatalf("post-reap status auth count = %d, want %d", got, before+1)
	}
}

func TestSSHDHubSideRepointCannotReapClientHeldMaster(t *testing.T) {
	h := newSSHDHarness(t)
	beforeHello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{})
	beforeAuth := h.authCount()

	other := filepath.Join(h.root, "same-id-other-pool")
	if err := os.MkdirAll(filepath.Join(other, ".agentchute", "loop", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "AGENTCHUTE.md"), []byte("# other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".agentchute", "loop", "state", "pool.id"), []byte(sshdPoolID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pubkey, err := os.ReadFile(h.keys["codex"] + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := h.runCLI(h.pool, "hub", "authorize", "--agent", "codex", "--pool", other, "--key", strings.TrimSpace(string(pubkey)), "--replace-key")
	if err != nil {
		t.Fatalf("hub-side repoint: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	afterHello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{})
	if afterHello.Pool != beforeHello.Pool || afterHello.Pool == other {
		t.Fatalf("client-held master changed pool snapshot: before %q after %q", beforeHello.Pool, afterHello.Pool)
	}
	if got := h.authCount(); got != beforeAuth {
		t.Fatalf("hub-side repoint reaped remote client master: auth count %d -> %d", beforeAuth, got)
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
