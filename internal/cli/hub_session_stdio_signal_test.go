//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
)

// Issue #223: sshd's SIGHUP (or a SIGTERM) cancels the hub session's context,
// and cancellation used to reach the session only by closing the transport.
// Closing a blocking pipe fd does not interrupt a read or write already in the
// kernel, so with sshd's pipes still open the session sat in read(fd 0) — or in
// a write to a full stdout — until its 20 s channel-read bound, holding the
// lane's serve claim. These rows run the real command over its own stdio,
// leave both pipes open, signal it, and require the claim gone within
// hubSignalReleaseBound.
const hubSignalReleaseBound = 5 * time.Second

func TestHubSessionSignalReleasesLeaseWithPipesOpen(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		for _, backpressure := range []bool{false, true} {
			name := sig.String() + "/idle"
			if backpressure {
				name = sig.String() + "/stdout full"
			}
			t.Run(name, func(t *testing.T) {
				h := startStdioHubSession(t)
				if backpressure {
					h.fillStdout(t)
				}
				start := time.Now()
				if err := h.cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				select {
				case <-h.exited:
				case <-time.After(hubSignalReleaseBound):
					_, statErr := os.Stat(h.claim)
					t.Fatalf("hub session still running %s after %s with its pipes open (claim stat: %v)", hubSignalReleaseBound, sig, statErr)
				}
				if status, ok := h.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					t.Fatalf("hub session was killed by %s instead of ending its session", status.Signal())
				}
				if _, err := os.Stat(h.claim); !os.IsNotExist(err) {
					t.Fatalf("serve claim survived %s: %v\nstderr: %s", sig, err, h.stderr.String())
				}
				t.Logf("released %s after %s", time.Since(start).Round(time.Millisecond), sig)
			})
		}
	}
}

type stdioHubSession struct {
	cmd    *exec.Cmd
	stdin  interface{ Close() error }
	stdout interface{ Close() error }
	writer *hubwire.Writer
	reader *hubwire.Reader
	stderr *strings.Builder
	exited chan struct{}
	claim  string
	nextID int64
}

// startStdioHubSession runs the real `hub session` command in a child process
// over its own stdin and stdout (TestHubSessionStdioHelper), says hello and
// acquires the lease.
func startStdioHubSession(t *testing.T) *stdioHubSession {
	t.Helper()
	pool, cfg := newHubPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHubSessionStdioHelper$")
	cmd.Env = append(hubHelperBaseEnv(),
		"ACTEST_HUB_SESSION_HELPER=1",
		"ACTEST_HUB_SESSION_HELPER_POOL="+pool,
		"SSH_ORIGINAL_COMMAND=agentchute-hub",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &stdioHubSession{cmd: cmd, stdin: stdin, stdout: stdout, writer: hubwire.NewWriter(stdin), reader: hubwire.NewReader(stdout), stderr: &strings.Builder{}, exited: make(chan struct{}), claim: filepath.Join(cfg.AgentStateDir("codex"), "serve.claim"), nextID: 3}
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		_ = stdout.Close()
		<-h.exited
	})
	if err := h.writer.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: 1, MinV: 1, Agent: "codex", Bin: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if raw, err := h.reader.Read(); err != nil || raw.T != "hello-ok" {
		t.Fatalf("hello = %s, %v\nstderr: %s", raw.T, err, h.stderr.String())
	}
	if err := h.writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	if raw, err := h.reader.Read(); err != nil || raw.T != "lease-ok" {
		t.Fatalf("lease = %s, %v\nstderr: %s", raw.T, err, h.stderr.String())
	}
	if _, err := os.Stat(h.claim); err != nil {
		t.Fatalf("no serve claim after lease-ok: %v", err)
	}
	return h
}

// fillStdout sends ticks without reading a single reply until the session is
// stuck writing one: stdout's pipe is full and the session has stopped
// reading, so the tick sender stalls too.
func (h *stdioHubSession) fillStdout(t *testing.T) {
	t.Helper()
	var sent atomic.Int64
	go func() {
		for id := h.nextID; ; id++ {
			if h.writer.Write(hubwire.Tick{RequestBase: hubwire.RequestBase{T: "tick", ID: id}}, nil) != nil {
				return
			}
			sent.Add(1)
		}
	}()
	last, since := int64(-1), time.Now()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		n := sent.Load()
		if n != last {
			last, since = n, time.Now()
		}
		if n > 100 && time.Since(since) > 200*time.Millisecond {
			return
		}
	}
	t.Fatalf("ticks never stalled (sent %d): the session's stdout did not fill", sent.Load())
}
