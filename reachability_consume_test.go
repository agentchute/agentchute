package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func consumeTestCfg(t *testing.T, root string) *loop.Config {
	t.Helper()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
	if err := loop.EnsurePrivateDir(cfg.AgentsDir()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// WI-E2: a VALID cached reachability fact proves recipient liveness WITHOUT a
// live probe — even when the live wake target is currently unreachable. This is
// the self-heal payoff: a recently-proven lane is treated as live for one TTL.
func TestLiveness_ValidCacheShortCircuitsLiveProbe(t *testing.T) {
	root := t.TempDir()
	cfg := consumeTestCfg(t, root)
	agentID := "codex"

	// Runner wake at the recipient's OWNED socket path, but with NO listener →
	// the live probe is unreachable. The cache (proven moments ago) makes the
	// lane live anyway.
	target := loop.RunnerWakeTarget(cfg.RunnerSocketPath(agentID))
	now := time.Now().UTC()
	reg := &loop.Registration{
		AgentID:            agentID,
		Vendor:             "openai",
		ControlRepo:        root,
		Host:               localHostname(),
		WakeMethod:         loop.RunnerWakeMethod,
		WakeTarget:         target,
		LastSeen:           now,
		Status:             loop.StatusActive,
		ReachableAt:        &now,
		ReachabilityMethod: loop.RunnerWakeMethod,
		ReachabilityTarget: target,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}

	got := evaluateRecipientLiveness(cfg, agentID, now)
	if !got.OK {
		t.Fatalf("liveness OK = false for a valid cached fact; message=%q", got.Message)
	}
	if got.Via != "reachable-cache" {
		t.Fatalf("liveness Via = %q, want reachable-cache", got.Via)
	}
}

// WI-E2 (codex backward-compat guardrail): a registration with NO ReachableAt
// (every pre-upgrade reg) MUST fall back to the live reachability / session /
// poller behavior — never default to "unreachable".
func TestLiveness_AbsentCacheFallsBackToLive(t *testing.T) {
	root := t.TempDir()
	cfg := consumeTestCfg(t, root)
	agentID := "codex"

	// A LIVE owned runner socket and NO cached fact: the live fallback must prove
	// liveness via the wake probe.
	socketPath := cfg.RunnerSocketPath(agentID)
	startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{AgentID: agentID})
	now := time.Now().UTC()
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "openai",
		ControlRepo: root,
		Host:        localHostname(),
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  loop.RunnerWakeTarget(socketPath),
		LastSeen:    now,
		Status:      loop.StatusActive,
		// No ReachableAt — pre-upgrade shape.
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}

	got := evaluateRecipientLiveness(cfg, agentID, now)
	if !got.OK {
		t.Fatalf("absent-cache liveness OK = false; must fall back to live probe; message=%q", got.Message)
	}
	if got.Via != "wake" {
		t.Fatalf("liveness Via = %q, want wake (live fallback)", got.Via)
	}
}

// WI-E2 (codex advisory-only guardrail): the reachability cache MUST NOT
// suppress inbox delivery or the structural poke. On a cache MISS (no
// ReachableAt) `send` still enqueues the message AND attempts the wake.
func TestSendStillEnqueuesAndPokesOnCacheMiss(t *testing.T) {
	root, cfg := setupSendFixture(t)

	// Re-point the recipient (codex) at a LIVE owned runner socket with NO cached
	// reachability fact (cache miss), so the structural poke can succeed and we
	// can prove it fired despite the empty cache.
	socketPath := cfg.RunnerSocketPath("codex")
	startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{AgentID: "codex"})
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
	if err != nil {
		t.Fatal(err)
	}
	reg.Host = localHostname()
	reg.WakeMethod = loop.RunnerWakeMethod
	reg.WakeTarget = loop.RunnerWakeTarget(socketPath)
	reg.ReachableAt = nil
	reg.ReachabilityMethod = ""
	reg.ReachabilityTarget = ""
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), reg); err != nil {
		t.Fatal(err)
	}
	if reg.IsReachable(time.Now().UTC(), recipientReachabilityTTL) {
		t.Fatal("precondition: recipient cache must be a MISS for this test")
	}

	before := countInbox(cfg.AgentInboxDir("codex"))

	// Pass explicit --control-repo/--loop-dir so cmdSend's discovery matches the
	// test cfg exactly (a cwd-based discovery would resolve the macOS /private
	// symlink and compute a different owned-socket path).
	out, serr := captureStdout(t, func() error {
		return cmdSend([]string{
			"--from", "claude-code", "--to", "codex", "--body", "hi", "--json",
			"--control-repo", cfg.ControlRepo, "--loop-dir", cfg.LoopDir,
		})
	})
	if serr != nil {
		t.Fatalf("cmdSend err = %v", serr)
	}
	_ = root

	// Delivery happened despite the cache miss.
	if after := countInbox(cfg.AgentInboxDir("codex")); after != before+1 {
		t.Fatalf("inbox depth = %d, want %d (delivery must happen on cache miss)", after, before+1)
	}

	// The structural poke was attempted and succeeded.
	var res sendResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse send JSON: %v\n%s", err, out)
	}
	if !res.WakeAttempted {
		t.Fatalf("wake_attempted = false on cache miss; structural poke must always fire: %+v", res)
	}
	if res.WakeResult != "ok" {
		t.Fatalf("wake_result = %q, want ok (poke succeeded on cache miss)", res.WakeResult)
	}
}
