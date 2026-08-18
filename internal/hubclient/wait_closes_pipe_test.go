package hubclient

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"
)

// M6 red #3, as a property of the transport rather than a story about it.
//
// The transport used to take cmd.StdoutPipe() and reap with cmd.Wait() from a
// goroutine started immediately. Go documents why that is wrong on StdoutPipe:
// "Wait will close the pipe after seeing the command exit... it is thus
// incorrect to call Wait before all reads from the pipe have completed." So the
// instant ssh exited — normally, exit 0, nothing wrong — Wait closed the read
// end while bytes the remote had already written were still sitting undrained in
// the OS pipe buffer. The next read failed with "file already closed", the wire
// reported that as a truncated control frame, and classifySSHFailure fell
// through to E_CHANNEL_LOST because ssh really had exited cleanly and said
// nothing.
//
// Every part of the observed failure follows from it: all streamed frames
// arrive (they are read while ssh is still running), the TERMINAL frame is the
// one lost (it is the last write, so it is the one most likely to still be
// buffered), exit status 0, empty stderr, and scheduling-dependent — so ubuntu
// runners hit it and macOS did not.
//
// The row drives the production constructor with a child that stands in for
// ssh, and the reader is deliberately LATE: it sleeps past the child's exit
// before reading a byte. It loops because the old code lost the race only
// sometimes, and a single iteration that happened to win would prove nothing.
func TestProcessTransportDeliversOutputWrittenBeforeExit(t *testing.T) {
	const want = 2000
	const iterations = 20

	for i := 0; i < iterations; i++ {
		// Writes and exits immediately, successfully. No failure is injected
		// anywhere: this is the healthy case, which is what made it undiagnosable.
		// No shell — nothing here is user input and a direct exec keeps it so.
		ctx, cancel := context.WithCancel(context.Background())
		p, err := startProcessTransport(exec.CommandContext(ctx, "head", "-c", "2000", "/dev/zero"), cancel)
		if err != nil {
			t.Fatal(err)
		}

		// The reader is busy elsewhere — printing a streamed frame, say — for
		// long enough that the child has exited and been reaped before the first
		// Read. Under the old code this is where the output went.
		time.Sleep(20 * time.Millisecond)

		data, readErr := io.ReadAll(p)
		closeErr := p.Close()

		if readErr != nil {
			t.Fatalf("iteration %d: read failed after the child exited cleanly: %v", i, readErr)
		}
		if len(data) != want {
			t.Fatalf("iteration %d: read %d of %d bytes — output written before exit was lost, which is red #3", i, len(data), want)
		}
		// The child exited CLEANLY. That is what made the loss undiagnosable
		// downstream: there was no error anywhere except on the read.
		if closeErr != nil {
			t.Fatalf("iteration %d: expected a clean child exit, got %v — this row is about the healthy case", i, closeErr)
		}
	}
}

// Close must be the only thing that closes the read end, and it must be safe to
// call more than once — classifySSHFailure closes, and so does every caller.
func TestProcessTransportCloseIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := startProcessTransport(exec.CommandContext(ctx, "head", "-c", "16", "/dev/zero"), cancel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(p); err != nil {
		t.Fatal(err)
	}
	first := p.Close()
	if second := p.Close(); second != first {
		t.Fatalf("second Close = %v, first = %v", second, first)
	}
	if _, err := p.Read(make([]byte, 1)); err == nil {
		t.Fatal("read succeeded after Close; the read end was never closed, so the fd leaks")
	}
}
