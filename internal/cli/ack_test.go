package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAckCommitsUnconditionallyUnderForeignBlocker is the F7 contract (replaces
// the old CLEAR-THEN-COMMIT test): `ack` archives claimed mail UNCONDITIONALLY —
// a claimed message PLUS an unrelated finish blocker (a second, uncommitted
// unread message someone else could have dropped after `check` claimed the
// first) no longer withholds the commit. `ack` still reports (and exits 2 for)
// the remaining blocker; it just no longer holds MY already-claimed mail
// hostage to it.
func TestAckCommitsUnconditionallyUnderForeignBlocker(t *testing.T) {
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
	ack := func() (string, error) {
		t.Helper()
		var out string
		var err error
		withCwd(t, root, func() {
			out, err = captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) })
		})
		return out, err
	}

	// msg1 -> claim it (now .claimed=1, inbox=0).
	send("first")
	check()
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed = %d after first claim; want 1", n)
	}

	// msg2 arrives but is NOT claimed -> a finish blocker (unread inbox mail),
	// unrelated to the already-claimed msg1.
	send("second")
	if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 1 {
		t.Fatalf("inbox = %d after second send; want 1 unread", n)
	}

	// ack commits msg1 REGARDLESS of the unrelated blocker, and reports+exits
	// errBlocked because finish is still not clear (1 unread message remains).
	out, err := ack()
	if !errors.Is(err, errBlocked) {
		t.Fatalf("err = %v, want errBlocked (finish still blocked after commit)", err)
	}
	if !strings.Contains(out, "still blocked after commit") {
		t.Fatalf("ack did not report the remaining finish-gate blocker; out=%q", out)
	}
	if !strings.Contains(out, "acked ") {
		t.Fatalf("ack did not report committing msg1 despite the unrelated blocker; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
		t.Fatalf(".claimed = %d after unconditional ack; want 0 (committed despite the blocker)", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
		t.Fatalf("archive = %d after unconditional ack; want 1 (msg1 committed)", n)
	}

	// Claim msg2 too, then ack again: now finish IS clear => exit 0, no
	// remaining-blocker text.
	check()
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed = %d after second claim; want 1", n)
	}
	out, err = ack()
	if err != nil {
		t.Fatalf("cmdAck(bob) with clear gate: %v", err)
	}
	if strings.Contains(out, "still blocked") {
		t.Fatalf("ack reported a blocker after the gate cleared; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
		t.Fatalf(".claimed = %d after clear ack; want 0 (committed)", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 2 {
		t.Fatalf("archive = %d after clear ack; want 2", n)
	}
}

// TestAckCommitsUnconditionallyUnderForeignMalformedFile is the exact F7
// scenario named in the design doc: a THIRD PARTY drops a malformed-named file
// into MY inbox after `check` already claimed a good message. `check` never
// touched the malformed file (it doesn't parse as a message), so it isn't part
// of .claimed — but under the old CLEAR-THEN-COMMIT contract it still blocked
// `ack` from committing the unrelated, already-claimed good message. `ack`
// must now commit it anyway.
func TestAckCommitsUnconditionallyUnderForeignMalformedFile(t *testing.T) {
	root, cfg := setupConsumeFixture(t)

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "first"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
	})
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
		t.Fatalf(".claimed = %d after claim; want 1", n)
	}

	// A third party's malformed drop, AFTER check claimed the good message.
	malformed := filepath.Join(cfg.AgentInboxDir("bob"), "not-a-valid-message-name.md")
	if err := os.WriteFile(malformed, []byte("---\nfrom: ??\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out string
	var err error
	withCwd(t, root, func() {
		out, err = captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) })
	})
	if !errors.Is(err, errBlocked) {
		t.Fatalf("err = %v, want errBlocked (malformed file still blocks finish)", err)
	}
	if !strings.Contains(out, "acked ") {
		t.Fatalf("ack did not commit the unrelated already-claimed message; out=%q", out)
	}
	if !strings.Contains(strings.ToLower(out), "malformed") {
		t.Fatalf("ack did not report the malformed-file blocker; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
		t.Fatalf(".claimed = %d after ack; want 0 (committed despite the foreign malformed file)", n)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
		t.Fatalf("archive = %d after ack; want 1", n)
	}
}

// TestAckJSONReportsGateClearVsBlocked pins the JSON exit-code contract scripts
// rely on: gate_clear distinguishes "committed AND done" from "committed BUT
// still blocked" so a caller checking only ack's JSON (or exit code) never
// mistakes the latter for the former.
func TestAckJSONReportsGateClearVsBlocked(t *testing.T) {
	root, cfg := setupConsumeFixture(t)

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "only"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
	})

	// A second, uncommitted unread message keeps finish blocked.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "blocker"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
	})

	var out string
	var err error
	withCwd(t, root, func() {
		out, err = captureStdout(t, func() error { return cmdAck([]string{"--as", "bob", "--json"}) })
	})
	if !errors.Is(err, errBlocked) {
		t.Fatalf("err = %v, want errBlocked", err)
	}
	if !strings.Contains(out, `"gate_clear": false`) {
		t.Fatalf("JSON should report gate_clear=false; out=%q", out)
	}
	if !strings.Contains(out, `"count": 1`) {
		t.Fatalf("JSON should still report the committed count; out=%q", out)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
		t.Fatalf("archive = %d; want 1 (committed despite gate_clear=false)", n)
	}

	// Claim the blocker and ack again: now gate_clear=true, exit 0.
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
	})
	withCwd(t, root, func() {
		out, err = captureStdout(t, func() error { return cmdAck([]string{"--as", "bob", "--json"}) })
	})
	if err != nil {
		t.Fatalf("cmdAck --json with clear gate: %v", err)
	}
	if !strings.Contains(out, `"gate_clear": true`) {
		t.Fatalf("JSON should report gate_clear=true; out=%q", out)
	}
}

// TestAckQuietStillReturnsErrBlocked pins that --quiet only suppresses output,
// never the exit-code signal a hook/script depends on.
func TestAckQuietStillReturnsErrBlocked(t *testing.T) {
	root, cfg := setupConsumeFixture(t)

	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "first"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatalf("cmdCheck(bob): %v", err)
		}
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "blocker"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
	})

	var out string
	var err error
	withCwd(t, root, func() {
		out, err = captureStdout(t, func() error { return cmdAck([]string{"--as", "bob", "--quiet"}) })
	})
	if !errors.Is(err, errBlocked) {
		t.Fatalf("err = %v, want errBlocked even under --quiet", err)
	}
	if out != "" {
		t.Fatalf("--quiet should suppress all output; got %q", out)
	}
	if n := countMessageFiles(t, cfg.ArchiveDir()); n != 1 {
		t.Fatalf("archive = %d; want 1 (quiet must not suppress the commit)", n)
	}
}
