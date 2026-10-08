package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

// C2: the review's reproduction — a 5,000,000-byte --body-file used to land
// and wedge the recipient's inbox. Now send refuses it by its stat size
// before reading it, and a body exactly at the cap still lands.
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

// The check that holds is on the composed file: a body at the cap plus a
// long --reply-to would land over MaxInboxMessageBytes and be quarantined
// unread, so send refuses it even though the body alone passed.
func TestSendRefusesEnvelopeOverInboxLimit(t *testing.T) {
	root, cfg := setupSendFixture(t)
	withCwd(t, root, func() {
		exact := filepath.Join(t.TempDir(), "exact.md")
		if err := os.WriteFile(exact, []byte(strings.Repeat("z", loop.MaxSendBodyBytes)), 0o600); err != nil {
			t.Fatal(err)
		}
		longRef := "to-claude-code_from-codex_" + strings.Repeat("r", loop.SendFrontmatterHeadroom)
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex", "--body-file", exact, "--reply-to", longRef})
		if err == nil {
			t.Fatal("expected the composed message to be refused over the inbox limit")
		}
		if !strings.Contains(err.Error(), "with its envelope") || !strings.Contains(err.Error(), "4194304") {
			t.Fatalf("error = %v", err)
		}
		assertNothingDelivered(t, cfg)
	})
}

// An oversized stdin is refused at the cap, not buffered whole: the writer
// side of the pipe gets to push only about the cap plus the pipe's own buffer
// before send stops reading.
func TestSendStdinOverCapIsRefusedWithoutBufferingItAll(t *testing.T) {
	root, cfg := setupSendFixture(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	const total = 64 << 20 // 64 MiB offered
	var written int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer w.Close()
		chunk := []byte(strings.Repeat("z", 1<<16))
		for written < total {
			n, err := w.Write(chunk)
			written += n
			if err != nil {
				return
			}
		}
	}()
	originalStdin := sendStdin
	sendStdin = r
	defer func() { sendStdin = originalStdin }()
	withCwd(t, root, func() {
		err := cmdSend([]string{"--from", "claude-code", "--to", "codex"})
		if err == nil || !strings.Contains(err.Error(), "the cap is") {
			t.Fatalf("expected the stdin body to be refused by the cap, got %v", err)
		}
		assertNothingDelivered(t, cfg)
	})
	_ = r.Close() // the writer now fails and stops
	wg.Wait()
	// Consumed = what send read (cap + 1 byte) plus whatever sat in the pipe
	// and the last chunk in flight; far below the 64 MiB on offer.
	if limit := loop.MaxSendBodyBytes + 1 + 4*(1<<20); written > limit {
		t.Fatalf("send consumed %d bytes of stdin, want at most ~%d (cap + pipe slack); it buffered the input whole", written, limit)
	}
	if written < loop.MaxSendBodyBytes+1 {
		t.Fatalf("writer pushed only %d bytes; the pipe did not reach the cap", written)
	}
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
