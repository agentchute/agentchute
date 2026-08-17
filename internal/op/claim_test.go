package op

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// The three stdout status lines are emitted AT their production points, not
// derived from the terminal summary — that is the whole reason they are events.
// This asserts POSITION, which is what a summary-driven renderer silently
// inverts: the limit line lands between message renders, and the CLAIMED line
// lands BEFORE the expired-obligation lines.
func TestClaimEmitsStatusLinesInProductionPositions(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	for i := 0; i < 3; i++ {
		deliver(t, cfg, "codex", "claude-code", "body")
	}
	recordExpiredOwed(t, cfg, "claude-code", "codex")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{Limit: 2}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	c.assertUnionInvariant(t)

	want := []string{"message", "message", "note/info", "note/info", "owed"}
	if got := c.kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("stream = %v, want %v", got, want)
	}
	infos := c.notes(NoteInfo)
	if infos[0] != "(reached limit of 2; 1 more pending)" {
		t.Fatalf("limit line = %q", infos[0])
	}
	if infos[1] != "note: messages CLAIMED (at-least-once), not yet archived. Run `agentchute ack` to commit; a crash before ack re-delivers them." {
		t.Fatalf("claimed line = %q", infos[1])
	}
	if sum.Claimed != 2 || sum.OwedExpired != 1 {
		t.Fatalf("summary = %+v, want 2 claimed / 1 expired", sum)
	}
	// The unclaimed remainder stays in the inbox for the next turn.
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 1 {
		t.Fatalf("inbox = %d files, want the 1 message the limit left behind", n)
	}
}

// The info notes carry NO level prefix: the renderer supplies it, so the wire
// and the local path cannot drift apart on the wording.
func TestClaimEmptyInboxEmitsTheEmptyLine(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")

	var c collector
	if _, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit); err != nil {
		t.Fatal(err)
	}
	if got := c.notes(NoteInfo); len(got) != 1 || got[0] != "(inbox empty)" {
		t.Fatalf("info notes = %v, want exactly the empty-inbox line", got)
	}
	// It must NOT return early: an agent with no new mail still needs the
	// expired-obligation report that follows.
	if got := c.kinds(); len(got) != 1 {
		t.Fatalf("stream = %v", got)
	}
}

// Two-phase consume: check CLAIMS but never archives, and a crash before ack
// RE-DELIVERS. The second claim finds the residue and re-emits it with the
// redelivered flag the banner renders from.
func TestClaimRedeliversUncommittedResidue(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "first")

	var first collector
	if _, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, first.emit); err != nil {
		t.Fatal(err)
	}
	if msgs := first.messages(); len(msgs) != 1 || msgs[0].Redelivered {
		t.Fatalf("first claim = %+v, want one fresh message", first.messages())
	}
	if n := countFiles(t, cfg.AgentClaimedDir("claude-code")); n != 1 {
		t.Fatalf(".claimed = %d files, want the claimed message held for ack", n)
	}

	var second collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, second.emit)
	if err != nil {
		t.Fatal(err)
	}
	msgs := second.messages()
	if len(msgs) != 1 || !msgs[0].Redelivered {
		t.Fatalf("second claim = %+v, want the residue redelivered", msgs)
	}
	if sum.Redelivered != 1 || sum.Claimed != 0 {
		t.Fatalf("summary = %+v, want 1 redelivered / 0 newly claimed", sum)
	}
}

// --no-archive is a DRY RUN: it displays in place and mutates nothing — no
// claim, no quarantine, and no owed discharge.
func TestClaimNoArchiveMutatesNothing(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "body")
	writeInboxFile(t, cfg, "claude-code", "not-a-protocol-name.md", "junk")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{NoArchive: true}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Quarantined != 0 {
		t.Fatalf("summary = %+v, want no quarantine under --no-archive", sum)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 2 {
		t.Fatalf("inbox = %d files, want both left in place", n)
	}
	if n := countFiles(t, cfg.AgentClaimedDir("claude-code")); n != 0 {
		t.Fatalf(".claimed = %d files, want none", n)
	}
	warns := c.notes(NoteWarn)
	if len(warns) != 1 || !strings.HasPrefix(warns[0], "1 non-§6.1 file(s) in inbox; --no-archive suppressed §11 enforcement:") {
		t.Fatalf("warn notes = %v, want the suppressed-enforcement report", warns)
	}
	if !strings.Contains(warns[0], "\n  not-a-protocol-name.md") {
		t.Fatalf("warn note dropped the file list: %q", warns[0])
	}
}

// §11 enforcement, both surfaces: a file whose NAME fails the reference
// encoding, and a message whose FRONTMATTER does not parse.
func TestClaimQuarantinesProtocolViolations(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	writeInboxFile(t, cfg, "claude-code", "not-a-protocol-name.md", "junk")
	id := deliver(t, cfg, "codex", "claude-code", "body")
	// A valid filename whose frontmatter block never closes.
	writeInboxFile(t, cfg, "claude-code", id.Filename(), "---\nfrom: codex\n")

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Quarantined != 2 {
		t.Fatalf("summary = %+v, want both violations quarantined", sum)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 0 {
		t.Fatalf("inbox = %d files, want both moved out", n)
	}
	if n := countFiles(t, cfg.MalformedDir()); n != 2 {
		t.Fatalf("malformed dir = %d files, want 2", n)
	}
	warns := c.notes(NoteWarn)
	if len(warns) != 2 ||
		!strings.HasPrefix(warns[0], "quarantined not-a-protocol-name.md (malformed §6.1 filename) -> ") ||
		!strings.Contains(warns[1], "(malformed §6.4 frontmatter:") {
		t.Fatalf("warn notes = %v", warns)
	}
	// A quarantined message is never claimed, but it still counts against
	// --limit, exactly as the shipped loop counts it.
	if sum.Claimed != 1 {
		t.Fatalf("summary = %+v, want the malformed message counted", sum)
	}
}

// The asker-side owed flip: consuming a reply that references OUR outstanding
// ask discharges that obligation. Suppressed under --no-archive, which mutates
// nothing.
func TestClaimDischargesOwedOnlyWhenItMutates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		noArchive     bool
		wantRemaining int
	}{
		{"claiming discharges", false, 0},
		{"dry run does not", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newPool(t)
			enroll(t, cfg, "claude-code")
			enroll(t, cfg, "codex")

			ask, err := Send(cfg, Context{ActorID: "claude-code"}, SendReq{
				To:      "codex",
				Content: loop.ComposeMessage("claude-code", "", "question"),
				Ask:     true,
			})
			if err != nil {
				t.Fatal(err)
			}
			replyContent := loop.ComposeMessage("codex", ask.Ref, "answer")
			if _, _, err := loop.SendTsMessageWithCommit(cfg, "codex", "claude-code", replyContent, ""); err != nil {
				t.Fatal(err)
			}

			var c collector
			if _, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{NoArchive: tc.noArchive}, c.emit); err != nil {
				t.Fatal(err)
			}
			owed, err := loop.LoadOwedLedger(cfg, "claude-code")
			if err != nil {
				t.Fatal(err)
			}
			if len(owed.Owed) != tc.wantRemaining {
				t.Fatalf("owed = %d entries, want %d", len(owed.Owed), tc.wantRemaining)
			}
		})
	}
}

// Residue is re-displayed through the mutating path even under --no-archive:
// the shipped loop discharges on redelivery regardless of the flag, because the
// message was already claimed in a prior turn.
func TestClaimResidueDischargesEvenUnderNoArchive(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")

	ask, err := Send(cfg, Context{ActorID: "claude-code"}, SendReq{
		To:      "codex",
		Content: loop.ComposeMessage("claude-code", "", "question"),
		Ask:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Claim the reply once (so it becomes residue), then restore the ledger so
	// the second pass has something left to discharge.
	if _, _, err := loop.SendTsMessageWithCommit(cfg, "codex", "claude-code", loop.ComposeMessage("codex", ask.Ref, "answer"), ""); err != nil {
		t.Fatal(err)
	}
	var first collector
	if _, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, first.emit); err != nil {
		t.Fatal(err)
	}
	id, ok := loop.ParseTsRef(ask.Ref)
	if !ok {
		t.Fatalf("unparseable ask ref %q", ask.Ref)
	}
	if err := loop.RecordOwed(cfg, "claude-code", id, time.Now().UTC().Add(time.Hour), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var second collector
	if _, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{NoArchive: true}, second.emit); err != nil {
		t.Fatal(err)
	}
	owed, err := loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(owed.Owed) != 0 {
		t.Fatalf("owed = %+v, want the redelivered reply to have discharged it", owed.Owed)
	}
}

// Streaming, not batching: an emit failure aborts after the current event, so
// the messages behind it are still in the inbox. A producer that materialized
// the whole batch first would have claimed all three.
func TestClaimEmitErrorAbortsMidStream(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	for i := 0; i < 3; i++ {
		deliver(t, cfg, "codex", "claude-code", "body")
	}

	c := collector{failAt: 1}
	_, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if !errors.Is(err, errEmitFailed) {
		t.Fatalf("err = %v, want the emitter's own error", err)
	}
	if n := countFiles(t, cfg.AgentClaimedDir("claude-code")); n != 1 {
		t.Fatalf(".claimed = %d files, want only the message that was mid-flight", n)
	}
	if n := countFiles(t, cfg.AgentInboxDir("claude-code")); n != 2 {
		t.Fatalf("inbox = %d files, want the untouched remainder", n)
	}
}

// Every message event carries what a renderer needs and nothing it has to
// re-derive: the reply ref in particular, because a remote client cannot parse
// a filename it never sees.
func TestClaimMessageEventCarriesReplyRefAndBody(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	content := append([]byte("---\nfrom: codex\nreply_required: true\n---\n"), []byte("\nping\n")...)
	id := deliver(t, cfg, "codex", "claude-code", "seed")
	writeInboxFile(t, cfg, "claude-code", id.Filename(), string(content))

	var c collector
	if _, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit); err != nil {
		t.Fatal(err)
	}
	msgs := c.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	got := msgs[0]
	if got.Sender != "codex" || got.Filename != id.Filename() {
		t.Fatalf("event = %+v", got)
	}
	if !got.ReplyRequired || got.ReplyRef == "" {
		t.Fatalf("event = %+v, want the reply ref a reply must echo", got)
	}
	if !strings.HasPrefix(got.ReplyRef, "to-claude-code_from-codex_") {
		t.Fatalf("reply ref = %q, want this inbox's own identity form", got.ReplyRef)
	}
	if string(got.Body) != string(content) {
		t.Fatalf("body = %q, want the message bytes verbatim (frontmatter included)", got.Body)
	}
	if _, err := time.Parse(time.RFC3339Nano, got.Stamp); err != nil {
		t.Fatalf("stamp = %q is not RFC3339Nano: %v", got.Stamp, err)
	}
}

func TestClaimRefusesUnregisteredAgent(t *testing.T) {
	cfg := newPool(t)
	var c collector
	_, err := Claim(cfg, Context{ActorID: "claude-code"}, ClaimReq{}, c.emit)
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
	if len(c.events) != 0 {
		t.Fatalf("a refused claim emitted %d events", len(c.events))
	}
}

func writeInboxFile(t *testing.T, cfg *loop.Config, agentID, name, content string) {
	t.Helper()
	if err := loop.EnsurePrivateDir(cfg.AgentInboxDir(agentID)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.AgentInboxDir(agentID), name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordExpiredOwed(t *testing.T, cfg *loop.Config, asker, to string) {
	t.Helper()
	now := time.Now().UTC()
	id := loop.TsID{To: to, From: asker, Stamp: "20260101T120000000000Z", Suffix: strings.Repeat("a", 32)}
	if err := loop.RecordOwed(cfg, asker, id, now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
}
