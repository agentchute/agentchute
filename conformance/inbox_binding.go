package conformance

import (
	"fmt"
	"sync"
	"time"
)

// inboxBinding = the N-PRIVATE-INBOXES model (today's agentchute).
//
// Reference storage is the filesystem: one directory per agent, files as
// messages, `ln` for no-overwrite, tmp+rename for atomic visibility (see internal/loop).
// Modeled in-memory here ONLY so the suite can drive concurrency and crashes
// deterministically — the semantics are identical to the FS binding.
//
// What this model buys: B1 — each recipient owns its bodies. No peer can read
// another agent's mail.
// What it costs (and the suite shows): cross-agent order is not real (separate
// inboxes, separate sender clocks), and presence needs a SEPARATE published fact
// (last_seen, v2.5 plan B5 — read directly from the registration row, no
// separate presence file), because an idle inbox looks identical whether the
// agent is alive or long gone.
type inboxBinding struct {
	mu        sync.Mutex
	inbox     map[string][]Msg     // id -> ordered messages
	seen      map[string]time.Time // id -> last_seen (the registration row's presence fact)
	delivered map[string]bool      // "to|id" -> already landed (v2.5 plan B7: collision detection ONLY, never a dedup no-op)
	malformed map[string][]string  // id -> quarantined item names (§11.1)
	window    time.Duration        // freshness window for "alive"
	crash     bool                 // C1 fault: panic after act, before commit
}

func newInbox() *inboxBinding {
	return &inboxBinding{inbox: map[string][]Msg{}, seen: map[string]time.Time{}, delivered: map[string]bool{}, malformed: map[string][]string{}, window: 30 * time.Second}
}

func (b *inboxBinding) Name() string { return "inbox (N private inboxes)" }

func (b *inboxBinding) Profile() string { return "inbox" }

func (b *inboxBinding) Register(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.inbox[id]; !ok {
		b.inbox[id] = nil // an agent registers by its inbox existing
	}
	b.seen[id] = time.Now()
	return nil
}

func (b *inboxBinding) Touch(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[id]; !ok {
		return fmt.Errorf("not registered: %s", id)
	}
	b.seen[id] = time.Now()
	return nil
}

func (b *inboxBinding) Presence(id string) (bool, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts, ok := b.seen[id]
	if !ok {
		return false, time.Time{}, false
	}
	return time.Since(ts) < b.window, ts, true
}

func (b *inboxBinding) Deliver(to string, m Msg) error {
	if m.From == "" {
		return fmt.Errorf("E1: message has no From") // normative field required
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.inbox[to]; !ok {
		// Dead-mailbox / D2: refuse delivery to an unregistered recipient.
		return fmt.Errorf("unknown recipient %q (mailbox dead / not registered)", to)
	}
	if m.ID != "" {
		// v2.5 plan B7: a collision is REFUSED, never a silent dedup no-op —
		// delivery is at-most-once now (TS3). The caller (a real sender) is
		// responsible for retrying with a fresh ID, exactly as
		// DeliverUnderRecipientLock does on a real link EEXIST (C4).
		k := to + "|" + m.ID
		if b.delivered[k] {
			return fmt.Errorf("TS3: id %q already delivered to %q (collision; retry with a fresh id)", m.ID, to)
		}
		b.delivered[k] = true
	}
	// Atomic + no-overwrite: the message is fully formed before it is appended
	// (visible), and appending a new slot never clobbers an existing message.
	b.inbox[to] = append(b.inbox[to], m)
	return nil
}

func (b *inboxBinding) Poll(id string) ([]Msg, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Msg, len(b.inbox[id]))
	copy(out, b.inbox[id])
	return out, nil // append order == arrival order; one sender's order is preserved (O1)
}

func (b *inboxBinding) Consume(id string, handler func(Msg) error) (int, error) {
	b.mu.Lock()
	msgs := append([]Msg(nil), b.inbox[id]...)
	b.mu.Unlock()

	n := 0
	for i := range msgs {
		if err := handler(msgs[i]); err != nil { // ACT
			return n, err
		}
		if b.crash {
			b.crash = false
			panic("C1: simulated crash after act, before commit")
		}
		// COMMIT: drop the oldest (the one we just handled). A crash before this
		// leaves the message in the inbox -> it re-delivers (at-least-once).
		// v2.5 plan B7: the delivered-map entry is NOT cleared here — an ID is
		// never intentionally reused (the real grammar's timestamp+random
		// suffix makes that pointless), so `delivered` is a permanent
		// collision record, not a pre-consume-only dedup window.
		b.mu.Lock()
		if len(b.inbox[id]) > 0 {
			b.inbox[id] = b.inbox[id][1:]
		}
		b.seen[id] = time.Now() // a consume is also a liveness signal
		b.mu.Unlock()
		n++
	}
	return n, nil
}

func (b *inboxBinding) PrivateBodies() bool { return true }

func (b *inboxBinding) PeekBodies(owner, reader string) []string {
	// The model ISOLATES inboxes: a non-owner has no path to another agent's
	// bodies. (FS binding: filesystem perms + the rule "never open a peer inbox".)
	if reader != owner {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, m := range b.inbox[owner] {
		out = append(out, m.Body)
	}
	return out
}

// --- test-only affordances (same-package) ---

func (b *inboxBinding) crashAfterActOnce() { b.crash = true }
func (b *inboxBinding) forceLastSeen(id string, t time.Time) {
	b.mu.Lock()
	b.seen[id] = t
	b.mu.Unlock()
}

// deliverSlow stages a message (invisible), signals, waits, then commits — the
// hook the D1 test uses to prove a mid-flight message is never observable.
func (b *inboxBinding) deliverSlow(to string, m Msg, staged chan<- struct{}, commit <-chan struct{}) error {
	if m.From == "" {
		return fmt.Errorf("E1: no From")
	}
	b.mu.Lock()
	_, ok := b.inbox[to]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown recipient %q", to)
	}
	close(staged) // prepared (models the tmp file) — NOT yet visible
	<-commit
	b.mu.Lock()
	b.inbox[to] = append(b.inbox[to], m) // atomic make-visible
	b.mu.Unlock()
	return nil
}

// DeliverRaw models §11.1 quarantine at the substrate boundary: an inbound
// item either decodes into a Msg (m != nil — same as Deliver) or it doesn't
// (m == nil), in which case it is quarantined rather than delivered. This is
// deliberately NOT part of the Binding interface: malformed-ness is a
// wire/filename-grammar concern specific to the FS+frontmatter substrate, not
// one of the seven substrate-agnostic invariants (a log-model record is
// already-typed Go data with nothing analogous to a bad filename or
// unparseable YAML — see logBinding, which has no DeliverRaw).
//
// Real reference behavior this stands in for (internal/loop.QuarantineInboxFile
// + check.go): quarantine is a pure directory move done BEFORE any seq/consume
// bookkeeping runs, so it never advances a seq counter and never occupies a
// delivery/consume slot — modeled here by routing straight to the `malformed`
// bucket instead of `inbox[to]`, touching no seq/delivered state at all.
func (b *inboxBinding) DeliverRaw(to, item string, m *Msg) error {
	if m == nil {
		// §11.1.1 (quarantine) + §11.1.3 (continue): record it where it stays
		// observable — never silently dropped — and do not block delivery of
		// any other item to this or any other recipient.
		b.mu.Lock()
		b.malformed[to] = append(b.malformed[to], item)
		b.mu.Unlock()
		return nil
	}
	return b.Deliver(to, *m)
}

// MalformedItems returns the quarantined item names for id, oldest-first.
func (b *inboxBinding) MalformedItems(id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.malformed[id]))
	copy(out, b.malformed[id])
	return out
}
