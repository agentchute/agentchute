package cli

import (
	"os"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// run_lease_wiring_test.go covers the construction site the seam extraction
// broke: the runner's lease has to reach BOTH the runtime handle its shutdown
// path releases and the op.Channel that adopts it for the tick, and those were
// two independent lines in a struct literal. One went missing, so every clean
// exit released nothing and left serve.claim behind — the lane looked live to
// the whole pool and the next `serve` for that id refused to start, while all
// 838 tests stayed green because every runner test builds its runtime through
// newPollTestRuntime, which did set the field (codex, PR #148 gate).
//
// newRunnerRuntime takes the lease once so the pair cannot desynchronize. This
// pins that, and pins that what the shutdown path receives is actually
// releasable.
func TestNewRunnerRuntimeWiresOneLeaseToBothHolders(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	opts := runnerOptions{AgentID: "runner-test", Vendor: "test", IntervalSeconds: 5}
	lease, err := loop.AcquireServeLease(cfg, opts.AgentID)
	if err != nil {
		t.Fatal(err)
	}

	rt := newRunnerRuntime(cfg, opts, root, lease, heartbeatTemplate(cfg, opts))

	if rt.lease != lease {
		t.Fatalf("rt.lease = %p, want the lease the constructor was given (%p)", rt.lease, lease)
	}
	if rt.channel == nil {
		t.Fatal("rt.channel is nil")
	}
	if rt.channel.Token() != lease.Token {
		t.Fatalf("channel token = %q, want the adopted lease's %q", rt.channel.Token(), lease.Token)
	}

	// The shutdown path's input must actually be releasable: a nil or foreign
	// lease there is what left the claim on disk.
	if err := loop.ReleaseLease(rt.lease); err != nil {
		t.Fatalf("the runtime's lease could not be released: %v", err)
	}
	if _, err := os.Stat(cfg.AgentStateDir(opts.AgentID) + "/serve.claim"); !os.IsNotExist(err) {
		t.Fatalf("serve.claim survived the release (stat err = %v)", err)
	}
	// And the id is launchable again, which is what a leaked claim prevents.
	next, err := loop.AcquireServeLease(cfg, opts.AgentID)
	if err != nil {
		t.Fatalf("the next serve for this id cannot start: %v", err)
	}
	_ = loop.ReleaseLease(next)
}
