package conformance

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// bindings under test. Add a new substrate here and it inherits the whole suite.
func bindings() []func() Binding {
	return []func() Binding{
		func() Binding { return newInbox() },
		func() Binding { return newLog() },
	}
}

func eachBinding(t *testing.T, fn func(t *testing.T, b Binding)) {
	for _, mk := range bindings() {
		b := mk()
		t.Run(b.Name(), func(t *testing.T) { fn(t, b) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// R1 — presence is a published fact with freshness. A fresh registration is
// alive; a stale last_seen is present-but-not-alive. THIS is how you detect the
// "came back days later, one agent never returned" dead mailbox.
// Catches: a binding that cannot tell a live agent from a long-gone one.
func TestR1_Presence(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		if _, _, reg := b.Presence("x"); reg {
			t.Fatal("an unregistered agent must not be registered/present")
		}
		must(t, b.Register("x"))
		alive, _, reg := b.Presence("x")
		if !reg || !alive {
			t.Fatal("a freshly registered agent must read alive")
		}
		// simulate a long-idle / dead agent
		b.(interface {
			forceLastSeen(string, time.Time)
		}).forceLastSeen("x", time.Now().Add(-time.Hour))
		alive, _, reg = b.Presence("x")
		if !reg || alive {
			t.Fatal("a stale agent must read registered-but-not-alive (the dead mailbox)")
		}
	})
}

// D1 — atomic visibility. A message mid-delivery (staged, not committed) is
// invisible to a concurrent reader; after commit it is fully visible.
// Catches: readers observing torn / half-written messages.
func TestD1_AtomicVisibility(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		sd := b.(interface {
			deliverSlow(string, Msg, chan<- struct{}, <-chan struct{}) error
		})
		staged := make(chan struct{})
		commit := make(chan struct{})
		done := make(chan struct{})
		go func() { _ = sd.deliverSlow("bob", Msg{From: "alice", Body: "hello"}, staged, commit); close(done) }()

		<-staged // delivery is in-flight but not committed
		if got, _ := b.Poll("bob"); len(got) != 0 {
			t.Fatalf("a staged-but-uncommitted message must be invisible; saw %d", len(got))
		}
		close(commit)
		<-done
		got, _ := b.Poll("bob")
		if len(got) != 1 || got[0].Body != "hello" {
			t.Fatalf("after commit, exactly the whole message must be visible; got %+v", got)
		}
	})
}

// D2 — no-overwrite under concurrency. N concurrent deliveries all survive; none
// is silently clobbered. (Inbox: ln-no-overwrite+retry. Log: unique append seq.)
// Catches: dropped coordination messages under load.
func TestD2_NoOverwrite(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		const N = 200
		var wg sync.WaitGroup
		for i := 0; i < N; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_ = b.Deliver("bob", Msg{From: fmt.Sprintf("s%d", i%5), Body: fmt.Sprintf("m%d", i)})
			}(i)
		}
		wg.Wait()
		if got, _ := b.Poll("bob"); len(got) != N {
			t.Fatalf("want %d delivered, got %d (clobber / loss)", N, len(got))
		}
	})
}

// O1 — per-sender FIFO is GUARANTEED. We deliberately do NOT assert a
// cross-sender total order: claiming one across independent senders is the
// fiction the v2 deltas remove. Cross-sender order is arrival order, advisory.
// Catches: a binding that reorders one sender's own messages.
func TestO1_PerSenderFIFO(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		for i := 0; i < 10; i++ {
			must(t, b.Deliver("bob", Msg{From: "alice", Body: fmt.Sprintf("a%d", i)}))
		}
		got, _ := b.Poll("bob")
		if len(got) != 10 {
			t.Fatalf("want 10, got %d", len(got))
		}
		for i := 0; i < 10; i++ {
			if got[i].Body != fmt.Sprintf("a%d", i) {
				t.Fatalf("per-sender FIFO broken at %d: %q", i, got[i].Body)
			}
		}
	})
}

// C1 — at-least-once + idempotent. A crash AFTER the handler acts but BEFORE the
// consume commits must RE-DELIVER on retry (never drop). Then msg_key collapses
// the duplicate on the receiver side.
// Catches: at-most-once consume losing a coordination message on a crash.
func TestC1_AtLeastOnceIdempotent(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		must(t, b.Deliver("bob", Msg{From: "alice", Body: "do-it", Key: "k1"}))

		var acts []string
		handler := func(m Msg) error { acts = append(acts, m.Body); return nil }

		// first consume crashes after acting, before committing
		b.(interface{ crashAfterActOnce() }).crashAfterActOnce()
		func() {
			defer func() { _ = recover() }() // absorb the simulated crash
			_, _ = b.Consume("bob", handler)
		}()
		if len(acts) != 1 {
			t.Fatalf("handler must have acted once before the crash; got %d", len(acts))
		}

		// retry: commit never happened, so the message must re-deliver
		n, _ := b.Consume("bob", handler)
		if n != 1 || len(acts) != 2 {
			t.Fatalf("at-least-once: message must re-deliver after crash; acts=%d n=%d", len(acts), n)
		}

		// receiver-side idempotency aid: msg_key collapses the duplicate effect
		d := NewDeduper()
		effects := 0
		for range acts { // both carry Key k1
			_ = d.Once(Msg{Key: "k1"}, func(Msg) error { effects++; return nil })
		}
		if effects != 1 {
			t.Fatalf("msg_key dedup must collapse re-delivery to one effect; got %d", effects)
		}
	})
}

// E1 — receivers ignore unknown fields; senders cannot omit a normative field.
// Catches: a future field silently breaking old receivers; an anonymous message
// being accepted.
func TestE1_Envelope(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		// unknown/future fields are carried but ignorable
		must(t, b.Deliver("bob", Msg{From: "alice", Body: "hi", Extra: map[string]string{"future_field": "v"}}))
		got, _ := b.Poll("bob")
		if got[0].From != "alice" || got[0].Body != "hi" {
			t.Fatal("normative fields must survive alongside unknown fields")
		}
		// a normative field (From) is required
		if err := b.Deliver("bob", Msg{Body: "no from"}); err == nil {
			t.Fatal("a message without From must be refused")
		}
	})
}

// B1 — THE §5 FORK, made executable. Inbox: a peer cannot read another agent's
// bodies. Log: it can. The SAME assertion yields opposite — but each
// correct-for-its-model — results. This test never "fails" a model; it PRINTS
// the privacy posture you are choosing. Run with -v to see the verdict.
func TestB1_PrivacyFork(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		must(t, b.Deliver("bob", Msg{From: "alice", Body: "secret for bob"}))
		peerSees := b.PeekBodies("bob", "carol")

		if b.PrivateBodies() {
			if len(peerSees) != 0 {
				t.Fatalf("PrivateBodies()==true but peer read %d bodies", len(peerSees))
			}
			t.Logf("B1 HOLDS — peer 'carol' sees 0 of bob's bodies. Choose this model if inter-agent privacy is required.")
		} else {
			if len(peerSees) == 0 {
				t.Fatal("PrivateBodies()==false but a peer read nothing (the log should expose them)")
			}
			t.Logf("B1 VOID — peer 'carol' can read %d of bob's bodies. Acceptable ONLY if the pool is single-owner / trusted.", len(peerSees))
		}
	})
}
