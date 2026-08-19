package hubclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The probe asks a possibly-hostile host to run a command of our choosing and
// then reads whatever comes back for up to 15 seconds. These rows pin that the
// reading is bounded, and — the part that actually matters — that flooding
// cannot change the VERDICT.

func TestProbeStdoutIsNotBuffered(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"

	t.Run("a flood before the nonce does not hide it", func(t *testing.T) {
		// The attack the obvious fix enables. Keep-the-first-N-bytes would drop
		// the nonce here and report the host PINNED — letting the hostile party
		// choose its own verdict. Incremental scanning cannot be fooled this way.
		s := &nonceLineScanner{nonce: []byte(nonce)}
		writeChunked(t, s, strings.Repeat("x", 2<<20)+"\n"+nonce+"\n")
		if !s.found {
			t.Fatal("2 MiB of noise before the nonce hid it; a hostile host could report itself pinned")
		}
	})

	t.Run("nothing is retained", func(t *testing.T) {
		s := &nonceLineScanner{nonce: []byte(nonce)}
		writeChunked(t, s, strings.Repeat("y", 4<<20))
		// A single 4 MiB line with no newline is the shape that would defeat a
		// line-buffered reader: it must be dropped as it arrives, not accumulated
		// waiting for a terminator that never comes.
		if got := len(s.line); got > len(nonce)+1 {
			t.Fatalf("scanner retained %d bytes of a 4 MiB unterminated line, want at most %d", got, len(nonce)+1)
		}
		if s.found {
			t.Fatal("scanner matched a line that is not the nonce")
		}
	})

	t.Run("a near-miss line is not a match", func(t *testing.T) {
		for _, line := range []string{nonce + "x", "x" + nonce, " " + nonce, nonce[:len(nonce)-1]} {
			s := &nonceLineScanner{nonce: []byte(nonce)}
			writeChunked(t, s, line+"\n")
			if s.found {
				t.Fatalf("matched %q, which is not the nonce", line)
			}
		}
	})

	// Chunk boundaries are not line boundaries: ssh's output arrives in whatever
	// pieces the pipe hands over, and the nonce can straddle two of them.
	t.Run("the nonce split across writes still matches", func(t *testing.T) {
		s := &nonceLineScanner{nonce: []byte(nonce)}
		_, _ = s.Write([]byte(nonce[:7]))
		_, _ = s.Write([]byte(nonce[7:]))
		_, _ = s.Write([]byte("\r\n"))
		if !s.found {
			t.Fatal("the nonce arriving in three writes, CRLF-terminated, was not matched")
		}
	})
}

func TestProbeStderrKeepsTheTailAndNothingMore(t *testing.T) {
	w := &tailCapWriter{limit: probeStderrLimit}
	writeChunked(t, w, strings.Repeat("z", 2<<20))
	writeChunked(t, w, "\nalex@hub.example: Permission denied (publickey).\n")
	if got := len(w.String()); got > probeStderrLimit {
		t.Fatalf("stderr retained %d bytes, want at most %d", got, probeStderrLimit)
	}
	if cap(w.buf) > 2*probeStderrLimit {
		t.Fatalf("stderr backing array grew to %d bytes; the cap is on the slice, not the memory", cap(w.buf))
	}
	// The tail is kept BECAUSE of this: ssh says why it gave up last, and both
	// consumers of stderr read the end.
	if !strings.Contains(w.String(), "Permission denied (publickey") {
		t.Fatal("a flood pushed out ssh's own verdict, which is the only part of stderr anything reads")
	}
}

// End to end through runProbeCommand with a stand-in `ssh` that floods both
// streams and then echoes the nonce. Without the incremental scanner this row
// retains 4 MiB and reports the wrong verdict.
func TestRunProbeCommandSurvivesAFloodingHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in for ssh")
	}
	const nonce = "deadbeefdeadbeefdeadbeefdeadbeef"
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"awk 'BEGIN{s=sprintf(\"%2097152s\",\"\");print s}' >&2\n" +
		"awk 'BEGIN{s=sprintf(\"%2097152s\",\"\");print s}'\n" +
		"echo " + nonce + "\n" +
		"echo 'alex@hub.example: Permission denied (publickey).' >&2\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// runProbeCommand, not runProbeCommandForNonce: the production entry has to
	// recover the nonce from the invocation it is handed. A version that scanned
	// for the wrong nonce would report every host pinned, and calling the inner
	// function directly would let that through.
	stdout, stderr, err := runProbeCommand([]string{"hub.example", "echo " + nonce}, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !nonceEchoed(stdout, nonce) {
		t.Fatal("4 MiB of flooding hid the nonce; the host would have been reported pinned")
	}
	if len(stdout) > len(nonce)+2 {
		t.Fatalf("stdout carried %d bytes; it is a verdict, not a transcript", len(stdout))
	}
	if len(stderr) > probeStderrLimit {
		t.Fatalf("stderr carried %d bytes, want at most %d", len(stderr), probeStderrLimit)
	}
	if !strings.Contains(stderr, "Permission denied (publickey") {
		t.Fatalf("the flood pushed out the line the classifier reads:\n%s", stderr)
	}
}

// The nonce the runner scans for must be the one the invocation actually asked
// for, or the probe reports "pinned" for every host.
func TestProbeNonceIsRecoveredFromTheInvocation(t *testing.T) {
	opts := probeFixture(t)
	invocation, nonce, err := buildPinningProbe(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := nonceFromProbeArgs(invocation.Args); got != nonce {
		t.Fatalf("recovered nonce %q, want %q", got, nonce)
	}
	for _, args := range [][]string{nil, {"hub.example"}, {"hub.example", "agentchute-hub"}} {
		if got := nonceFromProbeArgs(args); got != "" {
			t.Fatalf("recovered a nonce %q from a non-probe invocation %v", got, args)
		}
	}
}

func writeChunked(t *testing.T, w interface{ Write([]byte) (int, error) }, s string) {
	t.Helper()
	const chunk = 64 << 10
	for len(s) > 0 {
		n := chunk
		if n > len(s) {
			n = len(s)
		}
		if _, err := w.Write([]byte(s[:n])); err != nil {
			t.Fatal(err)
		}
		s = s[n:]
	}
}
