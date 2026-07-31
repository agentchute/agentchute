package conformance

import (
	"sort"
	"testing"
	"time"
)

// ts_test.go — v2.5 plan B7 vectors: the timestamp+random-suffix message
// identity that replaces the per-(sender,recipient) sequence counter.

// TS1 — the C3 filename grammar parses in chronological order (fixed-width
// C2 stamps make lexicographic sort == chronological sort) and rejects
// malformed names: uppercase hex, a short suffix, colon-separated
// timestamps, non-canonical widths, the wrong extension, and an invalid
// agent_id.
func TestTS1_FilenameGrammar(t *testing.T) {
	v := vectorByID(t, "TS1", "filename_grammar")
	for _, name := range v.ValidNames {
		from, stamp, suffix, ok := parseTsFilename(name)
		if !ok {
			t.Errorf("valid name %q failed to parse", name)
			continue
		}
		if from == "" || stamp == "" || suffix == "" {
			t.Errorf("valid name %q parsed with an empty field: from=%q stamp=%q suffix=%q", name, from, stamp, suffix)
		}
	}
	for _, name := range v.InvalidNames {
		if _, _, _, ok := parseTsFilename(name); ok {
			t.Errorf("invalid name %q parsed as valid, want rejected", name)
		}
	}
	// valid_names must already be listed in ascending order; a plain
	// lexicographic sort must not reorder them (O1-style exactness, applied
	// to the NEW grammar).
	sorted := append([]string(nil), v.ValidNames...)
	sort.Strings(sorted)
	for i := range sorted {
		if sorted[i] != v.ValidNames[i] {
			t.Fatalf("lexicographic sort of valid_names must equal chronological order; got %v, want %v", sorted, v.ValidNames)
		}
	}
}

// TS2 — the monotonic per-sender floor (C7): a mint after a "restart" (a
// fresh TsSender sharing the prior instance's durable Floor) never reissues
// at or before the last stamp actually handed out, even when `now` regresses
// — proving the write-ahead persist-before-deliver ordering is sufficient on
// its own, with no separate crash-recovery mechanism needed.
func TestTS2_MonotonicFloor(t *testing.T) {
	v := vectorByID(t, "TS2", "monotonic_floor")
	now := time.Date(2026, 7, 30, 18, 24, 15, 123456000, time.UTC)

	s1 := NewTsSender(v.Sender, "")
	first := s1.Mint(now)

	// "Restart": a fresh sender sharing the SAME durable floor, minting with
	// an EARLIER now (a clock regression, or simply a wall-clock read racing
	// a floor already ahead of it).
	s2 := NewTsSender(v.Sender, s1.Floor)
	second := s2.Mint(now.Add(-time.Hour))
	if second <= first {
		t.Fatalf("mint after restart = %q, must be strictly greater than %q", second, first)
	}
	floorTime, ok := parseStamp(first)
	if !ok {
		t.Fatalf("parse first stamp %q", first)
	}
	if want := formatStamp(floorTime.Add(time.Microsecond)); second != want {
		t.Fatalf("mint after restart = %q, want exactly floor+1us = %q", second, want)
	}

	// A mint clearly AHEAD of the floor needs no clamping.
	ahead := s2.Mint(now.Add(time.Hour))
	if want := formatStamp(now.Add(time.Hour)); ahead != want {
		t.Fatalf("mint clearly ahead of the floor = %q, want %q (no clamping needed)", ahead, want)
	}
}

// TS3 — on a delivery collision (the same ID delivered twice), the binding
// REFUSES the second delivery rather than silently deduping it (delivery is
// at-most-once, B7); the sender retries with a FRESH id and the retried
// delivery lands as a genuinely separate message (C4).
func TestTS3_CollisionRetry(t *testing.T) {
	v := vectorByID(t, "TS3", "collision_retry")
	eachApplicableBinding(t, v, func(t *testing.T, b Binding) {
		must(t, b.Register(v.Recipient))

		id := tsFilename(v.Sender, formatStamp(time.Now()), rand128hex())
		must(t, b.Deliver(v.Recipient, Msg{From: v.Sender, Body: v.Message.Body, ID: id}))

		// A resend under the IDENTICAL id must be REFUSED, not silently
		// deduped.
		if err := b.Deliver(v.Recipient, Msg{From: v.Sender, Body: "different body", ID: id}); err == nil {
			t.Fatal("expected a collision refusal for a reused id")
		}

		// Retrying with a FRESH id must succeed, and BOTH messages survive —
		// this is not dedup, it is two independent deliveries.
		freshID := tsFilename(v.Sender, formatStamp(time.Now()), rand128hex())
		must(t, b.Deliver(v.Recipient, Msg{From: v.Sender, Body: "different body", ID: freshID}))

		got, _ := b.Poll(v.Recipient)
		if len(got) != 2 {
			t.Fatalf("want 2 delivered messages (original + fresh-id retry), got %d", len(got))
		}
	})
}

// DR1 — during the migration window a lister must recognize BOTH the OLD
// (to,from,seq) grammar and the NEW (v2.5 plan B7) timestamp grammar, and
// must classify anything else as garbage (never silently misparsed as
// either). Mirrors the reference implementation's single dual-read choke
// point (parseAnyInboxName).
func TestDR1_DualReadListing(t *testing.T) {
	v := vectorByID(t, "DR1", "dual_read_listing")
	for _, name := range v.OldNames {
		kind, from := classifyInboxName(name)
		if kind != "old" || from == "" {
			t.Errorf("old-format name %q classified as (%s, from=%q), want (old, non-empty from)", name, kind, from)
		}
	}
	for _, name := range v.NewNames {
		kind, from := classifyInboxName(name)
		if kind != "new" || from == "" {
			t.Errorf("new-format name %q classified as (%s, from=%q), want (new, non-empty from)", name, kind, from)
		}
	}
	for _, name := range v.GarbageNames {
		if kind, _ := classifyInboxName(name); kind != "garbage" {
			t.Errorf("garbage name %q classified as %q, want garbage", name, kind)
		}
	}
}
