package pty

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	creackpty "github.com/creack/pty"
)

// The child must observe the inherited window size on its very first read,
// before any SIGWINCH/resize loop runs. A child started on a 0x0 PTY renders
// a blank screen in ratatui/Ink TUIs (the "blank boxes" startup race).
func TestStartInheritSizeChildSeesInitialSize(t *testing.T) {
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatalf("open pty pair: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	want := &creackpty.Winsize{Rows: 41, Cols: 97}
	if err := creackpty.Setsize(master, want); err != nil {
		t.Fatalf("setsize: %v", err)
	}

	cmd := exec.Command("stty", "size")
	ptmx, err := StartInheritSize(cmd, slave)
	if err != nil {
		t.Fatalf("StartInheritSize: %v", err)
	}
	defer ptmx.Close()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := ptmx.Read(buf)
		done <- string(buf[:n])
	}()

	select {
	case out := <-done:
		if !strings.Contains(out, "41 97") {
			t.Fatalf("child saw winsize %q at startup, want \"41 97\"", strings.TrimSpace(out))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child output")
	}
	_ = cmd.Wait()
}

// When the source fd has no usable size (not a terminal, or 0x0), fall back
// to a plain unsized start rather than failing.
func TestStartInheritSizeFallsBackWithoutTerminal(t *testing.T) {
	cmd := exec.Command("true")
	ptmx, err := StartInheritSize(cmd, nil)
	if err != nil {
		t.Fatalf("StartInheritSize with nil source: %v", err)
	}
	defer ptmx.Close()
	_ = cmd.Wait()
}
