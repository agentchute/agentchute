package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// TestSendWritesTimestampFormat guards that cmdSend lands the message under
// the new C3 filename grammar `<ts>_from-<from>_r<32hex>.md` (v2.5 plan B7,
// replaces TestSendWritesSeqFormat).
func TestSendWritesTimestampFormat(t *testing.T) {
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
	id, ok := loop.ParseTsFilename(name)
	if !ok {
		t.Fatalf("inbox file %q is not timestamp-format", name)
	}
	if id.From != "codex" {
		t.Fatalf("timestamp filename from = %q, want codex", id.From)
	}
	if _, ok := loop.ParseStamp(id.Stamp); !ok {
		t.Fatalf("timestamp filename stamp %q does not parse", id.Stamp)
	}
	// And the lister consumes it (not quarantined).
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("timestamp message must not be skipped/quarantined; skipped=%v", skipped)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %+v", msgs)
	}
}

// TestSendUnknownRecipientDoesNotAdvanceFloor confirms the preflight: sending
// to a missing inbox returns the unregistered error AND does NOT mint (so it
// cannot advance) the sender's monotonic send floor (v2.5 plan B7, replaces
// TestSendToUnregisteredRecipientNoSeqBurn — the floor is now per-SENDER only,
// not per-(from,to) pair, so "no burn" means the floor file is never even
// created by a preflight failure).
func TestSendUnknownRecipientDoesNotAdvanceFloor(t *testing.T) {
	root, cfg := setupSendFixture(t)
	floorPath := filepath.Join(cfg.AgentStateDir("codex"), "send.floor")
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_AGENT_ID", "codex")
		err := cmdSend([]string{"--to", "ghost", "--body", "x"})
		if err == nil {
			t.Fatal("expected error sending to unregistered recipient")
		}
		if !strings.Contains(err.Error(), `unknown agent "ghost"`) {
			t.Fatalf("error = %v, want unknown-agent error", err)
		}
		if _, statErr := os.Stat(floorPath); !os.IsNotExist(statErr) {
			t.Fatalf("failed preflight must not create/advance the send floor: stat err = %v", statErr)
		}
		// A legitimate send to a real recipient must still succeed and mint
		// fresh (mint happens AFTER preflight-pass only).
		if err := cmdSend([]string{"--to", "claude-code", "--body", "ok"}); err != nil {
			t.Fatal(err)
		}
	})
	if _, statErr := os.Stat(floorPath); statErr != nil {
		t.Fatalf("legitimate send must create the send floor: %v", statErr)
	}
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(msgs))
	}
	if _, ok := loop.ParseTsFilename(msgs[0].Filename); !ok {
		t.Fatalf("delivered filename %q is not timestamp-format", msgs[0].Filename)
	}
}
