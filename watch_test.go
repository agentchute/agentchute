package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// newWatchTestCfg sets up a control repo + inbox dir for the watch tests.
// Returns the cfg + the inbox dir so tests can drop new messages.
func newWatchTestCfg(t *testing.T) *loop.Config {
	t.Helper()
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
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

// Dedup: same message_id arriving twice (or the same file scanned across
// two ticks) must fire actions exactly once.
func TestRunWatchLoopDedupesByMessageID(t *testing.T) {
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
	// Let several ticks elapse — fires should remain at 1.
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if fires != 1 {
		t.Errorf("fires = %d, want 1 (dedup by message_id)", fires)
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
		Key:      "msg-test-1",
		From:     "codex",
		Task:     "review",
		Filename: "ts_from-codex_msg-aaaa.md",
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
	if err := loop.EnsurePrivateDir(filepath.Join(root, ".rehumanlabs", "loop")); err != nil {
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
