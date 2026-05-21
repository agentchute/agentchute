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

// InheritSize copies terminal dimensions from from to to when possible.
func InheritSize(from, to *os.File) error {
	return creackpty.InheritSize(from, to)
}
