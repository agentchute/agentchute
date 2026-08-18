//go:build sshd_integration

package sshd

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/op"
)

func TestSSHDClientPipeDeadlinesAreEnforceable(t *testing.T) {
	t.Run("channel read deadline", func(t *testing.T) {
		h := newSSHDHarness(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		transport, err := h.open(ctx, "codex", "discard-this-command", hubclient.SSHBuildOptions{Channel: true})
		if err != nil {
			t.Fatal(err)
		}
		defer transport.Close()
		channel, err := hubclient.OpenChannelTransport(transport, h.remote, "codex", "sshd-integration")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := channel.AcquireLease(op.LeaseReq{}); err != nil {
			t.Fatal(err)
		}
		if err := transport.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = transport.cmd.Process.Signal(syscall.SIGCONT) }()

		started := time.Now()
		_, err = channel.Tick(op.TickReq{})
		elapsed := time.Since(started)
		if code := hubclient.ErrorCode(err); code != "E_CHANNEL_LOST" {
			t.Fatalf("stopped-peer tick = %v, code %q", err, code)
		}
		if elapsed < 9*time.Second || elapsed > 13*time.Second {
			t.Fatalf("client read deadline fired after %s, want about 10s", elapsed)
		}
	})

	t.Run("transport write deadline", func(t *testing.T) {
		h := newSSHDHarness(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		transport, err := h.open(ctx, "codex", "discard-this-command", hubclient.SSHBuildOptions{Channel: true})
		if err != nil {
			t.Fatal(err)
		}
		defer transport.Close()
		if err := transport.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = transport.cmd.Process.Signal(syscall.SIGCONT) }()
		if err := transport.SetWriteDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatalf("set client write deadline: %v", err)
		}

		chunk := make([]byte, 64<<10)
		started := time.Now()
		for {
			_, err = transport.Write(chunk)
			if err != nil {
				break
			}
		}
		elapsed := time.Since(started)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("stopped-peer write = %v, want deadline exceeded", err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("client write deadline fired after %s", elapsed)
		}
	})
}
