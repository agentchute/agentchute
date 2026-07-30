package conformance

import "testing"

// TestInbox_PostConsumeResendRelandsThenKeyDedup pins post-consume delivery
// semantics for the INBOX binding: once a message is consumed+committed the inbox
// slot is freed, so a later resend of the SAME (to,from,seq) must RE-LAND — it is
// NOT suppressed by delivery (EEXIST) dedup forever. This matches the real FS,
// where EEXIST-as-no-op holds only PRE-consume/pre-archive. There is no protocol
// receiver-side dedup backstop after archive; a re-land is a second delivery.
// The Key/Deduper stanza below only demonstrates an OPT-IN handler-side aid that
// can collapse effects by key — not a substrate or protocol backstop. (Before a
// prior fix the in-memory `delivered` map deduped forever, over-optimistically
// green-lighting a post-consume delivery dedup the FS does not provide.)
//
// Inbox-only on purpose: the shared-log binding legitimately retains a committed
// delivery identity forever (the record stays in the append-only log), so this is
// not run via eachBinding.
func TestInbox_PostConsumeResendRelandsThenKeyDedup(t *testing.T) {
	b := newInbox()
	must(t, b.Register("bob"))

	// Deliver seq=1 (Key k1), then consume + commit it.
	must(t, b.Deliver("bob", Msg{From: "alice", Seq: 1, Key: "k1", Body: "do-it"}))
	var acts []Msg
	n, err := b.Consume("bob", func(m Msg) error { acts = append(acts, m); return nil })
	must(t, err)
	if n != 1 || len(acts) != 1 {
		t.Fatalf("first consume must deliver exactly 1; n=%d acts=%d", n, len(acts))
	}
	if got, _ := b.Poll("bob"); len(got) != 0 {
		t.Fatalf("inbox must be empty after consume+commit; saw %d", len(got))
	}

	// POST-CONSUME resend of the SAME (to,from,seq) must RE-LAND (the regression:
	// pre-fix it was silently EEXIST-deduped and never re-landed).
	if err := b.Deliver("bob", Msg{From: "alice", Seq: 1, Key: "k1", Body: "do-it"}); err != nil {
		t.Fatalf("post-consume resend must re-land, got err: %v", err)
	}
	if got, _ := b.Poll("bob"); len(got) != 1 {
		t.Fatalf("post-consume resend must be visible again (re-land); saw %d — delivery dedup wrongly suppressed it", len(got))
	}

	// OPT-IN handler aid only (not a protocol backstop): Key/Deduper can collapse
	// the duplicate EFFECT across the original consume and the re-landed resend
	// (both Key k1) -> exactly one effect when the handler chooses to use it.
	d := NewDeduper()
	effects := 0
	apply := func(m Msg) error {
		return d.Once(m, func(Msg) error { effects++; return nil })
	}
	must(t, apply(acts[0])) // the original, already consumed above
	_, cerr := b.Consume("bob", apply)
	must(t, cerr) // the re-landed resend
	if effects != 1 {
		t.Fatalf("opt-in Key/Deduper must collapse original + post-consume resend to ONE effect; got %d", effects)
	}
}
