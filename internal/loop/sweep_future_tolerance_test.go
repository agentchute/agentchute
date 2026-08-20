package loop

import (
	"math"
	"os"
	"testing"
	"time"
)

// #175, sighting 3, root-caused and now fixed.
//
// Registrations serialise last_seen with time.RFC3339 — SECOND precision —
// while the sweep captures `now` at full precision. A heartbeat landing after
// `now` that crosses a second boundary records a timestamp LATER than `now`,
// and the old clamp turned that negative age into math.MaxInt64: maximally
// stale, for a row that had just heartbeated.
//
// It presented as a CI flake and is not one. The under-lock re-check exists so
// a concurrent heartbeat beats the sweep (C12); in this window it deleted a
// live lane's registration row instead.
func TestRegistrationAgeTreatsASlightlyFutureHeartbeatAsFresh(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 400*int(time.Millisecond), time.UTC)
	rows := []struct {
		name     string
		lastSeen time.Time
		want     time.Duration
	}{
		{
			// The measured case: `now` is 12:00:00.4, the heartbeat lands at
			// 12:00:01.05 and serialises to 12:00:01 — 600ms "in the future".
			name:     "the second-precision artefact",
			lastSeen: now.Truncate(time.Second).Add(time.Second),
			want:     0,
		},
		{
			name:     "exactly at the tolerance is still fresh",
			lastSeen: now.Add(registrationFutureTolerance),
			want:     0,
		},
		{
			// The requirement in tension, and the reason the clamp existed: a
			// row dated far ahead must not become permanently sweep-immune.
			name:     "far ahead is still maximally stale",
			lastSeen: now.Add(2 * time.Hour),
			want:     math.MaxInt64,
		},
		{
			name:     "just past the tolerance is maximally stale",
			lastSeen: now.Add(registrationFutureTolerance + time.Second),
			want:     math.MaxInt64,
		},
		{
			name:     "an ordinary past heartbeat is its real age",
			lastSeen: now.Add(-90 * time.Second),
			want:     90 * time.Second,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := registrationAgeFrom(now, row.lastSeen); got != row.want {
				t.Fatalf("age = %v, want %v", got, row.want)
			}
		})
	}
}

// The end-to-end half: the same shape through SweepStaleRegistrations, made
// deterministic rather than raced for. The heartbeat lands in the gap between
// the outer scan and the per-candidate lock — the window C12 is about — and
// carries a last_seen a second ahead of the sweep's `now`, which is exactly
// what a real heartbeat crossing a second boundary writes.
func TestSweepKeepsARowWhoseHeartbeatLandedASecondAhead(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	seedSweepRegistration(t, cfg, "just-heartbeated", now.Add(-2*time.Hour))
	seedSweepRegistration(t, cfg, "still-dead", now.Add(-2*time.Hour))

	t.Cleanup(func() { afterSweepScanHook = nil })
	afterSweepScanHook = func() {
		// A second ahead of the sweep's now: the artefact, without needing a
		// real second to pass or a sleep to make it likely.
		seedSweepRegistration(t, cfg, "just-heartbeated", now.Add(time.Second))
	}

	removed, err := SweepStaleRegistrations(cfg, "self", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "still-dead" {
		t.Fatalf("removed = %v, want [still-dead] — a row that heartbeated during the pass was deleted", removed)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("just-heartbeated")); err != nil {
		t.Fatalf("the just-heartbeated row was removed: %v", err)
	}
}
