package pty

import (
	"os"
	"os/exec"

	creackpty "github.com/creack/pty"
)

// Start launches cmd with a new controlling pseudo-terminal.
func Start(cmd *exec.Cmd) (*os.File, error) {
	return creackpty.Start(cmd)
}

// StartInheritSize launches cmd with a new controlling pseudo-terminal whose
// window size is copied from `from` BEFORE the child starts. A PTY opened via
// plain Start has a 0x0 winsize until the first InheritSize, and the resize
// loop runs it too late for fast-booting TUIs: they read 0 rows/cols on their
// first draw and come up blank. When `from` is nil or has no usable size,
// fall back to an unsized start.
func StartInheritSize(cmd *exec.Cmd, from *os.File) (*os.File, error) {
	if from != nil {
		if sz, err := creackpty.GetsizeFull(from); err == nil && sz.Rows > 0 && sz.Cols > 0 {
			return creackpty.StartWithSize(cmd, sz)
		}
	}
	return creackpty.Start(cmd)
}

// InheritSize copies terminal dimensions from from to to when possible.
func InheritSize(from, to *os.File) error {
	return creackpty.InheritSize(from, to)
}
