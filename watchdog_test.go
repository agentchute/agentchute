package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestRunWatchdogCycleDefersFutureRestart(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, &future)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	logBytes, err := os.ReadFile(cfg.WatchdogLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBytes), "deferring codex until") {
		t.Fatalf("watchdog log missing deferral:\n%s", string(logBytes))
	}
}

func TestRunWatchdogCycleSkipsFreshAgent(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Second), loop.StatusActive, nil)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	logBytes, err := os.ReadFile(cfg.WatchdogLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBytes), "codex last_seen fresh") {
		t.Fatalf("watchdog log missing fresh skip:\n%s", string(logBytes))
	}
}

func TestRunWatchdogCycleUpdatesOwnLastSeen(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	initialLastSeen := now.Add(-10 * time.Minute)
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "", initialLastSeen, loop.StatusActive, nil)

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("watchdog"))
	if err != nil {
		t.Fatal(err)
	}
	if !reg.LastSeen.Equal(now) {
		t.Fatalf("watchdog last_seen not updated: got %v, want %v", reg.LastSeen, now)
	}
}

func TestRunWatchdogCycleIgnoresWatchdogLogWriteError(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Second), loop.StatusActive, nil)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))
	mustMkdir(t, cfg.WatchdogLogPath())

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A single peer with an unreadable inbox must be logged and skipped, not
// take down the entire daemon. Inverts the prior contract that propagated
// per-agent errors all the way out.
func TestRunWatchdogCycleSkipsPeerWithUnreadableInbox(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("gemini-cli"), "gemini-cli", "google", root, "%2", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("gemini-cli"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))
	if err := os.Remove(cfg.AgentInboxDir("codex")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, cfg.AgentInboxDir("codex"), []byte("not a directory"))

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("cycle returned error instead of skipping bad peer: %v", err)
	}

	logBytes, err := os.ReadFile(cfg.WatchdogLogPath())
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "codex error:") {
		t.Fatalf("watchdog log missing per-agent error for codex:\n%s", logText)
	}
	if !strings.Contains(logText, "gemini-cli") {
		t.Fatalf("watchdog did not continue to next peer after codex error:\n%s", logText)
	}
}

func setupWatchdogFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	mustMkdir(t, cfg.AgentsDir())
	mustMkdir(t, cfg.AgentInboxDir("codex"))
	return root, cfg
}

func writeRegistration(t *testing.T, path, agentID, vendor, root, tmux string, lastSeen time.Time, status loop.Status, restartAt *time.Time) {
	t.Helper()
	method := ""
	if tmux != "" {
		method = "tmux"
	}
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      vendor,
		ControlRepo: root,
		WakeMethod:  method,
		WakeTarget:  tmux,
		LastSeen:    lastSeen,
		Status:      status,
		RestartAt:   restartAt,
	}
	if err := loop.WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
}

func writeRegistrationWithHost(t *testing.T, path, agentID, vendor, root, host, tmux string, lastSeen time.Time, status loop.Status, restartAt *time.Time) {
	t.Helper()
	method := ""
	if tmux != "" {
		method = "tmux"
	}
	reg := &loop.Registration{
		AgentID:     agentID,
		Vendor:      vendor,
		ControlRepo: root,
		Host:        host,
		WakeMethod:  method,
		WakeTarget:  tmux,
		LastSeen:    lastSeen,
		Status:      status,
		RestartAt:   restartAt,
	}
	if err := loop.WriteRegistration(path, reg); err != nil {
		t.Fatal(err)
	}
}

// Cross-host peers MUST be skipped silently during the liveness sweep:
// the wake adapter is machine-local, so a poke from this host can't reach
// them anyway. AGENTCHUTE.md §10.5 / §12.
func TestRunLivenessSweepSkipsCrossHostPeers(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	// Watchdog on M5.local; peer on remote-machine.local with stale inbox.
	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "M5.local", "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "remote-machine.local", "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		LocalHost:           "M5.local",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	logBytes, _ := os.ReadFile(cfg.WatchdogLogPath())
	logText := string(logBytes)
	if strings.Contains(logText, "poked codex") {
		t.Fatalf("cross-host peer should be skipped silently, but log shows poke:\n%s", logText)
	}
}

// When both the local watchdog and the peer share a host, the sweep
// proceeds normally (poke happens for stale-unread peers).
func TestRunLivenessSweepDoesNotSkipSameHostPeers(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "M5.local", "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "M5.local", "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	// tmux send-keys will fail in the test env (no tmux server / no fake
	// binary stubbed); the sweep should log "poke codex failed" — which is
	// what we want to observe: it ATTEMPTED to poke the same-host peer.
	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		LocalHost:           "M5.local",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	logBytes, _ := os.ReadFile(cfg.WatchdogLogPath())
	logText := string(logBytes)
	if !strings.Contains(logText, "codex") {
		t.Fatalf("same-host peer should have been processed (poked or poke-failed), got:\n%s", logText)
	}
}

// Empty peer host should be treated as "legacy/unknown same-host" — the
// poke is attempted even when LocalHost is set. AGENTCHUTE.md §5: empty
// host = legacy/unknown.
func TestRunLivenessSweepTreatsEmptyPeerHostAsSameHost(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "rehumanlabs", root, "M5.local", "", now.Add(-time.Minute), loop.StatusActive, nil)
	// Peer with NO host field (empty).
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWrite(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), []byte("hi"))

	if err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		LocalHost:           "M5.local",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}

	logBytes, _ := os.ReadFile(cfg.WatchdogLogPath())
	logText := string(logBytes)
	if !strings.Contains(logText, "codex") {
		t.Fatalf("empty-host peer should be attempted (legacy/unknown = same host); got log:\n%s", logText)
	}
}
