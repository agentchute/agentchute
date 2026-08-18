//go:build sshd_integration

package sshd

import (
	"context"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
)

// The harness's own session transport must not lose what the hub already wrote.
//
// It used to. open() took cmd.StdoutPipe() and reaped with cmd.Wait() from a
// goroutine started at Start — the same pattern, in the same shape, that red #3
// turned out to be in the production transport. Wait closes the pipes IT created
// as soon as the child exits, so a frame the hub had already written could still
// be sitting undrained in the OS pipe buffer when ssh exited normally, and the
// next read failed with "file already closed".
//
// That is worse here than in production. A measuring instrument that
// occasionally reports "channel to the hub was lost" for a session in which
// nothing went wrong manufactures exactly the false signature that cost this
// milestone two rounds of diagnosis. It surfaced when the suite went under
// -race: slower scheduling was all it took, on a macOS runner.
//
// The row makes the reader late deliberately rather than racing for it: stdin is
// closed so ssh exits, the process is REAPED, and only then is the first byte
// read. Pre-fix that lost the frame 10 times out of 10 with the exact CI string
// (`truncated control frame: read |0: file already closed`); post-fix, 0.
//
// The frame's TYPE is deliberately not asserted. What is under test is that a
// frame written before the child exited is still readable after it is reaped —
// coupling this row to the handshake would make it fail for reasons that have
// nothing to do with the pipe.
func TestHarnessSessionDeliversFramesWrittenBeforeExit(t *testing.T) {
	requireSSHDTest(t)
	h := newSSHDHarness(t)
	const iterations = 10

	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		raw, err := h.open(ctx, "codex", "agentchute-hub", hubclient.SSHBuildOptions{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		writer := hubwire.NewWriter(raw)
		hello := hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, V: hubwire.Version, MinV: hubwire.Version, Agent: "codex", Bin: "sshd-integration"}
		if err := writer.Write(hello, nil); err != nil {
			cancel()
			t.Fatal(err)
		}
		// End the session and wait for the reap. The hub's answer is already
		// written; from here on the only question is whether it survives.
		_ = raw.stdin.Close()
		<-raw.done

		time.Sleep(50 * time.Millisecond)

		if _, err := hubwire.NewReader(raw).Read(); err != nil {
			cancel()
			t.Fatalf("iteration %d: the hub's answer was lost after ssh exited: %v", i, err)
		}
		_ = raw.Close()
		cancel()
	}
}
