package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// TestOSNotify_TaskNotInterpretedAsScript asserts that a task/message carrying
// AppleScript metacharacters or newlines is passed to osascript as argv DATA,
// never spliced into the script source where it could break out. We assert the
// built command args (osascript is unavailable in CI).
func TestOSNotify_TaskNotInterpretedAsScript(t *testing.T) {
	evil := "x\" \non run\ndo shell script \"touch /tmp/pwned\"\nend run\n\""
	name, args := macNotifyCommand("agentchute: new message", evil)

	if name != "osascript" {
		t.Fatalf("command = %q, want osascript", name)
	}

	// The fixed script template must be the first -e value and must read from
	// argv, never embed the message.
	if len(args) < 2 || args[0] != "-e" {
		t.Fatalf("args[0:2] = %v, want [-e <script>]", args[:min(2, len(args))])
	}
	script := args[1]
	if !strings.Contains(script, "item 1 of argv") {
		t.Fatalf("script does not read message from argv:\n%s", script)
	}
	if strings.Contains(script, "do shell script") || strings.Contains(script, "pwned") {
		t.Fatalf("evil payload leaked into the script source:\n%s", script)
	}

	// The sanitized message must be a positional arg AFTER the "--" separator,
	// and its control characters (newlines) must be neutralized so it cannot
	// even render as multi-line, let alone execute.
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(args) {
		t.Fatalf("no positional message arg after -- separator: %v", args)
	}
	deliveredMsg := args[sep+1]
	if strings.ContainsAny(deliveredMsg, "\n\r\x00") {
		t.Fatalf("delivered message still contains control chars: %q", deliveredMsg)
	}
	if strings.Contains(deliveredMsg, "do shell script") {
		// The literal text may survive (it's data), but it lives in an argv
		// slot the fixed script only ever uses as a notification string — never
		// evaluated. Confirm it is NOT in the script slot (already checked) and
		// is purely positional data here. This assertion documents intent.
		if deliveredMsg != sanitizeNotificationText(evil) {
			t.Fatalf("delivered message = %q, want sanitized form", deliveredMsg)
		}
	}
}

// newWatchTestCfg sets up a control repo + inbox dir for the watch tests.
// Returns the cfg + the inbox dir so tests can drop new messages.
func newWatchTestCfg(t *testing.T) *loop.Config {
	t.Helper()
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
	if err := loop.EnsurePrivateDir(cfg.AgentInboxDir("claude-code")); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// snapshotInbox captures the seen set from the current inbox without
// firing actions. Pre-existing messages should not trigger callbacks on
// the next scan.
func TestSnapshotInboxCapturesExistingAsSeen(t *testing.T) {
	cfg := newWatchTestCfg(t)
	inbox := cfg.AgentInboxDir("claude-code")
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex",
		[]byte("---\nfrom: codex\nto: claude-code\n---\n\nhi\n")); err != nil {
		t.Fatal(err)
	}
	seen, err := snapshotInbox(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("seen size = %d, want 1", len(seen))
	}
}

// runWatchLoop's startup snapshot must capture existing messages so the
// first tick does NOT fire actions for them. Then a new message dropped
// after startup should fire exactly once.
func TestRunWatchLoopFiresOnlyOnNewArrivals(t *testing.T) {
	cfg := newWatchTestCfg(t)
	inbox := cfg.AgentInboxDir("claude-code")

	// Pre-existing message (should NOT fire on first tick).
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex",
		[]byte("---\nmessage_id: pre-existing\nfrom: codex\nto: claude-code\ntask: old\n---\n\nb\n")); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var firedKeys []string
	opts := watchOptions{
		Cfg:     cfg,
		AgentID: "claude-code",
		Print:   true,
		PrintFn: func(_, message string) {
			mu.Lock()
			defer mu.Unlock()
			firedKeys = append(firedKeys, message)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWatchLoop(ctx, opts, 50*time.Millisecond) }()

	// Give the loop a tick to do its startup snapshot.
	time.Sleep(120 * time.Millisecond)

	// Drop a new message AFTER the snapshot. Use a microsecond-distinct
	// timestamp so the filename is unique.
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex",
		[]byte("---\nmessage_id: post-arrival\nfrom: codex\nto: claude-code\ntask: new\n---\n\nb\n")); err != nil {
		t.Fatal(err)
	}

	// Let the loop pick it up on the next tick.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(firedKeys) != 1 {
		t.Fatalf("fired %d times, want 1; messages = %v", len(firedKeys), firedKeys)
	}
	if !contains(firedKeys[0], "new") {
		t.Errorf("expected 'new' in fired message; got %q", firedKeys[0])
	}
}

// v0.1.3 hotfix (codex review on d73d4dd): two distinct deliveries
// carrying the same frontmatter message_id must BOTH fire. message_id
// is not delivery-unique per AGENTCHUTE.md §6.4; the identity tuple
// in the filename is authoritative. Same class as the v0.1.1 ledger bug.
func TestRunWatchLoopFiresOnBothFilesWithSharedMessageID(t *testing.T) {
	cfg := newWatchTestCfg(t)
	inbox := cfg.AgentInboxDir("claude-code")

	var mu sync.Mutex
	fires := 0
	opts := watchOptions{
		Cfg:     cfg,
		AgentID: "claude-code",
		Print:   true,
		PrintFn: func(_, _ string) {
			mu.Lock()
			fires++
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWatchLoop(ctx, opts, 50*time.Millisecond) }()

	time.Sleep(120 * time.Millisecond)
	// Two distinct files (distinct filenames via separate WriteInboxMessage
	// calls; each gets a fresh nonce + microsecond-distinct timestamp)
	// that share a frontmatter message_id.
	sharedID := "shared-msgid-test"
	body := []byte("---\nmessage_id: " + sharedID + "\nfrom: codex\nto: claude-code\n---\n\nb\n")
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex", body); err != nil {
		t.Fatal(err)
	}
	// Microsecond gap so the second filename is guaranteed distinct.
	time.Sleep(2 * time.Millisecond)
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex", body); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if fires != 2 {
		t.Errorf("fires = %d, want 2 (both files must fire; message_id is not delivery-unique per §6.4)", fires)
	}
}

// Dedup: same FILE scanned across two ticks must fire actions exactly
// once. Filename is the protocol's identity tuple; this is the legitimate
// dedup case.
func TestRunWatchLoopDedupesBySameFile(t *testing.T) {
	cfg := newWatchTestCfg(t)
	inbox := cfg.AgentInboxDir("claude-code")

	var mu sync.Mutex
	fires := 0
	opts := watchOptions{
		Cfg:     cfg,
		AgentID: "claude-code",
		Print:   true,
		PrintFn: func(_, _ string) {
			mu.Lock()
			fires++
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runWatchLoop(ctx, opts, 50*time.Millisecond) }()

	time.Sleep(120 * time.Millisecond)
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex",
		[]byte("---\nmessage_id: dedup-test\nfrom: codex\nto: claude-code\n---\n\nb\n")); err != nil {
		t.Fatal(err)
	}
	// Let several ticks elapse — fires should remain at 1 (same file scanned
	// repeatedly).
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if fires != 1 {
		t.Errorf("fires = %d, want 1 (same file dedup)", fires)
	}
}

// fireActions routes correctly to each enabled action function. Black-box
// test of the dispatch path without involving the polling loop.
func TestFireActionsCallsEnabledActions(t *testing.T) {
	notifies, prints, execs := 0, 0, 0
	var lastEnv map[string]string

	opts := watchOptions{
		AgentID: "claude-code",
		Notify:  true,
		Print:   true,
		ExecCmd: "true", // non-empty so fireActions invokes exec
		NotifyFn: func(_, _ string) error {
			notifies++
			return nil
		},
		PrintFn: func(_, _ string) { prints++ },
		ExecFn: func(_ string, env map[string]string) error {
			execs++
			lastEnv = env
			return nil
		},
	}

	fireActions(opts, watchEntry{
		Key:       "ts_from-codex_msg-aaaa.md",
		MessageID: "msg-test-1",
		From:      "codex",
		Task:      "review",
		Filename:  "ts_from-codex_msg-aaaa.md",
	})

	if notifies != 1 {
		t.Errorf("notifies = %d, want 1", notifies)
	}
	if prints != 1 {
		t.Errorf("prints = %d, want 1", prints)
	}
	if execs != 1 {
		t.Errorf("execs = %d, want 1", execs)
	}
	// AGENTCHUTE_MSG_ID surfaces the frontmatter message_id when present.
	if lastEnv["AGENTCHUTE_MSG_ID"] != "msg-test-1" {
		t.Errorf("AGENTCHUTE_MSG_ID = %q, want msg-test-1", lastEnv["AGENTCHUTE_MSG_ID"])
	}
	if lastEnv["AGENTCHUTE_FROM"] != "codex" {
		t.Errorf("AGENTCHUTE_FROM = %q, want codex", lastEnv["AGENTCHUTE_FROM"])
	}
	if lastEnv["AGENTCHUTE_TASK"] != "review" {
		t.Errorf("AGENTCHUTE_TASK = %q, want review", lastEnv["AGENTCHUTE_TASK"])
	}
}

// cmdWatch with no action flags must refuse to run silently.
func TestCmdWatchRequiresAnAction(t *testing.T) {
	root := t.TempDir()
	if err := osWriteFile(filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec")); err != nil {
		t.Fatal(err)
	}
	if err := loop.EnsurePrivateDir(filepath.Join(root, ".examplecorp", "loop")); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error {
			return cmdWatch([]string{"--as", "claude-code"})
		})
		if err == nil {
			t.Fatal("expected error when watch has no action flag")
		}
		if !contains(err.Error(), "--notify") {
			t.Errorf("error should mention the action flags: %v", err)
		}
	})
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
