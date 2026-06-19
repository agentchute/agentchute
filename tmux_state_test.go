package main

import (
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// newTmuxStateTestConfig builds a Config rooted at a temp dir with the agents
// dir created, suitable for exercising the peer-prune helpers directly.
func newTmuxStateTestConfig(t *testing.T) *loop.Config {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, root+"/AGENTCHUTE.md", []byte("# Spec"))
	mustMkdir(t, root+"/.examplecorp/loop")
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.EnsurePrivateDir(cfg.AgentsDir()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writePeerReg(t *testing.T, cfg *loop.Config, agentID, host, method, target string) {
	t.Helper()
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		Host:        host,
		WakeMethod:  method,
		WakeTarget:  target,
		LastSeen:    time.Now().UTC().Truncate(time.Second),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatal(err)
	}
}

// TestPruneSamePane_SkipsPeerThatMovedPane: a same-pane candidate is found on
// %1, but BEFORE the revalidated remove the peer rewrites its registration to a
// DIFFERENT pane (%2). The fresh, valid registration must NOT be removed and the
// peer must be reported as not-removed (data-loss defect: stale-decision delete).
func TestPruneSamePane_SkipsPeerThatMovedPane(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-moved"

	// Peer initially shares pane %1 with us.
	writePeerReg(t, cfg, peerID, host, "tmux", "%1")

	// FIND phase: candidate collected against the FINAL target %1.
	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AgentID != peerID {
		t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
	}

	// Between FIND and REMOVE the peer moved to a different pane (%2) — its reg is
	// now FRESH and VALID and must survive.
	writePeerReg(t, cfg, peerID, host, "tmux", "%2")

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1"))
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatalf("peer that moved to %%2 was removed; revalidation under the peer lock must skip it")
	}
	if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); statErr != nil {
		t.Fatalf("peer reg on %%2 must NOT be removed: %v", statErr)
	}
}

// TestPruneSamePane_RemovesStillMatchingPeer: the candidate still matches at
// remove-time, so the revalidated remove deletes it (no regression).
func TestPruneSamePane_RemovesStillMatchingPeer(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-still"

	writePeerReg(t, cfg, peerID, host, "tmux", "%1")

	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("find candidates = %v, want exactly one", candidates)
	}

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1"))
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatalf("still-matching same-pane peer must be removed")
	}
	if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); !os.IsNotExist(statErr) {
		t.Fatalf("peer reg should be gone, stat err=%v", statErr)
	}
}

// TestPruneStalePeer_SkipsPeerThatBecameValid: a stale-peer candidate is found
// (its tmux target unreachable), but before the revalidated remove the peer
// rebinds to a REACHABLE target. The now-valid reg must NOT be removed.
func TestPruneStalePeer_SkipsPeerThatBecameValid(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-revalidated"

	// Only %7 is reachable; %99 is not.
	withFakeTmuxTargets(t, "%7")

	// Peer initially bound to unreachable %99 → a stale candidate.
	writePeerReg(t, cfg, peerID, host, "tmux", "%99")

	candidates, err := findStalePeerTmuxWakeTargets(cfg, selfID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AgentID != peerID {
		t.Fatalf("stale candidates = %v, want exactly [%s]", candidates, peerID)
	}

	// Between FIND and REMOVE the peer rebinds to a reachable pane → no longer stale.
	writePeerReg(t, cfg, peerID, host, "tmux", "%7")

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], stalePeerStillMatches(cfg, selfID))
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatalf("peer that became reachable (%%7) was removed; revalidation must skip it")
	}
	if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); statErr != nil {
		t.Fatalf("revalidated peer reg must NOT be removed: %v", statErr)
	}
}
