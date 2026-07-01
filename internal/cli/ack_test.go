package cli

import (
	"strings"
	"testing"
)

// TestAckSelfGatesOnFinishBlocker is the BUG-1 contract: `ack` is a single
// CLEAR-THEN-COMMIT — it archives claimed mail ONLY IF the finish gate is clear.
// A claimed message PLUS a finish blocker (here a second unread inbox message)
// => ack archives NOTHING (.claimed intact, at-least-once preserved across the
// blocked turn). Remove the blocker (claim it too) => ack archives everything.
func TestAckSelfGatesOnFinishBlocker(t *testing.T) {
	root, cfg := setupConsumeFixture(t)

	send := func(body string) {
		t.Helper()
		withCwd(t, root, func() {
			if err := cmdSend([]string{"--from", "alice", "--to", "bob",
				"--body", body}); err != nil {
				t.Fatalf("cmdSend(%q): %v", body, err)
			}
		})
	}
	check := func() {
		t.Helper()
		withCwd(t, root, func() {
			if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
				t.Fatalf("cmdCheck(bob): %v", err)
			}
		})
	}
	ack := func() string {
		t.Helper()
		var out string
		withCwd(t, root, func() {
			var err error
			out, err = captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) })
			if err != nil {
				t.Fatalf("cmdAck(bob): %v", err) // ack must NOT error the hook, blocked or not.
			}
		})
		return out
	}

	// msg1 -> claim it (now .claimed=1, inbox=0).
	send("first")
	check()
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed = %d after first claim; want 1", n)
	}

	// msg2 arrives but is NOT claimed -> a finish blocker (unread inbox mail).
	send("second")
	if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 1 {
		t.Fatalf("inbox = %d after second send; want 1 unread", n)
	}

	// ack must self-gate: finish NOT clear (1 unread) => archive nothing.
	out := ack()
	if !strings.Contains(out, "finish gate not clear") {
		t.Fatalf("ack did not report a blocked finish gate; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed = %d after blocked ack; want 1 (uncommitted)", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 0 {
		t.Fatalf("archive = %d after blocked ack; want 0 (nothing committed)", n)
	}

	// Remove the blocker: claim msg2 too (inbox=0, .claimed=2, redelivers msg1).
	check()
	if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 0 {
		t.Fatalf("inbox = %d after second claim; want 0", n)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 2 {
		t.Fatalf(".claimed = %d after second claim; want 2", n)
	}

	// Now the finish gate is clear => ack archives BOTH.
	out = ack()
	if strings.Contains(out, "finish gate not clear") {
		t.Fatalf("ack still reported a blocked gate after the blocker cleared; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
		t.Fatalf(".claimed = %d after clear ack; want 0 (committed)", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 2 {
		t.Fatalf("archive = %d after clear ack; want 2", n)
	}
}
