package cli

import (
	"net"
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
	paused   bool
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
	t.paused = true
	<-t.released // the cancel lands here, mid-frame
	rest, err := t.Conn.Write(p[t.prefix:])
	return n + rest, err
}

func (t *halfWriteTransport) release() { t.once.Do(func() { close(t.released) }) }

// wrotePrefixBeforeRelease reports whether the transport actually split a frame.
// Without it, "the frame arrived whole" is also what a transport that never
// paused would produce — the row would pass for the wrong reason.
func (t *halfWriteTransport) wrotePrefixBeforeRelease() bool { return t.paused }

// A signal arriving while a hub session is mid-write must let the frame FINISH,
// not cut it in half.
//
// serveHubSession runs a goroutine that closes the transport on ctx.Done, and
// ctx is signal.NotifyContext(SIGTERM, SIGHUP) in production (hub.go:49).
// Nothing used to coordinate that close with a write in progress, so a signal
// delivered between the first and last byte of a control line cut the line in
// half; the peer received a partial JSON object with no newline, which
// hubwire's reader reports as a truncated control frame (codec.go:47).
//
// That is not a hypothetical teardown. sshd sends SIGHUP to a forced-command
// process when its channel goes away, so an ordinary disconnect at the wrong
// microsecond produced a CORRUPT frame rather than a clean end — "the hub looks
// broken" for a hub that is fine, and the peer cannot tell that from a protocol
// violation by the other side.
//
// This row was written to CHARACTERISE the truncation (de4da74) and is flipped
// here, in the same commit as the fix, because it is the only thing covering
// the behaviour — splitting them would leave a window with no coverage at all.
//
// The timing is constructed, not raced for: net.Pipe is synchronous, so a
// partial write only lands if the peer is already reading. The reader starts
// FIRST, the transport pauses after a fixed prefix, the context is cancelled
// while it is paused, and only then is the write allowed to continue.
func TestHubSessionSignalDuringWriteLetsTheFrameFinish(t *testing.T) {
	pool, _ := newHubPool(t)
	var transport *halfWriteTransport
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, func(c net.Conn) hubSessionTransport {
		transport = &halfWriteTransport{Conn: c, prefix: 12, released: make(chan struct{})}
		return transport
	}, nil)

	helloHub(t, s, "codex", 1)

	type result struct {
		raw hubwire.RawFrame
		err error
	}
	got := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		raw, err := s.reader.Read()
		got <- result{raw: raw, err: err}
	}()
	<-started

	transport.armed = true
	if err := s.writer.Write(hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}

	// Cancel while the hub's reply is held mid-frame, then release it well
	// inside the grace. The close must wait for the write it interrupted.
	time.Sleep(200 * time.Millisecond)
	s.cancel()
	time.Sleep(50 * time.Millisecond)
	transport.release()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the frame was still cut short: %v", r.err)
		}
		// Proof the write was genuinely IN FLIGHT rather than never started:
		// the prefix had already been delivered before the cancel, so a whole
		// frame here means the remainder was allowed to follow it.
		if !transport.wrotePrefixBeforeRelease() {
			t.Fatal("the transport never paused mid-frame, so this row proved nothing")
		}
		if r.raw.T != "error" && r.raw.T != "status-ok" {
			t.Fatalf("frame type = %q, want the hub's reply", r.raw.T)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader never returned")
	}
}

// The other half of the same design, and the hazard the fix had to avoid: the
// grace is BOUNDED. Those closers are the unwedging mechanism, so a write that
// never finishes must not hold a SIGTERM'd session open forever.
//
// A mutex here would do exactly that, and the deadline that would bound it is
// not available: stdioHubTransport wraps inherited os.Stdin/os.Stdout, which
// are typically not pollable, so SetWriteDeadline returns ErrNoDeadline.
func TestHubSessionCloseGraceIsBounded(t *testing.T) {
	pool, _ := newHubPool(t)
	var transport *halfWriteTransport
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, func(c net.Conn) hubSessionTransport {
		transport = &halfWriteTransport{Conn: c, prefix: 12, released: make(chan struct{})}
		return transport
	}, nil)
	t.Cleanup(transport.release)

	helloHub(t, s, "codex", 1)

	got := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := s.reader.Read()
		got <- err
	}()
	<-started

	transport.armed = true
	if err := s.writer.Write(hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Cancel and NEVER release: the write stays in flight for good.
	cancelled := time.Now()
	s.cancel()

	select {
	case <-got:
		waited := time.Since(cancelled)
		// It must not wait meaningfully longer than the grace. Generous slack,
		// because the assertion is "bounded", not "precisely 250ms".
		if waited > 3*time.Second {
			t.Fatalf("the close waited %v for a write that never finished; the grace is not bounded", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck write held the session open past every bound — the unwedge is gone")
	}
}
