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
// bad filename or unparseable YAML can occur once it's in the stream — so the
// vector uses applies_to:["inbox"] (same rationale as
// TestInbox_PostConsumeResendRelandsThenKeyDedup).
func TestQ1_MalformedQuarantineNeverDeliveredOrConsumedOrDropped(t *testing.T) {
	v := vectorByID(t, "Q1", "malformed_quarantine")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		ib, ok := b.(*inboxBinding)
		if !ok {
			t.Fatalf("malformed quarantine vector ran on unsupported binding %q", b.Name())
		}
		if len(v.Bodies) != 2 || len(v.MalformedItems) != 2 {
			t.Fatalf("malformed quarantine vector needs two valid bodies and two malformed items: %+v", v)
		}
		must(t, ib.Register(v.Recipient))

		first := Msg{From: v.Sender, Body: v.Bodies[0]}
		second := Msg{From: v.Sender, Body: v.Bodies[1]}
		must(t, ib.DeliverRaw(v.Recipient, "valid-1", &first))
		must(t, ib.DeliverRaw(v.Recipient, v.MalformedItems[0], nil))
		must(t, ib.DeliverRaw(v.Recipient, "valid-2", &second))

		// Never delivered, never blocks/reorders valid mail: Poll shows exactly
		// the two valid messages, in arrival order, with the malformed item absent.
		got, _ := ib.Poll(v.Recipient)
		if len(got) != 2 || got[0].Body != v.Bodies[0] || got[1].Body != v.Bodies[1] {
			t.Fatalf("malformed item must not appear in Poll and must not disturb valid message order/continuity; got %+v", got)
		}

		// Never silently dropped: it is quarantined (observable), not vanished.
		mal := ib.MalformedItems(v.Recipient)
		if len(mal) != 1 || mal[0] != v.MalformedItems[0] {
			t.Fatalf("malformed item must be quarantined (observable via MalformedItems), not silently dropped; got %v", mal)
		}

		// Never counted as consumed: Consume only sees the 2 valid messages.
		var acts []string
		n, err := ib.Consume(v.Recipient, func(m Msg) error { acts = append(acts, m.Body); return nil })
		must(t, err)
		if n != 2 || len(acts) != 2 {
			t.Fatalf("consume must only see the 2 valid messages, never the quarantined item; n=%d acts=%v", n, acts)
		}

		// A second quarantine of a DIFFERENT item is additive, not overwriting —
		// mirrors the real malformed/ dir accumulating distinct quarantined files.
		must(t, ib.DeliverRaw(v.Recipient, v.MalformedItems[1], nil))
		if mal := ib.MalformedItems(v.Recipient); len(mal) != 2 {
			t.Fatalf("a second malformed item must accumulate, not overwrite the first; got %v", mal)
		}
	})
}
