package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// m4_inject_recheck_test.go — M4 (deep-analysis-v2 addendum): the runner's
// injectLoop enqueues a wake as soon as pollOnce sees unseen inbox mail, but
// previously fired injectPrompt unconditionally once the injection window
// opened — with NO re-check that the mail was still there. In the claim
// race, the agent's own `check` (triggered by an earlier cue in the same
// batch) can claim the mail (moving it inbox -> .claimed) before this later
// cue actually injects, producing a spurious "check inbox" prompt into an
// already-empty inbox. Fix: injectIfPending re-lists the inbox immediately
// before injectPrompt and skips injection if nothing is pending.

// TestInjectIfPendingSkipsWhenInboxAlreadyClaimed is the load-bearing race
// test: mail that was claimed (moved out of the raw inbox) before injection
// fires must NOT produce an injection attempt.
func TestInjectIfPendingSkipsWhenInboxAlreadyClaimed(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()

	inbox := cfg.AgentInboxDir("runner-test")
	mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\nto: runner-test\n---\n\nhi\n"))
	msgs, _, err := loop.ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("seed: got %d inbox messages, want 1", len(msgs))
	}
	// Simulate the claim race: `check` (from an earlier cue) claims the mail
	// — moves it out of the raw inbox — before THIS wake gets to inject.
	if _, err := loop.ClaimMessage(msgs[0], cfg.AgentClaimedDir("runner-test")); err != nil {
		t.Fatal(err)
	}

	rt.injectIfPending()

	log := readRunnerLog(t, cfg, "runner-test")
	if strings.Contains(log, "agentchute serve: inject prompt:") {
		t.Fatalf("injectIfPending attempted an injection into an already-claimed (empty) inbox; log:\n%s", log)
	}
}

// TestInjectIfPendingStillInjectsWhenMailPending is the non-regression half:
// a wake with mail genuinely still sitting in the inbox must still inject.
func TestInjectIfPendingStillInjectsWhenMailPending(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()

	inbox := cfg.AgentInboxDir("runner-test")
	mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\nto: runner-test\n---\n\nhi\n"))
	// Mail is genuinely still pending (never claimed).

	rt.injectIfPending()

	log := readRunnerLog(t, cfg, "runner-test")
	if !strings.Contains(log, "agentchute serve: inject prompt:") {
		t.Fatalf("injectIfPending did not attempt an injection with mail still pending; log:\n%s", log)
	}
}

// TestInjectIfPendingStillInjectsOnMalformedFile mirrors the poll-side rule
// (TestRunnerPoll_WakesOnMalformedFile): a skipped/malformed file still
// counts as pending — the agent needs to run `check` to see it quarantined
// and get the corrective notice, so the cue must not be suppressed.
func TestInjectIfPendingStillInjectsOnMalformedFile(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	rt := newPollTestRuntime(t, cfg, "runner-test")
	rt.diag = newRunnerDiagnostics(cfg, "runner-test")
	defer rt.diag.close()

	inbox := cfg.AgentInboxDir("runner-test")
	mustWrite(t, filepath.Join(inbox, "not-a-seq-name.md"), []byte("body"))

	rt.injectIfPending()

	log := readRunnerLog(t, cfg, "runner-test")
	if !strings.Contains(log, "agentchute serve: inject prompt:") {
		t.Fatalf("injectIfPending did not attempt an injection with a pending malformed file; log:\n%s", log)
	}
}
