package loop

import (
	"testing"
	"time"
)

// WI-4 Fix 3: a future-dated (clock-skewed) heartbeat must read FRESH, not
// stale. Previously PollerFreshness returned `age >= 0 && age <= threshold`,
// so a heartbeat with a slightly-future last_seen yielded a negative age and
// was treated as stale — false-blocking the gate. A small negative age is
// clamped to fresh.
func TestPollerFreshness_FutureTimestampIsFresh(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	hb := &PollerHeartbeat{
		AgentID:         "claude-code",
		Method:          "poller-run",
		IntervalSeconds: DefaultPollerIntervalSeconds,
		// last_seen is 30s in the future relative to `now` (clock skew).
		LastSeen: now.Add(30 * time.Second),
	}
	fresh, age, _ := PollerFreshness(hb, now)
	if !fresh {
		t.Fatalf("future-dated heartbeat read stale (age=%s); want fresh under clock-skew clamp", age)
	}
}
