package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// stdoutCapture redirects os.Stdout to a temp file, returns a reader-like
// helper, and restores stdout on close. Boot emits its primary signal to
// stdout (status text, JSON, codex JSON shape) so tests need to inspect it.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	cmdErr := fn()
	_ = w.Close()
	<-done
	os.Stdout = orig
	return buf.String(), cmdErr
}

// setupBootFixture chdirs into a fresh temp root with AGENTCHUTE.md +
// loop dir scaffolding. Caller passes a body fn; cwd is restored on return.
func setupBootFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".rehumanlabs", "loop"))
	return root
}

func bootArgs(extra ...string) []string {
	base := []string{"--as", "claude-code", "--vendor", "anthropic"}
	return append(base, extra...)
}

// Test 8 (spec rev3 Part 4) line 1: fresh registration → exit 0, refreshed: true.
//
// Spec rev3 §A.1 semantics: `refreshed: true` means "boot wrote the
// registration file on this call", which is true for every successful
// boot (fresh enrollment OR an update to an existing registration).
// A separate internal field (ExistingFound, not serialized) preserves
// the fresh-vs-existing distinction for UX output verbs without
// diverging from the spec's wire shape.
func TestBootFreshRegistrationExitsZero(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		out, err := captureStdout(t, func() error {
			return cmdBoot(bootArgs("--json"))
		})
		if err != nil {
			t.Fatalf("cmdBoot: %v", err)
		}
		var got bootStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal output: %v\n%s", jerr, out)
		}
		if !got.Refreshed {
			t.Errorf("Refreshed = false on fresh enrollment; spec rev3 §A.1 says true on every successful write")
		}
		if got.UnreadCount != 0 || got.RepliesPending != 0 || got.Blocked {
			t.Errorf("status = %+v; want clean state", got)
		}
	})
}

func TestBootRefreshExistingRegistrationExitsZero(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		// First boot creates the registration.
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs("--json")) }); err != nil {
			t.Fatal(err)
		}
		// Second boot must refresh and exit 0.
		out, err := captureStdout(t, func() error { return cmdBoot(bootArgs("--json")) })
		if err != nil {
			t.Fatalf("second cmdBoot: %v", err)
		}
		var got bootStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if !got.Refreshed {
			t.Errorf("Refreshed = false on second boot; want true")
		}
		if got.Blocked {
			t.Errorf("Blocked = true on clean refresh")
		}
	})
}

// Test 8 line 3: registration with 1 unread direct mail → boot exits 2.
func TestBootWithUnreadMailReturnsBlocked(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		// First, register so the inbox dir exists.
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		// Drop a valid §6.1.2-shaped message into the inbox.
		inboxDir := filepath.Join(root, ".rehumanlabs", "loop", "inbox", "claude-code")
		now := time.Now().UTC()
		msgContent := []byte("---\nmessage_id: 2026-05-19T17:53:59.561894Z\nfrom: codex\nto: claude-code\ntask: review\n---\n\nbody\n")
		if _, err := loop.WriteInboxMessage(inboxDir, now, "codex", msgContent); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdBoot(bootArgs("--json")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked", err)
		}
		var got bootStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.UnreadCount != 1 {
			t.Errorf("UnreadCount = %d, want 1", got.UnreadCount)
		}
		if !got.Blocked {
			t.Error("Blocked = false on unread mail")
		}
	})
}

// Test 8 line 4: 1 pending-reply ledger entry → boot exits 2.
func TestBootWithPendingReplyLedgerReturnsBlocked(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		// Register first.
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}

		// Seed a pending-reply ledger entry directly via the loop package.
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		entry := loop.PendingReplyEntry{
			MessageID:        "2026-05-19T17:53:59.561894Z",
			From:             "codex",
			To:               "claude-code",
			Task:             "review",
			OriginalFilename: "2026-05-19T17-53-59-561894Z_from-codex_msg-aaaa.md",
			ArchivePath:      "archive/some-path.md",
		}
		if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdBoot(bootArgs("--json")) })
		if !errors.Is(err, errBlocked) {
			t.Fatalf("err = %v, want errBlocked", err)
		}
		var got bootStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.RepliesPending != 1 {
			t.Errorf("RepliesPending = %d, want 1", got.RepliesPending)
		}
		if !got.Blocked {
			t.Error("Blocked = false on pending reply")
		}
	})
}

// Test 8 line 5: command failure (e.g., loop dir missing) → exit 1 (NOT 2).
// The error must NOT be errBlocked.
func TestBootMissingLoopDirReturnsCommandFailure(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	// Note: no .rehumanlabs/loop directory; Discover should fail.
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) })
		if err == nil {
			t.Fatal("expected error; got nil")
		}
		if errors.Is(err, errBlocked) {
			t.Errorf("err = %v wraps errBlocked; want a distinct command-failure error", err)
		}
	})
}

// --context-only always exits 0 (returns nil error), regardless of unread.
func TestBootContextOnlyNeverBlocks(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		// Register first.
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		// Drop an unread message.
		inboxDir := filepath.Join(root, ".rehumanlabs", "loop", "inbox", "claude-code")
		_, err := loop.WriteInboxMessage(inboxDir, time.Now().UTC(), "codex", []byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error { return cmdBoot(bootArgs("--context-only")) })
		if err != nil {
			t.Errorf("--context-only returned error = %v; want nil (hook-safe mode never blocks)", err)
		}
		if !strings.Contains(out, "unread") {
			t.Errorf("--context-only output should mention unread: %q", out)
		}
	})
}

// --codex-hook SessionStart emits the codex hookSpecificOutput JSON shape
// and always exits 0 (never returns errBlocked).
func TestBootCodexHookSessionStartShape(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		out, err := captureStdout(t, func() error {
			return cmdBoot(bootArgs("--codex-hook", "SessionStart"))
		})
		if err != nil {
			t.Errorf("--codex-hook returned error = %v; want nil", err)
		}
		var wrap struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal codex hook output: %v\n%s", jerr, out)
		}
		if wrap.HookSpecificOutput.HookEventName != "SessionStart" {
			t.Errorf("HookEventName = %q, want SessionStart", wrap.HookSpecificOutput.HookEventName)
		}
		if !strings.Contains(wrap.HookSpecificOutput.AdditionalContext, "claude-code") {
			t.Errorf("AdditionalContext missing agent id: %q", wrap.HookSpecificOutput.AdditionalContext)
		}
	})
}

func TestBootEmitPromptLineSingleLine(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		out, err := captureStdout(t, func() error {
			return cmdBoot(bootArgs("--emit-prompt-line"))
		})
		if err != nil {
			t.Fatal(err)
		}
		// Single line: exactly one trailing newline, no embedded newlines.
		trimmed := strings.TrimRight(out, "\n")
		if strings.Contains(trimmed, "\n") {
			t.Errorf("--emit-prompt-line output spans multiple lines: %q", out)
		}
		if !strings.Contains(trimmed, "agentchute") {
			t.Errorf("--emit-prompt-line missing brand prefix: %q", trimmed)
		}
	})
}

// --quiet on clean boot should produce no output.
func TestBootQuietSuppressesCleanOutput(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		out, err := captureStdout(t, func() error {
			return cmdBoot(bootArgs("--quiet"))
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("--quiet on clean boot should suppress output; got %q", out)
		}
	})
}
