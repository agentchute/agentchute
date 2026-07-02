package conformance

import (
	"fmt"
	"sync"
	"time"
)

// logBinding = the SHARED APPEND-ONLY LOG model (the §5 fork), as a model.
//
//	┌──────────────────────────── one ordered stream ───────────────────────────┐
//	│ seq0 to=bob   seq1 to=alice  seq2 to=bob  seq3 to=carol  seq4 to=bob ...    │
//	└────────────────────────────────────────────────────────────────────────────┘
//	    bob.cursor ─┘                          alice.cursor ─┘
//
// Everyone APPENDS to the stream; each agent keeps a READ CURSOR and FILTERS for
// records addressed to it. Reference storage: a single append file, a git
// branch, a Redis Stream, or a Kafka topic — all the same shape.
//
// What this model BUYS (each shown by the conformance run against it):
//   - O1 cross-agent order is REAL — one global seq, no sender-clock fiction.
//   - Presence is FREE — an agent's last cursor advance IS its last_seen.
//     There is NO .live primitive anywhere. (Compare inbox_binding, which needs
//     a separate published fact.)
//   - No torn-read class — append-only records are atomic by construction.
//   - Audit/replay is inherent — the stream is the history.
//
// What it COSTS (also shown):
//   - B1 is VOID — every agent can read every record. No private bodies.
//
// This is the whole alternate model the team asked for, in ~90 lines. The decision is:
// is inter-agent privacy a real requirement for your pools? If not, this model
// is simpler AND more robust than N inboxes, and deletes the ordering fiction
// and the separate presence primitive in one move.
type record struct {
	seq uint64
	to  string
	m   Msg
}

type logBinding struct {
	mu        sync.Mutex
	log       []record             // the single shared stream (append-only)
	cursor    map[string]uint64    // id -> next seq to read
	advanced  map[string]time.Time // id -> last cursor advance == last_seen
	delivered map[string]bool      // "to|from|seq" -> already landed (EEXIST dedup)
	window    time.Duration
	crash     bool
}

func newLog() *logBinding {
	return &logBinding{cursor: map[string]uint64{}, advanced: map[string]time.Time{}, delivered: map[string]bool{}, window: 30 * time.Second}
}

func (b *logBinding) Name() string { return "log (shared append-only stream + cursors)" }

func (b *logBinding) Profile() string { return "log" }

func (b *logBinding) Register(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cursor[id]; !ok {
		// A new agent starts at the END of the log: it only sees messages sent
		// AFTER it joined — no replay of history it was never part of.
		b.cursor[id] = uint64(len(b.log))
	}
	b.advanced[id] = time.Now()
	return nil
}

func (b *logBinding) Touch(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cursor[id]; !ok {
		return fmt.Errorf("not registered: %s", id)
	}
	b.advanced[id] = time.Now()
	return nil
}

func (b *logBinding) Presence(id string) (bool, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts, ok := b.advanced[id]
	if !ok {
		return false, time.Time{}, false
	}
	// PRESENCE FALLS OUT OF THE CURSOR — no .live file exists in this model.
	return time.Since(ts) < b.window, ts, true
}

func (b *logBinding) Deliver(to string, m Msg) error {
	if m.From == "" {
		return fmt.Errorf("E1: message has no From")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cursor[to]; !ok {
		return fmt.Errorf("unknown recipient %q (mailbox dead / not registered)", to)
	}
	if m.Seq > 0 {
		// Same EEXIST-idempotency as the inbox binding, keyed (to,from,seq).
		k := fmt.Sprintf("%s|%s|%d", to, m.From, m.Seq)
		if b.delivered[k] {
			return nil
		}
		b.delivered[k] = true
	}
	// APPEND is atomic + no-overwrite by construction: a unique monotonic seq,
	// never clobbering a prior record. D1 and D2 come for free.
	seq := uint64(len(b.log))
	b.log = append(b.log, record{seq: seq, to: to, m: m})
	return nil
}

func (b *logBinding) Poll(id string) ([]Msg, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Msg
	for _, r := range b.log {
		if r.seq >= b.cursor[id] && r.to == id { // addressed-filter from the cursor
			out = append(out, r.m)
		}
	}
	// Returned in global seq order == REAL cross-agent order (the O1 win).
	return out, nil
}

func (b *logBinding) Consume(id string, handler func(Msg) error) (int, error) {
	b.mu.Lock()
	start := b.cursor[id]
	snapshot := append([]record(nil), b.log...)
	b.mu.Unlock()

	n := 0
	for _, r := range snapshot {
		if r.seq < start || r.to != id {
			continue
		}
		if err := handler(r.m); err != nil { // ACT
			return n, err
		}
		if b.crash {
			b.crash = false
			panic("C1: simulated crash after act, before cursor advance")
		}
		// COMMIT: advance the cursor PAST this record. A crash before this means
		// the cursor never moved -> the record re-delivers (at-least-once).
		b.mu.Lock()
		b.cursor[id] = r.seq + 1
		b.advanced[id] = time.Now()
		b.mu.Unlock()
		n++
	}
	return n, nil
}

func (b *logBinding) PrivateBodies() bool { return false }

func (b *logBinding) PeekBodies(owner, reader string) []string {
	// The whole stream is readable by anyone — a peer CAN read another agent's
	// bodies. This is the B1 cost of the model, made concrete.
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, r := range b.log {
		if r.to == owner {
			out = append(out, r.m.Body)
		}
	}
	return out
}

// --- test-only affordances (same-package) ---

func (b *logBinding) crashAfterActOnce() { b.crash = true }
func (b *logBinding) forceLastSeen(id string, t time.Time) {
	b.mu.Lock()
	b.advanced[id] = t
	b.mu.Unlock()
}

func (b *logBinding) deliverSlow(to string, m Msg, staged chan<- struct{}, commit <-chan struct{}) error {
	if m.From == "" {
		return fmt.Errorf("E1: no From")
	}
	b.mu.Lock()
	_, ok := b.cursor[to]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown recipient %q", to)
	}
	close(staged) // prepared, not yet appended (invisible)
	<-commit
	b.mu.Lock()
	seq := uint64(len(b.log))
	b.log = append(b.log, record{seq: seq, to: to, m: m})
	b.mu.Unlock()
	return nil
}
