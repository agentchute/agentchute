package conformance

import (
	"fmt"
	"sync"
	"time"
)

// inboxBinding = the N-PRIVATE-INBOXES model (today's agentchute).
//
// Reference storage is the filesystem: one directory per agent, files as
// messages, `ln` for no-overwrite, tmp+rename for atomic visibility (see ../ach).
// Modeled in-memory here ONLY so the suite can drive concurrency and crashes
// deterministically — the semantics are identical to the FS binding.
//
// What this model buys: B1 — each recipient owns its bodies. No peer can read
// another agent's mail.
// What it costs (and the suite shows): cross-agent order is not real (separate
// inboxes, separate sender clocks), and presence needs a SEPARATE published fact
// (last_seen / the .live file), because an idle inbox looks identical whether
// the agent is alive or long gone.
type inboxBinding struct {
	mu        sync.Mutex
	inbox     map[string][]Msg     // id -> ordered messages
	seen      map[string]time.Time // id -> last_seen  (the .live fact)
	delivered map[string]bool      // "to|from|seq" -> already landed (EEXIST dedup)
	window    time.Duration        // freshness window for "alive"
	crash     bool                 // C1 fault: panic after act, before commit
}

func newInbox() *inboxBinding {
	return &inboxBinding{inbox: map[string][]Msg{}, seen: map[string]time.Time{}, delivered: map[string]bool{}, window: 30 * time.Second}
}

func (b *inboxBinding) Name() string { return "inbox (N private inboxes)" }

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
	if m.Seq > 0 {
		// link()-EEXIST semantics: this exact (to,from,seq) already landed, so a
		// crash-uncertain resend is a SAFE NO-OP (success), not a duplicate. The
		// hazard the crash test exposes: if a DIFFERENT body reuses a seq, this
		// silently drops it — which is why the seq counter must be durable.
		k := fmt.Sprintf("%s|%s|%d", to, m.From, m.Seq)
		if b.delivered[k] {
			return nil
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

func (b *inboxBinding) crashAfterActOnce()              { b.crash = true }
func (b *inboxBinding) forceLastSeen(id string, t time.Time) { b.mu.Lock(); b.seen[id] = t; b.mu.Unlock() }

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
