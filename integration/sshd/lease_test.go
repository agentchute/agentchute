//go:build sshd_integration

package sshd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

func TestSSHDLeaseHeldRefusalKeepsFirstToken(t *testing.T) {
	h := newSSHDHarness(t)
	first, cancelFirst := openChannel(t, h, "codex")
	defer cancelFirst()
	lease, err := first.AcquireLease(op.LeaseReq{})
	if err != nil {
		t.Fatal(err)
	}

	second, cancelSecond := openChannel(t, h, "codex")
	defer cancelSecond()
	if _, err := second.AcquireLease(op.LeaseReq{}); hubclient.ErrorCode(err) != "E_LEASE_HELD" {
		t.Fatalf("second acquire = %v, code %q", err, hubclient.ErrorCode(err))
	}
	claim, err := loop.ReadServeClaim(h.cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if claim.ServeToken != lease.Token {
		t.Fatalf("first token changed: %q -> %q", lease.Token, claim.ServeToken)
	}
	if err := first.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHDReclaimDuringSendIsFencedAndLinksNothing(t *testing.T) {
	h := newSSHDHarness(t)
	first, cancelFirst := openChannel(t, h, "codex")
	old, err := first.AcquireLease(op.LeaseReq{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
	cancelFirst()

	second, cancelSecond := openChannel(t, h, "codex")
	defer cancelSecond()
	fresh, err := second.AcquireLease(op.LeaseReq{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Token == old.Token {
		t.Fatal("reacquire reused fencing token")
	}

	client, cancelClient := openOneShot(t, h, "codex")
	defer cancelClient()
	_, err = client.Send(op.SendReq{To: "grok", Content: []byte("must not link\n"), ServeToken: old.Token})
	if code := hubclient.ErrorCode(err); code != "E_FENCED" {
		t.Fatalf("send with stale token = %v, code %q", err, code)
	}
	count, err := h.State().InboxCount("grok")
	if err != nil || count != 0 {
		t.Fatalf("grok inbox count = %d, %v", count, err)
	}
	if err := second.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHDLiveLeaseRegisterRequiresInheritedToken(t *testing.T) {
	h := newSSHDHarness(t)
	channel, cancelChannel := openChannel(t, h, "codex")
	defer cancelChannel()
	if _, err := channel.AcquireLease(op.LeaseReq{}); err != nil {
		t.Fatal(err)
	}
	vendor := "custom"
	resp, err := channel.Register(op.RegisterReq{Vendor: &vendor, Host: "remote-child"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reg.Vendor != vendor || resp.Reg.Host != "remote-child" {
		t.Fatalf("channel register = %+v", resp.Reg)
	}

	client, cancelClient := openOneShot(t, h, "codex")
	defer cancelClient()
	_, err = client.Register(op.RegisterReq{Host: "tokenless"})
	if code := hubclient.ErrorCode(err); code != "E_HUB_IO" || !strings.Contains(err.Error(), "live elsewhere") {
		t.Fatalf("tokenless live-lease register = %v, code %q", err, code)
	}
	if err := channel.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
}

func TestSSHDBootRefRebootPIDReuseOutcomes(t *testing.T) {
	h := newSSHDHarness(t)
	probe, err := loop.AcquireServeLease(h.cfg, "boot-probe")
	if err != nil {
		t.Fatal(err)
	}
	current, err := loop.ReadServeClaim(h.cfg, "boot-probe")
	if err != nil {
		t.Fatal(err)
	}
	if current.BootRef == "" {
		t.Fatal("platform boot reference is empty")
	}
	if err := loop.ReleaseLease(probe); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		bootRef  string
		lastSeen time.Time
		wantHeld bool
	}{
		{name: "stale-different-reclaims-live-pid", bootRef: current.BootRef + "-prior", lastSeen: time.Now().Add(-time.Hour)},
		{name: "stale-matching-holds-live-pid", bootRef: current.BootRef, lastSeen: time.Now().Add(-time.Hour), wantHeld: true},
		{name: "stale-absent-preserves-upgrade-path", bootRef: "", lastSeen: time.Now().Add(-time.Hour), wantHeld: true},
		{name: "fresh-different-still-holds", bootRef: current.BootRef + "-prior", lastSeen: time.Now(), wantHeld: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "boot-" + tt.name
			claim := loop.ServeClaim{
				ID: id, Host: current.Host, PID: os.Getpid(), BootRef: tt.bootRef,
				ServeToken: "old-token", StartedAt: tt.lastSeen, LastSeen: tt.lastSeen,
			}
			writeClaim(t, h.cfg, claim, nil)
			lease, err := loop.AcquireServeLease(h.cfg, id)
			if tt.wantHeld {
				if !errors.Is(err, loop.ErrLeaseHeld) {
					t.Fatalf("acquire = %v, want ErrLeaseHeld", err)
				}
				got, readErr := loop.ReadServeClaim(h.cfg, id)
				if readErr != nil || got.ServeToken != "old-token" {
					t.Fatalf("held claim = %+v, %v", got, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease.Token == "old-token" {
				t.Fatal("reclaim preserved old token")
			}
			if err := loop.ReleaseLease(lease); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSSHDClockStepsDoNotStealLiveLease(t *testing.T) {
	h := newSSHDHarness(t)
	for _, tt := range []struct {
		name     string
		lastSeen time.Time
	}{
		{name: "forward-hours", lastSeen: time.Now().Add(-4 * time.Hour)},
		{name: "backward-hours", lastSeen: time.Now().Add(4 * time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := "clock-" + tt.name
			lease, err := loop.AcquireServeLease(h.cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := loop.ReadServeClaim(h.cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			claim.LastSeen = tt.lastSeen
			writeClaim(t, h.cfg, *claim, nil)
			before, err := os.ReadFile(claimPath(h.cfg, id))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loop.AcquireServeLease(h.cfg, id); !errors.Is(err, loop.ErrLeaseHeld) {
				t.Fatalf("competing acquire = %v", err)
			}
			after, err := os.ReadFile(claimPath(h.cfg, id))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("competing acquire changed live claim bytes")
			}
			if err := loop.ReleaseLease(lease); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSSHDHalfOpenClientHitsHubReadDeadlineAndReleases(t *testing.T) {
	h := newSSHDHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	transport, err := h.open(ctx, "codex", "discard-this-command", hubclient.SSHBuildOptions{Channel: true})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	channel, err := hubclient.OpenChannelTransport(transport, h.remote, "codex", "sshd-integration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channel.AcquireLease(op.LeaseReq{}); err != nil {
		t.Fatal(err)
	}
	if err := transport.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.cmd.Process.Signal(syscall.SIGCONT) }()

	// The stopped client leaves the server's forced-command stdin open but
	// silent. The hub-side watchdog must break that half-open read and return
	// through the session's existing deferred lease release.
	waitClaimAbsent(t, h.cfg, "codex", 25*time.Second)
}

func TestSSHDBootRefSurvivesHeartbeatAndUnknownKeyDrops(t *testing.T) {
	h := newSSHDHarness(t)
	lease, err := loop.AcquireServeLease(h.cfg, "heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := loop.ReadServeClaim(h.cfg, "heartbeat")
	if err != nil || claim.BootRef == "" {
		t.Fatalf("initial claim = %+v, %v", claim, err)
	}
	bootRef := claim.BootRef
	writeClaim(t, h.cfg, *claim, map[string]any{"future_field": "tolerated"})
	for i := 0; i < 3; i++ {
		if err := loop.RenewLease(lease); err != nil {
			t.Fatal(err)
		}
		got, err := loop.ReadServeClaim(h.cfg, "heartbeat")
		if err != nil || got.BootRef != bootRef {
			t.Fatalf("renew %d boot_ref = %q, %v", i, got.BootRef, err)
		}
	}
	data, err := os.ReadFile(claimPath(h.cfg, "heartbeat"))
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if _, present := object["future_field"]; present {
		t.Fatal("unknown claim key survived RenewLease")
	}
	if err := loop.ReleaseLease(lease); err != nil {
		t.Fatal(err)
	}
}

func openChannel(t *testing.T, h *sshdHarness, id string) (*hubclient.Channel, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	transport, err := h.open(ctx, id, "discard-this-command", hubclient.SSHBuildOptions{Channel: true})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	channel, err := hubclient.OpenChannelTransport(transport, h.remote, id, "sshd-integration")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return channel, cancel
}

func openOneShot(t *testing.T, h *sshdHarness, id string) (*hubclient.OneShot, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	transport, err := h.open(ctx, id, "discard-this-command", hubclient.SSHBuildOptions{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client, err := hubclient.OpenOneShotTransport(transport, h.remote, id, "sshd-integration")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return client, cancel
}

func claimPath(cfg *loop.Config, id string) string {
	return filepath.Join(cfg.AgentStateDir(id), "serve.claim")
}

func writeClaim(t *testing.T, cfg *loop.Config, claim loop.ServeClaim, extra map[string]any) {
	t.Helper()
	if err := os.MkdirAll(cfg.AgentStateDir(claim.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	for key, value := range extra {
		object[key] = value
	}
	data, err = json.MarshalIndent(object, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claimPath(cfg, claim.ID), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
