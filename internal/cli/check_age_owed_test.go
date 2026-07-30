package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// check_age_owed_test.go — v2.5 implementation plan slice A3: check's C18 age
// banner and C19 stale-owed prune offer.

// TestCheckPrintsAgeBannerOnOldMail proves C18: a message older than
// oldMailBannerAfter (24h) prints the age banner above its body.
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
	if !strings.Contains(out, "[!] this message is 3 days old") {
		t.Fatalf("output missing the age banner; got:\n%s", out)
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
	if strings.Contains(out, "days old") {
		t.Fatalf("fresh mail printed an age banner; got:\n%s", out)
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
