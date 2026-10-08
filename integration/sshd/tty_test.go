//go:build sshd_integration

package sshd

import (
	"bytes"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	creackpty "github.com/creack/pty"
)

// runCLITTY runs the CLI with a real pseudo-terminal on stdin, as an operator at
// a terminal would. hub authorize gates --replace-key on a terminal, and join's
// rotation carries one to the hub with `ssh -t` only when it has one itself —
// so the rotation rows go through the REAL path, not a seam compiled into the
// binary. stdout and stderr stay separate buffers; only stdin is the terminal.
func (h *sshdHarness) runCLITTY(dir string, args ...string) (string, string, error) {
	h.t.Helper()
	ptmx, tty, err := creackpty.Open()
	if err != nil {
		h.t.Fatalf("open pty: %v", err)
	}
	defer ptmx.Close()
	// Drain anything written back to the terminal (ssh -t puts it in raw mode),
	// so a full pty buffer can never stall the child.
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()
	cmd := exec.Command(h.binary, args...)
	cmd.Dir = dir
	cmd.Env = h.commandEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, &stdout, &stderr
	err = cmd.Run()
	_ = tty.Close()
	return stdout.String(), stderr.String(), err
}

// The other side of the gate, through the real binary and real sshd: a
// --replace-key with no terminal is refused and the authorized key survives.
func TestSSHDReplaceKeyWithoutATerminalIsRefused(t *testing.T) {
	h := newSSHDHarness(t)
	before := readFileString(t, h.authorized)
	attackerKey := filepath.Join(h.root, "attacker_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "attacker", "-f", attackerKey)
	attacker := strings.TrimSpace(readFileString(t, attackerKey+".pub"))
	stdout, stderr, err := h.runCLI(h.pool, "hub", "authorize", "--agent", "codex", "--pool", h.pool, "--key", attacker, "--replace-key")
	if err == nil || !strings.Contains(stderr+stdout, "interactive terminal") {
		t.Fatalf("non-terminal --replace-key = %v\nstdout:\n%s\nstderr:\n%s\nwant a refusal", err, stdout, stderr)
	}
	if after := readFileString(t, h.authorized); after != before {
		t.Fatal("a refused --replace-key changed authorized_keys")
	}
}
