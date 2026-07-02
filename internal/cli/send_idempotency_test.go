package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// send_idempotency_test.go — F1 (deep-analysis-v2, step-6 adopted): expose the
// already-built AllocateSeq/SendSeqMessage idempotency-key re-issue machinery
// via an opt-in `send --idempotency-key <key>` flag. Guardrails (C.6): opt-in
// only, no default body-hash key, no retries, no delivery guarantee beyond
// the write.

func setupIdempotencySendFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		if err := cmdRegister([]string{"--as", "sender", "--vendor", "test"}); err != nil {
			t.Fatal(err)
		}
		if err := cmdRegister([]string{"--as", "recipient", "--vendor", "test"}); err != nil {
			t.Fatal(err)
		}
	})
	return root
}

func recipientInboxEntries(t *testing.T, root string) []os.DirEntry {
	t.Helper()
	inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "recipient")
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestSendIdempotencyKeySameKeyReissuesSameSeq is the load-bearing F1 test: a
// resend carrying the SAME --idempotency-key must re-issue the same seq, not
// consume a new one -- exactly one file must land in the recipient's inbox.
func TestSendIdempotencyKeySameKeyReissuesSameSeq(t *testing.T) {
	root := setupIdempotencySendFixture(t)
	withCwd(t, root, func() {
		args := []string{"--from", "sender", "--to", "recipient", "--body", "hello", "--idempotency-key", "k1"}
		if err := cmdSend(args); err != nil {
			t.Fatalf("first send: %v", err)
		}
		if err := cmdSend(args); err != nil {
			t.Fatalf("resend with same key: %v", err)
		}
	})
	entries := recipientInboxEntries(t, root)
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("same --idempotency-key must re-issue the same seq (1 file); got %d: %v", len(entries), names)
	}
}

// TestSendIdempotencyKeyDifferentKeyAllocatesNewSeq is the non-regression
// half: two sends with DIFFERENT keys are two distinct logical messages and
// must both land.
func TestSendIdempotencyKeyDifferentKeyAllocatesNewSeq(t *testing.T) {
	root := setupIdempotencySendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "sender", "--to", "recipient", "--body", "hello", "--idempotency-key", "k1"}); err != nil {
			t.Fatalf("first send: %v", err)
		}
		if err := cmdSend([]string{"--from", "sender", "--to", "recipient", "--body", "hello", "--idempotency-key", "k2"}); err != nil {
			t.Fatalf("second send: %v", err)
		}
	})
	entries := recipientInboxEntries(t, root)
	if len(entries) != 2 {
		t.Fatalf("different --idempotency-key values must allocate distinct seqs (2 files); got %d", len(entries))
	}
}

// TestSendWithoutIdempotencyKeyIsUnchangedAtMostOnce pins the unset case:
// omitting --idempotency-key must keep today's behavior -- two sends allocate
// two distinct seqs (no re-issue path engaged at all).
func TestSendWithoutIdempotencyKeyIsUnchangedAtMostOnce(t *testing.T) {
	root := setupIdempotencySendFixture(t)
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "sender", "--to", "recipient", "--body", "hello"}); err != nil {
			t.Fatalf("first send: %v", err)
		}
		if err := cmdSend([]string{"--from", "sender", "--to", "recipient", "--body", "hello"}); err != nil {
			t.Fatalf("second send: %v", err)
		}
	})
	entries := recipientInboxEntries(t, root)
	if len(entries) != 2 {
		t.Fatalf("omitting --idempotency-key must keep at-most-once (2 files for 2 sends); got %d", len(entries))
	}
}
