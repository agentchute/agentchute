package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

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
