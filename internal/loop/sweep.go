package loop

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sweep.go — the lazy stale-registration sweep (v2.5 plan B1, C11/C12). A
// registration row with no fresh serve lease and an age past the pool's
// configured StaleAfter threshold is deleted so the pool's live view stays
// honest without any agent having to explicitly deregister. Triggered from
// two places only (C11): `boot`, once, immediately after the caller
// registers itself and before it peeks its own inbox; and `serve`'s poll
// tick, at most once per sweepInterval. Never send/status/doctor/gate —
// those commands only report.

// MaxSweepPerPass caps how many stale rows a single SweepStaleRegistrations
// call removes (C12). A hard bound keeps one sweep call cheap and bounded
// even if a pool has accumulated many dead rows (e.g. after a long serve
// outage); the remainder is swept on the next trigger.
const MaxSweepPerPass = 5

// afterSweepScanHook is a test-only seam; see its call site below. nil in
// production.
var afterSweepScanHook func()

// SweepStaleRegistrations removes stale, lease-dead registration rows under
// cfg's agents directory, skipping selfID. A row is a sweep candidate when
// its age exceeds StaleAfter(cfg) AND its serve claim is absent or stale
// (ClaimIsStale) — a fresh lease immunizes an old-looking row (C12: never
// steal a live lane just because its last registration write happens to
// predate the threshold). A row that fails to parse (corrupt frontmatter)
// uses the file's mtime as its age proxy (C12) so a corrupt row is not
// immortal merely because ReadRegistration can't recover its last_seen.
//
// Candidates are re-checked (age AND lease, freshly re-read) under
// WithAgentLock(id) immediately before os.Remove — a concurrent heartbeat
// that revives the row between the initial scan and the delete wins; the
// sweep backs off rather than deleting a row that just got fresh again.
//
// A per-candidate failure (e.g. a poisoned state/<id>/.lock that is itself a
// directory, so withAgentLock cannot even open it) NEVER aborts the pass:
// the failure is accumulated and the loop continues to the next candidate,
// so one wedged id can never permanently block every other row from being
// swept (claude-code/codex review, PR #91 round 3 — the same shape codex
// independently fixed the same day in InvalidateAllServeLeases, PR #92: a
// fleet-wide maintenance operation must never be disableable by the contents
// of the pool it maintains). The returned error, when non-nil, is an
// errors.Join of every per-candidate failure; removed still reports every id
// that WAS successfully swept even when other candidates failed.
//
// Sweeping never touches inboxes, mail, or any other agent state — only the
// agents/*.md registration file itself.
func SweepStaleRegistrations(cfg *Config, selfID string, now time.Time) ([]string, error) {
	dir := cfg.AgentsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	threshold := StaleAfter(cfg)

	type candidate struct {
		id   string
		path string
	}
	var candidates []candidate

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() ||
			strings.HasPrefix(name, ".") ||
			!strings.HasSuffix(name, ".md") ||
			strings.HasSuffix(name, ".example.md") ||
			name == "README.md" {
			continue
		}
		id := strings.TrimSuffix(name, ".md")
		if ValidateAgentID(id) != nil {
			// A stray non-agent-id .md file (a hand-dropped note, a typo) is
			// not a registration candidate at all — never a sweep failure.
			// Every other directory enumerator in this codebase already
			// guards this (presence_scan.go, setup_wipe.go); an unguarded
			// candidate here would reach sweepOneCandidate's withAgentLock,
			// which hard-errors on an invalid id and would abort the WHOLE
			// pass before any later (sorted) candidate is even considered
			// (codex/claude-code review, PR #91 round 2, BLOCKER 1). Belt
			// and suspenders alongside claimProvablyDead's fail-closed
			// handling below (BLOCKER 2's fix already independently
			// immunizes an invalid id too, since ReadServeClaim validates
			// it and a validation error is not os.ErrNotExist) — this guard
			// is the cheaper, more direct fix and matches the established
			// precedent, so it is not left to that side effect alone.
			continue
		}
		if id == selfID {
			continue
		}
		path := filepath.Join(dir, name)
		age, ok := registrationAge(path, now)
		if !ok {
			continue // vanished between ReadDir and stat; nothing to sweep.
		}
		if age <= threshold {
			continue
		}
		if !claimProvablyDead(cfg, id, now) {
			continue // a fresh lease owns this id, OR we can't prove otherwise.
		}
		candidates = append(candidates, candidate{id: id, path: path})
	}

	// Deterministic order (not filesystem order) so a capped pass behaves the
	// same across repeated test runs and log reads.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })

	// afterSweepScanHook, when non-nil, runs once here — after the outer scan
	// has fixed the candidate list but before any per-candidate lock is taken.
	// Test-only (nil in production): lets a test deterministically simulate a
	// revival racing the sweep (e.g. call HeartbeatRegistration for one
	// candidate) and confirm sweepOneCandidate's under-lock re-check backs off
	// for that id instead of deleting a row that got fresh again in between.
	if afterSweepScanHook != nil {
		afterSweepScanHook()
	}

	var (
		removed  []string
		failures []error
	)
	for _, c := range candidates {
		if len(removed) >= MaxSweepPerPass {
			break
		}
		swept, err := sweepOneCandidate(cfg, c.id, c.path, threshold, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", c.id, err))
			continue
		}
		if swept {
			removed = append(removed, c.id)
		}
	}
	return removed, errors.Join(failures...)
}

// sweepOneCandidate re-checks id's staleness (age AND lease) under
// WithAgentLock(id) and removes its registration row iff both still hold —
// closing the TOCTOU window between the outer scan and the delete.
//
// Side effect, noted per review (PR #91 round 2, SHOULD-FIX 3): like every
// WithAgentLock(id) caller, this creates state/<id>/ + its .lock file as a
// side effect of taking the lock — even for a candidate that turns out to
// still be fresh (survives the re-check) and even for the id it just
// deleted. This is the same accepted tradeoff clean.go's mailbox/owed clean
// already documents (clean.go: cmdCleanOwed's doc comment, and the
// WithAgentLock(target) comment in the --mailbox apply path): the lock is
// load-bearing for correctness (it is what closes the TOCTOU window against
// a concurrent heartbeat or serve launch), and cleaning up the directory
// afterward would race that same concurrent activity for no correctness
// benefit — only cosmetic state-dir accumulation for ids that no longer have
// a live registration, which doctor/gate/status never read.
func sweepOneCandidate(cfg *Config, id, path string, threshold time.Duration, now time.Time) (bool, error) {
	var swept bool
	err := withAgentLock(cfg, id, func() error {
		age, ok := registrationAge(path, now)
		if !ok {
			return nil // already gone (raced with a concurrent sweep/removal).
		}
		if age <= threshold {
			return nil // refreshed since the outer scan; not stale anymore.
		}
		if !claimProvablyDead(cfg, id, now) {
			return nil // a fresh lease appeared, or its claim is no longer provably dead.
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		swept = true
		return syncDir(filepath.Dir(path))
	})
	if err != nil {
		return false, err
	}
	return swept, nil
}

// claimProvablyDead reports whether id's serve claim can be PROVEN dead: it
// must be genuinely absent (os.ErrNotExist) and stale-or-nonexistent is not
// enough on its own — ClaimIsStale(nil, now) already reports true for a nil
// claim, so the absent case is folded into the same ClaimIsStale check. Any
// OTHER read error (corrupt JSON, an invalid id, a symlink rejected by
// openRegularNoFollow, an oversize file, a permission error) means the claim
// cannot be read at all — and "cannot prove ownership" must fail CLOSED here,
// the same direction VerifyFence/AcquireServeLease already fail in for this
// exact ambiguity (codex/claude-code review, PR #91 round 2, BLOCKER 2: the
// prior `cerr == nil && !stale` shape failed OPEN on any non-absent error,
// so a single corrupt claim file could delete a live lane's registration —
// and worse, that lane could then neither self-heal via HeartbeatRegistration
// (VerifyFence also fails closed on a parse error) nor restart serve
// (AcquireServeLease fails closed on an unreadable claim too), silently
// vanishing it from the pool with no recovery path).
func claimProvablyDead(cfg *Config, id string, now time.Time) bool {
	claim, err := ReadServeClaim(cfg, id)
	if err == nil {
		return ClaimIsStale(claim, now)
	}
	return os.IsNotExist(err)
}

// registrationAge returns path's staleness age: the parsed last_seen when the
// file parses as a registration, or the file's mtime (C12) when it doesn't —
// so a hand-corrupted or otherwise unparseable row is not immortal just
// because ReadRegistration can't recover a last_seen from it. ok is false
// only when the file itself is gone or unstatable.
//
// A negative age (a future-dated last_seen — clock skew or a hand edit) is
// clamped to an arbitrarily-large positive value rather than left negative
// (grok review residual, PR #91 round 2, item 6): unlike a serve claim's
// staleness check, where failing a future timestamp closed as "still fresh"
// protects a live lane from being stolen, a registration row is discoverable
// display state, not a lock — a bogus future timestamp granting it permanent
// sweep-immunity is the wrong failure direction here. Erring toward eventual
// cleanup, not toward permanence, is what keeps a corrupted timestamp from
// producing an unkillable ghost row.
func registrationAge(path string, now time.Time) (age time.Duration, ok bool) {
	if reg, err := ReadRegistration(path); err == nil {
		return clampNonNegativeAge(now.Sub(reg.LastSeen)), true
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return clampNonNegativeAge(now.Sub(info.ModTime())), true
}

func clampNonNegativeAge(age time.Duration) time.Duration {
	if age < 0 {
		return math.MaxInt64
	}
	return age
}
