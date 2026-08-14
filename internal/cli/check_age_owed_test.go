package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// check_age_owed_test.go — v2.5 implementation plan slice A3: check's C18 age
// banner and C19 stale-owed prune offer.

// TestCheckPrintsAgeBannerOnOldMail proves C18: a message older than
// oldMailBannerAfter (24h) prints the age banner above its body — checked by
// POSITION (review nit), not just substring presence, so a refactor that
// moved the banner after the header would still be caught.
func TestCheckPrintsAgeBannerOnOldMail(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte("---\nfrom: bob\n---\n\nhi\n"), 3*24*time.Hour)

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	bannerIdx := strings.Index(out, "[!] STALE: sent ")
	headerIdx := strings.Index(out, "----")
	if bannerIdx == -1 {
		t.Fatalf("output missing the age banner; got:\n%s", out)
	}
	if headerIdx == -1 || bannerIdx >= headerIdx {
		t.Fatalf("age banner did not print above the ---- header (banner@%d, header@%d); got:\n%s", bannerIdx, headerIdx, out)
	}
	// The banner must carry a compact age and NAME THE SENDER, so the reader
	// knows both how old the mail is and who to confirm with. The pre-2026-08-14
	// wording ("this message is 3 days old") did neither usefully — it stated an
	// age and stopped, and the lane acted on the mail anyway.
	if !strings.Contains(out, "3d ago") {
		t.Fatalf("banner missing the compact age %q; got:\n%s", "3d ago", out)
	}
	if !strings.Contains(out, "confirm with bob before acting on it") {
		t.Fatalf("banner does not name the sender to confirm with; got:\n%s", out)
	}
}

// TestCheckBannerRendersHoursNotWholeDays pins the regression the 2026-08-14
// incident exposed in the banner's own wording: the triggering message was
// ~31h old, and int(age.Hours()/24) rendered that as the ungrammatical and
// misleadingly coarse "1 days old". Hour-scale mail must read in hours.
func TestCheckBannerRendersHoursNotWholeDays(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte("---\nfrom: bob\n---\n\nhi\n"), 31*time.Hour)

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if !strings.Contains(out, "31h ago") {
		t.Fatalf("31h-old mail did not render in hours; got:\n%s", out)
	}
	if strings.Contains(out, "1 days") || strings.Contains(out, "1d ago") {
		t.Fatalf("31h-old mail rendered as whole days; got:\n%s", out)
	}
}

// TestHumanAge pins the unit boundaries directly, including the negative-age
// case a shared loop dir can produce when two peers' clocks disagree.
func TestHumanAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-5 * time.Hour, "0m"},
		{30 * time.Second, "0m"},
		{time.Minute, "1m"},
		{45 * time.Minute, "45m"},
		{time.Hour, "1h"},
		{31 * time.Hour, "31h"},
		{47*time.Hour + 59*time.Minute, "47h"},
		{48 * time.Hour, "2d"},
		{3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := humanAge(c.in); got != c.want {
			t.Errorf("humanAge(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCheckNoBannerOnFreshMail proves the threshold's other side: mail well
// under 24h old prints no banner.
func TestCheckNoBannerOnFreshMail(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	mustWriteSeqInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte("---\nfrom: bob\n---\n\nhi\n"))

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if strings.Contains(out, "[!] STALE") {
		t.Fatalf("fresh mail printed an age banner; got:\n%s", out)
	}
}

// TestCheckNoBannerJustUnderThreshold pins the actual 24h boundary (review
// should-fix): mail aged 23h — under the threshold, but far from "fresh" —
// must print no banner. Without this, shrinking oldMailBannerAfter (e.g. to
// 1h) would leave the whole suite green, since TestCheckNoBannerOnFreshMail's
// near-zero-age mail stays under any plausible threshold.
func TestCheckNoBannerJustUnderThreshold(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte("---\nfrom: bob\n---\n\nhi\n"), 23*time.Hour)

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if strings.Contains(out, "[!] STALE") || strings.Contains(out, "[!]") {
		t.Fatalf("23h-old mail (under the 24h threshold) printed an age banner; got:\n%s", out)
	}
}

// TestCheckBannerOnRedeliveryPath proves the banner also fires on
// crash-orphaned residue redelivered from .claimed (review nit): all three
// display paths share printConsumedBody today, but nothing structural
// guarantees that stays true.
func TestCheckBannerOnRedeliveryPath(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	inbox := cfg.AgentInboxDir("alice")
	mustWriteAgedInbox(t, inbox, "bob", 1, []byte("---\nfrom: bob\n---\n\nhi\n"), 3*24*time.Hour)
	msgs, _, err := loop.ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("seed: got %d inbox messages, want 1", len(msgs))
	}
	// Simulate a crash between claim and ack: move it to .claimed directly.
	if _, err := loop.ClaimMessage(msgs[0], cfg.AgentClaimedDir("alice")); err != nil {
		t.Fatal(err)
	}

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if !strings.Contains(out, "REDELIVERED") {
		t.Fatalf("expected the redelivery banner to fire; got:\n%s", out)
	}
	if !strings.Contains(out, "[!] STALE: sent ") {
		t.Fatalf("redelivered residue missing the age banner; got:\n%s", out)
	}
}

// TestCheckBannerOnNoArchivePath proves the banner also fires under
// --no-archive (dry-run display, displayConsumedReadOnly) (review nit).
func TestCheckBannerOnNoArchivePath(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	mustWriteAgedInbox(t, cfg.AgentInboxDir("alice"), "bob", 1, []byte("---\nfrom: bob\n---\n\nhi\n"), 3*24*time.Hour)

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice", "--no-archive"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if !strings.Contains(out, "[!] STALE: sent ") {
		t.Fatalf("--no-archive display missing the age banner; got:\n%s", out)
	}
}

// TestCheckOffersStaleOwedPrune proves C19: an expired reply obligation is
// surfaced with a copy-pasteable prune command — even with an empty inbox
// (a deliberate deviation from the plan's literal anchor point, disclosed in
// the PR: that anchor sits after the "(inbox empty)" early return, which
// would otherwise hide the offer whenever there's no fresh mail to look at).
func TestCheckOffersStaleOwedPrune(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	now := time.Now().UTC()
	key := loop.MsgID{To: "bob", From: "alice", Seq: 1}
	if err := loop.RecordOwed(cfg, "alice", key, now.Add(-time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if !strings.Contains(out, "stale reply obligation") || !strings.Contains(out, "agentchute clean --owed --as alice") {
		t.Fatalf("output missing the stale-owed prune offer; got:\n%s", out)
	}
	if !strings.Contains(out, key.RefString()) {
		t.Fatalf("output missing the obligation's ref; got:\n%s", out)
	}
	if !strings.Contains(out, "(inbox empty)") {
		t.Fatalf("expected the empty-inbox line to still print alongside the prune offer; got:\n%s", out)
	}

	// Print-only covenant (review should-fix): the offer must never itself
	// clear the obligation. Without this assertion, an accidental auto-Clear
	// added inside the offer loop would leave the whole suite green.
	ledger, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Owed) != 1 || !ledger.Owed[0].Key().Equal(key) {
		t.Fatalf("the print-only offer mutated the ledger: %+v, want the obligation still present", ledger.Owed)
	}
}

// TestCheckPruneOfferReflectsSameTurnDischarge is the review's should-fix:
// when THIS turn's own claim loop consumes a reply that discharges an
// expired obligation (ClearOwed, via displayConsumed), the prune offer
// computed later in the SAME cmdCheck call must not still advertise it —
// the ledger read for the offer must happen AFTER the claim loop, not before.
func TestCheckPruneOfferReflectsSameTurnDischarge(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	now := time.Now().UTC()
	key := loop.MsgID{To: "bob", From: "alice", Seq: 1}
	if err := loop.RecordOwed(cfg, "alice", key, now.Add(-time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// bob's reply, discharging the obligation, sitting unread in alice's
	// inbox — this SAME cmdCheck call will claim and process it.
	mustWriteSeqInbox(t, cfg.AgentInboxDir("alice"), "bob", 1,
		[]byte(fmt.Sprintf("---\nfrom: bob\nin_reply_to: %s\n---\n\nhere is my reply\n", key.RefString())))

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if strings.Contains(out, "stale reply obligation") {
		t.Fatalf("offer advertised an obligation that THIS SAME turn's reply already discharged; got:\n%s", out)
	}

	ledger, err := loop.LoadOwedLedger(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Owed) != 0 {
		t.Fatalf("obligation was not cleared by the discharging reply: %+v", ledger.Owed)
	}
}

// TestCheckDoesNotOfferOutstandingObligation proves the offer lists only
// EXPIRED entries: a not-yet-due obligation must not appear.
func TestCheckDoesNotOfferOutstandingObligation(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	now := time.Now().UTC()
	key := loop.MsgID{To: "bob", From: "alice", Seq: 1}
	if err := loop.RecordOwed(cfg, "alice", key, now.Add(30*time.Minute), now); err != nil {
		t.Fatal(err)
	}

	var out string
	withCwd(t, root, func() {
		o, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		if err != nil {
			t.Fatal(err)
		}
		out = o
	})
	if strings.Contains(out, "stale reply obligation") {
		t.Fatalf("a not-yet-expired obligation was offered for pruning; got:\n%s", out)
	}
}

// TestCheckCorruptOwedLedgerWarnsNotFatal proves the C19 tolerance: a
// corrupt/unreadable .owed ledger is a warning, never a check failure —
// mirroring gate.go's identical tolerance for the same condition.
func TestCheckCorruptOwedLedgerWarnsNotFatal(t *testing.T) {
	root, cfg := setupConsumeFixture(t)
	path := filepath.Join(cfg.AgentStateDir("alice"), "owed.json")
	mustWrite(t, path, []byte("{ this is not valid json"))

	var cmdErr error
	var stderrOut string
	withCwd(t, root, func() {
		stderrOut = captureStderr(t, func() {
			_, cmdErr = captureStdout(t, func() error { return cmdCheck([]string{"--as", "alice"}) })
		})
	})
	if cmdErr != nil {
		t.Fatalf("check on a corrupt owed ledger err = %v, want nil (warning, not fatal)", cmdErr)
	}
	if !strings.Contains(stderrOut, "owed-reply ledger is corrupt or unreadable") {
		t.Fatalf("stderr missing the corrupt-ledger warning; got:\n%s", stderrOut)
	}
}
