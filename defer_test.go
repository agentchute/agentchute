package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// setupDeferFixture sets up a control repo with two registered agents
// (claude-code, codex) and seeds a pending-reply ledger entry on
// claude-code from codex. Returns the seeded entry's message_id and the
// loop config so individual tests can verify state.
func setupDeferFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	root := setupBootFixture(t)

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}

	// Register both agents (use cmdRegister directly so existing helpers compose).
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMUX_PANE", "%2")
		if err := cmdRegister([]string{"--as", "codex", "--vendor", "openai"}); err != nil {
			t.Fatal(err)
		}
	})

	// Seed the ledger entry.
	msgID := "2026-05-19T17:53:59.561894Z"
	entry := loop.PendingReplyEntry{
		MessageID:        msgID,
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DEFER_ROOT", root)
	return msgID, cfg
}

// Test 5 (spec rev3 Part 4): defer transitions the ledger entry to
// deferred, populates deferred_at + reason (+ optional deferred_until),
// and gate --before finish exits 0 afterwards. Also: an automatic
// deferral-ack message lands in the sender's inbox.
func TestDeferTransitionsLedgerAndAcksSender(t *testing.T) {
	msgID, cfg := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{
				"--as", "claude-code",
				"--message", msgID,
				"--reason", "needs research",
				"--until", "24h",
			})
		})
		if err != nil {
			t.Fatalf("cmdDefer: %v", err)
		}
	})

	// Ledger entry transitioned.
	ledger, err := loop.LoadPendingLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ledger.FindByMessageID(msgID)
	if !ok {
		t.Fatal("ledger entry vanished")
	}
	if got.Status != loop.PendingReplyStatusDeferred {
		t.Errorf("Status = %q, want deferred", got.Status)
	}
	if got.DeferredReason == nil || *got.DeferredReason != "needs research" {
		t.Errorf("DeferredReason = %v, want \"needs research\"", got.DeferredReason)
	}
	if got.DeferredAt == nil {
		t.Error("DeferredAt is nil; want populated")
	}
	if got.DeferredUntil == nil || *got.DeferredUntil == "" {
		t.Errorf("DeferredUntil = %v, want populated", got.DeferredUntil)
	}

	// Deferral-ack landed in codex's inbox.
	codexInbox := cfg.AgentInboxDir("codex")
	entries, err := os.ReadDir(codexInbox)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(codexInbox, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		if strings.Contains(s, "task: deferred-reply") && strings.Contains(s, "needs research") {
			found = true
			if !strings.Contains(s, `in_reply_to: "`+msgID) {
				t.Errorf("ack missing in_reply_to=%s: %s", msgID, s)
			}
			break
		}
	}
	if !found {
		t.Errorf("deferral-ack not found in codex inbox; entries=%v", entries)
	}

	// Gate finish must now pass.
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdGate(gateArgs("finish")) })
		if err != nil {
			t.Errorf("gate finish after defer err = %v, want nil", err)
		}
	})
}

func TestDeferRejectsMissingMessageID(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", "nope", "--reason", "x"})
		})
		if err == nil {
			t.Fatal("expected error for missing ledger entry")
		}
		if errors.Is(err, errBlocked) {
			t.Errorf("missing-entry should be command failure, not errBlocked: %v", err)
		}
	})
}

func TestDeferRejectsAlreadyDeferred(t *testing.T) {
	msgID, _ := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")
	withCwd(t, root, func() {
		if _, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", msgID, "--reason", "first"})
		}); err != nil {
			t.Fatal(err)
		}
		// Second defer must refuse.
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", msgID, "--reason", "second"})
		})
		if err == nil {
			t.Fatal("expected error on second defer")
		}
	})
}

func TestDeferRequiresReason(t *testing.T) {
	msgID, _ := setupDeferFixture(t)
	root := os.Getenv("TEST_DEFER_ROOT")
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", msgID})
		})
		if err == nil {
			t.Fatal("expected error when --reason is omitted")
		}
	})
}

func TestNormalizeDeferUntil(t *testing.T) {
	pastRFC := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name    string
		input   string
		wantErr bool
		check   func(t *testing.T, got string)
	}{
		{"parses durations", "24h", false, func(t *testing.T, got string) {
			parsed, err := time.Parse(time.RFC3339, got)
			if err != nil {
				t.Fatalf("not RFC3339: %q (%v)", got, err)
			}
			if d := time.Until(parsed); d < 23*time.Hour || d > 25*time.Hour {
				t.Errorf("24h offset out of range: %s", got)
			}
		}},
		{"accepts RFC3339", "2026-05-26T00:00:00Z", false, func(t *testing.T, got string) {
			if got != "2026-05-26T00:00:00Z" {
				t.Errorf("got %q", got)
			}
		}},
		{"empty passes through", "", false, func(t *testing.T, got string) {
			if got != "" {
				t.Errorf("got %q, want empty", got)
			}
		}},
		{"rejects garbage", "tomorrow morning", true, nil},
		{"rejects past RFC3339", pastRFC, true, nil},
		{"rejects zero duration", "0s", true, nil},
		{"rejects negative duration", "-1h", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeDeferUntil(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.check != nil {
				c.check(t, got)
			}
		})
	}
}

// Codex review (eb58443): if the ledger contains an entry whose To field
// doesn't match the deferring agent's id, defer must refuse rather than
// silently route the ack from a mismatched namespace.
func TestDeferRejectsLedgerEntryWithMismatchedTo(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic"}); err != nil {
			t.Fatal(err)
		}
	})

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	// Hand-write a ledger entry whose `to` is some other agent.
	if err := ensurePrivateDirHelper(cfg.PendingRepliesPath("claude-code")); err != nil {
		t.Fatal(err)
	}
	corrupt := `{"pending":[{"message_id":"mismatched","from":"codex","to":"someone-else","task":"x","original_filename":"f.md","archive_path":"a.md","recorded_at":"2026-01-01T00:00:00Z","status":"pending","reply_sent_at":null,"reply_message_id":null,"deferred_at":null,"deferred_until":null,"deferred_reason":null}]}`
	if err := os.WriteFile(cfg.PendingRepliesPath("claude-code"), []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdDefer([]string{"--as", "claude-code", "--message", "mismatched", "--reason", "test"})
		})
		if err == nil {
			t.Fatal("expected error on ledger entry whose To doesn't match the deferring agent")
		}
	})
}

// Helper because defer_test is in package main and ensurePrivateDir lives
// in the internal/loop package. We need a 0700 dir for the ledger file.
func ensurePrivateDirHelper(filePath string) error {
	return os.MkdirAll(filepath.Dir(filePath), 0o700)
}

