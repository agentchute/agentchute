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

func eachApplicableBinding(t *testing.T, v testVector, fn func(t *testing.T, b Binding)) {
	t.Helper()
	ran := 0
	for _, mk := range bindings() {
		b := mk()
		if !v.appliesTo(bindingProfile(b)) {
			continue
		}
		ran++
		t.Run(b.Name(), func(t *testing.T) { fn(t, b) })
	}
	if ran == 0 {
		t.Fatalf("vector %s applies_to matched no bindings", v.ID)
	}
}

func (v testVector) appliesTo(profile string) bool {
	if v.AppliesTo == nil {
		return true
	}
	for _, allowed := range v.AppliesTo {
		if allowed == profile {
			return true
		}
	}
	return false
}

func bindingProfile(b Binding) string {
	if profiled, ok := b.(interface{ Profile() string }); ok {
		return profiled.Profile()
	}
	return b.Name()
}

func knownProfile(profile string) bool {
	return profile == "inbox" || profile == "log"
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
	v := vectorByID(t, "R1", "presence_freshness")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		if _, _, reg := b.Presence(v.Agent); reg {
			t.Fatal("an unregistered agent must not be registered/present")
		}
		must(t, b.Register(v.Agent))
		alive, _, reg := b.Presence(v.Agent)
		if !reg || !alive {
			t.Fatal("a freshly registered agent must read alive")
		}
		// simulate a long-idle / dead agent
		b.(interface {
			forceLastSeen(string, time.Time)
		}).forceLastSeen(v.Agent, time.Now().Add(-time.Duration(v.StaleSeconds)*time.Second))
		alive, _, reg = b.Presence(v.Agent)
		if !reg || alive {
			t.Fatal("a stale agent must read registered-but-not-alive (the dead mailbox)")
		}
	})
}

// D1 — atomic visibility. A message mid-delivery (staged, not committed) is
// invisible to a concurrent reader; after commit it is fully visible.
// Catches: readers observing torn / half-written messages.
func TestD1_AtomicVisibility(t *testing.T) {
	v := vectorByID(t, "D1", "atomic_visibility")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))
		sd := b.(interface {
			deliverSlow(string, Msg, chan<- struct{}, <-chan struct{}) error
		})
		staged := make(chan struct{})
		commit := make(chan struct{})
		done := make(chan struct{})
		go func() { _ = sd.deliverSlow(v.Recipient, v.Message.msg(), staged, commit); close(done) }()

		<-staged // delivery is in-flight but not committed
		if got, _ := b.Poll(v.Recipient); len(got) != 0 {
			t.Fatalf("a staged-but-uncommitted message must be invisible; saw %d", len(got))
		}
		close(commit)
		<-done
		got, _ := b.Poll(v.Recipient)
		if len(got) != 1 || got[0].Body != v.Message.Body {
			t.Fatalf("after commit, exactly the whole message must be visible; got %+v", got)
		}
	})
}

// D2 — no-overwrite under concurrency. N concurrent deliveries all survive; none
// is silently clobbered. (Inbox: ln-no-overwrite+retry. Log: unique append seq.)
// Catches: dropped coordination messages under load.
func TestD2_NoOverwrite(t *testing.T) {
	v := vectorByID(t, "D2", "no_overwrite")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))
		var wg sync.WaitGroup
		for i := 0; i < v.Count; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_ = b.Deliver(v.Recipient, Msg{From: v.senderFor(i), Body: fmt.Sprintf("%s%d", v.BodyPrefix, i)})
			}(i)
		}
		wg.Wait()
		if got, _ := b.Poll(v.Recipient); len(got) != v.Count {
			t.Fatalf("want %d delivered, got %d (clobber / loss)", v.Count, len(got))
		}
	})
}

// O1 — per-sender FIFO is GUARANTEED. We deliberately do NOT assert a
// cross-sender total order: claiming one across independent senders is the
// fiction the v2 deltas remove. Cross-sender order is arrival order, advisory.
// Catches: a binding that reorders one sender's own messages.
func TestO1_PerSenderFIFO(t *testing.T) {
	v := vectorByID(t, "O1", "per_sender_fifo")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))
		for _, body := range v.Bodies {
			must(t, b.Deliver(v.Recipient, Msg{From: v.Sender, Body: body}))
		}
		got, _ := b.Poll(v.Recipient)
		if len(got) != len(v.Bodies) {
			t.Fatalf("want %d, got %d", len(v.Bodies), len(got))
		}
		for i, want := range v.Bodies {
			if got[i].Body != want {
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
	v := vectorByID(t, "C1", "consume_redelivery")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))
		must(t, b.Deliver(v.Recipient, v.Message.msg()))

		var acts []string
		handler := func(m Msg) error { acts = append(acts, m.Body); return nil }

		// first consume crashes after acting, before committing
		b.(interface{ crashAfterActOnce() }).crashAfterActOnce()
		func() {
			defer func() { _ = recover() }() // absorb the simulated crash
			_, _ = b.Consume(v.Recipient, handler)
		}()
		if len(acts) != 1 {
			t.Fatalf("handler must have acted once before the crash; got %d", len(acts))
		}

		// retry: commit never happened, so the message must re-deliver
		n, _ := b.Consume(v.Recipient, handler)
		if n != 1 || len(acts) != 2 {
			t.Fatalf("at-least-once: message must re-deliver after crash; acts=%d n=%d", len(acts), n)
		}

		// receiver-side idempotency aid: msg_key collapses the duplicate effect
		d := NewDeduper()
		effects := 0
		for range acts { // both carry the same Key
			_ = d.Once(Msg{Key: v.Message.Key}, func(Msg) error { effects++; return nil })
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
	v := vectorByID(t, "E1", "envelope")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))
		// unknown/future fields are carried but ignorable
		must(t, b.Deliver(v.Recipient, v.Message.msg()))
		got, _ := b.Poll(v.Recipient)
		if got[0].From != v.Message.From || got[0].Body != v.Message.Body {
			t.Fatal("normative fields must survive alongside unknown fields")
		}
		// a normative field (From) is required
		if err := b.Deliver(v.Recipient, v.InvalidMessage.msg()); err == nil {
			t.Fatal("a message without From must be refused")
		}
	})
}

// B1 — THE §5 FORK, made executable. Inbox: a peer cannot read another agent's
// bodies. Log: it can. The SAME assertion yields opposite — but each
// correct-for-its-model — results. This test never "fails" a model; it PRINTS
// the privacy posture you are choosing. Run with -v to see the verdict.
func TestB1_PrivacyFork(t *testing.T) {
	v := vectorByID(t, "B1", "body_privacy")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))
		must(t, b.Deliver(v.Recipient, v.Message.msg()))
		peerSees := b.PeekBodies(v.Recipient, v.Reader)

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
