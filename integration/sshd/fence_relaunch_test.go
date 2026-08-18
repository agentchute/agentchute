//go:build sshd_integration

package sshd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// A3 / §10.3 "`E_FENCED` single relaunch attempt".
//
// Distinct from the channel-drop rows: nothing here breaks the transport. The
// lane's serve LEASE is invalidated underneath it, so the hub fences its next
// tick. A default lane must then relaunch EXACTLY once — one new child, one new
// token — and a lane whose id has meanwhile been taken by a rival must stop
// instead, because relaunching into someone else's lease is how two live lanes
// end up writing as one agent.
//
// The real-world event this stands for: a hub-side binary upgrade invalidates
// every serve lease, which is the shipped behaviour and the reason its notice
// tells operators their supervisors will exit on the next tick.
func TestSSHDFencedLaneRelaunchesExactlyOnce(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)
	childLog := filepath.Join(h.root, "fenced-child.log")
	writeFakeCodex(t, h, childLog)
	serve := startServe(t, h, checkout, false)
	defer serve.stop()

	first := waitChildStarts(t, childLog, 1, 10*time.Second)[0]
	if first.Token == "" {
		t.Fatalf("first child carried no serve token: %+v", first)
	}

	// Fence it: invalidate the lease the lane holds. The transport stays up
	// throughout, which is what separates this row from the channel-drop ones.
	invalidated, err := loop.InvalidateAllServeLeases(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if invalidated == 0 {
		t.Fatal("no lease to invalidate; the lane never acquired one and this row would test nothing")
	}

	starts := waitChildStarts(t, childLog, 2, 30*time.Second)
	second := starts[1]
	if second.PID == first.PID {
		t.Fatalf("relaunch reused the child pid %d", first.PID)
	}
	if second.Token == first.Token || second.Token == "" {
		t.Fatalf("relaunch reused or dropped the serve token: first=%q second=%q", first.Token, second.Token)
	}
	if processExists(first.PID) {
		t.Fatalf("old child pid %d survived the relaunch", first.PID)
	}
	// EXACTLY once. A lane that relaunched once per tick would keep adding
	// children while every assertion above still passed on the first two.
	//
	// The window has to OUTLAST the relaunch period it is looking for, and it is
	// derived rather than chosen: wait for two further poll cycles to be
	// OBSERVED — each tick renews the lease, so an advancing claim LastSeen is
	// evidence a cycle ran — and only then count. The first version of this
	// slept a flat 2s against a 5s interval, so a once-per-tick storm would have
	// produced its third child after the count had already passed: the
	// assertion written to catch that failure could not see it (codex and grok,
	// PR #162 gate).
	waitClaimTicks(t, h.cfg, agentID, 2, 45*time.Second)
	if got := readChildEvents(t, childLog, "start"); len(got) != 2 {
		t.Fatalf("fenced lane started %d children, want exactly 2: %+v", len(got), got)
	}
	claim, err := loop.ReadServeClaim(h.cfg, agentID)
	if err != nil || claim.ServeToken != second.Token {
		t.Fatalf("hub claim after relaunch = %+v, %v (want the new child's token)", claim, err)
	}
}

// The rival half: the id is held by someone else when the fenced lane tries to
// come back, so the relaunch must FAIL and the lane must stop rather than run a
// second live instance of the same agent.
func TestSSHDFencedLaneStopsWhenARivalHoldsTheLease(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)
	childLog := filepath.Join(h.root, "rival-child.log")
	writeFakeCodex(t, h, childLog)
	serve := startServe(t, h, checkout, false)
	defer serve.stop()

	first := waitChildStarts(t, childLog, 1, 10*time.Second)[0]

	if _, err := loop.InvalidateAllServeLeases(h.cfg); err != nil {
		t.Fatal(err)
	}
	// A rival takes the id in the window the fence opened, and holds it for the
	// rest of the test: the lane must find it held rather than racing for it.
	rival, err := loop.AcquireServeLease(h.cfg, agentID)
	if err != nil {
		t.Fatalf("rival could not take the fenced id: %v", err)
	}
	defer func() { _ = loop.ReleaseLease(rival) }()

	if err := serve.wait(45 * time.Second); err == nil {
		t.Fatal("lane survived a fence it could not reacquire")
	}
	if got := readChildEvents(t, childLog, "start"); len(got) != 1 {
		t.Fatalf("lane started %d children against a held lease, want 1: %+v", len(got), got)
	}
	if processExists(first.PID) {
		t.Fatalf("child pid %d survived the stopped lane", first.PID)
	}
	// The rival keeps the id: the stopped lane must not have taken it back.
	claim, err := loop.ReadServeClaim(h.cfg, agentID)
	if err != nil || claim.ServeToken != rival.Token {
		t.Fatalf("claim = %+v, %v (want the rival's token %q)", claim, err, rival.Token)
	}
	if out := serve.stderr.String(); !strings.Contains(out, "already active") && !strings.Contains(out, "lease") {
		t.Fatalf("stopped lane gave no lease-related reason: %q", out)
	}
}
