//go:build !windows

package cli

import (
	"os"
	"syscall"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
)

// Only startup sizing (StartInheritSize, pty_size_test.go) had coverage
// before this: a resize arriving mid-session — the common case of a human
// resizing their terminal window while a wrapper is running — was untested.
// This drives resizeLoop's real SIGWINCH path end-to-end: a size change on
// the runner's own terminal must propagate to the child PTY, and the
// session (the data channel to the child) must remain usable afterward.
func TestResizeLoopPropagatesSIGWINCHAndSessionSurvives(t *testing.T) {
	// "the runner's own terminal" that resizeLoop reads size from (os.Stdin).
	stdinMaster, stdinSlave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open stdin pty pair: %v", err)
	}
	defer stdinMaster.Close()
	defer stdinSlave.Close()
	if err := creackpty.Setsize(stdinMaster, &creackpty.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("setsize initial: %v", err)
	}

	// "the child PTY" that resizeLoop copies the size onto.
	childMaster, childSlave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open child pty pair: %v", err)
	}
	defer childMaster.Close()
	defer childSlave.Close()

	origStdin := os.Stdin
	os.Stdin = stdinSlave
	defer func() { os.Stdin = origStdin }()

	rt := &runnerRuntime{ptmx: childMaster, stopCh: make(chan struct{})}
	loopDone := make(chan struct{})
	go func() {
		rt.resizeLoop()
		close(loopDone)
	}()
	defer func() {
		close(rt.stopCh)
		<-loopDone
	}()

	assertSessionAlive := func(marker byte) {
		t.Helper()
		if _, err := childSlave.Write([]byte{marker}); err != nil {
			t.Fatalf("write to child slave: %v", err)
		}
		got := make(chan byte, 1)
		readErr := make(chan error, 1)
		go func() {
			buf := make([]byte, 1)
			if _, err := childMaster.Read(buf); err != nil {
				readErr <- err
				return
			}
			got <- buf[0]
		}()
		select {
		case b := <-got:
			if b != marker {
				t.Fatalf("session echoed %q, want %q", b, marker)
			}
		case err := <-readErr:
			t.Fatalf("read from child pty: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out reading from child pty — session did not survive")
		}
	}

	// Prove the channel works before touching size at all.
	assertSessionAlive('a')

	// Let resizeLoop finish its startup InheritSize call and register its
	// SIGWINCH handler before we resize and signal.
	time.Sleep(50 * time.Millisecond)

	if err := creackpty.Setsize(stdinMaster, &creackpty.Winsize{Rows: 41, Cols: 97}); err != nil {
		t.Fatalf("setsize resize: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("send SIGWINCH: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := creackpty.GetsizeFull(childMaster)
		if err != nil {
			t.Fatalf("getsize child pty: %v", err)
		}
		if got.Rows == 41 && got.Cols == 97 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pty winsize = %dx%d after SIGWINCH, want 41x97", got.Rows, got.Cols)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The resize must not have disrupted the data channel.
	assertSessionAlive('b')
}
