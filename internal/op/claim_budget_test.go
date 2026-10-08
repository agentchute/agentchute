package op

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// ---------- C1: byte-budgeted batches ----------

func bodyOf(n int) string { return strings.Repeat("x", n) }

// Three 5,000-byte messages against the default budget: two fit, the third
// would push the rendered output past it, so it stays in the inbox and the
// budget line says so. The next check claims it.
func TestClaimDefaultBudgetStopsClaimingAndReportsPending(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	for i := 0; i < 3; i++ {
		deliver(t, cfg, "codex", "claude-code", bodyOf(5000))
	}

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Claimed != 2 {
		t.Fatalf("claimed = %d, want 2 under the %d-byte default budget; stream=%v", sum.Claimed, DefaultClaimBudgetBytes, c.kinds())
	}
	infos := c.notes(NoteInfo)
	if len(infos) == 0 || infos[0] != "(reached budget of 12288 bytes; 1 more pending)" {
		t.Fatalf("budget line = %q", infos)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 1 {
		t.Fatalf("inbox = %d files, want the 1 message the budget left behind", n)
	}

	var c2 collector
	sum, err = Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c2.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Claimed != 1 || sum.Redelivered != 2 {
		t.Fatalf("second check = %+v, want 1 claimed / 2 redelivered", sum)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 0 {
		t.Fatalf("inbox = %d files after the second check, want 0", n)
	}
}

// One message larger than the whole budget is still claimed: the budget
// bounds a batch, it never starves a message.
func TestClaimBudgetAlwaysClaimsAtLeastOne(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", bodyOf(3*DefaultClaimBudgetBytes))
	deliver(t, cfg, "codex", "claude-code", "small")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Claimed != 1 {
		t.Fatalf("claimed = %d, want exactly the one oversize-for-budget message", sum.Claimed)
	}
	if infos := c.notes(NoteInfo); len(infos) == 0 || !strings.HasPrefix(infos[0], "(reached budget of") {
		t.Fatalf("budget line missing: %q", infos)
	}
}

// Explicit budgets: a negative value lifts the budget; a positive one is used
// as given; --limit still wins when it is reached first.
func TestClaimBudgetExplicitValues(t *testing.T) {
	rows := []struct {
		name        string
		req         ClaimReq
		wantClaimed int
		wantLine    string
	}{
		{name: "negative = unlimited", req: ClaimReq{BudgetBytes: -1}, wantClaimed: 4, wantLine: ""},
		{name: "explicit small budget", req: ClaimReq{BudgetBytes: 6000}, wantClaimed: 1, wantLine: "(reached budget of 6000 bytes; 3 more pending)"},
		{name: "limit reached before budget", req: ClaimReq{Limit: 1, BudgetBytes: -1}, wantClaimed: 1, wantLine: "(reached limit of 1; 3 more pending)"},
		{name: "budget reached before limit", req: ClaimReq{Limit: 3}, wantClaimed: 2, wantLine: "(reached budget of 12288 bytes; 2 more pending)"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := newPool(t)
			enroll(t, cfg, "claude-code")
			enroll(t, cfg, "codex")
			for i := 0; i < 4; i++ {
				deliver(t, cfg, "codex", "claude-code", bodyOf(5000))
			}
			var c collector
			sum, err := Claim(cfg, Context{ActorID: "claude-code"}, row.req, c.emit)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Claimed != row.wantClaimed {
				t.Fatalf("claimed = %d, want %d", sum.Claimed, row.wantClaimed)
			}
			infos := c.notes(NoteInfo)
			if row.wantLine == "" {
				if len(infos) != 1 || !strings.HasPrefix(infos[0], "note: messages CLAIMED") {
					t.Fatalf("expected only the CLAIMED line, got %q", infos)
				}
				return
			}
			if len(infos) == 0 || infos[0] != row.wantLine {
				t.Fatalf("status line = %q, want %q", infos, row.wantLine)
			}
		})
	}
}

// ---------- C2: oversize / unreadable files are quarantined, never a wedge ----------

const oversizeName = "20260101T000000000000Z_from-codex_r00000000000000000000000000000001.md"

func writeOversize(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, oversizeName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("y", loop.MaxInboxMessageBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The review's sealed-pool reproduction: an oversize file sorted first, a
// normal message from the same sender behind it. Before: every check failed
// at the oversize file, claimed nothing, and the gate stayed blocked. After:
// the file is quarantined with a warning and the normal message is claimed.
func TestClaimQuarantinesOversizeInboxFileAndContinues(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	writeOversize(t, cfg.AgentInboxDir("claude-code"))
	deliver(t, cfg, "codex", "claude-code", "after the big one")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if err != nil {
		t.Fatalf("check must not fail on an oversize file: %v", err)
	}
	if sum.Quarantined != 1 || sum.Claimed != 1 {
		t.Fatalf("summary = %+v, want 1 quarantined / 1 claimed", sum)
	}
	warns := c.notes(NoteWarn)
	if len(warns) != 1 || !strings.Contains(warns[0], oversizeName) || !strings.Contains(warns[0], "quarantined") {
		t.Fatalf("warning = %q, want one naming the quarantined file", warns)
	}
	if n := countFiles(t, cfg.MalformedDir()); n != 1 {
		t.Fatalf("malformed/ = %d files, want 1", n)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 0 {
		t.Fatalf("inbox = %d files, want 0", n)
	}
}

func TestClaimQuarantinesUnreadableInboxFileAndContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	inbox := cfg.AgentInboxDir("claude-code")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(inbox, oversizeName)
	if err := os.WriteFile(path, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	deliver(t, cfg, "codex", "claude-code", "readable")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if err != nil {
		t.Fatalf("check must not fail on an unreadable file: %v", err)
	}
	if sum.Quarantined != 1 || sum.Claimed != 1 {
		t.Fatalf("summary = %+v, want 1 quarantined / 1 claimed", sum)
	}
	if n := countFiles(t, cfg.MalformedDir()); n != 1 {
		t.Fatalf("malformed/ = %d files, want 1", n)
	}
}

// Residue past the limit is quarantined too, and still counted as found.
func TestClaimQuarantinesOversizeResidue(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	writeOversize(t, cfg.AgentClaimedDir("claude-code"))
	deliver(t, cfg, "codex", "claude-code", "fresh")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if err != nil {
		t.Fatalf("check must not fail on oversize residue: %v", err)
	}
	if sum.Redelivered != 1 || sum.Quarantined != 1 || sum.Claimed != 1 {
		t.Fatalf("summary = %+v, want 1 redelivered(found) / 1 quarantined / 1 claimed", sum)
	}
	if n := countFiles(t, cfg.AgentClaimedDir("claude-code")); n != 1 {
		t.Fatalf(".claimed = %d files, want only the fresh claim", n)
	}
}

// --no-archive is a dry run: the oversize file is reported, not moved, and
// the loop still continues to the readable message.
func TestClaimNoArchiveReportsOversizeWithoutMoving(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	writeOversize(t, cfg.AgentInboxDir("claude-code"))
	deliver(t, cfg, "codex", "claude-code", "readable")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{NoArchive: true}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Quarantined != 0 {
		t.Fatalf("dry run quarantined %d", sum.Quarantined)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 2 {
		t.Fatalf("inbox = %d files, want both untouched", n)
	}
	if warns := c.notes(NoteWarn); len(warns) != 1 || !strings.Contains(warns[0], "--no-archive") {
		t.Fatalf("warning = %q", warns)
	}
	if len(c.messages()) != 1 {
		t.Fatalf("dry run displayed %d messages, want the readable one", len(c.messages()))
	}
}

// When the file cannot be read AND cannot be quarantined, check stops: it
// claims nothing further, returns an error, and what it already claimed stays
// claimed.
func TestClaimStopsClaimingWhenQuarantineFails(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "first, claimable")
	writeOversize(t, cfg.AgentInboxDir("claude-code")) // sorts after: later stamp than "first"? no — see below
	// The oversize name carries a 2026-01-01 stamp, so it sorts FIRST among
	// codex's mail; put the claimable message ahead of it by a different sender.
	enroll(t, cfg, "grok")
	deliver(t, cfg, "grok", "claude-code", "from grok")
	// Make malformed/ a regular file so the quarantine move must fail.
	if err := os.WriteFile(cfg.MalformedDir(), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{BudgetBytes: -1}, c.emit)
	if err == nil {
		t.Fatal("expected an error when an unreadable file cannot be quarantined")
	}
	if !strings.Contains(err.Error(), oversizeName) {
		t.Fatalf("error should name the file: %v", err)
	}
	inbox := countFiles(t, cfg.AgentInboxDir("claude-code"))
	claimed := countFiles(t, cfg.AgentClaimedDir("claude-code"))
	if sum.Claimed != claimed || inbox+claimed != 3 {
		t.Fatalf("claimed=%d .claimed=%d inbox=%d: nothing after the error may be claimed, nothing before it un-claimed", sum.Claimed, claimed, inbox)
	}
	if inbox < 1 {
		t.Fatal("the unreadable file and everything after it must stay in the inbox")
	}
}
