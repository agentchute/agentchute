package spectest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func AssertLeaseVectors(t *testing.T, vectors []Vector) {
	t.Helper()
	for _, vector := range vectors {
		vector := vector
		t.Run(vector.ID+"/"+vector.Kind, func(t *testing.T) {
			cfg := newLeasePool(t)
			switch vector.ID {
			case "L1":
				assertL1(t, cfg)
			case "L2":
				assertL2(t, cfg)
			case "L3":
				assertL3(t, cfg)
			case "L4":
				assertL4(t, cfg)
			default:
				t.Fatalf("unknown lease vector %s", vector.ID)
			}
		})
	}
}

func newLeasePool(t *testing.T) *loop.Config {
	t.Helper()
	root := t.TempDir()
	return &loop.Config{ControlRepo: root, LoopDir: filepath.Join(root, ".agentchute", "loop"), Vendor: "agentchute"}
}

func assertL1(t *testing.T, cfg *loop.Config) {
	if _, err := loop.AcquireServeLease(cfg, "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.AcquireServeLease(cfg, "codex"); !errors.Is(err, loop.ErrLeaseHeld) {
		t.Fatalf("second acquire = %v", err)
	}
}

func assertL2(t *testing.T, cfg *loop.Config) {
	lease, err := loop.AcquireServeLease(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")
	rewriteClaimToken(t, claimPath, "successor")
	before, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.RenewLease(lease); !errors.Is(err, loop.ErrFenced) {
		t.Fatalf("renew = %v", err)
	}
	if _, err := loop.MintSendStamp(cfg, "codex", time.Now().UTC(), lease.Token); !errors.Is(err, loop.ErrFenced) {
		t.Fatalf("mint = %v", err)
	}
	after, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("fenced heartbeat or mint changed the successor claim")
	}
	if _, err := os.Stat(filepath.Join(cfg.AgentStateDir("codex"), "send.floor")); !os.IsNotExist(err) {
		t.Fatalf("fenced mint wrote send.floor: %v", err)
	}
}

func assertL3(t *testing.T, cfg *loop.Config) {
	lease, err := loop.AcquireServeLease(cfg, "codex")
	if err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")
	rewriteClaimToken(t, claimPath, "successor")
	if err := loop.ReleaseLease(lease); !errors.Is(err, loop.ErrFenced) {
		t.Fatalf("release = %v", err)
	}
	claim, err := loop.ReadServeClaim(cfg, "codex")
	if err != nil || claim.ServeToken != "successor" {
		t.Fatalf("successor claim = %+v, %v", claim, err)
	}
}

func assertL4(t *testing.T, cfg *loop.Config) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	claim := loop.ServeClaim{
		ID: "codex", Host: host, PID: 1 << 30, ServeToken: "old",
		StartedAt: time.Now().Add(-time.Hour).UTC(), LastSeen: time.Now().Add(-time.Hour).UTC(),
	}
	claimPath := filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")
	if err := loop.EnsurePrivateDir(filepath.Dir(claimPath)); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(claim, "", "  ")
	if err := os.WriteFile(claimPath, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := loop.AcquireServeLease(cfg, "codex")
	if err != nil {
		t.Fatalf("reclaim = %v", err)
	}
	if lease.Token == "old" {
		t.Fatal("reclaim kept old token")
	}
	if err := loop.VerifyFence(cfg, "codex", "old"); !errors.Is(err, loop.ErrFenced) {
		t.Fatalf("old token = %v", err)
	}
}

func rewriteClaimToken(t *testing.T, path, token string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var claim loop.ServeClaim
	if err := json.Unmarshal(b, &claim); err != nil {
		t.Fatal(err)
	}
	claim.ServeToken = token
	b, _ = json.MarshalIndent(claim, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
