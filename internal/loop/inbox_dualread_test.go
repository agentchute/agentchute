package loop

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Gate 4 dual-read window: ListInboxMessagesWithSkipped must enumerate BOTH the
// legacy nonce format (`<ts>_from-<s>_msg-<nonce>.md`) and the canonical seq
// format (`from-<s>_seq-<020d>.md`), never quarantine a seq file, and preserve
// EXACT per-sender FIFO across both formats with the filename-lexicographic sort.

// writeRaw drops a file with an exact name (bypassing the writers) so the lister
// can be driven against hand-crafted legacy + seq names deterministically.
func writeRaw(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func legacyName(t time.Time, sender, nonce string) string {
	return formatInboxFilename(t, sender, nonce)
}

func seqName(from string, seq uint64) string {
	return MsgID{From: from, Seq: seq}.Filename()
}

// TestDualReadInterleavedOrdering interleaves legacy and seq files from the SAME
// and DIFFERENT senders and asserts: (a) both formats land in msgs (none in
// skipped), (b) per-sender FIFO is exact — legacy (chronological) before seq
// (ascending by seq) within each sender, (c) the LegacyNonce flag is set
// correctly, proving the dual-read drain returns pre-existing nonce mail.
func TestDualReadInterleavedOrdering(t *testing.T) {
	inbox := t.TempDir()

	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)
	t2 := t0.Add(2 * time.Second)

	// Write in a deliberately scrambled order to prove the SORT establishes
	// order, not insertion order.
	writeRaw(t, inbox, seqName("bob", 2), "bob-seq-2")
	writeRaw(t, inbox, legacyName(t2, "alice", "aaaa"), "alice-legacy-t2")
	writeRaw(t, inbox, seqName("alice", 1), "alice-seq-1")
	writeRaw(t, inbox, legacyName(t0, "bob", "bbbb"), "bob-legacy-t0")
	writeRaw(t, inbox, seqName("alice", 10), "alice-seq-10")
	writeRaw(t, inbox, legacyName(t1, "alice", "cccc"), "alice-legacy-t1")
	writeRaw(t, inbox, seqName("bob", 1), "bob-seq-1")
	// A high seq written FIRST and a low seq written LATER must still order by
	// seq, not by mtime — proves the sort key is the zero-padded seq, not mtime.
	writeRaw(t, inbox, seqName("alice", 2), "alice-seq-2")

	msgs, skipped, err := ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("seq files must never be skipped/quarantined; skipped=%v", skipped)
	}
	if len(msgs) != 8 {
		t.Fatalf("got %d msgs, want 8", len(msgs))
	}

	// Per-sender FIFO: filter by sender and assert the expected order.
	wantPerSender := map[string][]string{
		// legacy (t0<t1<t2) first, then seq ascending (1,2,10).
		"alice": {
			legacyName(t1, "alice", "cccc"), // t1
			legacyName(t2, "alice", "aaaa"), // t2 — note: no t0 legacy for alice
			seqName("alice", 1),
			seqName("alice", 2),
			seqName("alice", 10),
		},
		"bob": {
			legacyName(t0, "bob", "bbbb"),
			seqName("bob", 1),
			seqName("bob", 2),
		},
	}
	got := map[string][]string{}
	for _, m := range msgs {
		got[m.Sender] = append(got[m.Sender], m.Filename)
	}
	for sender, want := range wantPerSender {
		g := got[sender]
		if len(g) != len(want) {
			t.Fatalf("sender %s: got %d msgs %v, want %d %v", sender, len(g), g, len(want), want)
		}
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("sender %s order[%d] = %q, want %q (full=%v)", sender, i, g[i], want[i], g)
			}
		}
	}

	// LegacyNonce flag correctness + drain count.
	legacyCount := 0
	for _, m := range msgs {
		isSeqName := seqFilenameRE.MatchString(m.Filename)
		if m.LegacyNonce == isSeqName {
			t.Fatalf("LegacyNonce=%v but filename %q seq-shaped=%v (mismatch)", m.LegacyNonce, m.Filename, isSeqName)
		}
		if m.LegacyNonce {
			legacyCount++
		}
	}
	if legacyCount != 3 {
		t.Fatalf("legacy count = %d, want 3", legacyCount)
	}
	if got := CountLegacyNonce(msgs); got != legacyCount {
		t.Fatalf("CountLegacyNonce = %d, want %d", got, legacyCount)
	}
}

// TestDualReadFirstByteBoundary is the regression guard for the cross-format
// ordering proof (P1): every legacy name (starts with a digit year) sorts before
// every seq name (starts with 'f' of "from-"). If a future writer emits a name
// outside these two shapes, this catches it.
func TestDualReadFirstByteBoundary(t *testing.T) {
	inbox := t.TempDir()
	leg := legacyName(time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC), "zoe", "ffff")
	seq := seqName("aaron", 1) // earliest sender slug + lowest seq
	writeRaw(t, inbox, seq, "seq")
	writeRaw(t, inbox, leg, "legacy")

	msgs, _, err := ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d msgs, want 2", len(msgs))
	}
	// Even with a far-future legacy timestamp and an early-alphabet seq sender,
	// the legacy file MUST sort first (digit < 'f').
	if !msgs[0].LegacyNonce {
		t.Fatalf("first msg should be legacy (digit < 'f'); got %q", msgs[0].Filename)
	}
	if msgs[1].LegacyNonce {
		t.Fatalf("second msg should be seq; got %q", msgs[1].Filename)
	}
}

// TestDualReadSeqTimestampFromMtime verifies a seq message's advisory Timestamp
// is populated from the file mtime (load-bearing for staleness/display), while a
// legacy message keeps its filename-embedded timestamp.
func TestDualReadSeqTimestampFromMtime(t *testing.T) {
	inbox := t.TempDir()
	legTS := time.Date(2026, 6, 1, 10, 0, 0, 123456000, time.UTC)
	writeRaw(t, inbox, legacyName(legTS, "alice", "aaaa"), "legacy")
	seqPath := writeRaw(t, inbox, seqName("alice", 1), "seq")

	mtime := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(seqPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	msgs, _, err := ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	var leg, seq *Message
	for i := range msgs {
		if msgs[i].LegacyNonce {
			leg = &msgs[i]
		} else {
			seq = &msgs[i]
		}
	}
	if leg == nil || seq == nil {
		t.Fatalf("expected one legacy + one seq message, got %v", msgs)
	}
	if !leg.Timestamp.Equal(legTS) {
		t.Fatalf("legacy Timestamp = %s, want %s", leg.Timestamp, legTS)
	}
	if seq.Timestamp.IsZero() {
		t.Fatal("seq Timestamp must not be zero (would mark every seq message ancient)")
	}
	if !seq.Timestamp.UTC().Truncate(time.Second).Equal(mtime.UTC().Truncate(time.Second)) {
		t.Fatalf("seq Timestamp = %s, want ~mtime %s", seq.Timestamp.UTC(), mtime.UTC())
	}
}

// TestInferSenderFromSeqFilename confirms sender inference is total over both
// formats (belt-and-suspenders for any seq name that ever reaches an inference
// caller).
func TestInferSenderFromSeqFilename(t *testing.T) {
	from, ok := InferSenderFromFilename(seqName("codex", 7))
	if !ok || from != "codex" {
		t.Fatalf("InferSenderFromFilename(seq) = %q,%v; want codex,true", from, ok)
	}
}
