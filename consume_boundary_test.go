package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// consume_boundary_test.go proves the Gate 5 two-phase consume across a REAL
// cross-turn boundary, the property the in-memory conformance C1 cannot model:
// each cmd* call re-reads disk, so a separate cmdCheck/cmdAck pair IS a faithful
// turn boundary, and a "crash" is simply NOT calling ack between two checks.

// setupConsumeFixture registers alice + bob in a fresh control repo and returns
// the loop config. Both are pokable peers so sends have a wake target; tests
// pass --no-wake to keep the poke out of the way.
func setupConsumeFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		// alice gets an explicit (different) host so registering bob on this host
		// does not prune alice as a stale same-host tmux peer.
		if err := cmdRegister([]string{"--as", "alice", "--vendor", "anthropic", "--host", "peer-host", "--wake-method", "tmux", "--wake-target", "%1"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdRegister([]string{"--as", "bob", "--vendor", "openai", "--wake-method", "tmux", "--wake-target", "%2"}); err != nil {
			t.Fatal(err)
		}
	})
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg
}

// countMessageFiles counts non-dot regular files in dir (0 if dir is missing).
// The .claimed subdir of an inbox is dot-prefixed AND a directory, so it never
// counts as an inbox message; claimed messages themselves carry their canonical
// (non-dot) name.
func countMessageFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n++
	}
	return n
}

// TestConsumeBoundary_ClaimRedeliverAck is the load-bearing Gate 5 test:
// check CLAIMS (no archive) → crash (no ack) RE-DELIVERS → ack COMMITS
// (archives) → subsequent check does not re-display → ack again is idempotent.
func TestConsumeBoundary_ClaimRedeliverAck(t *testing.T) {
	root, cfg := setupConsumeFixture(t)

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob",
			"--task", "greet", "--body", "hello bob", "--no-wake"}); err != nil {
			t.Fatal(err)
		}
	})

	checkBob := func() string {
		t.Helper()
		var out string
		withCwd(t, root, func() {
			var err error
			out, err = captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) })
			if err != nil {
				t.Fatalf("cmdCheck(bob): %v", err)
			}
		})
		return out
	}
	ackBob := func() {
		t.Helper()
		withCwd(t, root, func() {
			if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) }); err != nil {
				t.Fatalf("cmdAck(bob): %v", err)
			}
		})
	}

	// Turn 1: CLAIM + display. File leaves inbox for .claimed; nothing archived.
	out := checkBob()
	if !strings.Contains(out, "hello bob") {
		t.Fatalf("first check did not display the body; out=%q", out)
	}
	if strings.Contains(out, "REDELIVERED") {
		t.Fatalf("first check must NOT mark redelivery; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 0 {
		t.Fatalf("inbox has %d message(s) after claim; want 0", n)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed has %d message(s) after claim; want 1", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 0 {
		t.Fatalf("archive has %d message(s) before ack; want 0", n)
	}

	// Turn 2 WITHOUT ack == a crash between check and finish. The message is
	// RE-DELIVERED (at-least-once), still in .claimed, still not archived.
	out = checkBob()
	if !strings.Contains(out, "REDELIVERED") {
		t.Fatalf("crash re-check did not mark REDELIVERED; out=%q", out)
	}
	if !strings.Contains(out, "hello bob") {
		t.Fatalf("crash re-check did not re-display the body; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed has %d after redelivery; want 1", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 0 {
		t.Fatalf("archive has %d after redelivery; want 0", n)
	}

	// ack: COMMIT. .claimed empties; the message is archived exactly once.
	ackBob()
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
		t.Fatalf(".claimed has %d after ack; want 0", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
		t.Fatalf("archive has %d after ack; want 1", n)
	}

	// Post-ack check: nothing to re-display.
	out = checkBob()
	if strings.Contains(out, "hello bob") || strings.Contains(out, "REDELIVERED") {
		t.Fatalf("post-ack check re-displayed a committed message; out=%q", out)
	}

	// ack again: idempotent no-op (no error, archive count unchanged).
	ackBob()
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
		t.Fatalf("archive has %d after second ack; want 1 (idempotent)", n)
	}
}

// TestConsumeBoundary_NoArchiveDoesNotClaim confirms --no-archive is a true dry
// run: it displays without moving the message into .claimed.
func TestConsumeBoundary_NoArchiveDoesNotClaim(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "dry run", "--no-wake"}); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob", "--no-archive"}) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "dry run") {
			t.Fatalf("--no-archive did not display the body; out=%q", out)
		}
	})
	if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 1 {
		t.Fatalf("--no-archive moved the message; inbox has %d, want 1", n)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
		t.Fatalf("--no-archive claimed the message; .claimed has %d, want 0", n)
	}
}

// TestOwedFlip_RecordClearExpireGateWarn proves the asker-owned obligation
// lifecycle: send --ask RECORDS .owed; a reply carrying in_reply_to=<ref> CLEARS
// it on the asker's check; a past-deadline entry surfaces via ExpiredOwed and as
// a NON-BLOCKING gate warning.
func TestOwedFlip_RecordClearExpireGateWarn(t *testing.T) {
	root, cfg := setupConsumeFixture(t)

	// alice ASKS bob → alice records an owed obligation keyed (to=bob, from=alice, seq=1).
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob",
			"--task", "review", "--ask", "--body", "please review", "--no-wake"}); err != nil {
			t.Fatal(err)
		}
	})
	owed, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	out := owed.OutstandingOwed()
	if len(out) != 1 {
		t.Fatalf("alice .owed has %d entries after --ask; want 1", len(out))
	}
	key := out[0].Key()
	if key.To != "bob" || key.From != "alice" || key.Seq != 1 {
		t.Fatalf("owed key = %+v; want {To:bob From:alice Seq:1}", key)
	}
	ref := key.RefString()

	// bob replies, echoing the ref as in_reply_to (via --reply-to).
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "bob", "--to", "alice",
			"--task", "review-reply", "--reply-to", ref, "--body", "done", "--no-wake"}); err != nil {
			t.Fatal(err)
		}
	})

	// alice consumes the reply → check parses in_reply_to and ClearOwed's it.
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(owed.OutstandingOwed()); n != 0 {
		t.Fatalf("alice .owed has %d entries after the reply; want 0 (cleared by in_reply_to flip)", n)
	}

	// Record a past-deadline obligation directly to exercise the expiry signal.
	nowT := time.Now().UTC()
	pastKey := loop.MsgID{To: "bob", From: "alice", Seq: 99}
	if err := loop.RecordOwed(cfg, "alice", pastKey, nowT.Add(-1*time.Hour), nowT.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(owed.ExpiredOwed(nowT)); n != 1 {
		t.Fatalf("ExpiredOwed = %d; want 1", n)
	}

	// gate --before finish: the expired obligation is a NON-BLOCKING warning, not
	// a blocking reason. (alice's only other state is unacked .claimed residue,
	// also non-blocking.)
	var gout string
	withCwd(t, root, func() {
		var gerr error
		gout, gerr = captureStdout(t, func() error {
			return cmdGate([]string{"--as", "alice", "--before", "finish", "--json"})
		})
		if gerr != nil {
			t.Fatalf("gate finish returned %v; expired-owed must NOT block", gerr)
		}
	})
	var st gateStatus
	if err := json.Unmarshal([]byte(gout), &st); err != nil {
		t.Fatalf("parse gate json: %v\n%s", err, gout)
	}
	if st.Blocked {
		t.Fatalf("gate blocked=true; expired-owed must be non-blocking. reasons=%v", st.Reasons)
	}
	if st.OwedExpired != 1 {
		t.Fatalf("gate OwedExpired = %d; want 1", st.OwedExpired)
	}
	foundWarn := false
	for _, w := range st.Warnings {
		if strings.Contains(w, "past deadline") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Fatalf("gate warnings missing the expired-owed signal; warnings=%v", st.Warnings)
	}
}
