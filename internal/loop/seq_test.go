package loop

import (
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func newSeqTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
}

// mkInbox creates the recipient's inbox dir (writeTsMessage requires it to
// exist).
func mkInbox(t *testing.T, cfg *Config, to string) {
	t.Helper()
	if err := ensurePrivateDir(cfg.AgentInboxDir(to)); err != nil {
		t.Fatalf("mkInbox %s: %v", to, err)
	}
}

// mkFreshRecipient creates to's inbox dir AND a fresh registration row. B3's
// DeliverUnderRecipientLock requires the registration to consider `to`
// reachable; tests that exercise the full mint-then-deliver path need this.
// Tests that call writeTsMessage or MintSendStamp directly (bypassing
// delivery's enforcement by design) keep using the plain mkInbox.
func mkFreshRecipient(t *testing.T, cfg *Config, to string) {
	t.Helper()
	mkInbox(t, cfg, to)
	reg := &Registration{
		AgentID:     to,
		Vendor:      "agentchute",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC(),
	}
	if err := WriteRegistration(cfg.AgentRegistrationPath(to), reg); err != nil {
		t.Fatalf("mkFreshRecipient %s: %v", to, err)
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
		"from-alice_seq-42.md",                               // not zero-padded to 20
		"2026-05-09T16-32-00-123456Z_from-alice_msg-ab12.md", // legacy nonce format
		"from-alice_seq-00000000000000000042.txt",            // wrong suffix
		"from-_seq-00000000000000000001.md",                  // empty sender
		"from-BAD_seq-00000000000000000001.md",               // invalid agent_id (uppercase)
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
