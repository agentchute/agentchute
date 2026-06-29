package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// pendingArgs builds the standard arg slice for cmdPending in a test fixture.
func pendingArgs(extra ...string) []string {
	base := []string{"--as", "claude-code"}
	return append(base, extra...)
}

// readFrontmatter is the shared hook-safe peek helper used by pending, boot,
// self-poll, and watch. It must refuse to read a peer-planted file that
// exceeds the inbox cap, matching the capped consume path in check.go — a
// validly named but oversized inbox file must not be slurped unbounded just
// to peek frontmatter (codex review finding, 2026-06-16).
func TestReadFrontmatterRejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversize.md")
	big := make([]byte, loop.MaxInboxMessageBytes+1)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrontmatter(path); err == nil {
		t.Fatal("expected oversize rejection from readFrontmatter, got nil error")
	}
}

// AGENTCHUTE.md §6.4: pending --json now reports pending-reply ledger entries
// alongside the unread inbox count, so per-turn hook context surfaces the
// full obligation picture.
func TestPendingJSONIncludesPendingReplies(t *testing.T) {
	root, cfg := setupSendFixture(t)
	entry := loop.PendingReplyEntry{
		MessageID:        "msg-1",
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "msg-1_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error { return cmdPending(pendingArgs("--json")) })
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Count          int `json:"count"`
			RepliesPending int `json:"replies_pending"`
			PendingReplies []struct {
				MessageID string `json:"message_id"`
				From      string `json:"from"`
				Task      string `json:"task"`
			} `json:"pending_replies"`
		}
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.Count != 0 {
			t.Errorf("Count = %d, want 0 (no inbox messages)", got.Count)
		}
		if got.RepliesPending != 1 {
			t.Errorf("RepliesPending = %d, want 1", got.RepliesPending)
		}
		if len(got.PendingReplies) != 1 || got.PendingReplies[0].MessageID != "msg-1" {
			t.Errorf("PendingReplies = %+v, want [msg-1]", got.PendingReplies)
		}
	})
}

// --fail-if-any now triggers on pending replies too (was inbox-only).
func TestPendingFailIfAnyTriggersOnLedger(t *testing.T) {
	root, cfg := setupSendFixture(t)
	entry := loop.PendingReplyEntry{
		MessageID:        "msg-1",
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "msg-1_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdPending(pendingArgs("--fail-if-any")) })
		if !errors.Is(err, errFailIfAny) {
			t.Fatalf("err = %v, want errFailIfAny", err)
		}
	})
}

// Codex UserPromptSubmit hook output now mentions pending-reply
// obligations explicitly, not just unread mail.
func TestPendingCodexHookSurfacesLedger(t *testing.T) {
	root, cfg := setupSendFixture(t)
	entry := loop.PendingReplyEntry{
		MessageID:        "msg-1",
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "msg-1_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPending(pendingArgs("--codex-hook", "UserPromptSubmit"))
		})
		if err != nil {
			t.Fatal(err)
		}
		var wrap struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		ctx := wrap.HookSpecificOutput.AdditionalContext
		if !strings.Contains(ctx, "pending reply obligation") {
			t.Errorf("codex hook context missing pending-reply mention:\n%s", ctx)
		}
		if !strings.Contains(ctx, "agentchute defer") {
			t.Errorf("codex hook context missing defer hint:\n%s", ctx)
		}
		if !strings.Contains(ctx, `--reply-to <message-id> --body "..."`) {
			t.Errorf("codex hook context missing runnable reply hint:\n%s", ctx)
		}
		if !strings.Contains(ctx, `--message <message-id> --reason "..."`) {
			t.Errorf("codex hook context missing runnable defer hint:\n%s", ctx)
		}
	})
}

// Clean state: pending reports the empty case explicitly, including
// the no-obligations message in --codex-hook output.
func TestPendingCleanCodexHookText(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPending(pendingArgs("--codex-hook", "UserPromptSubmit"))
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "no unread messages") {
			t.Errorf("clean state missing canonical empty phrasing:\n%s", out)
		}
		if !strings.Contains(out, "no pending reply obligations") {
			t.Errorf("clean state missing pending-reply empty phrasing:\n%s", out)
		}
	})
}

// v0.1.2: --claude-hook UserPromptSubmit emits the Claude-Code-specific
// hook JSON shape (hookSpecificOutput.additionalContext nested per
// code.claude.com/docs/en/hooks.md). Convergent with --codex-hook — both
// wrappers accept the same envelope today.
func TestPendingClaudeHookUserPromptSubmitShape(t *testing.T) {
	root, cfg := setupSendFixture(t)
	entry := loop.PendingReplyEntry{
		MessageID:        "msg-1",
		From:             "codex",
		To:               "claude-code",
		Task:             "review",
		OriginalFilename: "msg-1_from-codex_msg-aaaa.md",
		ArchivePath:      "archive/x.md",
	}
	if err := loop.RecordPendingReply(cfg, "claude-code", entry, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdPending(pendingArgs("--claude-hook", "UserPromptSubmit"))
		})
		if err != nil {
			t.Fatalf("--claude-hook returned err = %v; want nil", err)
		}
		var wrap struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if jerr := json.Unmarshal([]byte(out), &wrap); jerr != nil {
			t.Fatalf("unmarshal claude hook output: %v\n%s", jerr, out)
		}
		if wrap.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
			t.Errorf("HookEventName = %q, want UserPromptSubmit", wrap.HookSpecificOutput.HookEventName)
		}
		if !strings.Contains(wrap.HookSpecificOutput.AdditionalContext, "pending reply obligation") {
			t.Errorf("AdditionalContext missing pending-reply mention:\n%s", wrap.HookSpecificOutput.AdditionalContext)
		}
		if !strings.Contains(wrap.HookSpecificOutput.AdditionalContext, `--reply-to <message-id> --body "..."`) {
			t.Errorf("AdditionalContext missing runnable reply hint:\n%s", wrap.HookSpecificOutput.AdditionalContext)
		}
		if !strings.Contains(wrap.HookSpecificOutput.AdditionalContext, `--message <message-id> --reason "..."`) {
			t.Errorf("AdditionalContext missing runnable defer hint:\n%s", wrap.HookSpecificOutput.AdditionalContext)
		}
	})
}

// Convergence guard: --claude-hook and --codex-hook emit byte-identical
// JSON for the same inbox state today. If we ever diverge (one wrapper
// adds a wrapper-specific field), this test fails — re-evaluate whether
// the shared emitter still makes sense.
func TestPendingClaudeAndCodexHooksAreCurrentlyConvergent(t *testing.T) {
	root, _ := setupSendFixture(t)
	withCwd(t, root, func() {
		claudeOut, err := captureStdout(t, func() error {
			return cmdPending(pendingArgs("--claude-hook", "UserPromptSubmit"))
		})
		if err != nil {
			t.Fatal(err)
		}
		codexOut, err := captureStdout(t, func() error {
			return cmdPending(pendingArgs("--codex-hook", "UserPromptSubmit"))
		})
		if err != nil {
			t.Fatal(err)
		}
		if claudeOut != codexOut {
			t.Errorf("--claude-hook and --codex-hook UserPromptSubmit outputs diverged:\nclaude: %q\ncodex:  %q", claudeOut, codexOut)
		}
	})
}

// Codex follow-up on 0d468fa: pending's frontmatter peek must use the
// same lenient delimiter semantics as the validator/recorder. A
// hand-protocol message with `---   \n` must surface its message_id,
// task, and reply_required flag in the unread display — not blank
// fields just because of trailing whitespace on the delimiter line.
func TestPendingReadsWhitespaceTolerantFrontmatter(t *testing.T) {
	root, cfg := setupSendFixture(t)
	inbox := cfg.AgentInboxDir("claude-code")
	body := "---   \n" +
		"message_id: ws-pending-test\n" +
		"from: codex\n" +
		"to: claude-code\n" +
		"reply_required: true\n" +
		"task: whitespace-pending\n" +
		"   ---   \n" +
		"\n" +
		"body content\n"
	if err := os.WriteFile(filepath.Join(inbox, "2026-05-19T22-10-00-000000Z_from-codex_msg-abcd.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error { return cmdPending(pendingArgs("--json")) })
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Messages []struct {
				MessageID     string `json:"message_id"`
				Task          string `json:"task"`
				ReplyRequired bool   `json:"reply_required"`
			} `json:"messages"`
		}
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("Messages = %d, want 1", len(got.Messages))
		}
		m := got.Messages[0]
		if m.MessageID != "ws-pending-test" {
			t.Errorf("MessageID = %q, want ws-pending-test (lenient delimiters not honored)", m.MessageID)
		}
		if m.Task != "whitespace-pending" {
			t.Errorf("Task = %q, want whitespace-pending", m.Task)
		}
		if !m.ReplyRequired {
			t.Error("ReplyRequired = false; want true with whitespace delimiters")
		}
	})
}

// pending must be strictly read-only.
// Running it many times over a populated inbox must NOT change the inbox
// file count, archive count, or write to malformed dir. This is the
// hook-safety contract — SessionStart / UserPromptSubmit / BeforeAgent
// hooks fire on every turn and must not silently drain mail.
func TestPendingIsStrictlyReadOnlyAcrossManyInvocations(t *testing.T) {
	root, cfg := setupSendFixture(t)

	// Seed inbox with 3 valid messages + 1 malformed file.
	inbox := cfg.AgentInboxDir("claude-code")
	for i := 0; i < 3; i++ {
		if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC().Add(time.Duration(i)*time.Microsecond), "codex",
			[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n")); err != nil {
			t.Fatal(err)
		}
	}
	malformed := filepath.Join(inbox, "not-a-valid-message-name.md")
	if err := osWriteFile(malformed, []byte("---\nfrom: weird\n---\nbody\n")); err != nil {
		t.Fatal(err)
	}

	archiveDir := cfg.ArchiveDir()
	malformedDir := cfg.MalformedDir()

	beforeInbox := countFiles(t, inbox)
	beforeArchive := countFiles(t, archiveDir)
	beforeMalformed := countFiles(t, malformedDir)

	// Run pending 50 times across the JSON, text, and codex-hook modes.
	withCwd(t, root, func() {
		for i := 0; i < 50; i++ {
			variant := []string{"--json", "", "--codex-hook=UserPromptSubmit"}[i%3]
			args := pendingArgs()
			if variant != "" {
				args = append(args, variant)
			}
			if _, err := captureStdout(t, func() error { return cmdPending(args) }); err != nil {
				t.Fatalf("iter %d (variant=%s): %v", i, variant, err)
			}
		}
	})

	if got := countFiles(t, inbox); got != beforeInbox {
		t.Errorf("inbox file count changed: %d -> %d (pending must be side-effect-free)", beforeInbox, got)
	}
	if got := countFiles(t, archiveDir); got != beforeArchive {
		t.Errorf("archive file count changed: %d -> %d (pending must not archive)", beforeArchive, got)
	}
	if got := countFiles(t, malformedDir); got != beforeMalformed {
		t.Errorf("malformed dir file count changed: %d -> %d (pending must not quarantine)", beforeMalformed, got)
	}
}

// countFiles counts regular non-dotfile entries in dir; returns 0 for
// missing dirs (test-helper sugar).
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n++
	}
	return n
}

func osWriteFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// Verify the inbox-only path still reports unread without false-flagging
// the ledger as having entries.
func TestPendingInboxOnlyReportsZeroLedger(t *testing.T) {
	root, cfg := setupSendFixture(t)
	inbox := cfg.AgentInboxDir("claude-code")
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex",
		[]byte("---\nfrom: codex\nto: claude-code\ntask: hi\n---\n\nb\n")); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error { return cmdPending(pendingArgs("--json")) })
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Count          int `json:"count"`
			RepliesPending int `json:"replies_pending"`
		}
		if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
			t.Fatalf("unmarshal: %v\n%s", jerr, out)
		}
		if got.Count != 1 {
			t.Errorf("Count = %d, want 1", got.Count)
		}
		if got.RepliesPending != 0 {
			t.Errorf("RepliesPending = %d, want 0", got.RepliesPending)
		}
	})
	// Silence the unused-import warning for filepath if no other test needs it.
	_ = filepath.Join("", "")
}
