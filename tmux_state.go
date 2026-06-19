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
func revalidateAndRemovePeer(cfg *loop.Config, peer peerWakeStale, stillMatches func(*loop.Registration) bool) (bool, error) {
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
		if !stillMatches(fresh) {
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
func stalePeerStillMatches(cfg *loop.Config, selfID string) func(*loop.Registration) bool {
	localHost, _ := os.Hostname()
	localHost = strings.TrimSpace(localHost)
	return func(fresh *loop.Registration) bool {
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
// order on (LastSeen, AgentID) and delete the peer ONLY if it is strictly
// "older" than us: less(peer) < less(us), where less(x) = (x.LastSeen, x.AgentID)
// compared lexicographically (older LastSeen first, ties broken by AgentID "<").
//
// Correctness: in the reciprocal case A(LastSeen tA) vs B(LastSeen tB) with
// tA<tB — A's remove of B sees B newer (tB>tA) → A does NOT delete B; B's remove
// of A sees A older (tA<tB) → B DELETES A. Single survivor B (the later writer),
// never a mutual delete. A genuine older duplicate (strictly smaller LastSeen)
// is still pruned by the newer registrant. Equal LastSeen is broken
// deterministically by AgentID so exactly one side deletes — no both-skip.
//
// `ourLastSeen` is OUR publish LastSeen (the `now`/`reg.LastSeen` we just wrote);
// the comparison is the PEER's FRESH (re-read) LastSeen against it. This applies
// to the same-pane prune ONLY — the stale-peer unreachable prune
// (stalePeerStillMatches) keeps its no-tie-breaker predicate by design.
func samePaneStillMatches(selfID, host, target string, ourLastSeen time.Time) func(*loop.Registration) bool {
	host = strings.TrimSpace(host)
	target = strings.TrimSpace(target)
	return func(fresh *loop.Registration) bool {
		if fresh.AgentID == selfID ||
			strings.TrimSpace(fresh.Host) != host ||
			strings.TrimSpace(fresh.WakeMethod) != "tmux" ||
			strings.TrimSpace(fresh.WakeTarget) != target {
			return false
		}
		// Tie-breaker: remove only a peer strictly older than us by the
		// (LastSeen, AgentID) total order. A newer (or equal-time-but-
		// higher-AgentID) peer wins the pane; we yield to it.
		peerOlder := fresh.LastSeen.Before(ourLastSeen) ||
			(fresh.LastSeen.Equal(ourLastSeen) && fresh.AgentID < selfID)
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
// `ourLastSeen` is OUR publish LastSeen (the value we just wrote, == `now`); it
// feeds the last-writer-wins tie-breaker in samePaneStillMatches so a reciprocal
// same-pane delete leaves exactly one survivor (the later writer).
func revalidateAndRemoveSamePanePeers(cfg *loop.Config, peers []peerWakeStale, selfID, host, target string, ourLastSeen time.Time) ([]peerWakeStale, error) {
	match := samePaneStillMatches(selfID, host, target, ourLastSeen)
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
