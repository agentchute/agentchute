package conformance

import "testing"

// D1 (durable commit ordering). Proves the canonical sequence
// write(tmp) -> fsync(tmp) -> link -> fsync(dir), and that a crash at EVERY step
// leaves the record absent-or-whole, never torn. The load-bearing assertion:
// after a crash AFTER link but BEFORE fsync(dir), the record is not yet durable;
// and there is no state where a record is durable+present but its contents were
// never fsync'd. Catches: linking before fsync -> a record that survives a power
// cut without its body.
func TestD1_FsyncOrdering(t *testing.T) {
	// happy path: full sequence, correct order
	var f fakeFS
	durableCommit(&f, "")
	want := "[write(tmp) -> fsync(tmp) -> link(tmp,final) -> fsync(dir)]"
	if f.orderString() != want {
		t.Fatalf("commit order wrong:\n got %s\nwant %s", f.orderString(), want)
	}
	if p, w := f.survivesWhole(); !p || !w {
		t.Fatalf("after full commit a record must be present+whole; present=%v whole=%v", p, w)
	}

	// crash at each step: the record must be absent-or-whole, never torn.
	for _, crashAt := range []op{opWriteTmp, opFsyncTmp, opLink, opFsyncDir} {
		var c fakeFS
		durableCommit(&c, crashAt)
		present, whole := c.survivesWhole()
		// the only forbidden state: present (durable dir entry) but NOT whole.
		if present && !whole {
			t.Fatalf("crash before %s yields a TORN record (present, contents not fsync'd): %s", crashAt, c.orderString())
		}
		// fsync(tmp) MUST come before link in any sequence that linked.
		if c.linked && !c.tmpFsynced {
			t.Fatalf("crash before %s: linked without fsync(tmp) first (the load-bearing ordering bug)", crashAt)
		}
	}
}

// C1, sender side (crash-resume + EEXIST dedup). The consumer-crash half is in
// TestC1_AtLeastOnceIdempotent; THIS is the sender half and the one most likely
// to catch a real bug in the per-(from,to) seq allocator.
//
// Scenario: a sender links seq=N, then crashes before its durable counter
// advanced. On resume it RE-ISSUES seq=N. EEXIST must make that a safe no-op
// (exactly one copy), and the next genuinely-new message must get N+1.
//
// It also pins the HAZARD (§7 dup-writer assumption): reusing a seq for
// DIFFERENT content is silently dropped. The test asserts that this is what
// happens — making the assumption executable, and documenting why the seq
// counter must be durable+monotonic and ids must be unique per process.
func TestC1_SenderCrashResume(t *testing.T) {
	eachBinding(t, func(t *testing.T, b Binding) {
		must(t, b.Register("bob"))
		s := NewSeqSender("alice")

		// normal send: seq=1 lands
		seq1, err := s.Send(b, "bob", "m1", "k1")
		must(t, err)
		if seq1 != 1 {
			t.Fatalf("first seq must be 1, got %d", seq1)
		}

		// CRASH: counter not made durable -> on resume the sender re-issues seq=1
		s.loseCounter("bob")
		seqR, err := s.Send(b, "bob", "m1", "k1") // identical resend
		must(t, err)
		if seqR != 1 {
			t.Fatalf("resume must re-issue seq=1, got %d", seqR)
		}

		// EEXIST made the resend a no-op: exactly one copy of m1.
		got, _ := b.Poll("bob")
		if n := count(got, "m1"); n != 1 {
			t.Fatalf("EEXIST must dedup the crash-resend; want 1 copy of m1, got %d", n)
		}

		// the next genuinely-new message gets the next seq and lands.
		seq2, err := s.Send(b, "bob", "m2", "k2")
		must(t, err)
		if seq2 != 2 {
			t.Fatalf("next message must get seq=2, got %d", seq2)
		}
		got, _ = b.Poll("bob")
		if count(got, "m2") != 1 {
			t.Fatal("m2 must land")
		}

		// HAZARD (executable §7 assumption): reusing an already-landed seq for
		// DIFFERENT content is SILENTLY DROPPED. This is why the impl MUST keep
		// the seq counter durable+monotonic and give each process a unique id.
		must(t, b.Deliver("bob", Msg{From: "alice", Body: "DIFFERENT", Seq: 1}))
		got, _ = b.Poll("bob")
		if count(got, "DIFFERENT") != 0 {
			t.Fatal("expected the seq-reuse-with-different-content to be dropped (the documented hazard)")
		}
		t.Logf("HAZARD confirmed: reusing seq for different content is dropped -> seq counter MUST be durable+monotonic, ids unique per process (§7).")
	})
}

func count(ms []Msg, body string) int {
	n := 0
	for _, m := range ms {
		if m.Body == body {
			n++
		}
	}
	return n
}
