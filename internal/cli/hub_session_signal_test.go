package cli

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
)

// halfWriteTransport writes the first n bytes of the frame the test is watching
// for, then blocks until released. It stands for a hub whose write is in flight
// when something asynchronous happens — a scheduler pause on a loaded machine,
// a slow pipe, anything at all. The point is only that a write is PARTWAY DONE.
type halfWriteTransport struct {
	net.Conn
	prefix   int
	armed    bool
	released chan struct{}
	once     sync.Once
}

func (t *halfWriteTransport) Write(p []byte) (int, error) {
	if !t.armed || len(p) <= t.prefix {
		return t.Conn.Write(p)
	}
	t.armed = false
	n, err := t.Conn.Write(p[:t.prefix])
	if err != nil {
		return n, err
	}
	<-t.released // the cancel lands here, mid-frame
	rest, err := t.Conn.Write(p[t.prefix:])
	return n + rest, err
}

func (t *halfWriteTransport) release() { t.once.Do(func() { close(t.released) }) }

// A signal arriving while a hub session is mid-write truncates the frame instead
// of finishing it, and the client reports "truncated control frame".
//
// serveHubSession runs a goroutine that closes the transport on ctx.Done, and ctx
// is signal.NotifyContext(SIGTERM, SIGHUP) in production (hub.go:49). Nothing
// coordinates that close with a write in progress, so a signal delivered between
// the first and last byte of a control line cuts the line in half. The peer has
// already received a partial JSON object with no newline, which is exactly what
// hubwire's reader calls a truncated control frame (codec.go:47).
//
// This is not a hypothetical teardown. sshd sends SIGHUP to a forced-command
// process when its channel goes away, so an ordinary disconnect at the wrong
// microsecond produces a CORRUPT frame rather than a clean end — and the peer
// cannot tell that from a protocol violation by the other side.
//
// The row constructs the timing deterministically rather than racing for it: the
// transport is paused after a fixed prefix, the context is cancelled while it is
// paused, and only then is the write allowed to continue.
func TestHubSessionSignalDuringWriteTruncatesTheFrame(t *testing.T) {
	pool, _ := newHubPool(t)
	var transport *halfWriteTransport
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, func(c net.Conn) hubSessionTransport {
		transport = &halfWriteTransport{Conn: c, prefix: 12, released: make(chan struct{})}
		return transport
	}, nil)

	helloHub(t, s, "codex", 1)

	// net.Pipe is synchronous, so the partial write only lands if the peer is
	// already reading. Start the read FIRST: it consumes the prefix, then blocks
	// waiting for a newline that is never coming — which is precisely the state
	// this row is about.
	type result struct{ err error }
	got := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := s.reader.Read()
		got <- result{err: err}
	}()
	<-started

	transport.armed = true
	if err := s.writer.Write(hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}

	// Cancel while the write is held mid-frame, then let it resume into a
	// transport the ctx goroutine has already closed underneath it.
	time.Sleep(200 * time.Millisecond)
	s.cancel()
	time.Sleep(50 * time.Millisecond)
	transport.release()

	select {
	case r := <-got:
		if r.err == nil {
			t.Fatal("expected the frame to be cut short, got a complete one")
		}
		if errors.Is(r.err, io.EOF) {
			t.Fatalf("got a clean EOF, not a truncated frame — the write was not actually in flight: %v", r.err)
		}
		if !strings.Contains(r.err.Error(), "truncated control frame") {
			t.Fatalf("error = %v, want the truncated-control-frame report that red #3 produces on ubuntu", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader never returned")
	}
}
