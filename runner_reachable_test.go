package main

import (
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// acceptCountingListener wraps a real unix listener and counts every accepted
// connection. A nonzero count proves a dial reached the socket.
type acceptCountingListener struct {
	ln       net.Listener
	accepted *int64
	done     chan struct{}
}

// shortSocketPath returns a unix socket path short enough for the ~104-byte
// sun_path limit (deep t.TempDir() paths overflow it on darwin). Uses a fresh
// dir under os.TempDir() that is cleaned up with the test.
func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ac-evil-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// listenCounting binds a real unix socket at path and drains accepts into a
// counter so a test can assert whether the socket was ever dialed.
func listenCounting(t *testing.T, path string) *acceptCountingListener {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	var accepted int64
	l := &acceptCountingListener{ln: ln, accepted: &accepted, done: make(chan struct{})}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(l.done)
				return
			}
			atomic.AddInt64(&accepted, 1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return l
}

func (l *acceptCountingListener) count() int64 { return atomic.LoadInt64(l.accepted) }

// TestRunnerReachableForRecipient_UnownedReturnsFalseNoDial: a registration
// naming a wake_target the recipient does not own must be reported UNREACHABLE
// without any socket dial — even if a live listener is standing at that path.
func TestRunnerReachableForRecipient_UnownedReturnsFalseNoDial(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}

	// A real listening "evil" socket at a path codex does NOT own.
	evilPath := shortSocketPath(t, "evil.sock")
	evil := listenCounting(t, evilPath)

	reg := &loop.Registration{
		AgentID:    "codex",
		WakeMethod: loop.RunnerWakeMethod,
		WakeTarget: loop.RunnerWakeTarget(evilPath),
	}
	if runnerReachableForRecipient(cfg, reg, time.Second) {
		t.Fatal("unowned wake_target reported reachable; want false")
	}
	// Give any errant dial a moment to land before asserting zero.
	time.Sleep(50 * time.Millisecond)
	if c := evil.count(); c != 0 {
		t.Fatalf("unowned socket was dialed %d time(s); owned-check must short-circuit before dial", c)
	}
}

// TestRunnerReachableForRecipient_OwnedLiveSocketReturnsTrue: a registration
// pointing at a live socket the recipient legitimately owns is reachable.
func TestRunnerReachableForRecipient_OwnedLiveSocketReturnsTrue(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}

	// The recipient's own runner socket, with a real runner answering ping.
	ownedPath := cfg.RunnerSocketPath("codex")
	startFakeRunnerPingSocket(t, ownedPath, loop.RunnerPingResponse{AgentID: "codex"})

	reg := &loop.Registration{
		AgentID:    "codex",
		WakeMethod: loop.RunnerWakeMethod,
		WakeTarget: loop.RunnerWakeTarget(ownedPath),
	}
	if !runnerReachableForRecipient(cfg, reg, time.Second) {
		t.Fatal("owned live socket reported unreachable; want true")
	}
}
