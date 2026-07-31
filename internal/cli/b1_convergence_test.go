package cli

import (
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// TestB1ConvergencePoolWithDeadRowsAndLiveServe is B1's own Done-when
// acceptance (v2.5 plan B1), proven in a test rather than by hand: a pool
// with one live serve and two dead rows converges — dead rows are gone
// after boot or a serve poll tick; the live row survives indefinitely;
// killing the serve lets its own row age out and be swept by a peer's boot;
// restarting recreates it.
//
// A very short --stale-after ("50ms") lets the test control staleness by
// backdating registration timestamps rather than sleeping in real time; a
// poll tick's lazy sweep is due immediately on its first call regardless
// (lastSweep's zero value), so no sweepInterval shrinking is needed either.
func TestB1ConvergencePoolWithDeadRowsAndLiveServe(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSetupPoolState(cfg, "runner", nil, "50ms"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	writeConvergenceRegistration(t, cfg, "dead-one", now.Add(-time.Hour))
	writeConvergenceRegistration(t, cfg, "dead-two", now.Add(-time.Hour))

	// The live serve: a real runnerRuntime (real lease, real registration).
	rt := newPollTestRuntime(t, cfg, "live-agent")

	// dead-one/dead-two gone after ONE poll tick (the "≤10min of serve" path;
	// lastSweep's zero value makes the very first tick due immediately).
	rt.pollOnce()
	for _, id := range []string{"dead-one", "dead-two"} {
		if _, err := os.Stat(cfg.AgentRegistrationPath(id)); !os.IsNotExist(err) {
			t.Fatalf("%s should be swept by serve's poll tick: stat err = %v", id, err)
		}
	}
	live, err := loop.ReadRegistration(cfg.AgentRegistrationPath("live-agent"))
	if err != nil {
		t.Fatalf("live-agent row missing after its own poll tick: %v", err)
	}
	// Registration timestamps round-trip at whole-second precision
	// (formatTimestamp uses time.RFC3339), so compare against `now` truncated
	// the same way rather than a spurious same-second sub-second mismatch.
	if live.LastSeen.Before(now.Truncate(time.Second)) {
		t.Fatalf("live-agent row was not heartbeated: LastSeen = %s", live.LastSeen)
	}

	// The live row survives across further ticks.
	rt.pollOnce()
	if _, err := os.Stat(cfg.AgentRegistrationPath("live-agent")); err != nil {
		t.Fatalf("live-agent row should survive while heartbeating: %v", err)
	}

	// Kill the serve: release its lease (no more heartbeats), then let its
	// own row age out past stale_after. A DIFFERENT agent's boot sweeps it —
	// live-agent is not that peer's self, so it is not exempt.
	if err := loop.ReleaseLease(rt.lease); err != nil {
		t.Fatal(err)
	}
	writeConvergenceRegistration(t, cfg, "live-agent", time.Now().UTC().Add(-time.Hour))

	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdBoot([]string{"--as", "peer-agent", "--vendor", "anthropic", "--json"})
		}); err != nil {
			t.Fatalf("peer boot: %v", err)
		}
	})
	if _, err := os.Stat(cfg.AgentRegistrationPath("live-agent")); !os.IsNotExist(err) {
		t.Fatalf("dead live-agent row should be swept by the peer's boot: stat err = %v", err)
	}

	// Restart recreates it.
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdBoot([]string{"--as", "live-agent", "--vendor", "test", "--json"})
		}); err != nil {
			t.Fatalf("restart boot: %v", err)
		}
	})
	if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath("live-agent")); err != nil {
		t.Fatalf("live-agent row should be recreated on restart: %v", err)
	}
}

func writeConvergenceRegistration(t *testing.T, cfg *loop.Config, agentID string, lastSeen time.Time) {
	t.Helper()
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      "test",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    lastSeen,
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath(agentID), reg); err != nil {
		t.Fatalf("seed registration %s: %v", agentID, err)
	}
}
