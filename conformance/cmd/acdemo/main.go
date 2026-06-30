// acdemo runs a narrated handoff against a chosen MODEL so the §5 fork is
// visible, not just argued:  go run ./cmd/acdemo inbox   |   go run ./cmd/acdemo log
//
// It exercises the protocol at the wire level (no PTY here — that's the serve
// spike). The point is to SHOW the three places the two models differ:
//   1. cross-agent order  (advisory vs real)
//   2. presence source    (.live file vs cursor advance)
//   3. body privacy (B1)  (private vs shared)
package main

import (
	"fmt"
	"os"

	ac "agentchute.dev/spike/conformance"
)

func main() {
	model := "inbox"
	if len(os.Args) > 1 {
		model = os.Args[1]
	}
	b, err := ac.New(model)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	rule := "──────────────────────────────────────────────────────────"
	fmt.Printf("\nMODEL: %s\n%s\n", b.Name(), rule)

	// everyone registers (publishes existence)
	for _, id := range []string{"alice", "bob", "carol"} {
		must(b.Register(id))
	}
	fmt.Println("registered: alice, bob, carol")

	// alice -> bob, reply-required, with an idempotency key
	must(b.Deliver("bob", ac.Msg{From: "alice", Body: "PING: please review PR 42", ReplyRequired: true, Key: "rev-42"}))
	fmt.Println("alice delivered a reply-required review request to bob")

	// bob consumes and replies
	bobReply := ""
	_, _ = b.Consume("bob", func(m ac.Msg) error {
		fmt.Printf("bob consumed: %q (from %s, reply_required=%v)\n", m.Body, m.From, m.ReplyRequired)
		if m.ReplyRequired {
			bobReply = m.From
		}
		return nil
	})
	if bobReply != "" {
		must(b.Deliver(bobReply, ac.Msg{From: "bob", Body: "PONG: reviewed, looks good", InReplyTo: "rev-42"}))
		fmt.Println("bob replied PONG -> alice")
	}

	// alice consumes the reply
	_, _ = b.Consume("alice", func(m ac.Msg) error {
		fmt.Printf("alice consumed: %q (from %s)\n", m.Body, m.From)
		return nil
	})

	fmt.Printf("%s\n", rule)

	// (1) ordering source
	switch model {
	case "log":
		fmt.Println("ORDER : real cross-agent order — one global sequence")
	default:
		fmt.Println("ORDER : per-sender FIFO guaranteed; cross-sender = arrival order (advisory)")
	}

	// (2) presence source
	aliveA, _, _ := b.Presence("alice")
	aliveC, _, _ := b.Presence("carol")
	switch model {
	case "log":
		fmt.Printf("PRES  : derived from CURSOR advance — no .live file. alice alive=%v, carol alive=%v\n", aliveA, aliveC)
	default:
		fmt.Printf("PRES  : a published .live fact per agent. alice alive=%v, carol alive=%v\n", aliveA, aliveC)
	}

	// (3) the B1 fork — can a peer read bob's message?
	peer := b.PeekBodies("bob", "carol")
	if b.PrivateBodies() {
		fmt.Printf("B1    : PRIVATE — peer 'carol' sees %d of bob's bodies. Keep this model if inter-agent privacy is required.\n", len(peer))
	} else {
		fmt.Printf("B1    : SHARED  — peer 'carol' can read bob's bodies: %q. OK only for a single-owner / trusted pool.\n", peer)
	}
	fmt.Println()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
