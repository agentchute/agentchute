package hubclient

import (
	"io"
	"os/exec"
	"testing"
	"time"
)

// ROOT CAUSE of M6 red #3, pinned as a property of the pattern processTransport
// uses rather than as a story about it.
//
// startSSH does this (ssh.go:259):
//
//	go func() { p.waitCh <- cmd.Wait() }()
//
// with p.stdout coming from cmd.StdoutPipe(). Go's own documentation on
// StdoutPipe is explicit: "Wait will close the pipe after seeing the command
// exit... it is thus incorrect to call Wait before all reads from the pipe have
// completed." We call it immediately, in a goroutine, at start.
//
// So when ssh finishes — normally, exit 0, nothing wrong — Wait returns and
// closes the read end while bytes the remote already wrote may still be sitting
// in the OS pipe buffer undrained. The next read fails with "file already
// closed", hubwire reports that as "truncated control frame" (codec.go:47), and
// classifySSHFailure falls through to E_CHANNEL_LOST because waitErr is nil and
// stderr is empty — ssh really did exit cleanly and say nothing.
//
// That is exactly the observed failure, in all of its parts:
//   - every streamed frame arrives (read while ssh is still running)
//   - the TERMINAL frame is lost (it is the last write, so it is the one most
//     likely to still be in the buffer when ssh exits)
//   - ssh exit status 0, stderr empty
//   - scheduling-dependent, so ubuntu runners hit it and macOS does not
//
// The row loops because the race needs the reader to be late; a single iteration
// that happened to win would prove nothing, and a test that reports success
// while demonstrating nothing is the defect class this milestone keeps finding.
func TestWaitBeforeDrainLosesBufferedOutput(t *testing.T) {
	const want = 2000
	const iterations = 20

	lost := 0
	var firstErr error
	for i := 0; i < iterations; i++ {
		// A child that writes and exits immediately, successfully. No failure is
		// being injected anywhere: this is the healthy case.
		// No shell: nothing here is user input, and a direct exec keeps it that way.
		cmd := exec.Command("head", "-c", "2000", "/dev/zero")
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitCh := make(chan error, 1)
		go func() { waitCh <- cmd.Wait() }() // processTransport's exact pattern

		// The reader is busy elsewhere — printing a streamed frame, say.
		time.Sleep(20 * time.Millisecond)

		data, readErr := io.ReadAll(out)
		waitErr := <-waitCh

		if len(data) != want || readErr != nil {
			lost++
			if firstErr == nil {
				firstErr = readErr
				t.Logf("iteration %d: read %d of %d bytes, read error = %v, ssh-equivalent wait error = %v",
					i, len(data), want, readErr, waitErr)
			}
			// The child exited CLEANLY. That is the part that makes this
			// undiagnosable downstream: there is no error anywhere except on the
			// read, and the read's error gets flattened into a generic sentence.
			if waitErr != nil {
				t.Fatalf("expected a clean child exit, got %v — this row is about the healthy case", waitErr)
			}
		}
	}

	if lost == 0 {
		t.Fatalf("no output was lost in %d iterations; either the pattern was fixed (good — delete this row) "+
			"or the reader was never late enough to expose it (bad — the row proves nothing as written)", iterations)
	}
	t.Logf("output lost or truncated in %d of %d iterations; first read error: %v", lost, iterations, firstErr)
}
