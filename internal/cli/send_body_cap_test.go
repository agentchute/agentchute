package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

// C2: the review's reproduction — a 5,000,000-byte --body-file used to land
// and wedge the recipient's inbox. Now send refuses it before delivery, and a
// body exactly at the cap still lands.
func TestSendBodyFileOverCapIsRefused(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		path := filepath.Join(t.TempDir(), "big.md")
		if err := os.WriteFile(path, []byte(strings.Repeat("z", 5_000_000)), 0o600); err != nil {
			t.Fatal(err)
		}
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", path})
		if err == nil {
			t.Fatal("expected a 5,000,000-byte body to be refused")
		}
		for _, want := range []string{"5000000 bytes", "4190208", "4194304", "quarantined"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		assertNothingDelivered(t, cfg)

		exact := filepath.Join(t.TempDir(), "exact.md")
		if err := os.WriteFile(exact, []byte(strings.Repeat("z", loop.MaxSendBodyBytes)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", exact}); err != nil {
			t.Fatalf("a body exactly at the cap must land: %v", err)
		}
		if n := bodyFileInboxCount(t, cfg, "codex"); n != 1 {
			t.Fatalf("delivered %d, want 1", n)
		}
	})
}

func TestSendStdinOverCapIsRefused(t *testing.T) {
	root, cfg := setupSendFixture(t)
	stdinPath := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(stdinPath, []byte(strings.Repeat("z", loop.MaxSendBodyBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	originalStdin := sendStdin
	sendStdin = f
	defer func() { sendStdin = originalStdin }()
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex"})
		if err == nil || !strings.Contains(err.Error(), "the cap is") {
			t.Fatalf("expected the stdin body to be refused by the cap, got %v", err)
		}
		assertNothingDelivered(t, cfg)
	})
}

// One constant: the hub refuses a body by the same rule the local reader uses.
func TestWireBodyCapIsTheInboxMessageCap(t *testing.T) {
	if hubwire.MaxBody != loop.MaxInboxMessageBytes {
		t.Fatalf("hubwire.MaxBody = %d, loop.MaxInboxMessageBytes = %d", hubwire.MaxBody, loop.MaxInboxMessageBytes)
	}
	if loop.MaxSendBodyBytes >= loop.MaxInboxMessageBytes {
		t.Fatal("send cap must leave envelope headroom under the reader's limit")
	}
}
