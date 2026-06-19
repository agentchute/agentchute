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
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
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
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
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
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", initialLastSeen, loop.StatusActive, nil)

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
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
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
	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
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

// TestWatchdogPoke_RefusesUnownedRunnerSocket: a stale peer that advertises a
// runner wake_target it does not own must be REFUSED (never dialed) — the
// watchdog routes through loop.PokeRegistration, whose owned-check fails before
// any adapter dial. The "refused:" prefix in the log proves the refusal short-
// circuited ahead of the dial (a real dial would log a connect error instead).
func TestWatchdogPoke_RefusesUnownedRunnerSocket(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	// Stale peer with an unowned runner socket.
	evil := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  "unix:/tmp/evil.sock",
		LastSeen:    now.Add(-10 * time.Minute),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), evil); err != nil {
		t.Fatal(err)
	}
	mustWriteAgedInbox(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), now.Add(-8*time.Minute))

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
	logText := string(logBytes)
	if !strings.Contains(logText, "poke codex failed") || !strings.Contains(logText, "refused") {
		t.Fatalf("watchdog log missing runner-socket refusal for codex:\n%s", logText)
	}
}

// TestWatchdogPoke_OwnedRunnerSocketAttemptsDial: the complement — an owned
// runner socket is NOT refused; it proceeds to the dial (which fails because no
// real runner listens, logging a connect error, NOT a "refused" error).
func TestWatchdogPoke_OwnedRunnerSocketAttemptsDial(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	owned := loop.RunnerWakeTarget(cfg.RunnerSocketPath("codex"))
	reg := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  owned,
		LastSeen:    now.Add(-10 * time.Minute),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), reg); err != nil {
		t.Fatal(err)
	}
	mustWriteAgedInbox(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), now.Add(-8*time.Minute))

	if err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}
	logText := mustReadString(t, cfg.WatchdogLogPath())
	if strings.Contains(logText, "refused") {
		t.Fatalf("owned runner socket must NOT be refused:\n%s", logText)
	}
	// The dial fails (no listener) but it was ATTEMPTED — the failure is a
	// connect error, distinct from a recipient-binding refusal.
	if !strings.Contains(logText, "poke codex failed") {
		t.Fatalf("expected a dial attempt+failure for the owned socket:\n%s", logText)
	}
}

// TestWatchdogReachability_DoesNotDialUnownedSocket: the runner-supervision
// reachability probe in the liveness sweep must NOT dial a peer's advertised
// runner socket unless that path is one the recipient legitimately owns. A
// hostile registration naming a live attacker-controlled socket would otherwise
// be connected to during the sweep. We stand up a REAL listener at an unowned
// path with an accept-counter and assert the sweep never touched it.
//
// This closes the gap in TestWatchdogPoke_RefusesUnownedRunnerSocket: that test
// only proved the later poke logged "refused" (its evil path had no listener so
// the reachability dial failed silently); it did NOT prove the reachability
// probe was skipped. Here a live listener makes any dial observable.
func TestWatchdogReachability_DoesNotDialUnownedSocket(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	// A REAL listening "evil" socket at a path codex does NOT own.
	evilPath := shortSocketPath(t, "evil.sock")
	evil := listenCounting(t, evilPath)

	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	peer := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  loop.RunnerWakeTarget(evilPath),
		LastSeen:    now.Add(-10 * time.Minute),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), peer); err != nil {
		t.Fatal(err)
	}
	mustWriteAgedInbox(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), now.Add(-8*time.Minute))

	if err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}

	// Give any errant dial a moment to land before asserting zero.
	time.Sleep(50 * time.Millisecond)
	if c := evil.count(); c != 0 {
		t.Fatalf("watchdog sweep dialed the unowned runner socket %d time(s); owned-check must short-circuit before dial", c)
	}
}

// A peer whose inbox contains ONLY a malformed (unparseable) file must still
// be poked: gate blocks the peer until `check` quarantines it, so the watchdog
// must treat skipped files as wake-worthy work. The old ListInboxMessages-only
// scan saw an "empty" inbox and skipped the poke.
func TestWatchdog_WakesPeerWithOnlyMalformedInbox(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	// Only a malformed file (invalid nonce) — parses as skipped, not a Message.
	malformed := filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-30-00-123456Z_from-claude-code_msg-zz.md")
	mustWrite(t, malformed, []byte("hi"))
	// Back-date the file's mtime well past the message-age threshold so the
	// only reason a poke could be skipped is the malformed-invisibility bug.
	old := now.Add(-30 * time.Minute)
	if err := os.Chtimes(malformed, old, old); err != nil {
		t.Fatal(err)
	}

	if err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}

	logText := mustReadString(t, cfg.WatchdogLogPath())
	// tmux send-keys fails in the test env (no fake binary) — but the watchdog
	// must have ATTEMPTED the poke, which logs "poke codex failed". If the
	// malformed file were invisible, codex would not be processed at all.
	if !strings.Contains(logText, "codex") {
		t.Fatalf("watchdog ignored peer with only a malformed inbox file:\n%s", logText)
	}
	if strings.Contains(logText, "below threshold") {
		t.Fatalf("watchdog treated malformed-only inbox as empty/no-age:\n%s", logText)
	}
}

// A future/skewed filename timestamp must NOT suppress the poke. The age is
// derived from filesystem arrival time, so a back-dated (future) sender-encoded
// filename can't clamp the age to zero and silence the watchdog indefinitely.
func TestWatchdog_BackdatedFilenameStillPokes(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	writeRegistration(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	// Filename timestamp is in the FUTURE relative to now (2027). With the old
	// sender-timestamp basis, now.Sub(future) is negative → clamped to 0 →
	// always below threshold → poke suppressed forever.
	future := filepath.Join(cfg.AgentInboxDir("codex"), "2027-01-01T00-00-00-000000Z_from-claude-code_msg-abcd.md")
	mustWrite(t, future, []byte("hi"))
	// The file actually ARRIVED long ago; arrival time must drive the age.
	old := now.Add(-30 * time.Minute)
	if err := os.Chtimes(future, old, old); err != nil {
		t.Fatal(err)
	}

	if err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	}); err != nil {
		t.Fatal(err)
	}

	logText := mustReadString(t, cfg.WatchdogLogPath())
	if strings.Contains(logText, "below threshold") {
		t.Fatalf("future-dated filename suppressed the poke via clamped age:\n%s", logText)
	}
	if !strings.Contains(logText, "codex") {
		t.Fatalf("watchdog did not process codex with a future-dated filename:\n%s", logText)
	}
}

// A reset/update race can delete the watchdog's OWN registration mid-flight.
// runWatchdogCycle must log-and-continue on a missing self-registration (or a
// self UpdateLastSeen failure) rather than returning a hard error that kills
// the daemon loop.
func TestWatchdog_SelfRegDeletedDoesNotKillDaemon(t *testing.T) {
	root, cfg := setupWatchdogFixture(t)
	now := time.Date(2026, 5, 9, 16, 40, 0, 0, time.UTC)

	// No self-registration for "watchdog" (simulating a setup/update reset
	// that cleared regs). A peer still has stale unread mail to be swept.
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	msg := filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-30-00-123456Z_from-claude-code_msg-abcd.md")
	mustWrite(t, msg, []byte("hi"))
	oldT := now.Add(-30 * time.Minute)
	if err := os.Chtimes(msg, oldT, oldT); err != nil {
		t.Fatal(err)
	}

	err := runWatchdogCycle(context.Background(), cfg, watchdogOptions{
		AgentID:             "watchdog",
		StaleThreshold:      5 * time.Minute,
		MessageAgeThreshold: time.Minute,
		Now:                 func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("missing self-registration killed the cycle: %v", err)
	}

	// The sweep must still have run despite the missing self-reg.
	logText := mustReadString(t, cfg.WatchdogLogPath())
	if !strings.Contains(logText, "codex") {
		t.Fatalf("watchdog aborted the sweep after a missing self-registration:\n%s", logText)
	}
}

func mustReadString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setupWatchdogFixture(t *testing.T) (string, *loop.Config) {
	t.Helper()
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
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
	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "M5.local", "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "remote-machine.local", "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWriteAgedInbox(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), now.Add(-8*time.Minute))

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

	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "M5.local", "", now.Add(-time.Minute), loop.StatusActive, nil)
	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "M5.local", "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWriteAgedInbox(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), now.Add(-8*time.Minute))

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

	writeRegistrationWithHost(t, cfg.AgentRegistrationPath("watchdog"), "watchdog", "examplecorp", root, "M5.local", "", now.Add(-time.Minute), loop.StatusActive, nil)
	// Peer with NO host field (empty).
	writeRegistration(t, cfg.AgentRegistrationPath("codex"), "codex", "openai", root, "%1", now.Add(-10*time.Minute), loop.StatusActive, nil)
	mustWriteAgedInbox(t, filepath.Join(cfg.AgentInboxDir("codex"), "2026-05-09T16-32-00-123456Z_from-claude-code_msg-abcd.md"), now.Add(-8*time.Minute))

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
