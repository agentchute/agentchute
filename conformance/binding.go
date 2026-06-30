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

// Msg is the message AFTER the v2-delta cuts.
//
// WHY these fields and no others:
//   - From          : normative. Who sent it. (Receiver MUST reject a message
//     with no From — an anonymous message has no accountability.)
//   - ReplyRequired : the ONE cross-agent coordination bit worth keeping. Under
//     the v2 deltas the *obligation* is owned by the asker; this
//     bit is only an advisory hint to the recipient.
//   - InReplyTo     : optional thread link.
//   - Key (msg_key) : optional idempotency key. No-overwrite stops DELIVERY
//     duplicates; it does nothing for CRASH-RETRY duplicates
//     (sender resends, unsure the first landed). Key lets the
//     receiver dedup the logical event.
//   - Extra         : unknown/future fields. Carried, never required. Proves
//     forward-compat (E1): old receivers ignore new fields.
//
// What's deliberately ABSENT: `to` (addressing is structural — which inbox / the
// record's recipient), and `message_id` (identity is whatever the binding
// confirms on delivery, not a second sender-asserted handle). Both were cut.
type Msg struct {
	From          string
	Body          string
	ReplyRequired bool
	InReplyTo     string
	Key           string
	Extra         map[string]string

	// Seq is the per-(sender,recipient) sequence number (the team's v2 identity:
	// (to,from,seq) replaces the random nonce as sort key AND identity). When >0,
	// a binding treats (to,from,seq) as the delivery key: a re-delivery of the
	// SAME tuple is a no-op success (link()-EEXIST = "already landed"). This is
	// what makes a crash-uncertain sender's resend safe. Seq==0 = legacy append.
	Seq uint64
}

// SeqSender allocates per-(from,to) sequence numbers and delivers with
// EEXIST-idempotent semantics. A crash-uncertain resend reuses the same seq, so
// EEXIST = safe no-op. The counter MUST be durable+monotonic in a real impl;
// the crash test shows exactly why (reusing a seq for DIFFERENT content drops it).
type SeqSender struct {
	From string
	next map[string]uint64 // to -> last seq issued
}

func NewSeqSender(from string) *SeqSender { return &SeqSender{From: from, next: map[string]uint64{}} }

func (s *SeqSender) Send(b Binding, to, body, key string) (uint64, error) {
	seq := s.next[to] + 1
	if err := b.Deliver(to, Msg{From: s.From, Body: body, Key: key, Seq: seq}); err != nil {
		return 0, err
	}
	s.next[to] = seq
	return seq, nil
}

// loseCounter simulates the sender crashing before its seq counter was made
// durable: on resume it will re-issue the seq it already used.
func (s *SeqSender) loseCounter(to string) { s.next[to] = s.next[to] - 1 }

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
	// send instead of swallowing it.
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

// Deduper is the RECEIVER-SIDE idempotency aid the spec calls for. At-least-once
// means the same logical message can arrive twice; a receiver that cares about
// side-effects uses msg_key to collapse the duplicate. Dedup is the HANDLER's
// job, not the binding's — this shows the pattern in ~5 lines.
type Deduper struct{ seen map[string]bool }

func NewDeduper() *Deduper { return &Deduper{seen: map[string]bool{}} }

func (d *Deduper) Once(m Msg, fn func(Msg) error) error {
	if m.Key != "" && d.seen[m.Key] {
		return nil // already applied this logical event
	}
	if err := fn(m); err != nil {
		return err
	}
	if m.Key != "" {
		d.seen[m.Key] = true
	}
	return nil
}
