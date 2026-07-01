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
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	return root
}

func bootArgs(extra ...string) []string {
	base := []string{"--as", "claude-code", "--vendor", "anthropic"}
	return append(base, extra...)
}

// Fresh registration → exit 0, refreshed: true.
//
// Registration semantics (AGENTCHUTE.md §5): `refreshed: true` means "boot wrote
// the registration file on this call", which is true for every successful
// boot (fresh enrollment OR an update to an existing registration).
// A separate internal field (ExistingFound, not serialized) preserves
// the fresh-vs-existing distinction for UX output verbs without
// diverging from the wire shape.
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
			t.Errorf("Refreshed = false on fresh enrollment; refreshed is true on every successful registration write")
		}
		if got.UnreadCount != 0 || got.Blocked {
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
		// Drop a valid §6.1-shaped message into the inbox.
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
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

// v0.9.0 `.owed` redesign: reply obligations are asker-owned only; the
// recipient-side pending-reply ledger was removed, so boot NEVER blocks on a
// reply_required message. A reply_required unread message blocks only as unread
// mail (covered by TestBootWithUnreadMailReturnsBlocked); once consumed, boot
// clears even though the asker is still owed a reply. Boot's Blocked term is
// now len(unread) > 0 only.
func TestBootDoesNotBlockOnOwedReplies(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		if _, err := captureStdout(t, func() error { return cmdBoot(bootArgs()) }); err != nil {
			t.Fatal(err)
		}
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		// Seed an asker-owned obligation (we are owed a reply BY codex). This
		// MUST NOT block boot — it is a non-blocking asker-side signal.
		now := time.Now().UTC()
		key := loop.MsgID{To: "codex", From: "claude-code", Seq: 1}
		if err := loop.RecordOwed(cfg, "claude-code", key, now.Add(30*time.Minute), now); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error { return cmdBoot(bootArgs("--json")) })
		if err != nil {
			t.Fatalf("boot with an owed obligation should NOT block; err = %v", err)
		}
		var got bootStatus
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.Blocked {
			t.Error("Blocked = true with only an owed (asker-side) obligation; want false")
		}
	})
}

// Test 8 line 5: command failure (e.g., loop dir missing) → exit 1 (NOT 2).
// The error must NOT be errBlocked.
func TestBootMissingLoopDirReturnsCommandFailure(t *testing.T) {
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	// Note: no .agentchute/loop directory; Discover should fail.
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
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
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
		if !strings.Contains(out, `--reply-to <ref> --body "..."`) {
			t.Errorf("--context-only output missing runnable reply hint:\n%s", out)
		}
		// v0.9.0: the removed `defer` command must NOT be suggested.
		if strings.Contains(out, "agentchute defer") {
			t.Errorf("--context-only output must not mention the removed defer command:\n%s", out)
		}
	})
}

func TestBootCodexHookSessionStartGuidanceIsRunnable(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		t.Setenv("TMUX_PANE", "%1")
		inboxDir := filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code")
		mustMkdir(t, inboxDir)
		msgContent := []byte("---\nmessage_id: 2026-05-19T17:53:59.561894Z\nfrom: codex\nto: claude-code\ntask: review\n---\n\nbody\n")
		if _, err := loop.WriteInboxMessage(inboxDir, time.Now().UTC(), "codex", msgContent); err != nil {
			t.Fatal(err)
		}
		out, err := captureStdout(t, func() error {
			return cmdBoot(bootArgs("--codex-hook", "SessionStart"))
		})
		if err != nil {
			t.Errorf("--codex-hook returned error = %v; want nil", err)
		}
		var wrap struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal codex hook output: %v\n%s", jerr, out)
		}
		ctx := wrap.HookSpecificOutput.AdditionalContext
		if !strings.Contains(ctx, `--reply-to <ref> --body "..."`) {
			t.Errorf("SessionStart context missing runnable reply hint:\n%s", ctx)
		}
		// v0.9.0: the removed `defer` command must NOT be suggested.
		if strings.Contains(ctx, "agentchute defer") {
			t.Errorf("SessionStart context must not mention the removed defer command:\n%s", ctx)
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
