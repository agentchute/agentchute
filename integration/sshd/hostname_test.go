//go:build sshd_integration

package sshd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHDHostNameUsesResolvedKnownHostsEntry(t *testing.T) {
	h := newSSHDHarness(t)
	cfg := filepath.Join(h.root, "hostname_config")
	text := fmt.Sprintf("Host review-hub\n HostName 127.0.0.1\n Port %d\n", h.port)
	if err := os.WriteFile(cfg, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-F", cfg, "-vv", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile=" + h.knownHosts, "-i", h.adminKey, h.user + "@review-hub", "true"}
	out, err := exec.Command(h.ssh, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("SSH control: %v %s", err, out)
	}
	want := fmt.Sprintf("Host '[127.0.0.1]:%d' is known and matches", h.port)
	if !strings.Contains(string(out), want) {
		t.Fatalf("missing effective-name match %q: %s", want, out)
	}
	t.Log(want)
}
