package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func newSeqTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
}

// mkInbox creates the recipient's inbox dir (writeSeqMessage requires it to
// exist, mirroring WriteInboxMessage).
func mkInbox(t *testing.T, cfg *Config, to string) {
	t.Helper()
	if err := ensurePrivateDir(cfg.AgentInboxDir(to)); err != nil {
		t.Fatalf("mkInbox %s: %v", to, err)
	}
}

// countInboxBody reads the recipient's inbox and counts seq-format files whose
// content equals body. (ListInboxMessages parses the legacy §6.1 format only, so
// the seq files are invisible to it — by design for Gate 2.)
func countInboxBody(t *testing.T, cfg *Config, to, body string) int {
	t.Helper()
	dir := cfg.AgentInboxDir(to)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read inbox %s: %v", to, err)
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if _, _, ok := ParseSeqFilename(name); !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(data) == body {
			n++
		}
	}
	return n
}

// rollbackSeqCounter simulates a crash before the seq counter was made durable:
// it decrements last_issued by one and drops the binding for `key`, exactly like
// conformance SeqSender.loseCounter. The state file is read/written directly
// (this is a same-package test).
func rollbackSeqCounter(t *testing.T, cfg *Config, from, to, key string) {
	t.Helper()
	var st *seqState
	err := withAgentLock(cfg, from, func() error {
		var e error
		st, e = loadSeqState(cfg, from, to)
		if e != nil {
			return e
		}
		if st.LastIssued > 0 {
			st.LastIssued--
		}
		kept := st.Recent[:0]
		for _, r := range st.Recent {
			if r.Key != key {
				kept = append(kept, r)
			}
		}
		st.Recent = kept
		return saveSeqState(cfg, from, to, st)
	})
	if err != nil {
		t.Fatalf("rollbackSeqCounter: %v", err)
	}
}

func TestMsgIDFilenameRoundTrip(t *testing.T) {
	id := MsgID{To: "bob", From: "alice", Seq: 42}
	name := id.Filename()
	want := "from-alice_seq-00000000000000000042.md"
	if name != want {
		t.Fatalf("Filename = %q, want %q", name, want)
	}
	from, seq, ok := ParseSeqFilename(name)
	if !ok {
		t.Fatal("ParseSeqFilename failed on a canonical name")
	}
	if from != "alice" || seq != 42 {
		t.Fatalf("Parse = (%q,%d), want (alice,42)", from, seq)
	}
	// To is NOT recoverable from the filename (it's the inbox location).
}

func TestParseSeqFilenameRejectsNonCanonical(t *testing.T) {
	bad := []string{
		"from-alice_seq-42.md",                            // not zero-padded to 20
		"2026-05-09T16-32-00-123456Z_from-alice_msg-ab12.md", // legacy nonce format
		"from-alice_seq-00000000000000000042.txt",         // wrong suffix
		"from-_seq-00000000000000000001.md",               // empty sender
		"from-BAD_seq-00000000000000000001.md",            // invalid agent_id (uppercase)
		"random.md",
	}
	for _, name := range bad {
		if _, _, ok := ParseSeqFilename(name); ok {
			t.Errorf("ParseSeqFilename(%q) = ok, want not-ok", name)
		}
	}
}

func TestSeqFilenameLexicographicFIFO(t *testing.T) {
	// 20-digit zero-pad => lexicographic sort == numeric seq order (O1 exact).
	var names []string
	for _, s := range []uint64{10, 2, 1, 100, 3} {
		names = append(names, MsgID{From: "alice", Seq: s}.Filename())
	}
	sort.Strings(names)
	var seqs []uint64
	for _, n := range names {
		_, s, ok := ParseSeqFilename(n)
		if !ok {
			t.Fatalf("parse %q", n)
		}
		seqs = append(seqs, s)
	}
	want := []uint64{1, 2, 3, 10, 100}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("lexicographic order = %v, want %v", seqs, want)
		}
	}
}

func TestAllocateSeqFirstIsOne(t *testing.T) {
	cfg := newSeqTestConfig(t)
	seq, err := AllocateSeq(cfg, "alice", "bob", "k1", "")
	if err != nil {
		t.Fatalf("AllocateSeq: %v", err)
	}
	if seq != 1 {
		t.Fatalf("first seq = %d, want 1", seq)
	}
}

func TestAllocateSeqReissuesOnSameKey(t *testing.T) {
	cfg := newSeqTestConfig(t)
	s1, err := AllocateSeq(cfg, "alice", "bob", "k1", "")
	if err != nil {
		t.Fatal(err)
	}
	// Same key, no crash: must re-issue the same seq WITHOUT advancing.
	s2, err := AllocateSeq(cfg, "alice", "bob", "k1", "")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != 1 || s2 != 1 {
		t.Fatalf("reissue: got (%d,%d), want (1,1)", s1, s2)
	}
	// A genuinely-new key takes the next seq (counter only advanced once).
	s3, err := AllocateSeq(cfg, "alice", "bob", "k2", "")
	if err != nil {
		t.Fatal(err)
	}
	if s3 != 2 {
		t.Fatalf("next new key seq = %d, want 2", s3)
	}
}

// TestSeqSenderCrashResume mirrors conformance/seq_durability_test.go
// TestC1_SenderCrashResume against a REAL filesystem.
func TestSeqSenderCrashResume(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkInbox(t, cfg, "bob")

	// normal send: seq=1 lands.
	id1, err := SendSeqMessage(cfg, "alice", "bob", []byte("m1"), "k1", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", id1.Seq)
	}
	if n := countInboxBody(t, cfg, "bob", "m1"); n != 1 {
		t.Fatalf("m1 copies = %d, want 1", n)
	}

	// CRASH: counter not made durable -> on resume re-issue seq=1.
	rollbackSeqCounter(t, cfg, "alice", "bob", "k1")
	idR, err := SendSeqMessage(cfg, "alice", "bob", []byte("m1"), "k1", "") // identical resend
	if err != nil {
		t.Fatal(err)
	}
	if idR.Seq != 1 {
		t.Fatalf("resume seq = %d, want 1", idR.Seq)
	}
	// EEXIST made the resend a no-op: exactly one copy of m1.
	if n := countInboxBody(t, cfg, "bob", "m1"); n != 1 {
		t.Fatalf("EEXIST must dedup the crash-resend; m1 copies = %d, want 1", n)
	}

	// the next genuinely-new message gets the next seq and lands.
	id2, err := SendSeqMessage(cfg, "alice", "bob", []byte("m2"), "k2", "")
	if err != nil {
		t.Fatal(err)
	}
	if id2.Seq != 2 {
		t.Fatalf("next seq = %d, want 2", id2.Seq)
	}
	if n := countInboxBody(t, cfg, "bob", "m2"); n != 1 {
		t.Fatalf("m2 copies = %d, want 1", n)
	}
}

// TestSeqEEXISTAlreadyLanded: a direct re-link of the SAME (to,from,seq) is a
// safe no-op success (alreadyLanded=true), one copy. This is the substrate
// confirming "this exact identity already delivered" (D2 + C1 delivery-dedup).
func TestSeqEEXISTAlreadyLanded(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkInbox(t, cfg, "bob")
	id := MsgID{To: "bob", From: "alice", Seq: 7}

	landed, err := writeSeqMessage(cfg.AgentInboxDir("bob"), id, []byte("hello"))
	if err != nil || landed {
		t.Fatalf("first write: landed=%v err=%v", landed, err)
	}
	landed, err = writeSeqMessage(cfg.AgentInboxDir("bob"), id, []byte("hello"))
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if !landed {
		t.Fatal("resend of same (to,from,seq) must report alreadyLanded=true")
	}
	if n := countInboxBody(t, cfg, "bob", "hello"); n != 1 {
		t.Fatalf("copies = %d, want 1", n)
	}
}

// TestSeqReuseDifferentContentDropped pins the §7 HAZARD as executable: reusing
// an already-landed seq for DIFFERENT content (allocator BYPASSED) is silently
// dropped by EEXIST — which is exactly why the seq counter must be durable +
// monotonic and ids unique per process.
func TestSeqReuseDifferentContentDropped(t *testing.T) {
	cfg := newSeqTestConfig(t)
	mkInbox(t, cfg, "bob")
	id := MsgID{To: "bob", From: "alice", Seq: 1}

	if _, err := writeSeqMessage(cfg.AgentInboxDir("bob"), id, []byte("ORIGINAL")); err != nil {
		t.Fatal(err)
	}
	// Bypass the allocator and reuse seq=1 for different content.
	landed, err := writeSeqMessage(cfg.AgentInboxDir("bob"), id, []byte("DIFFERENT"))
	if err != nil {
		t.Fatal(err)
	}
	if !landed {
		t.Fatal("reuse must hit EEXIST (alreadyLanded)")
	}
	if n := countInboxBody(t, cfg, "bob", "DIFFERENT"); n != 0 {
		t.Fatalf("seq-reuse-with-different-content must be dropped; DIFFERENT copies = %d, want 0", n)
	}
	if n := countInboxBody(t, cfg, "bob", "ORIGINAL"); n != 1 {
		t.Fatalf("ORIGINAL must survive; copies = %d, want 1", n)
	}
}

// TestAllocateSeqConcurrentDistinct: concurrent same-(from,to) sends get
// DISTINCT, monotonic seqs (the per-agent lock serializes the allocation).
func TestAllocateSeqConcurrentDistinct(t *testing.T) {
	cfg := newSeqTestConfig(t)
	const n = 50
	var (
		mu   sync.Mutex
		got  []uint64
		wg   sync.WaitGroup
		errs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := AllocateSeq(cfg, "alice", "bob", fmt.Sprintf("k%d", i), "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			got = append(got, seq)
		}(i)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("AllocateSeq errors: %v", errs)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != n {
		t.Fatalf("got %d seqs, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		if got[i] != uint64(i+1) {
			t.Fatalf("seq set not distinct/monotonic at %d: %v", i, got)
		}
	}
}

// TestAllocateSeqEmptyKeyDegrades: with no idempotency key, every call takes a
// fresh seq (degraded at-least-once; receiver dedups by Key).
func TestAllocateSeqEmptyKeyDegrades(t *testing.T) {
	cfg := newSeqTestConfig(t)
	s1, _ := AllocateSeq(cfg, "alice", "bob", "", "")
	s2, _ := AllocateSeq(cfg, "alice", "bob", "", "")
	if s1 != 1 || s2 != 2 {
		t.Fatalf("empty-key allocs = (%d,%d), want (1,2)", s1, s2)
	}
}

// TestAllocateSeqPerRecipientScope: seq is per-(from,to); the SAME from to two
// recipients keeps independent counters (else (bob,6) aliases across recipients
// and breaks EEXIST idempotency — protocol-v2 §2).
func TestAllocateSeqPerRecipientScope(t *testing.T) {
	cfg := newSeqTestConfig(t)
	b1, _ := AllocateSeq(cfg, "alice", "bob", "k", "")
	c1, _ := AllocateSeq(cfg, "alice", "carol", "k", "")
	b2, _ := AllocateSeq(cfg, "alice", "bob", "k2", "")
	if b1 != 1 || c1 != 1 || b2 != 2 {
		t.Fatalf("per-recipient scope: bob=(%d,%d) carol=%d, want bob=(1,2) carol=1", b1, b2, c1)
	}
}

// TestAllocateSeqFenceMismatchAborts: when a serve token is supplied, AllocateSeq
// VerifyFence's it BEFORE persisting; a stale token (claim reclaimed) aborts with
// ErrFenced and does NOT advance the counter.
func TestAllocateSeqFenceMismatchAborts(t *testing.T) {
	cfg := newSeqTestConfig(t)

	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("AcquireServeLease: %v", err)
	}

	// A correct token allocates fine.
	if _, err := AllocateSeq(cfg, "alice", "bob", "k1", lease.Token); err != nil {
		t.Fatalf("AllocateSeq with valid token: %v", err)
	}

	// Simulate reclaim: overwrite the claim with a different token.
	overwriteClaimToken(t, cfg, "alice", "STALE-OTHER-TOKEN")

	if _, err := AllocateSeq(cfg, "alice", "bob", "k2", lease.Token); err != ErrFenced {
		t.Fatalf("AllocateSeq with stale token err = %v, want ErrFenced", err)
	}

	// The fenced attempt must NOT have advanced the counter: the next valid
	// allocation (with the new token) is still seq=2.
	next, err := AllocateSeq(cfg, "alice", "bob", "k2", "STALE-OTHER-TOKEN")
	if err != nil {
		t.Fatalf("AllocateSeq with new token: %v", err)
	}
	if next != 2 {
		t.Fatalf("seq after fenced abort = %d, want 2 (no advance)", next)
	}
}
