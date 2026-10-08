//go:build darwin || linux

package cli

import (
	"os"
	"testing"

	creackpty "github.com/creack/pty"
)

// --takeover's terminal gate must mean a TERMINAL. It tested "stdin is a
// character device", and /dev/null is one — so `hub authorize ... --takeover
// </dev/null`, exactly what a script or an exec'd tool call gets, passed the
// gate. The real-sshd row caught it; this pins it without sshd.
func TestHubAuthorizeTerminalGateIsNotFooledByDevNull(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	orig := os.Stdin
	t.Cleanup(func() { os.Stdin = orig })

	os.Stdin = devNull
	if hubAuthorizeStdinIsTTY() {
		t.Fatal("stdin = /dev/null passed the terminal gate")
	}

	// A pipe and a regular file are not terminals either.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	os.Stdin = r
	if hubAuthorizeStdinIsTTY() {
		t.Fatal("stdin = a pipe passed the terminal gate")
	}
	regular, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	os.Stdin = regular
	if hubAuthorizeStdinIsTTY() {
		t.Fatal("stdin = a regular file passed the terminal gate")
	}

	ptmx, tty, err := creackpty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	os.Stdin = tty
	if !hubAuthorizeStdinIsTTY() {
		t.Fatal("a real pseudo-terminal on stdin failed the terminal gate")
	}
}
