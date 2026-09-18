package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// check_recheck_test.go pins the same-session re-check (mail-flow decision
// 2026-09-17, item B; codex review R1's test matrix).
//
// Once a lane had claimed mail in a turn, a second `check` in that same
// guarded session was denied twice over — by the PreToolUse guard's deny list
// and by check's own self-denial — while every gate phase blocks on unread
// mail. Mail landing mid-turn therefore forced the lane to END ITS TURN just
// to read it (codex yielded twice in one afternoon for exactly this). The fix
// is pure subtraction: `check` leaves the deny list and drops its self-denial.
// Everything else holds, and these rows are what say so: the latch stays
// armed and keeps denying the same commands, uncommitted residue replays as
// REDELIVERED on every check (a set latch is NOT proof that every claimed
// message was displayed — see check_latch_residue_test.go), limit and peek
// semantics are unchanged, and turn-end still archives everything exactly
// once.

// armGuard enables the guard for the current test as if this process were a
// child of `ac serve` with the given session token.
func armGuard(t *testing.T, token string) {
	t.Helper()
	t.Setenv("AGENTCHUTE_SERVE_TOKEN", token)
	t.Setenv("AGENTCHUTE_GUARD", "1")
}

// sendUnguarded delivers alice -> bob with the guard env cleared for the send
// only, then re-arms it with token. AGENTCHUTE_SERVE_TOKEN also fences send's
// MintSendStamp against the FROM agent's own serve lease, and alice never
// acquired one.
func sendUnguarded(t *testing.T, token, body string) {
	t.Helper()
	clearGuardEnv(t)
	if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", body}); err != nil {
		t.Fatalf("cmdSend(%q): %v", body, err)
	}
	armGuard(t, token)
}

func checkAs(t *testing.T, id string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"--as", id}, extra...)
	return captureStdout(t, func() error { return cmdCheck(args) })
}

func claimedNames(t *testing.T, cfg *loop.Config, id string) []string {
	t.Helper()
	msgs, err := loop.ListClaimedMessages(cfg.AgentClaimedDir(id))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(msgs))
	for i, m := range msgs {
		names[i] = m.Filename
	}
	return names
}

func inboxNames(t *testing.T, cfg *loop.Config, id string) []string {
	t.Helper()
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir(id))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(msgs))
	for i, m := range msgs {
		names[i] = m.Filename
	}
	return names
}

// redeliveredHeader / freshHeader are the two header lines printConsumedBody
// can print for a message, so a row can assert WHICH branch displayed it.
func redeliveredHeader(name string) string { return "---- " + name + " [REDELIVERED" }
func freshHeader(name string) string       { return "---- " + name + " ----" }

func mustLatchSession(t *testing.T, cfg *loop.Config, id, want string) {
	t.Helper()
	latch, err := loop.ReadGuardLatch(cfg, id)
	if err != nil {
		t.Fatalf("guard latch for %s: %v", id, err)
	}
	if latch.Session != want {
		t.Fatalf("latch.Session = %q, want %q", latch.Session, want)
	}
}

func mustNoLatch(t *testing.T, cfg *loop.Config, id string) {
	t.Helper()
	if _, err := loop.ReadGuardLatch(cfg, id); !os.IsNotExist(err) {
		t.Fatalf("latch for %s should be gone; err=%v", id, err)
	}
}

// turnEndArchived runs turn-end --json and returns the archived items,
// failing the test unless every filename is archived exactly once.
func turnEndArchived(t *testing.T, args []string) []ackItem {
	t.Helper()
	out, err := captureStdout(t, func() error { return cmdTurnEnd(args) })
	if err != nil {
		t.Fatalf("turn-end: %v\n%s", err, out)
	}
	var te turnEndJSON
	if jerr := json.Unmarshal([]byte(out), &te); jerr != nil {
		t.Fatalf("turn-end --json: %v\n%s", jerr, out)
	}
	seen := map[string]bool{}
	for _, item := range te.Archived {
		if seen[item.Filename] {
			t.Errorf("turn-end archived %s twice", item.Filename)
		}
		seen[item.Filename] = true
	}
	return te.Archived
}

// The matrix's first row, and the fix itself: claim A; B lands mid-turn; a
// second check in the SAME session redelivers A (REDELIVERED) and claims B;
// the commit gate clears with the latch still armed; turn-end archives A and
// B exactly once.
func TestRecheckSameSessionRedeliversResidueAndClaimsNewMail(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		sendUnguarded(t, "tok-1", "message A")
		first, err := checkAs(t, "bob")
		if err != nil {
			t.Fatalf("first check: %v", err)
		}
		names := claimedNames(t, cfg, "bob")
		if len(names) != 1 {
			t.Fatalf(".claimed = %v after the first check, want exactly A", names)
		}
		a := names[0]
		if !strings.Contains(first, freshHeader(a)) || strings.Contains(first, "[REDELIVERED") {
			t.Fatalf("first check must display A fresh:\n%s", first)
		}
		mustLatchSession(t, cfg, "bob", "tok-1")

		// B lands mid-turn, after the latch is armed.
		sendUnguarded(t, "tok-1", "message B")
		inbox := inboxNames(t, cfg, "bob")
		if len(inbox) != 1 {
			t.Fatalf("inbox = %v after B landed, want exactly B", inbox)
		}
		b := inbox[0]

		second, err := checkAs(t, "bob")
		if err != nil {
			t.Fatalf("second check in the same session must be allowed (this is the forced-yield fix): %v", err)
		}
		if !strings.Contains(second, redeliveredHeader(a)) {
			t.Errorf("second check must replay A as REDELIVERED:\n%s", second)
		}
		if !strings.Contains(second, freshHeader(b)) || strings.Contains(second, redeliveredHeader(b)) {
			t.Errorf("second check must claim B fresh, not replay it:\n%s", second)
		}
		if got := claimedNames(t, cfg, "bob"); len(got) != 2 {
			t.Fatalf(".claimed = %v after the second check, want A and B", got)
		}
		if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 0 {
			t.Fatalf("inbox = %d after the second check, want 0", n)
		}
		// The re-check neither cleared nor re-owned the latch.
		mustLatchSession(t, cfg, "bob", "tok-1")

		// The commit gate clears while the latch is still armed: nothing is
		// unread, and the latch is not the gate's business.
		gateOut, gateErr := captureStdout(t, func() error {
			return cmdGate([]string{"--as", "bob", "--before", "commit", "--json"})
		})
		if gateErr != nil {
			t.Fatalf("gate --before commit after the re-check: %v\n%s", gateErr, gateOut)
		}
		var gate gateStatus
		if jerr := json.Unmarshal([]byte(gateOut), &gate); jerr != nil {
			t.Fatalf("gate --json: %v\n%s", jerr, gateOut)
		}
		if gate.Blocked {
			t.Fatalf("commit gate blocked after the re-check: %+v", gate)
		}
		mustLatchSession(t, cfg, "bob", "tok-1")

		// turn-end archives A and B exactly once.
		archived := turnEndArchived(t, []string{"--as", "bob", "--vendor", "openai", "--json"})
		got := map[string]bool{}
		for _, item := range archived {
			got[item.Filename] = true
		}
		if len(archived) != 2 || !got[a] || !got[b] {
			t.Fatalf("turn-end archived %+v, want exactly A=%s and B=%s", archived, a, b)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 2 {
			t.Errorf("archive = %d, want 2", n)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Errorf(".claimed = %d after turn-end, want 0", n)
		}
		mustNoLatch(t, cfg, "bob")

		// "Exactly once": a second turn-end finds nothing more to archive.
		if again := turnEndArchived(t, []string{"--as", "bob", "--vendor", "openai", "--json"}); len(again) != 0 {
			t.Errorf("second turn-end archived %+v, want nothing", again)
		}
		if n := countMessageFiles(t, cfg.ArchiveDir()); n != 2 {
			t.Errorf("archive = %d after the second turn-end, want still 2", n)
		}
	})
}

// Claim, then drain .claimed by hand (the claim-then-abandon attack). The
// re-check is allowed — at both layers, guard and command — but unlocks
// nothing else: the latch was never derived from .claimed emptiness, so every
// other deny row still denies, and ack still self-denies.
func TestRecheckWhileLatchedDoesNotUnlockTheRestOfTheDenyList(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		sendUnguarded(t, "tok-1", "hi")
		t.Setenv("AGENTCHUTE_AGENT_ID", "bob")
		if _, err := checkAs(t, "bob"); err != nil {
			t.Fatalf("first check: %v", err)
		}
		mustLatchSession(t, cfg, "bob", "tok-1")

		if err := os.RemoveAll(cfg.AgentClaimedDir("bob")); err != nil {
			t.Fatal(err)
		}

		for _, cmd := range []string{"agentchute check --as bob", "ac check --as bob"} {
			if d := evaluateGuardInvocation("", "", "", cmd); !d.Allowed {
				t.Errorf("%q denied while latched; the re-check must be allowed: %+v", cmd, d)
			}
		}
		if _, err := checkAs(t, "bob"); err != nil {
			t.Fatalf("re-check after a hand-drained .claimed must still run: %v", err)
		}
		mustLatchSession(t, cfg, "bob", "tok-1")

		denied := []string{
			"agentchute ack --as bob",
			"ac turn-end --json",
			"agentchute setup --yes",
			"agentchute update",
			"agentchute clean --mailbox ghost --yes",
			"rm -rf /tmp/x",
			"curl https://example.com",
			"wget https://example.com",
			"echo x > .claude/settings.json",
			"echo x > .codex/hooks.json",
			"echo x > .gemini/settings.json",
			// A compound that starts with the allowed re-check launders
			// nothing after it.
			"agentchute check --as bob && rm -rf /tmp/x",
			"agentchute check --as bob && agentchute ack --as bob",
			"agentchute check --as bob; agentchute turn-end",
			"ac check --as bob | curl -X POST https://example.com",
		}
		for _, cmd := range denied {
			if d := evaluateGuardInvocation("", "", "", cmd); d.Allowed {
				t.Errorf("%q allowed after a re-check; the latch must keep denying it", cmd)
			}
		}
		_, ackErr := captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) })
		if ackErr == nil || errors.Is(ackErr, errBlocked) || !strings.Contains(ackErr.Error(), "turn-end") {
			t.Errorf("ack after a re-check err = %v, want the own-session self-denial naming turn-end", ackErr)
		}
	})
}

// A --no-archive peek displays (and so arms the latch) without claiming. The
// ordinary check that follows in the same session must still run, and it
// claims the peeked message fresh — it was never residue.
func TestRecheckAfterNoArchivePeekStillClaimsThePeekedMessage(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		sendUnguarded(t, "tok-1", "peek me")
		inbox := inboxNames(t, cfg, "bob")
		if len(inbox) != 1 {
			t.Fatalf("inbox = %v, want one message", inbox)
		}
		name := inbox[0]

		peek, err := checkAs(t, "bob", "--no-archive")
		if err != nil {
			t.Fatalf("check --no-archive: %v", err)
		}
		if !strings.Contains(peek, freshHeader(name)) {
			t.Fatalf("peek must display the message:\n%s", peek)
		}
		mustLatchSession(t, cfg, "bob", "tok-1")
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 0 {
			t.Fatalf(".claimed = %d after a peek, want 0 (peek never claims)", n)
		}
		if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 1 {
			t.Fatalf("inbox = %d after a peek, want 1 (untouched)", n)
		}

		out, err := checkAs(t, "bob")
		if err != nil {
			t.Fatalf("ordinary check after a --no-archive peek must be allowed: %v", err)
		}
		if !strings.Contains(out, freshHeader(name)) || strings.Contains(out, "[REDELIVERED") {
			t.Errorf("the peeked message is claimed fresh, not replayed as residue:\n%s", out)
		}
		if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != 0 {
			t.Errorf("inbox = %d after the claiming check, want 0", n)
		}
		if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != 1 {
			t.Errorf(".claimed = %d after the claiming check, want 1", n)
		}
		mustLatchSession(t, cfg, "bob", "tok-1")
	})
}

// --limit 1 across repeated checks in one session: each call replays every
// uncommitted message and then advances through exactly one FRESH inbox
// message. The limit budgets fresh claims only; residue replay is never
// counted against it.
func TestRecheckLimitOneAdvancesThroughFreshMailWhileReplayingResidue(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		clearGuardEnv(t)
		for _, body := range []string{"one", "two", "three"} {
			if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", body}); err != nil {
				t.Fatalf("cmdSend(%q): %v", body, err)
			}
		}
		armGuard(t, "tok-1")
		names := inboxNames(t, cfg, "bob")
		if len(names) != 3 {
			t.Fatalf("inbox = %v, want three messages", names)
		}

		for i := range names {
			out, err := checkAs(t, "bob", "--limit", "1")
			if err != nil {
				t.Fatalf("check --limit 1 #%d: %v", i+1, err)
			}
			for j := 0; j < i; j++ {
				if !strings.Contains(out, redeliveredHeader(names[j])) {
					t.Errorf("check #%d must replay %s as REDELIVERED:\n%s", i+1, names[j], out)
				}
			}
			if !strings.Contains(out, freshHeader(names[i])) {
				t.Errorf("check #%d must claim %s fresh:\n%s", i+1, names[i], out)
			}
			for j := i + 1; j < len(names); j++ {
				if strings.Contains(out, names[j]) {
					t.Errorf("check #%d displayed %s ahead of its turn:\n%s", i+1, names[j], out)
				}
			}
			remaining := len(names) - (i + 1)
			limitLine := fmt.Sprintf("(reached limit of 1; %d more pending)", remaining)
			if remaining > 0 && !strings.Contains(out, limitLine) {
				t.Errorf("check #%d missing %q:\n%s", i+1, limitLine, out)
			}
			if remaining == 0 && strings.Contains(out, "reached limit") {
				t.Errorf("check #%d reported a limit with nothing left:\n%s", i+1, out)
			}
			if n := countMessageFiles(t, cfg.AgentClaimedDir("bob")); n != i+1 {
				t.Errorf("check #%d: .claimed = %d, want %d", i+1, n, i+1)
			}
			if n := countMessageFiles(t, cfg.AgentInboxDir("bob")); n != remaining {
				t.Errorf("check #%d: inbox = %d, want %d", i+1, n, remaining)
			}
			mustLatchSession(t, cfg, "bob", "tok-1")
		}
	})
}

// Crash between checks, a relaunched session's foreign latch, or the same
// session's own latch: whatever the latch says, every uncommitted message
// replays under the checking session, and the latch ends up owned by it.
func TestRecheckReplaysEveryUncommittedMessageUnderAnyLatchState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		latch string // "" = no latch on disk
	}{
		{"no latch: crashed before arming", ""},
		{"foreign latch: relaunched session", "tok-old"},
		{"own latch: same session re-checking", "tok-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg := setupConsumeFixture(t)
			withCwd(t, root, func() {
				clearGuardEnv(t)
				claimedDir := cfg.AgentClaimedDir("bob")
				mustWriteSeqInbox(t, claimedDir, "alice", 1, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue one\n"))
				mustWriteSeqInbox(t, claimedDir, "alice", 2, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue two\n"))
				if tc.latch != "" {
					if err := loop.SetGuardLatch(cfg, "bob", tc.latch); err != nil {
						t.Fatal(err)
					}
				}
				armGuard(t, "tok-1")

				out, err := checkAs(t, "bob")
				if err != nil {
					t.Fatalf("check: %v", err)
				}
				names := claimedNames(t, cfg, "bob")
				if len(names) != 2 {
					t.Fatalf(".claimed = %v, want both residue files still held", names)
				}
				for _, name := range names {
					if !strings.Contains(out, redeliveredHeader(name)) {
						t.Errorf("%s not replayed as REDELIVERED:\n%s", name, out)
					}
				}
				mustLatchSession(t, cfg, "bob", "tok-1")
			})
		})
	}
}

// A residue read fails after zero or one displayed message; the latch is armed
// either way (check_latch_residue_test.go). The retry in the same session must
// run and display the residue that was never seen — a set latch is not proof
// of display, so nothing may be skipped on its account.
func TestRecheckAfterResidueReadFailureDisplaysTheUnseenResidue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unreadable uint64
		seenBefore int
	}{
		{"failure before anything displayed", 1, 0},
		{"failure after one displayed", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg := setupConsumeFixture(t)
			withCwd(t, root, func() {
				clearGuardEnv(t)
				claimedDir := cfg.AgentClaimedDir("bob")
				mustWriteSeqInbox(t, claimedDir, "alice", 1, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue one\n"))
				mustWriteSeqInbox(t, claimedDir, "alice", 2, []byte("---\nfrom: alice\nto: bob\n---\n\nresidue two\n"))
				blocked := filepath.Join(claimedDir, loop.MsgID{From: "alice", Seq: tc.unreadable}.Filename())
				if err := os.Chmod(blocked, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
				armGuard(t, "tok-1")

				first, err := checkAs(t, "bob")
				if err == nil {
					t.Fatal("check must fail on an unreadable claimed message")
				}
				if got := strings.Count(first, "[REDELIVERED"); got != tc.seenBefore {
					t.Fatalf("displayed %d message(s) before the failure, want %d:\n%s", got, tc.seenBefore, first)
				}
				mustLatchSession(t, cfg, "bob", "tok-1")

				if err := os.Chmod(blocked, 0o600); err != nil {
					t.Fatal(err)
				}
				second, err := checkAs(t, "bob")
				if err != nil {
					t.Fatalf("retry after the residue became readable must be allowed in the same session: %v", err)
				}
				for _, seq := range []uint64{1, 2} {
					name := loop.MsgID{From: "alice", Seq: seq}.Filename()
					if !strings.Contains(second, redeliveredHeader(name)) {
						t.Errorf("retry did not display %s:\n%s", name, second)
					}
				}
				mustLatchSession(t, cfg, "bob", "tok-1")
			})
		})
	}
}

// The hub path of the same fix: the self-denial lived in cmdCheck, which is
// the same code whether the pool is local or reached through a hub. A guarded
// remote lane claims A, B lands, and the re-check over the hub redelivers A
// and claims B; turn-end over the hub archives both once. Latch state lives
// in the client's shadow loop dir, never on the hub (no wire delta).
func TestRecheckSameSessionOverHubRedeliversAndClaims(t *testing.T) {
	h := newWI57Harness(t, "codex", "openai")
	// The harness pins codex's registration to its fixed `now`; a send checks
	// the recipient's freshness against the wall clock, so refresh it.
	writeWI57Registration(t, h.cfg, "codex", "openai", "fixture-host", time.Now().UTC())
	enrollHubAgent(t, h.cfg, "grok")
	deliverHubMessage(t, h.cfg, "grok", "codex", "message A")
	shadow := &loop.Config{LoopDir: h.remote.ShadowLoopDir}

	withCwd(t, h.root, func() {
		armGuard(t, "tok-hub")
		check := func() (string, error) {
			return captureStdout(t, func() error {
				return cmdCheck([]string{"--as", "codex", "--control-repo", h.remote.URL})
			})
		}

		first, err := check()
		if err != nil {
			t.Fatalf("first remote check: %v", err)
		}
		names := claimedNames(t, h.cfg, "codex")
		if len(names) != 1 {
			t.Fatalf("hub .claimed = %v after the first check, want exactly A", names)
		}
		a := names[0]
		if !strings.Contains(first, freshHeader(a)) {
			t.Fatalf("first remote check must display A fresh:\n%s", first)
		}
		mustLatchSession(t, shadow, "codex", "tok-hub")

		deliverHubMessage(t, h.cfg, "grok", "codex", "message B")
		inbox := inboxNames(t, h.cfg, "codex")
		if len(inbox) != 1 {
			t.Fatalf("hub inbox = %v after B landed, want exactly B", inbox)
		}
		b := inbox[0]

		second, err := check()
		if err != nil {
			t.Fatalf("second remote check in the same session must be allowed: %v", err)
		}
		if !strings.Contains(second, redeliveredHeader(a)) {
			t.Errorf("second remote check must replay A as REDELIVERED:\n%s", second)
		}
		if !strings.Contains(second, freshHeader(b)) || strings.Contains(second, redeliveredHeader(b)) {
			t.Errorf("second remote check must claim B fresh:\n%s", second)
		}
		if got := claimedNames(t, h.cfg, "codex"); len(got) != 2 {
			t.Fatalf("hub .claimed = %v after the second check, want A and B", got)
		}
		if n := countMessageFiles(t, h.cfg.AgentInboxDir("codex")); n != 0 {
			t.Fatalf("hub inbox = %d after the second check, want 0", n)
		}
		mustLatchSession(t, shadow, "codex", "tok-hub")

		archived := turnEndArchived(t, []string{"--as", "codex", "--control-repo", h.remote.URL, "--json"})
		got := map[string]bool{}
		for _, item := range archived {
			got[item.Filename] = true
		}
		if len(archived) != 2 || !got[a] || !got[b] {
			t.Fatalf("remote turn-end archived %+v, want exactly A=%s and B=%s", archived, a, b)
		}
		if n := countMessageFiles(t, h.cfg.ArchiveDir()); n != 2 {
			t.Errorf("hub archive = %d, want 2", n)
		}
		if n := countMessageFiles(t, h.cfg.AgentClaimedDir("codex")); n != 0 {
			t.Errorf("hub .claimed = %d after turn-end, want 0", n)
		}
		mustNoLatch(t, shadow, "codex")
	})
}
