package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

var tmuxProbeBinary = "tmux"

func currentTmuxPane() string {
	return strings.TrimSpace(os.Getenv("TMUX_PANE"))
}

func tmuxTargetReachable(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	if _, err := exec.LookPath(tmuxProbeBinary); err != nil {
		return false
	}
	return exec.Command(tmuxProbeBinary, "list-panes", "-t", target).Run() == nil
}

type peerWakeStale struct {
	AgentID string `json:"agent_id"`
	Target  string `json:"target"`
	Path    string `json:"-"`
}

func findStalePeerTmuxWakeTargets(cfg *loop.Config, selfID string) ([]peerWakeStale, error) {
	localHost, _ := os.Hostname()
	if strings.TrimSpace(localHost) == "" {
		return nil, nil
	}
	if _, err := exec.LookPath(tmuxProbeBinary); err != nil {
		return nil, nil
	}

	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var stale []peerWakeStale
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") || name == "README.md" {
			continue
		}
		path := filepath.Join(cfg.AgentsDir(), name)
		reg, err := loop.ReadRegistration(path)
		if err != nil {
			continue
		}
		if reg.AgentID == selfID || reg.Host != localHost || strings.TrimSpace(reg.WakeMethod) != "tmux" {
			continue
		}
		target := strings.TrimSpace(reg.WakeTarget)
		if target == "" || tmuxTargetReachable(target) {
			continue
		}
		stale = append(stale, peerWakeStale{AgentID: reg.AgentID, Target: target, Path: path})
	}
	return stale, nil
}

// revalidateAndRemovePeer removes a peer's registration ONLY if, after taking
// the PEER's own agent lock and RE-READING its registration, the fresh reg still
// matches the removal criteria (`stillMatches`). This closes the lock-free
// scan-then-remove data-loss defect: a peer that moved pane / went non-tmux /
// became reachable between the candidate FIND and the REMOVE has a FRESH, VALID
// registration that must survive the prune. The find phase produces a stale
// snapshot; the decision to delete is re-made here under the peer's lock against
// the authoritative current reg.
//
// DEADLOCK SAFETY: this acquires the PEER's agent lock. Every caller invokes it
// with NO other lock held — the self-agent lock and the tmux pane lock are
// already released before any revalidated remove runs (see register.go: the
// same-pane FIND happens in-lock, the REMOVE loop runs after WithAgentLock
// returns; the stale-peer prune runs entirely outside the register critical
// section). Because at most ONE peer lock is held at a time and never while
// holding the self-agent or pane lock, there is no self-agent -> pane ->
// peer-agent chain and thus no AB-BA with a peer's own agent -> pane path.
//
// A peer reg that vanished between find and remove (ReadRegistration NotExist)
// is treated as already-gone: not reported as removed-by-us, not an error.
//
// The peer's FILE mtime is stat'd HERE, under the peer's lock — the file is
// stable (we hold the peer lock, so no concurrent rewrite) so the mtime reflects
// the peer's ACTUAL last write time. It is passed to the predicate as the
// same-pane publish-order signal (the stale-peer predicate ignores it).
//
// FAIL-CLOSED ON STAT FAILURE: if the os.Stat of the peer file fails, we CANNOT
// prove the peer's publish order, so we pass mtimeKnown=false and leave peerMtime
// zero. The same-pane predicate then YIELDS (never deletes on uncertainty),
// matching the existing vanished/corrupt-reg skip above. A zero peerMtime would
// otherwise sort as the OLDEST possible time and wrongly trigger a delete.
func revalidateAndRemovePeer(cfg *loop.Config, peer peerWakeStale, stillMatches func(*loop.Registration, time.Time, bool) bool) (bool, error) {
	if peer.Path == "" {
		return false, nil
	}
	var removed bool
	err := loop.WithAgentLock(cfg, peer.AgentID, func() error {
		fresh, rerr := loop.ReadRegistration(peer.Path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				// Already gone — nothing for us to remove; not our delete.
				return nil
			}
			// Unparseable/corrupt peer reg: leave it; the find-phase scan also
			// skips such files, so a partial write must not trigger a blind delete.
			return nil
		}
		// Peer file mtime = the peer's actual write time (stable under the peer
		// lock). Drives the same-pane publish-order tie-break; ignored by the
		// stale-peer predicate. On a stat error mtimeKnown stays false → the
		// same-pane predicate yields (fail-closed: never delete when we cannot
		// prove the peer's publish order).
		peerMtime, mtimeKnown := time.Time{}, false
		if info, serr := os.Stat(peer.Path); serr == nil {
			peerMtime = info.ModTime()
			mtimeKnown = true
		}
		if !stillMatches(fresh, peerMtime, mtimeKnown) {
			// Peer moved / rebound / became valid between find and remove. Its
			// fresh reg is authoritative; skip — this is the data-loss fix.
			return nil
		}
		if err := os.Remove(peer.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove tmux registration %q: %w", peer.AgentID, err)
		}
		removed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// stalePeerStillMatches returns the staleness predicate that
// findStalePeerTmuxWakeTargets applies, re-evaluated against a fresh peer reg:
// not us, same host, wake_method tmux, non-empty target, target UNREACHABLE.
// Kept in exact lockstep with the find scan (tmux_state.go) so the revalidated
// remove never deletes anything the scan would not have collected.
//
// The stale-peer prune has NO publish-order tie-breaker by design, so it ignores
// BOTH the `peerMtime` and `mtimeKnown` args; it only needs the
// (fresh, peerMtime, mtimeKnown) signature shared with the same-pane predicate so
// both can be threaded through revalidateAndRemovePeer. Its delete decision is
// pure unreachability — independent of publish order — so an unknown peer mtime
// does NOT change it: a vanished pane is still pruned regardless of mtimeKnown.
func stalePeerStillMatches(cfg *loop.Config, selfID string) func(*loop.Registration, time.Time, bool) bool {
	localHost, _ := os.Hostname()
	localHost = strings.TrimSpace(localHost)
	return func(fresh *loop.Registration, _ time.Time, _ bool) bool {
		if fresh.AgentID == selfID {
			return false
		}
		if localHost == "" || strings.TrimSpace(fresh.Host) != localHost {
			return false
		}
		if strings.TrimSpace(fresh.WakeMethod) != "tmux" {
			return false
		}
		target := strings.TrimSpace(fresh.WakeTarget)
		if target == "" {
			return false
		}
		return !tmuxTargetReachable(target)
	}
}

func pruneStalePeerTmuxRegistrations(cfg *loop.Config, selfID string) ([]peerWakeStale, error) {
	stale, err := findStalePeerTmuxWakeTargets(cfg, selfID)
	if err != nil {
		return nil, err
	}
	match := stalePeerStillMatches(cfg, selfID)
	var removed []peerWakeStale
	for _, peer := range stale {
		ok, rerr := revalidateAndRemovePeer(cfg, peer, match)
		if rerr != nil {
			return nil, fmt.Errorf("remove stale tmux registration %q: %w", peer.AgentID, rerr)
		}
		if ok {
			removed = append(removed, peer)
		}
	}
	return removed, nil
}

func findSamePanePeerTmuxRegistrations(cfg *loop.Config, selfID, host, target string) ([]peerWakeStale, error) {
	host = strings.TrimSpace(host)
	target = strings.TrimSpace(target)
	if host == "" || target == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var peers []peerWakeStale
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".example.md") || name == "README.md" {
			continue
		}
		path := filepath.Join(cfg.AgentsDir(), name)
		reg, err := loop.ReadRegistration(path)
		if err != nil {
			continue
		}
		if reg.AgentID == selfID || strings.TrimSpace(reg.Host) != host || strings.TrimSpace(reg.WakeMethod) != "tmux" || strings.TrimSpace(reg.WakeTarget) != target {
			continue
		}
		peers = append(peers, peerWakeStale{AgentID: reg.AgentID, Target: target, Path: path})
	}
	return peers, nil
}

// samePaneStillMatches returns the same-pane predicate that
// findSamePanePeerTmuxRegistrations applies, re-evaluated against a fresh peer
// reg: not us, same host, wake_method tmux, wake_target equal to the FINAL
// target we wrote. Kept in exact lockstep with the find scan so the revalidated
// remove never deletes a peer that has since moved pane or gone non-tmux.
//
// LAST-WRITER-WINS TIE-BREAKER (same-pane prune ONLY). The host/method/target
// predicate alone cannot catch a RECIPROCAL same-pane delete: when A and B both
// land on pane %1 and each finds the other as a candidate, at remove-time BOTH
// fresh regs still legitimately match the pane, so BOTH removes would fire and
// the pane would lose both registrations (data loss). We add a strict total
// order on (mtime, AgentID) and delete the peer ONLY if it is strictly "older"
// than us: less(peer) < less(us), where less(x) = (x.fileMtime, x.AgentID)
// compared lexicographically (older mtime first, ties broken by AgentID "<").
//
// WHY FILE MTIME, NOT reg.LastSeen: reg.LastSeen is the `now` the caller captured
// BEFORE performRegister (boot.go / self_check.go / register.go) — it is the
// PRE-write wall time, not the actual publish time. A STALLED registrant
// misorders under LastSeen: A captures t0 then stalls before the write; B
// captures t1>t0 and writes FIRST (finds no A); A then writes LATE with stale
// LastSeen=t0, finds B, and (B.LastSeen=t1 newer) YIELDS — but B never found A →
// BOTH survive (a duplicate same-pane reg). The FILE mtime is the actual OS write
// time: A wrote AFTER B ⇒ ourMtime(A) > peerMtime(B) ⇒ A sees B older ⇒ A deletes
// B; B (wrote first) never found A ⇒ single survivor A (the actual later writer).
// mtime reflects real write order; reg.LastSeen can be stale relative to it.
//
// Correctness: in the reciprocal case A(mtime mA) vs B(mtime mB) with mA<mB —
// A's remove of B sees B newer (mB>mA) → A does NOT delete B; B's remove of A
// sees A older (mA<mB) → B DELETES A. Single survivor B (the later writer), never
// a mutual delete. A genuine older duplicate (strictly smaller mtime) is still
// pruned by the newer registrant. Equal mtime — possible on coarse-granularity
// filesystems where two near-simultaneous writes share a timestamp — is broken
// deterministically by AgentID (unique per reg) so exactly one side deletes — no
// both-skip, no both-delete.
//
// `ourMtime` is OUR registration FILE's mtime, stat'd right after our write (the
// actual publish time); the comparison is the PEER's FRESH (re-read, under the
// peer lock) file mtime against it. This applies to the same-pane prune ONLY —
// the stale-peer unreachable prune (stalePeerStillMatches) keeps its
// no-tie-breaker predicate by design.
//
// FAIL-CLOSED ON UNKNOWN PEER MTIME: if the peer's mtime could not be determined
// (`mtimeKnown=false` — the os.Stat in revalidateAndRemovePeer failed), the
// publish order is UNKNOWN and we cannot prove the peer is strictly older, so the
// predicate YIELDS (returns false, no delete). This is "skip on uncertainty",
// matching the vanished/corrupt-reg skip in revalidateAndRemovePeer. A zero
// peerMtime would otherwise sort as the OLDEST possible time and wrongly delete a
// matching peer whose publish order we never established.
func samePaneStillMatches(selfID, host, target string, ourMtime time.Time) func(*loop.Registration, time.Time, bool) bool {
	host = strings.TrimSpace(host)
	target = strings.TrimSpace(target)
	return func(fresh *loop.Registration, peerMtime time.Time, mtimeKnown bool) bool {
		if fresh.AgentID == selfID ||
			strings.TrimSpace(fresh.Host) != host ||
			strings.TrimSpace(fresh.WakeMethod) != "tmux" ||
			strings.TrimSpace(fresh.WakeTarget) != target {
			return false
		}
		// Fail-closed: without the peer's publish mtime we cannot prove it is
		// strictly older than us, so yield (never delete on uncertainty). This
		// must come BEFORE the tie-break compare so a zero peerMtime can never
		// sort as "oldest" and trigger a delete.
		if !mtimeKnown {
			return false
		}
		// Tie-breaker: remove only a peer strictly older than us by the
		// (mtime, AgentID) total order. A newer (or equal-mtime-but-
		// higher-AgentID) peer wins the pane; we yield to it. The AgentID tie
		// keeps the order strict+deterministic even when a coarse FS gives two
		// near-simultaneous writes the SAME mtime.
		peerOlder := peerMtime.Before(ourMtime) ||
			(peerMtime.Equal(ourMtime) && fresh.AgentID < selfID)
		return peerOlder
	}
}

// revalidateAndRemoveSamePanePeers runs the per-peer revalidated remove for a
// set of already-collected same-pane candidates, keyed on the FINAL target.
// Split out so register.go can FIND candidates inside the self-agent + pane lock
// (write-time snapshot keyed on the FINAL written target) and run the REMOVE
// after the critical section is released. Returns the peers ACTUALLY removed.
//
// PRECONDITION: must be called with NO self-agent lock and NO pane lock held —
// it takes each PEER's agent lock via revalidateAndRemovePeer (see that func's
// deadlock argument). register.go honors this by deferring the call until after
// WithAgentLock returns.
//
// `ourMtime` is OUR registration FILE's mtime (stat'd right after our write, the
// actual publish time); it feeds the last-writer-wins tie-breaker in
// samePaneStillMatches so a reciprocal same-pane delete leaves exactly one
// survivor (the actual later writer, by real OS write order).
func revalidateAndRemoveSamePanePeers(cfg *loop.Config, peers []peerWakeStale, selfID, host, target string, ourMtime time.Time) ([]peerWakeStale, error) {
	match := samePaneStillMatches(selfID, host, target, ourMtime)
	var removed []peerWakeStale
	for _, peer := range peers {
		ok, rerr := revalidateAndRemovePeer(cfg, peer, match)
		if rerr != nil {
			return nil, fmt.Errorf("remove same-pane tmux registration %q: %w", peer.AgentID, rerr)
		}
		if ok {
			removed = append(removed, peer)
		}
	}
	return removed, nil
}

// tmuxPaneLockObserver, when non-nil, is invoked with the target each time
// withTmuxPaneRegistrationLock actually acquires a pane lock (i.e. the real
// host+target path, not the empty-target passthrough). Test-only observation
// seam; nil in production. It fires while the lock is held, before fn runs.
var tmuxPaneLockObserver func(target string)

func withTmuxPaneRegistrationLock(cfg *loop.Config, host, target string, fn func() (*registerResult, error)) (*registerResult, error) {
	host = strings.TrimSpace(host)
	target = strings.TrimSpace(target)
	if host == "" || target == "" {
		return fn()
	}

	lockRoot := filepath.Join(cfg.LoopDir, "state", "locks")
	if err := loop.EnsurePrivateDir(lockRoot); err != nil {
		return nil, fmt.Errorf("create tmux registration lock dir: %w", err)
	}
	sum := sha256.Sum256([]byte(host + "\x00" + target))
	lockPath := filepath.Join(lockRoot, "tmux-"+hex.EncodeToString(sum[:])[:16]+".lock")

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			defer os.Remove(lockPath)
			if tmuxPaneLockObserver != nil {
				tmuxPaneLockObserver(target)
			}
			return fn()
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire tmux registration lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for tmux registration lock for %s", target)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
