package loop

import (
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
		if claim, cerr := ReadServeClaim(cfg, id); cerr == nil && !ClaimIsStale(claim, now) {
			continue // a fresh lease owns this id; the row is not dead.
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

	var removed []string
	for _, c := range candidates {
		if len(removed) >= MaxSweepPerPass {
			break
		}
		swept, err := sweepOneCandidate(cfg, c.id, c.path, threshold, now)
		if err != nil {
			return removed, err
		}
		if swept {
			removed = append(removed, c.id)
		}
	}
	return removed, nil
}

// sweepOneCandidate re-checks id's staleness (age AND lease) under
// WithAgentLock(id) and removes its registration row iff both still hold —
// closing the TOCTOU window between the outer scan and the delete.
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
		if claim, cerr := ReadServeClaim(cfg, id); cerr == nil && !ClaimIsStale(claim, now) {
			return nil // a fresh lease appeared since the outer scan.
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

// registrationAge returns path's staleness age: the parsed last_seen when the
// file parses as a registration, or the file's mtime (C12) when it doesn't —
// so a hand-corrupted or otherwise unparseable row is not immortal just
// because ReadRegistration can't recover a last_seen from it. ok is false
// only when the file itself is gone or unstatable.
func registrationAge(path string, now time.Time) (age time.Duration, ok bool) {
	if reg, err := ReadRegistration(path); err == nil {
		return now.Sub(reg.LastSeen), true
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return now.Sub(info.ModTime()), true
}
