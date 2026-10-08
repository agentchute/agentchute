//go:build sshd_integration

package sshd

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
)

func TestSSHDDeferredRotationPreservesActiveJoin(t *testing.T) {
	h := newSSHDHarness(t)
	if err := os.WriteFile(filepath.Join(h.clientHome, ".ssh", "known_hosts"), []byte(readFileString(t, h.knownHosts)), 0600); err != nil {
		t.Fatal(err)
	}
	checkout, agentID := joinNamedCodex(t, h)
	if out, stderr, err := h.runCLI(checkout, "register", "--as", agentID, "--vendor", "test"); err != nil {
		t.Fatalf("register: %v %s %s", err, out, stderr)
	}
	remote := parseRemoteForHome(t, h)
	keyBase := filepath.Join(remote.HubDir, "keys", agentID+"_ed25519")
	beforePub := activePubKey(t, h, agentID)
	beforeAuth := readFileString(t, h.authorized)
	beforePin := readFileString(t, filepath.Join(remote.HubDir, "known_hosts"))
	beforePointer := readFileString(t, filepath.Join(checkout, ".agentchute-control-repo"))
	var beforeCfg hubclient.HubConfig
	if err := json.Unmarshal([]byte(readFileString(t, remote.ConfigPath)), &beforeCfg); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex", "--rotate-key")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("rotation result=%v out=%s stderr=%s", err, out, stderr)
	}
	staged := keyBase + ".v2"
	stagedPub := pubKeyBody(t, staged+".pub")
	if activePubKey(t, h, agentID) != beforePub || readFileString(t, h.authorized) != beforeAuth {
		t.Fatal("incomplete rotation changed the active key or authorization")
	}
	if readFileString(t, filepath.Join(remote.HubDir, "known_hosts")) != beforePin || readFileString(t, filepath.Join(checkout, ".agentchute-control-repo")) != beforePointer {
		t.Fatal("incomplete rotation changed host pin or pointer")
	}
	if !strings.Contains(out, "--replace-key") || !strings.Contains(out, stagedPub) {
		t.Fatalf("printed command does not name staged key: %s", out)
	}
	var afterCfg hubclient.HubConfig
	if err := json.Unmarshal([]byte(readFileString(t, remote.ConfigPath)), &afterCfg); err != nil {
		t.Fatal(err)
	}
	t.Logf("non-TTY rotation exit=2; active key, authorized_keys, host pin and pointer unchanged; staged v2 exists; pool12 before=%q after=%q; fingerprint unchanged=%v", beforeCfg.Pool12, afterCfg.Pool12, beforeCfg.HostKeyFingerprint == afterCfg.HostKeyFingerprint)
	if beforeCfg.Pool12 != afterCfg.Pool12 || beforeCfg.Pool != afterCfg.Pool {
		t.Error("incomplete rotation discarded joined pool identity")
	}
	_, statusErr, statusRunErr := h.runCLI(checkout, "status", "--as", agentID)
	t.Logf("status after incomplete rotation: %v %s", statusRunErr, statusErr)
	if statusRunErr != nil {
		t.Error("old authorized key can no longer use the joined pool")
	}
	// Complete the printed operator step in the isolated fixture, then plain rejoin.
	h.stopMuxMasters()
	if out, stderr, err := h.runCLITTY(h.pool, "hub", "authorize", "--agent", agentID, "--pool", h.pool, "--key", strings.TrimSpace(readFileString(t, staged+".pub")), "--replace-key"); err != nil {
		t.Fatalf("operator step: %v %s %s", err, out, stderr)
	}
	if out, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex"); err != nil {
		t.Fatalf("plain rejoin: %v %s %s", err, out, stderr)
	}
	if activePubKey(t, h, agentID) != stagedPub || authorizedContains(t, h, beforePub) {
		t.Fatal("rotation did not converge")
	}
	if _, err := os.Stat(keyBase + ".v1"); !os.IsNotExist(err) {
		t.Fatalf("old key not retired: %v", err)
	}
	t.Log("operator authorization on a PTY plus a plain non-TTY rejoin converges")
}
