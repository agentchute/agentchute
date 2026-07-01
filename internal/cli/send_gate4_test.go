package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// TestSendWritesSeqFormat guards that cmdSend lands the message under the
// canonical (to,from,seq) filename `from-<from>_seq-<020d>.md`.
func TestSendWritesSeqFormat(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_AGENT_ID", "codex")
		if err := cmdSend([]string{"--to", "claude-code", "--body", "hi"}); err != nil {
			t.Fatal(err)
		}
	})

	inbox := cfg.AgentInboxDir("claude-code")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files = append(files, e.Name())
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 inbox file, got %v", files)
	}
	name := files[0]
	from, seq, ok := loop.ParseSeqFilename(name)
	if !ok {
		t.Fatalf("inbox file %q is not seq-format", name)
	}
	if from != "codex" {
		t.Fatalf("seq filename from = %q, want codex", from)
	}
	if seq != 1 {
		t.Fatalf("first message seq = %d, want 1", seq)
	}
	// And the lister consumes it (not quarantined).
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("seq message must not be skipped/quarantined; skipped=%v", skipped)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %+v", msgs)
	}
}

// TestSendToUnregisteredRecipientNoSeqBurn confirms the preflight: sending to a
// missing inbox returns the unregistered error AND does NOT burn a durable seq
// on the sender (the next legitimate send still starts at seq 1).
func TestSendToUnregisteredRecipientNoSeqBurn(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_AGENT_ID", "codex")
		err := cmdSend([]string{"--to", "ghost", "--body", "x"})
		if err == nil {
			t.Fatal("expected error sending to unregistered recipient")
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Fatalf("error = %v, want 'not registered'", err)
		}
		// A legitimate send to a real recipient must still start at seq 1,
		// proving the failed send did not advance codex's (codex,ghost) — and,
		// more importantly, that no seq leaked onto the live path.
		if err := cmdSend([]string{"--to", "claude-code", "--body", "ok"}); err != nil {
			t.Fatal(err)
		}
	})
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(msgs))
	}
	_, seq, ok := loop.ParseSeqFilename(msgs[0].Filename)
	if !ok || seq != 1 {
		t.Fatalf("delivered seq = %d (ok=%v), want 1", seq, ok)
	}
}
