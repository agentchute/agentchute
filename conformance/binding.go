// Package conformance turns the agentchute protocol's invariants into a runnable
// suite, and ships TWO bindings — the inbox model (today) and the shared-log
// model (the §5 fork) — so the same suite proves both on equal footing.
//
// The point (the §4 "reframe" made executable): the INVARIANTS are the protocol;
// the substrate is swappable. A new substrate (git, Redis Streams, HTTP+ETag)
// becomes conformant by implementing Binding and being added to bindings().
package conformance

import (
	"fmt"
	"time"
)

// Msg is the message AFTER the v2-delta cuts, updated for v2.5 plan B7 (the
// wire break: timestamp+random-suffix identity replaces the per-(from,to)
// sequence counter; delivery becomes AT-MOST-ONCE, with no sender-asserted
// idempotency key or receiver-side dedup backstop).
//
// WHY these fields and no others:
//   - From          : normative. Who sent it. (Receiver MUST reject a message
//     with no From — an anonymous message has no accountability.)
//   - ReplyRequired : the ONE cross-agent coordination bit worth keeping. Under
//     the v2 deltas the *obligation* is owned by the asker; this
//     bit is only an advisory hint to the recipient.
//   - InReplyTo     : optional thread link.
//   - Extra         : unknown/future fields. Carried, never required. Proves
//     forward-compat (E1): old receivers ignore new fields.
//   - ID            : the OPAQUE committed delivery identity (C6: a timestamp
//     plus 128-bit random suffix in the real grammar; here just an opaque
//     string so the model stays substrate-neutral). When non-empty, a
//     binding keys its delivered-map on it PURELY to be able to SIMULATE a
//     collision (TS3) — unlike the deleted Seq-keyed map, a collision is
//     NEVER a safe no-op here: delivery is at-most-once, so the SENDER is
//     responsible for retrying with a fresh ID (C4), exactly as
//     DeliverUnderRecipientLock does in the reference implementation.
//
// What's deliberately ABSENT: `to` (addressing is structural — which inbox / the
// record's recipient); a sender-asserted idempotency key (deleted with the seq
// allocator — B7); and any delivery-side dedup guarantee (at-most-once now).
type Msg struct {
	From          string
	Body          string
	ReplyRequired bool
	InReplyTo     string
	Extra         map[string]string
	ID            string
}

// Binding is one substrate's realization of the protocol. The suite drives ONLY
// these methods, so every binding is judged by the same invariants.
type Binding interface {
	Name() string

	// R1 — presence is a PUBLISHED FACT WITH FRESHNESS.
	// Register publishes existence; Touch refreshes last_seen (a heartbeat, or a
	// cursor advance); Presence reports {alive, last_seen, registered}. A stale
	// last_seen reads as present-but-not-alive — that is how you detect the
	// "came back days later, one agent never returned" dead mailbox.
	Register(id string) error
	Touch(id string) error
	Presence(id string) (alive bool, lastSeen time.Time, registered bool)

	// D1 (atomic visibility) + D2 (no-overwrite). Deliver is all-or-nothing —
	// a reader never sees a torn message — and never clobbers an existing one.
	// Delivery to an UNREGISTERED recipient is refused: a dead mailbox fails the
	// send instead of swallowing it. Delivery is AT-MOST-ONCE (B7): a Msg whose
	// ID collides with one already delivered (TS3) is refused, NOT silently
	// deduped — the caller must retry with a fresh ID, mirroring the reference
	// implementation's link-EEXIST-then-fresh-suffix discipline (C4).
	Deliver(to string, m Msg) error

	// O1 — per-sender FIFO is GUARANTEED; cross-sender order is arrival order
	// and is ADVISORY (claiming a cross-sender total order across independent
	// clocks is the fiction the v2 deltas remove). Poll is read-only.
	Poll(id string) ([]Msg, error)

	// C1 — consume is AT-LEAST-ONCE. The handler runs (act), THEN the consume is
	// committed. A crash between the two re-delivers on retry; at-most-once would
	// silently drop a coordination message, the worst failure for this bus.
	Consume(id string, handler func(Msg) error) (consumed int, err error)

	// B1 — the §5 FORK, as a single bool. Inbox model: bodies are private to the
	// recipient (true). Shared-log model: every agent can read every record
	// (false). PeekBodies attempts a cross-agent read the way a real peer could.
	PrivateBodies() bool
	PeekBodies(owner, reader string) []string
}

// New returns a binding for a named model. Used by the demo; the suite uses the
// in-package constructors directly.
func New(model string) (Binding, error) {
	switch model {
	case "inbox":
		return newInbox(), nil
	case "log":
		return newLog(), nil
	default:
		return nil, fmt.Errorf("unknown model %q (want: inbox | log)", model)
	}
}
