package loop

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedSweepRegistration writes a minimal registration for agentID with the
// given LastSeen, for sweep staleness-threshold tests.
func seedSweepRegistration(t *testing.T, cfg *Config, agentID string, lastSeen time.Time) {
	t.Helper()
	reg := &Registration{
		AgentID:     agentID,
		Vendor:      "agentchute",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    lastSeen,
		Status:      StatusActive,
	}
	if err := WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatalf("seed registration %s: %v", agentID, err)
	}
}

// seedFreshClaim writes a fresh (non-stale) serve claim for agentID directly,
// bypassing AcquireServeLease — a fixture for "a live lease immunizes an
// old-looking row" tests.
func seedFreshClaim(t *testing.T, cfg *Config, agentID string, now time.Time) {
	t.Helper()
	writeClaim(t, cfg, &ServeClaim{
		ID:         agentID,
		Host:       "some-host",
		PID:        99999,
		ServeToken: "fresh-token-" + agentID,
		StartedAt:  now,
		LastSeen:   now,
	})
}

func TestSweepStaleRegistrationsRemovesStaleDeadRow(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	seedSweepRegistration(t, cfg, "dead", now.Add(-2*time.Hour)) // stale, no claim.
	seedSweepRegistration(t, cfg, "fresh", now.Add(-time.Minute))

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "dead" {
		t.Fatalf("removed = %v, want [dead]", removed)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("dead")); !os.IsNotExist(err) {
		t.Fatalf("dead row survived: stat err = %v", err)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("fresh")); err != nil {
		t.Fatalf("fresh row was removed: %v", err)
	}
}

// TestSweepStaleRegistrationsFreshLeaseImmunizesStaleRow: an old-looking row
// (last_seen past StaleAfter) with a FRESH serve claim must survive — a fresh
// lease proves the lane is alive even if its last registration write happens
// to predate the threshold (C12).
func TestSweepStaleRegistrationsFreshLeaseImmunizesStaleRow(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	seedSweepRegistration(t, cfg, "alive", now.Add(-2*time.Hour))
	seedFreshClaim(t, cfg, "alive", now)

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (fresh lease must immunize)", removed)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("alive")); err != nil {
		t.Fatalf("row with a fresh lease was removed: %v", err)
	}
}

func TestSweepStaleRegistrationsNeverSelf(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	seedSweepRegistration(t, cfg, "self", now.Add(-2*time.Hour))

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (never sweep self)", removed)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("self")); err != nil {
		t.Fatalf("self row was removed: %v", err)
	}
}

// TestSweepStaleRegistrationsCapHonored: 7 stale dead rows seeded, one pass
// removes exactly MaxSweepPerPass (5) — deterministically the first 5 in
// sorted-id order, per SweepStaleRegistrations' documented ordering.
func TestSweepStaleRegistrationsCapHonored(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	ids := []string{"a", "b", "c", "d", "e", "f", "g"}
	for _, id := range ids {
		seedSweepRegistration(t, cfg, id, now.Add(-2*time.Hour))
	}

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != MaxSweepPerPass {
		t.Fatalf("removed %d rows, want %d (cap)", len(removed), MaxSweepPerPass)
	}
	want := []string{"a", "b", "c", "d", "e"}
	for i, id := range want {
		if removed[i] != id {
			t.Fatalf("removed = %v, want %v (sorted, capped)", removed, want)
		}
	}
	for _, id := range []string{"f", "g"} {
		if _, err := os.Stat(cfg.AgentRegistrationPath(id)); err != nil {
			t.Fatalf("row %s beyond the cap was removed: %v", id, err)
		}
	}
}

// TestSweepStaleRegistrationsUnderLockRecheckBacksOffOnRevival: a candidate
// that gets HEARTBEATED (revived) between the outer scan and its
// per-candidate lock acquisition must survive — the re-check under
// WithAgentLock is what closes this TOCTOU window. afterSweepScanHook fires
// after the candidate list is fixed but before any lock is taken, letting
// this test deterministically simulate the race in a single goroutine.
func TestSweepStaleRegistrationsUnderLockRecheckBacksOffOnRevival(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	// Neither row has a claim yet at scan time, so both are candidates —
	// the divergence must come entirely from the hook firing in the gap
	// between the outer scan and the per-candidate lock.
	seedSweepRegistration(t, cfg, "revived", now.Add(-2*time.Hour))
	seedSweepRegistration(t, cfg, "still-dead", now.Add(-2*time.Hour))

	t.Cleanup(func() { afterSweepScanHook = nil })
	afterSweepScanHook = func() {
		// Mirrors a real revival: a fresh serve process starts between the
		// scan and the sweep's per-candidate lock, acquires the lease
		// (succeeds — no claim existed yet), and heartbeats.
		lease, err := AcquireServeLease(cfg, "revived")
		if err != nil {
			t.Fatalf("revival acquire: %v", err)
		}
		template := Registration{AgentID: "revived", Vendor: "agentchute", ControlRepo: cfg.ControlRepo, Status: StatusActive}
		if err := HeartbeatRegistration(cfg, template, lease.Token); err != nil {
			t.Fatalf("revival heartbeat: %v", err)
		}
	}

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "still-dead" {
		t.Fatalf("removed = %v, want [still-dead] (revived row must survive)", removed)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("revived")); err != nil {
		t.Fatalf("revived row was removed despite the race-closing re-check: %v", err)
	}
}

// TestSweepStaleRegistrationsCorruptRowSweptByMtime: a hand-corrupted
// registration file (fails ReadRegistration) is not immortal — its file
// mtime stands in for last_seen (C12).
func TestSweepStaleRegistrationsCorruptRowSweptByMtime(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	path := cfg.AgentRegistrationPath("corrupt")
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte("{not a valid registration")); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "corrupt" {
		t.Fatalf("removed = %v, want [corrupt] (mtime-aged corrupt row)", removed)
	}
}

// TestSweepStaleRegistrationsNeverSweepsReadmeOrExample: the allowlist skip
// (README.md, *.example.md) applies even when those files look arbitrarily
// stale by mtime.
func TestSweepStaleRegistrationsNeverSweepsReadmeOrExample(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	dir := cfg.AgentsDir()
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-24 * time.Hour)
	for _, name := range []string{"README.md", "codex.example.md"} {
		p := filepath.Join(dir, name)
		if err := atomicWriteFile(p, []byte("not a registration")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none (README/example must never sweep)", removed)
	}
	for _, name := range []string{"README.md", "codex.example.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
}

func TestSweepStaleRegistrationsMissingAgentsDirIsCleanEmpty(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	removed, err := SweepStaleRegistrations(cfg, "self", time.Now().UTC())
	if err != nil {
		t.Fatalf("missing agents dir should be a clean empty result, got err: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
}
