package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
// the loop config. Pull-only: sends deliver by writing the inbox unconditionally.
func setupConsumeFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		// alice gets an explicit (different) host so registering bob on this host
		// does not prune alice as a stale same-host peer.
		if err := cmdRegister([]string{"--as", "alice", "--vendor", "anthropic", "--host", "peer-host"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdRegister([]string{"--as", "bob", "--vendor", "openai"}); err != nil {
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
			"--body", "hello bob"}); err != nil {
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
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "dry run"}); err != nil {
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
			"--ask", "--body", "please review"}); err != nil {
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
			"--reply-to", ref, "--body", "done"}); err != nil {
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

func TestOwedFlip_ThirdPartyReplyDoesNotClear(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "carol", "--vendor", "google", "--host", "third-host"}); err != nil {
			t.Fatal(err)
		}
	})

	// alice asks bob; alice is owed a reply specifically by bob.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob",
			"--ask", "--body", "please review"}); err != nil {
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

	// carol forges a reply using bob's ref; consuming it must not clear bob's
	// obligation.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "carol", "--to", "alice",
			"--reply-to", ref, "--body", "spoofed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	out = owed.OutstandingOwed()
	if len(out) != 1 {
		t.Fatalf("forged third-party reply cleared obligation; alice .owed has %d entries, want 1", len(out))
	}
	if got := out[0].Key(); !got.Equal(key) {
		t.Fatalf("remaining owed key = %+v; want %+v", got, key)
	}

	// bob's actual reply still clears the obligation.
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "bob", "--to", "alice",
			"--reply-to", ref, "--body", "done"}); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(owed.OutstandingOwed()); n != 0 {
		t.Fatalf("alice .owed has %d entries after bob's reply; want 0", n)
	}
}

// TestOwedFlip_UsesFilenameSenderNotFrontmatter pins the exact refactor
// hazard the N1 fix's safety rests on: the discharge guard at check.go:259
// must key off msg.Sender (filename-derived, validated by seqFilenameRE) and
// never off the body frontmatter's `from:` field (unvalidated free text). A
// hand-crafted message can disagree between the two -- `cmdSend` cannot
// produce such a message (ComposeMessage always keeps them in sync), so both
// variants are hand-planted directly, bypassing send.
func TestOwedFlip_UsesFilenameSenderNotFrontmatter(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "carol", "--vendor", "google", "--host", "third-host"}); err != nil {
			t.Fatal(err)
		}
	})

	// alice asks bob; alice is owed a reply specifically by bob.
	arm := func() loop.OwedKey {
		withCwd(t, root, func() {
			if err := cmdSend([]string{"--from", "alice", "--to", "bob",
				"--ask", "--body", "please review"}); err != nil {
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
		return out[0].Key()
	}

	// Variant 1: filename says bob (the true owing agent); frontmatter
	// falsely claims carol. Discharge trusts the FILENAME, so this MUST
	// clear -- proving fm["from"] is never consulted by the guard.
	key1 := arm()
	withCwd(t, root, func() {
		body := "---\nfrom: carol\nin_reply_to: " + key1.RefString() + "\n---\n\nspoofed-frontmatter-real-bob-filename\n"
		mustWriteSeqInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte(body))
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(owed.OutstandingOwed()); n != 0 {
		t.Fatalf("variant 1 (filename=bob, frontmatter from=carol) did not clear; alice .owed has %d entries, want 0 -- filename identity must win", n)
	}

	// Variant 2: filename says carol (NOT the owing agent); frontmatter
	// falsely claims bob. Discharge must NOT clear -- proving a forged
	// frontmatter from: cannot impersonate the filename identity. This is
	// the exact hazard: switching the guard to fm["from"] would flip this.
	key2 := arm()
	withCwd(t, root, func() {
		body := "---\nfrom: bob\nin_reply_to: " + key2.RefString() + "\n---\n\nreal-carol-filename-spoofed-frontmatter\n"
		mustWriteSeqInbox(t, cfg.AgentInboxDir("alice"), "carol", 1, []byte(body))
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	out := owed.OutstandingOwed()
	if len(out) != 1 {
		t.Fatalf("variant 2 (filename=carol, frontmatter from=bob) incorrectly cleared; alice .owed has %d entries, want 1", len(out))
	}
	if got := out[0].Key(); !got.Equal(key2) {
		t.Fatalf("remaining owed key after variant 2 = %+v; want %+v", got, key2)
	}
}

func TestOwedFlip_ThirdPartyReplyDoesNotClear_Timestamp(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "carol", "--vendor", "google", "--host", "third-host"}); err != nil {
			t.Fatal(err)
		}
	})

	key := loop.TsID{
		To:     "bob",
		From:   "alice",
		Stamp:  "20260730T182415123456Z",
		Suffix: "11111111111111111111111111111111",
	}
	now := time.Now().UTC()
	if err := loop.RecordOwed(cfg, "alice", key, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		spoof := loop.TsID{From: "carol", Stamp: "20260730T182416000000Z", Suffix: "22222222222222222222222222222222"}
		body := "---\nfrom: carol\nin_reply_to: " + key.RefString() + "\n---\n\nspoofed\n"
		mustWriteTsInbox(t, cfg.AgentInboxDir("alice"), spoof, []byte(body))
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 1 || !owed.Owed[0].MatchesRef(key) {
		t.Fatalf("third-party timestamp reply cleared obligation: %+v", owed.Owed)
	}

	withCwd(t, root, func() {
		reply := loop.TsID{From: "bob", Stamp: "20260730T182417000000Z", Suffix: "33333333333333333333333333333333"}
		body := "---\nfrom: bob\nin_reply_to: " + key.RefString() + "\n---\n\ndone\n"
		mustWriteTsInbox(t, cfg.AgentInboxDir("alice"), reply, []byte(body))
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 0 {
		t.Fatalf("bob's timestamp reply did not clear obligation: %+v", owed.Owed)
	}
}

func TestOwedFlip_UsesFilenameSenderNotFrontmatter_Timestamp(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	withCwd(t, root, func() {
		if err := cmdRegister([]string{"--as", "carol", "--vendor", "google", "--host", "third-host"}); err != nil {
			t.Fatal(err)
		}
	})

	record := func(stamp, suffix string) loop.TsID {
		t.Helper()
		key := loop.TsID{To: "bob", From: "alice", Stamp: stamp, Suffix: suffix}
		now := time.Now().UTC()
		if err := loop.RecordOwed(cfg, "alice", key, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		return key
	}

	key1 := record("20260730T182415123456Z", "44444444444444444444444444444444")
	withCwd(t, root, func() {
		reply := loop.TsID{From: "bob", Stamp: "20260730T182418000000Z", Suffix: "55555555555555555555555555555555"}
		body := "---\nfrom: carol\nin_reply_to: " + key1.RefString() + "\n---\n\nfilename-bob\n"
		mustWriteTsInbox(t, cfg.AgentInboxDir("alice"), reply, []byte(body))
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 0 {
		t.Fatalf("filename bob did not clear timestamp obligation: %+v", owed.Owed)
	}

	key2 := record("20260730T182419000000Z", "66666666666666666666666666666666")
	withCwd(t, root, func() {
		spoof := loop.TsID{From: "carol", Stamp: "20260730T182420000000Z", Suffix: "77777777777777777777777777777777"}
		body := "---\nfrom: bob\nin_reply_to: " + key2.RefString() + "\n---\n\nfilename-carol\n"
		mustWriteTsInbox(t, cfg.AgentInboxDir("alice"), spoof, []byte(body))
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) }); err != nil {
			t.Fatal(err)
		}
	})
	owed, err = loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 1 || !owed.Owed[0].MatchesRef(key2) {
		t.Fatalf("filename carol incorrectly cleared timestamp obligation: %+v", owed.Owed)
	}
}

func TestDualReadMixedInboxClaimsQuarantinesClearsAndMatchesRefs(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	now := time.Now().UTC()
	oldOwed := loop.MsgID{To: "bob", From: "alice", Seq: 91}
	newOwed := loop.TsID{
		To:     "bob",
		From:   "alice",
		Stamp:  "20260730T182421000000Z",
		Suffix: "88888888888888888888888888888888",
	}
	if err := loop.RecordOwed(cfg, "alice", oldOwed, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := loop.RecordOwed(cfg, "alice", newOwed, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	oldMessage := loop.MsgID{To: "alice", From: "bob", Seq: 7}
	newMessage := loop.TsID{
		To:     "alice",
		From:   "bob",
		Stamp:  "20260730T182422000000Z",
		Suffix: "99999999999999999999999999999999",
	}
	oldBody := "---\nfrom: bob\nreply_required: true\nin_reply_to: " + oldOwed.RefString() + "\n---\n\nold-format-body\n"
	newBody := "---\nfrom: bob\nreply_required: true\nin_reply_to: " + newOwed.RefString() + "\n---\n\nnew-format-body\n"
	mustWriteSeqInbox(t, cfg.AgentInboxDir("alice"), oldMessage.From, oldMessage.Seq, []byte(oldBody))
	mustWriteTsInbox(t, cfg.AgentInboxDir("alice"), newMessage, []byte(newBody))
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("alice"), "garbage.md"), []byte("bad\n"))

	var out string
	withCwd(t, root, func() {
		var err error
		out, err = captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"old-format-body",
		"new-format-body",
		oldMessage.RefString(),
		newMessage.RefString(),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("check output missing %q:\n%s", want, out)
		}
	}

	claimed, err := loop.ListClaimedMessages(cfg.AgentClaimedDir("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed messages = %#v, want old and timestamp messages", claimed)
	}
	if countMessageFiles(t, cfg.MalformedDir()) != 1 {
		t.Fatalf("malformed count = %d, want quarantined garbage", countMessageFiles(t, cfg.MalformedDir()))
	}
	owed, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 0 {
		t.Fatalf("old and timestamp obligations were not both discharged: %+v", owed.Owed)
	}
}
