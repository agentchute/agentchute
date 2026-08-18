//go:build sshd_integration

package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/op"
)

func TestSSHDRegisterFieldAndSweepSemantics(t *testing.T) {
	h := newSSHDHarness(t)
	client, cancel := openOneShot(t, h, "codex")
	defer cancel()
	vendor := "bespoke"
	bio := "preserve me\n"
	resp, err := client.Register(op.RegisterReq{Vendor: &vendor, Host: "old-machine", Bio: &bio})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reg.Vendor != vendor || resp.Reg.Host != "old-machine" || resp.Reg.Body != bio {
		t.Fatalf("initial register = %+v", resp.Reg)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	client, cancel = openOneShot(t, h, "codex")
	defer cancel()
	resp, err = client.Register(op.RegisterReq{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reg.Vendor != vendor || resp.Reg.Body != bio {
		t.Fatalf("nil fields did not preserve existing values: %+v", resp.Reg)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	client, cancel = openOneShot(t, h, "codex")
	defer cancel()
	empty := ""
	resp, err = client.Register(op.RegisterReq{Bio: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reg.Body != "" {
		t.Fatalf("empty bio did not clear body: %q", resp.Reg.Body)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	staleVendor := "stale"
	if _, err := op.Register(h.cfg, op.Context{ActorID: "stale-peer"}, op.RegisterReq{Vendor: &staleVendor, Host: "gone"}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	client, cancel = openOneShot(t, h, "codex")
	defer cancel()
	if _, err := client.Register(op.RegisterReq{Sweep: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.cfg.AgentRegistrationPath("stale-peer")); err != nil {
		t.Fatalf("sweep=false removed peer: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	client, cancel = openOneShot(t, h, "codex")
	defer cancel()
	resp, err = client.Register(op.RegisterReq{Sweep: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.cfg.AgentRegistrationPath("stale-peer")); !os.IsNotExist(err) {
		t.Fatalf("sweep=true left stale peer: %v", err)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("sweep warnings = %v", resp.Warnings)
	}
	if _, err := os.Stat(filepath.Join(h.remote.ShadowLoopDir, "agents", "stale-peer.md")); !os.IsNotExist(err) {
		t.Fatalf("remote sweep touched shadow: %v", err)
	}
}

func TestSSHDRemoteStatusRowsAnd64KiBPrefix(t *testing.T) {
	h := newSSHDHarness(t)
	checkout := h.newCheckout()
	stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--as", "status-reader")
	if err != nil {
		t.Fatalf("hub join: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	stdout, stderr, err = h.runCLI(checkout, "register", "--as", "status-reader", "--vendor", "test")
	if err != nil {
		t.Fatalf("remote register: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	vendor := "test"
	for _, id := range []string{"alpha", "bravo", "zulu"} {
		host := id + "-host"
		if id == "zulu" {
			host = strings.Repeat("z", 66<<10)
		}
		if _, err := op.Register(h.cfg, op.Context{ActorID: id}, op.RegisterReq{Vendor: &vendor, Host: host}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	client, cancel := openOneShot(t, h, "codex")
	defer cancel()
	status, err := client.Status(func(op.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, agent := range status.Agents {
		found[agent.AgentID] = true
	}
	if !found["alpha"] || !found["bravo"] || found["zulu"] {
		t.Fatalf("status prefix ids = %v", found)
	}
	if !status.Truncated {
		t.Fatal("status response did not report truncation")
	}
	stdout, stderr, err = h.runCLI(checkout, "status", "--as", "status-reader")
	want := "note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing."
	if err != nil || !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "bravo") || strings.Contains(stdout, "zulu") || !strings.Contains(stdout, want) {
		t.Fatalf("truncated status = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
}
