package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// check_latch_residue_test.go pins the arming rule the seam extraction has to
// preserve: the guard latch tracks HOLDING claimed-but-unacked mail, so
// uncommitted residue EXISTING arms it — whether or not the bytes could be
// read and displayed.
//
// The shipped pre-seam loop armed on `len(redelivered) > 0` before reading any
// body, so a residue file that fails to read left the lane latched and exiting
// non-zero. Routing the display through an event stream moved arming to the
// first emitted message, which is only reached AFTER the read succeeds — a
// reachable divergence with no coverage in either direction (opus-xhigh, PR
// #148 gate). This test is that coverage.
//
// Reachable in production two ways: a permissions change on a `.claimed` file,
// and residue larger than loop.MaxInboxMessageBytes — the hand-protocol path
// (AGENTCHUTE.md Appendix C) writes inbox files directly, so an oversized one
// is not hypothetical.
func TestGuardLatchArmsOverUnreadableClaimedResidue(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		// A crashed prior turn: mail already claimed, no latch left behind.
		claimedDir := cfg.AgentClaimedDir("bob")
		mustWriteSeqInbox(t, claimedDir, "alice", 1, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue\n"))

		entries, err := os.ReadDir(claimedDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf(".claimed = %d entries, want the one residue file", len(entries))
		}
		residue := filepath.Join(claimedDir, entries[0].Name())
		if err := os.Chmod(residue, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(residue, 0o600) })

		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "tok-unreadable")
		t.Setenv("AGENTCHUTE_GUARD", "1")

		_, checkErr := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) })
		if checkErr == nil {
			t.Fatal("check must still fail on an unreadable claimed message")
		}

		// The lane ends this turn holding claimed residue. That is precisely
		// the state the latch covers, so it must be armed even though nothing
		// was displayed.
		latch, err := loop.ReadGuardLatch(cfg, "bob")
		if err != nil {
			t.Fatalf("latch not armed over unreadable claimed residue: %v", err)
		}
		if latch.Session != "tok-unreadable" {
			t.Fatalf("latch.Session = %q, want tok-unreadable", latch.Session)
		}
	})
}
