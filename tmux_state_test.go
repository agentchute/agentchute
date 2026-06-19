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
	writePeerRegAt(t, cfg, agentID, host, method, target, time.Now().UTC().Truncate(time.Second))
}

// writePeerRegAt writes a peer registration with an explicit last_seen, for the
// same-pane tie-break tests that need to order two registrations by LastSeen.
func writePeerRegAt(t *testing.T, cfg *loop.Config, agentID, host, method, target string, lastSeen time.Time) {
	t.Helper()
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		Host:        host,
		WakeMethod:  method,
		WakeTarget:  target,
		LastSeen:    lastSeen.UTC(),
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
	// now FRESH and VALID and must survive. The moved peer fails the host/target
	// predicate regardless of the tie-breaker, so ourLastSeen is immaterial here.
	writePeerReg(t, cfg, peerID, host, "tmux", "%2")

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", time.Now().UTC()))
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

	// The same-pane prune now carries a last-writer-wins tie-breaker: a peer is
	// removed only if it is OLDER than us. Write the peer with a LastSeen strictly
	// before our publish LastSeen so the happy-path removal still holds under the
	// tie-breaker (this asserts the genuine-older-duplicate case, not the
	// reciprocal case which is covered by its own test).
	ourLastSeen := time.Now().UTC().Truncate(time.Second)
	writePeerRegAt(t, cfg, peerID, host, "tmux", "%1", ourLastSeen.Add(-time.Minute))

	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("find candidates = %v, want exactly one", candidates)
	}

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourLastSeen))
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

// TestPruneSamePane_ReciprocalDeleteAvoided proves the last-writer-wins
// tie-breaker stops the mutual same-pane delete (data loss). Two agents A and B
// land on the same pane %1; each finds the other as a same-pane candidate and
// each runs its revalidated remove. Without the tie-breaker BOTH removes fire
// (the host/method/target predicate is still true for both fresh regs) and the
// pane loses BOTH registrations. With the tie-breaker the remove only fires
// against a peer that is OLDER than us; so only the older side gets deleted and
// the later writer survives.
func TestPruneSamePane_ReciprocalDeleteAvoided(t *testing.T) {
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-agent"
	T := time.Now().UTC().Truncate(time.Second)

	// Direction 1: the peer is NEWER than us (it re-registered after we wrote).
	// We must YIELD — the peer's fresh reg wins the pane and is NOT removed.
	t.Run("yield_to_newer_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		ourLastSeen := T
		// Peer's fresh reg is T+Δ (newer than our publish LastSeen).
		writePeerRegAt(t, cfg, peerID, host, "tmux", "%1", T.Add(time.Second))

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != peerID {
			t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
		}

		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourLastSeen))
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatalf("newer same-pane peer was removed; the later writer must win the pane")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); statErr != nil {
			t.Fatalf("newer peer reg must survive: %v", statErr)
		}
	})

	// Direction 2: the peer is OLDER than us. We are the later writer, so we DO
	// prune the older same-pane peer. Together with direction 1 this proves only
	// the older side is ever deleted — a single survivor, never a mutual delete.
	t.Run("prune_older_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		ourLastSeen := T
		// Peer's fresh reg is T-Δ (older than our publish LastSeen).
		writePeerRegAt(t, cfg, peerID, host, "tmux", "%1", T.Add(-time.Second))

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != peerID {
			t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
		}

		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourLastSeen))
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatalf("older same-pane peer must be pruned by the later writer")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); !os.IsNotExist(statErr) {
			t.Fatalf("older peer reg should be gone, stat err=%v", statErr)
		}
	})
}

// TestPruneSamePane_EqualLastSeenDeterministicTieBreak proves equal-LastSeen is
// broken deterministically by AgentID string order so exactly one side deletes.
// less(x) = (x.LastSeen, x.AgentID); we delete the peer only if less(peer) <
// less(us). With equal LastSeen this reduces to peer.AgentID < our.AgentID:
// the higher-AgentID side prunes the lower-AgentID peer, and the lower-AgentID
// side does NOT prune the higher-AgentID peer.
func TestPruneSamePane_EqualLastSeenDeterministicTieBreak(t *testing.T) {
	host, _ := os.Hostname()
	T := time.Now().UTC().Truncate(time.Second)
	const lowID = "agent-aaa"
	const highID = "agent-zzz"

	// Us = high AgentID, peer = low AgentID, equal LastSeen → peer is "older" by
	// the AgentID tie-break (low < high) → WE delete the peer.
	t.Run("higher_id_us_deletes_lower_id_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		writePeerRegAt(t, cfg, lowID, host, "tmux", "%1", T)

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, highID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != lowID {
			t.Fatalf("find candidates = %v, want exactly [%s]", candidates, lowID)
		}

		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(highID, host, "%1", T))
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatalf("equal-LastSeen: higher-AgentID self must delete lower-AgentID peer")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(lowID)); !os.IsNotExist(statErr) {
			t.Fatalf("lower-AgentID peer reg should be gone, stat err=%v", statErr)
		}
	})

	// Us = low AgentID, peer = high AgentID, equal LastSeen → peer is "newer" by
	// the AgentID tie-break (high > low) → we do NOT delete; we yield.
	t.Run("lower_id_us_does_not_delete_higher_id_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		writePeerRegAt(t, cfg, highID, host, "tmux", "%1", T)

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, lowID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != highID {
			t.Fatalf("find candidates = %v, want exactly [%s]", candidates, highID)
		}

		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(lowID, host, "%1", T))
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatalf("equal-LastSeen: lower-AgentID self must NOT delete higher-AgentID peer")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(highID)); statErr != nil {
			t.Fatalf("higher-AgentID peer reg must survive: %v", statErr)
		}
	})
}

// TestPruneSamePane_DeletesGenuineOlderDuplicate guards the real dedup path: a
// peer strictly older than us on the same pane is a genuine stale duplicate and
// must still be pruned by the new registrant (no regression from the tie-break).
func TestPruneSamePane_DeletesGenuineOlderDuplicate(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-stale-dup"

	ourLastSeen := time.Now().UTC().Truncate(time.Second)
	// Peer is a much older duplicate on the same pane.
	writePeerRegAt(t, cfg, peerID, host, "tmux", "%1", ourLastSeen.Add(-time.Hour))

	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AgentID != peerID {
		t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
	}

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourLastSeen))
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatalf("genuine older same-pane duplicate must be pruned")
	}
	if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); !os.IsNotExist(statErr) {
		t.Fatalf("older duplicate peer reg should be gone, stat err=%v", statErr)
	}
}
