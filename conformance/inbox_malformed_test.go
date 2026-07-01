package conformance

import "testing"

// TestInbox_MalformedQuarantineNeverDeliveredOrConsumedOrDropped pins §11.1's
// enforcement-action contract at the substrate boundary: an item that fails
// to decode is quarantined, not delivered — it never reaches Poll or
// Consume, it is never silently dropped (it stays observable via
// MalformedItems), and it never blocks or reorders valid mail to the same
// recipient. This is the one lifecycle guarantee the reference CLI's
// check.go + internal/loop.QuarantineInboxFile provide that the core seven
// invariants (R1/D1/D2/O1/C1/E1/B1) don't otherwise cover.
//
// Inbox-only on purpose: quarantine is a wire/filename-grammar concern
// specific to the FS+frontmatter substrate. The shared-log binding has no
// analogous concept — a log record is already-typed Go data, nothing like a
// bad filename or unparseable YAML can occur once it's in the stream — so
// this is not run via eachBinding (same rationale as
// TestInbox_PostConsumeResendRelandsThenKeyDedup).
func TestInbox_MalformedQuarantineNeverDeliveredOrConsumedOrDropped(t *testing.T) {
	b := newInbox()
	must(t, b.Register("bob"))

	must(t, b.DeliverRaw("bob", "valid-1", &Msg{From: "alice", Body: "first"}))
	must(t, b.DeliverRaw("bob", "not-a-valid-message-name.md", nil))
	must(t, b.DeliverRaw("bob", "valid-2", &Msg{From: "alice", Body: "second"}))

	// Never delivered, never blocks/reorders valid mail: Poll shows exactly
	// the two valid messages, in arrival order, with the malformed item absent.
	got, _ := b.Poll("bob")
	if len(got) != 2 || got[0].Body != "first" || got[1].Body != "second" {
		t.Fatalf("malformed item must not appear in Poll and must not disturb valid message order/continuity; got %+v", got)
	}

	// Never silently dropped: it is quarantined (observable), not vanished.
	mal := b.MalformedItems("bob")
	if len(mal) != 1 || mal[0] != "not-a-valid-message-name.md" {
		t.Fatalf("malformed item must be quarantined (observable via MalformedItems), not silently dropped; got %v", mal)
	}

	// Never counted as consumed: Consume only sees the 2 valid messages.
	var acts []string
	n, err := b.Consume("bob", func(m Msg) error { acts = append(acts, m.Body); return nil })
	must(t, err)
	if n != 2 || len(acts) != 2 {
		t.Fatalf("consume must only see the 2 valid messages, never the quarantined item; n=%d acts=%v", n, acts)
	}

	// A second quarantine of a DIFFERENT item is additive, not overwriting —
	// mirrors the real malformed/ dir accumulating distinct quarantined files.
	must(t, b.DeliverRaw("bob", "also-bad.md", nil))
	if mal := b.MalformedItems("bob"); len(mal) != 2 {
		t.Fatalf("a second malformed item must accumulate, not overwrite the first; got %v", mal)
	}
}
