package main

import (
	"os"
	"path/filepath"
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

func TestTmuxTargetReachableWithinHonorsTimeout(t *testing.T) {
	old := tmuxProbeBinary
	path := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxProbeBinary = path
	t.Cleanup(func() { tmuxProbeBinary = old })

	start := time.Now()
	if tmuxTargetReachableWithin("%slow", 75*time.Millisecond) {
		t.Fatal("slow tmux probe reported reachable; want timeout/unreachable")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("tmux probe ignored timeout; elapsed=%s", elapsed)
	}
}

// setPeerMtime forces a peer registration FILE's mtime, which is the same-pane
// publish-order signal the prune now uses (NOT reg.LastSeen). The same-pane
// tie-breaker reads the actual OS write time via os.Stat, so tests must drive it
// by os.Chtimes rather than by the last_seen field. Returns the mtime set.
func setPeerMtime(t *testing.T, cfg *loop.Config, agentID string, mtime time.Time) time.Time {
	t.Helper()
	path := cfg.AgentRegistrationPath(agentID)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return mtime
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
	// predicate regardless of the tie-breaker, so ourMtime is immaterial here.
	writePeerReg(t, cfg, peerID, host, "tmux", "%2")

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", time.Now()))
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

	// The same-pane prune carries a last-writer-wins tie-breaker keyed on the
	// registration FILE mtime (actual publish order), NOT reg.LastSeen. Force the
	// peer's file mtime strictly BEFORE our ourMtime so the happy-path removal
	// holds under the tie-breaker (genuine-older-duplicate by write order).
	writePeerReg(t, cfg, peerID, host, "tmux", "%1")
	peerMtime := setPeerMtime(t, cfg, peerID, time.Now().Add(-time.Minute))
	ourMtime := peerMtime.Add(time.Minute) // we wrote AFTER the peer

	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("find candidates = %v, want exactly one", candidates)
	}

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourMtime))
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

// TestSamePaneStillMatches_YieldsWhenPeerMtimeUnknown is the fail-closed guard:
// when the peer's publish mtime is UNKNOWN (the os.Stat in revalidateAndRemovePeer
// failed → mtimeKnown=false), the same-pane predicate must YIELD (return false /
// do NOT delete) even though the host/method/target match — because we cannot
// prove the peer is strictly older than us. "Delete on uncertainty" was the
// defect; the fix is "if we cannot prove the peer is older, skip". The decision
// must NOT depend on the (meaningless) peerMtime/ourMtime values when mtimeKnown
// is false: this asserts it across BOTH a peer-mtime that would otherwise sort
// older AND one that would sort newer.
func TestSamePaneStillMatches_YieldsWhenPeerMtimeUnknown(t *testing.T) {
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-agent"
	ourMtime := time.Now()

	// A fresh reg that fully MATCHES host/method/target — the ONLY thing that
	// should still spare it is mtimeKnown=false.
	fresh := &loop.Registration{
		AgentID:    peerID,
		Host:       host,
		WakeMethod: "tmux",
		WakeTarget: "%1",
	}
	match := samePaneStillMatches(selfID, host, "%1", ourMtime)

	// Sub-case A: an UNKNOWN peerMtime that, IF treated as known, would sort the
	// peer as strictly older (peerMtime < ourMtime) → would otherwise be DELETED.
	if match(fresh, ourMtime.Add(-time.Minute), false) {
		t.Fatalf("mtimeKnown=false must YIELD (no delete) even when peerMtime would sort older")
	}
	// Sub-case B: an UNKNOWN peerMtime that would sort the peer newer (peer wins
	// anyway). Still must yield — the gate is mtimeKnown, not the value.
	if match(fresh, ourMtime.Add(time.Minute), false) {
		t.Fatalf("mtimeKnown=false must YIELD (no delete) regardless of peerMtime value")
	}
	// Sub-case C: zero peerMtime (the actual value left after a failed os.Stat).
	// Zero sorts as the OLDEST possible time → under the old code it would DELETE;
	// the fail-closed gate must yield.
	if match(fresh, time.Time{}, false) {
		t.Fatalf("mtimeKnown=false with zero peerMtime must YIELD (the exact fail-closed defect)")
	}
}

// TestSamePaneStillMatches_DeletesOlderWhenMtimeKnown is the no-regression
// counterpart: with mtimeKnown=true and the peer strictly older by file mtime,
// the predicate still returns true (delete the genuine older duplicate).
func TestSamePaneStillMatches_DeletesOlderWhenMtimeKnown(t *testing.T) {
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-agent"
	ourMtime := time.Now()

	fresh := &loop.Registration{
		AgentID:    peerID,
		Host:       host,
		WakeMethod: "tmux",
		WakeTarget: "%1",
	}
	match := samePaneStillMatches(selfID, host, "%1", ourMtime)

	if !match(fresh, ourMtime.Add(-time.Minute), true) {
		t.Fatalf("mtimeKnown=true with strictly-older peer must DELETE (return true)")
	}
	// And a strictly-newer KNOWN peer still wins the pane (we yield).
	if match(fresh, ourMtime.Add(time.Minute), true) {
		t.Fatalf("mtimeKnown=true with strictly-newer peer must YIELD (return false)")
	}
}

// TestStalePeerStillMatches_IgnoresMtimeKnown proves the stale-peer (unreachable)
// prune is UNCHANGED by the new mtimeKnown arg: its delete decision is pure
// unreachability and is independent of publish order, so it returns the SAME
// result whether mtimeKnown is true or false (and regardless of peerMtime).
func TestStalePeerStillMatches_IgnoresMtimeKnown(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-agent"

	// Only %7 is reachable; %99 is not.
	withFakeTmuxTargets(t, "%7")
	match := stalePeerStillMatches(cfg, selfID)

	// Unreachable target → stale → delete, for BOTH mtimeKnown values and any mtime.
	stale := &loop.Registration{AgentID: peerID, Host: host, WakeMethod: "tmux", WakeTarget: "%99"}
	for _, known := range []bool{true, false} {
		if !match(stale, time.Now(), known) {
			t.Fatalf("unreachable stale peer must be deleted regardless of mtimeKnown=%v", known)
		}
		if !match(stale, time.Time{}, known) {
			t.Fatalf("unreachable stale peer must be deleted with zero mtime, mtimeKnown=%v", known)
		}
	}

	// Reachable target → not stale → keep, for BOTH mtimeKnown values.
	reachable := &loop.Registration{AgentID: peerID, Host: host, WakeMethod: "tmux", WakeTarget: "%7"}
	for _, known := range []bool{true, false} {
		if match(reachable, time.Now(), known) {
			t.Fatalf("reachable peer must be kept regardless of mtimeKnown=%v", known)
		}
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
// against a peer that is OLDER than us by FILE mtime (actual publish order); so
// only the older writer gets deleted and the later writer survives.
func TestPruneSamePane_ReciprocalDeleteAvoided(t *testing.T) {
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-agent"
	T := time.Now()

	// Direction 1: the peer wrote AFTER us (newer file mtime). We must YIELD —
	// the peer's fresh reg wins the pane and is NOT removed. Order is by mtime, so
	// the fixture forces the peer's file mtime later than our ourMtime.
	t.Run("yield_to_newer_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		ourMtime := T
		writePeerReg(t, cfg, peerID, host, "tmux", "%1")
		setPeerMtime(t, cfg, peerID, T.Add(time.Second)) // peer wrote after us

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != peerID {
			t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
		}

		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourMtime))
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

	// Direction 2: the peer wrote BEFORE us (older file mtime). We are the later
	// writer, so we DO prune the older same-pane peer. Together with direction 1
	// this proves only the older side is ever deleted — a single survivor.
	t.Run("prune_older_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		ourMtime := T
		writePeerReg(t, cfg, peerID, host, "tmux", "%1")
		setPeerMtime(t, cfg, peerID, T.Add(-time.Second)) // peer wrote before us

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != peerID {
			t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
		}

		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourMtime))
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

// TestPruneSamePane_EqualMtimeAgentIDTieBreak proves equal-mtime is broken
// deterministically by AgentID string order so exactly one side deletes — this
// matters on coarse-granularity filesystems where two near-simultaneous writes
// can land on the SAME mtime. less(x) = (mtime, x.AgentID); we delete the peer
// only if less(peer) < less(us). With equal mtime this reduces to
// peer.AgentID < our.AgentID: the higher-AgentID side prunes the lower-AgentID
// peer, and the lower-AgentID side does NOT prune the higher-AgentID peer.
func TestPruneSamePane_EqualMtimeAgentIDTieBreak(t *testing.T) {
	host, _ := os.Hostname()
	T := time.Now()
	const lowID = "agent-aaa"
	const highID = "agent-zzz"

	// Us = high AgentID, peer = low AgentID, EQUAL file mtime → peer is "older" by
	// the AgentID tie-break (low < high) → WE delete the peer.
	t.Run("higher_id_us_deletes_lower_id_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		writePeerReg(t, cfg, lowID, host, "tmux", "%1")
		setPeerMtime(t, cfg, lowID, T) // identical mtime to our ourMtime

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
			t.Fatalf("equal-mtime: higher-AgentID self must delete lower-AgentID peer")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(lowID)); !os.IsNotExist(statErr) {
			t.Fatalf("lower-AgentID peer reg should be gone, stat err=%v", statErr)
		}
	})

	// Us = low AgentID, peer = high AgentID, EQUAL file mtime → peer is "newer" by
	// the AgentID tie-break (high > low) → we do NOT delete; we yield.
	t.Run("lower_id_us_does_not_delete_higher_id_peer", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		writePeerReg(t, cfg, highID, host, "tmux", "%1")
		setPeerMtime(t, cfg, highID, T) // identical mtime to our ourMtime

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
			t.Fatalf("equal-mtime: lower-AgentID self must NOT delete higher-AgentID peer")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(highID)); statErr != nil {
			t.Fatalf("higher-AgentID peer reg must survive: %v", statErr)
		}
	})
}

// TestPruneSamePane_DeletesGenuineOlderDuplicate guards the real dedup path: a
// peer whose FILE was written strictly before us on the same pane is a genuine
// stale duplicate and must still be pruned by the new registrant (no regression
// from the tie-break, driven by file mtime ordering).
func TestPruneSamePane_DeletesGenuineOlderDuplicate(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-stale-dup"

	// Peer is a much older duplicate on the same pane (file written an hour ago).
	writePeerReg(t, cfg, peerID, host, "tmux", "%1")
	peerMtime := setPeerMtime(t, cfg, peerID, time.Now().Add(-time.Hour))
	ourMtime := peerMtime.Add(time.Hour)

	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AgentID != peerID {
		t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
	}

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourMtime))
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

// TestPruneSamePane_OrdersByFileMtimeNotLastSeen is the core defect guard. It
// constructs the exact misorder the rev#5 (reg.LastSeen-based) tie-breaker got
// wrong: the publish ORDER (file mtime) is INVERTED relative to the captured
// LastSeen.
//
//   - Peer B's FILE mtime = T_early  → B actually wrote FIRST.
//   - OUR ourMtime        = T_late   → WE actually wrote LAST.
//   - But B's reg.LastSeen = T_late  and our reg.LastSeen = T_early (the stall:
//     each side captured `now` BEFORE the write, and our captured-now is older
//     even though our write landed later).
//
// Correct behavior (mtime-driven): we are the later writer, so we PRUNE B.
// The OLD LastSeen-driven code would see B.LastSeen newer than ours → YIELD and
// NOT remove B → both survive. Asserting B is removed proves the decision is
// driven by file mtime, not by reg.LastSeen.
func TestPruneSamePane_OrdersByFileMtimeNotLastSeen(t *testing.T) {
	cfg := newTmuxStateTestConfig(t)
	host, _ := os.Hostname()
	const selfID = "self-agent"
	const peerID = "peer-b"

	T := time.Now().UTC().Truncate(time.Second)
	tEarly := T.Add(-time.Minute)
	tLate := T

	// Peer B: file written FIRST (mtime tEarly) but reg.LastSeen = tLate+1m (its
	// captured now is STRICTLY LATER than our ourMtime — the LastSeen order is
	// INVERTED vs the file-mtime order). The strict inequality is deliberate: it
	// removes any AgentID-tie-break confound so the OLD reg.LastSeen-based code
	// would unambiguously see B as newer and YIELD (keep B), while the new
	// mtime-based code sees B's file as older and REMOVES it.
	writePeerRegAt(t, cfg, peerID, host, "tmux", "%1", tLate.Add(time.Minute))
	setPeerMtime(t, cfg, peerID, tEarly)

	// We wrote LAST: ourMtime = tLate (later file write than B's tEarly). Under
	// the OLD code this same value is read as our reg.LastSeen, which is strictly
	// EARLIER than B's reg.LastSeen (tLate+1m) → OLD code yields to B and keeps it.
	ourMtime := tLate

	candidates, err := findSamePanePeerTmuxRegistrations(cfg, selfID, host, "%1")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AgentID != peerID {
		t.Fatalf("find candidates = %v, want exactly [%s]", candidates, peerID)
	}

	removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(selfID, host, "%1", ourMtime))
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatalf("decision must be driven by FILE mtime (B wrote first → removed), not reg.LastSeen")
	}
	if _, statErr := os.Stat(cfg.AgentRegistrationPath(peerID)); !os.IsNotExist(statErr) {
		t.Fatalf("earlier-written peer B should be pruned, stat err=%v", statErr)
	}
}

// TestPruneSamePane_StalledWriterSingleSurvivor exercises the full stall scenario
// from BOTH sides and asserts exactly ONE survivor, ordered by actual write time
// (file mtime), not by the stale captured LastSeen.
//
// Scenario: A captures now=t0 then STALLS; B captures now=t1>t0 and writes FIRST
// (file mtime t1_file_early); A then writes LATE (file mtime t0_file_late, the
// later wall-clock write) carrying stale reg.LastSeen=t0. By mtime A wrote last.
//   - B's prune of A: A's file mtime is LATER → A is "newer" → B YIELDS (keeps A).
//   - A's prune of B: B's file mtime is EARLIER → B is "older" → A REMOVES B.
//
// Result: single survivor A (the actual later writer). This is the property the
// rev#5 LastSeen tie-breaker violated (both survived).
func TestPruneSamePane_StalledWriterSingleSurvivor(t *testing.T) {
	host, _ := os.Hostname()
	const aID = "agent-a-stalled"
	const bID = "agent-b-first"

	// Wall-clock write order: B's file first, A's file later (A wrote last).
	bFileMtime := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	aFileMtime := bFileMtime.Add(time.Minute)

	// B's prune of A: B sees A; A's file is NEWER → B must YIELD (A survives).
	t.Run("B_yields_to_later_writer_A", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		// A is on the pane, file written LATE.
		writePeerReg(t, cfg, aID, host, "tmux", "%1")
		setPeerMtime(t, cfg, aID, aFileMtime)

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, bID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != aID {
			t.Fatalf("B find candidates = %v, want [%s]", candidates, aID)
		}
		// B's ourMtime is B's own (earlier) file write time.
		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(bID, host, "%1", bFileMtime))
		if err != nil {
			t.Fatal(err)
		}
		if removed {
			t.Fatalf("B must YIELD to later writer A; A wrote last (newer mtime)")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(aID)); statErr != nil {
			t.Fatalf("later writer A must survive: %v", statErr)
		}
	})

	// A's prune of B: A sees B; B's file is OLDER → A REMOVES B.
	t.Run("A_removes_earlier_writer_B", func(t *testing.T) {
		cfg := newTmuxStateTestConfig(t)
		// B is on the pane, file written FIRST (earlier).
		writePeerReg(t, cfg, bID, host, "tmux", "%1")
		setPeerMtime(t, cfg, bID, bFileMtime)

		candidates, err := findSamePanePeerTmuxRegistrations(cfg, aID, host, "%1")
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 || candidates[0].AgentID != bID {
			t.Fatalf("A find candidates = %v, want [%s]", candidates, bID)
		}
		// A's ourMtime is A's own (later) file write time.
		removed, err := revalidateAndRemovePeer(cfg, candidates[0], samePaneStillMatches(aID, host, "%1", aFileMtime))
		if err != nil {
			t.Fatal(err)
		}
		if !removed {
			t.Fatalf("A (later writer) must REMOVE earlier writer B")
		}
		if _, statErr := os.Stat(cfg.AgentRegistrationPath(bID)); !os.IsNotExist(statErr) {
			t.Fatalf("earlier writer B should be pruned, stat err=%v", statErr)
		}
	})
}
