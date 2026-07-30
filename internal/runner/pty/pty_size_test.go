package pty

import (
	"os/exec"
	"testing"

	creackpty "github.com/creack/pty"
)

// The child must observe the inherited window size on its very first read,
// before any SIGWINCH/resize loop runs. A child started on a 0x0 PTY renders
// a blank screen in ratatui/Ink TUIs (the "blank boxes" startup race).
//
// The window size is a kernel property shared by the returned PTY master and
// the child's slave. Read it directly from the new PTY instead of depending on
// a short-lived `stty` child to emit output before its slave closes. There is no
// resize loop in this test, so the observed size can only be the startup size.
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

	cmd := exec.Command("sleep", "10")
	ptmx, err := StartInheritSize(cmd, slave)
	if err != nil {
		t.Fatalf("StartInheritSize: %v", err)
	}
	defer ptmx.Close()
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	got, err := creackpty.GetsizeFull(ptmx)
	if err != nil {
		t.Fatalf("get child PTY size: %v", err)
	}
	if got.Rows != want.Rows || got.Cols != want.Cols {
		t.Fatalf("child PTY startup size = %dx%d, want %dx%d", got.Rows, got.Cols, want.Rows, want.Cols)
	}
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
