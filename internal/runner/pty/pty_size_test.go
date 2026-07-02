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
//
// creack/pty's StartWithAttrs calls Setsize on the new pty and checks its
// error BEFORE cmd.Start() ever forks the child (run.go) — there is no
// product-level window where the child can run before the size is applied.
// What IS racy under CI scheduling is forking/exec'ing "stty size" and
// getting its output back to us: a single eager Read can lose that race
// (or observe a spurious empty read) even though the pty was correctly
// sized from the first instant the child could read it. Poll with a tight
// budget instead of trusting one Read to land in time. This stays a hard
// assertion on STARTUP sizing — the budget is short specifically so it
// cannot be satisfied by the separate SIGWINCH heal path (which this
// isolated test doesn't even wire up).
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

	chunks := make(chan string, 16)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunks <- string(buf[:n])
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()

	deadline := time.After(500 * time.Millisecond)
	var got strings.Builder
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("child output ended before showing winsize within 500ms; got %q", got.String())
			}
			got.WriteString(chunk)
			if strings.Contains(got.String(), "41 97") {
				_ = cmd.Wait()
				return
			}
		case <-deadline:
			t.Fatalf("child saw winsize %q within 500ms at startup, want \"41 97\"", strings.TrimSpace(got.String()))
		}
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
