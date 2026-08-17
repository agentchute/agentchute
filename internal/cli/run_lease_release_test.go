package cli

import (
	"os"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// run_lease_release_test.go covers the one production path no unit test reached:
// runWrapper's own construction of runnerRuntime, and the deferred lease release
// that depends on it.
//
// The regression it exists to catch: the runtime literal gained a `channel`
// field and lost its `lease:` line, so the deferred `loop.ReleaseLease(rt.lease)`
// ran against nil, logged an error, and left `serve.claim` on disk — a lane that
// exited cleanly still looked live to every peer, and the next `serve` for that
// id would refuse to start. Every existing runner test builds its runtime
// through newPollTestRuntime, which sets the field, so the whole suite stayed
// green while production leaked a claim (codex, PR #148 gate).
//
// Driving the real runWrapper is the only way to assert this: the bug is a
// missing field at ONE construction site, and a test that builds the struct
// itself would just restate the mistake.
func TestRunWrapperReleasesItsLeaseOnCleanExit(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}

	opts := runnerOptions{
		AgentID:         "runner-test",
		Vendor:          "test",
		IntervalSeconds: 5,
		// A child that exits immediately: runWrapper returns as soon as it
		// reaps it, which is the clean-shutdown path under test.
		WrapperArgs: []string{"true"},
	}

	done := make(chan error, 1)
	go func() { done <- runWrapper(cfg, opts, root) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWrapper: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runWrapper did not return after its child exited")
	}

	// The claim must be gone. A surviving serve.claim is what makes a cleanly
	// exited lane look live to the pool.
	if _, err := os.Stat(claimPathForTest(cfg, "runner-test")); !os.IsNotExist(err) {
		t.Fatalf("serve.claim survived a clean exit (stat err = %v); the lease was never released", err)
	}
	// And it must be released, not merely absent: a fresh acquire has to
	// succeed for the next launch of this id.
	lease, err := loop.AcquireServeLease(cfg, "runner-test")
	if err != nil {
		t.Fatalf("the next serve for this id cannot start: %v", err)
	}
	_ = loop.ReleaseLease(lease)
}

func claimPathForTest(cfg *loop.Config, agentID string) string {
	return cfg.AgentStateDir(agentID) + "/serve.claim"
}
