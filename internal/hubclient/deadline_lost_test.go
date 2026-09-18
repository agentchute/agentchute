package hubclient

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// deadline_lost_test.go pins the one path in the one-shot client that returned
// a RAW transport error with no hub code: a deadline that cannot be set.
//
// Both OpenOneShotTransport and do() set a deadline, write, set the read
// deadline, read. Every write and read failure goes through
// classifySSHFailure and comes back as a coded *Error; the two deadline-set
// calls around them did not. net.Pipe's SetReadDeadline returns io.ErrClosedPipe
// once the far end is closed, so a hub that closes right after consuming the
// request — exactly what result_unknown_wired_test.go's server does — made
// ErrorCode() come back "" whenever the close landed before the client's
// SetReadDeadline call. That is the race behind the "drops before anything is
// streamed" CI failures on main (fc83163, run 32429551014) and on #206.
//
// The rows here remove the timing: the hook fires INSIDE the deadline call, so
// the close is guaranteed to land first. A deadline the transport can no
// longer take is a lost channel, and it must be reported as one — with the
// same code the neighbouring read failure gets, and with transmitted/streamed
// semantics untouched.

// deadlineTransport wraps a net.Pipe end and lets a row decide what happens at
// each deadline call: run a hook before delegating, or fail outright.
type deadlineTransport struct {
	net.Conn
	beforeReadDeadline func()
	afterReadDeadline  func()
	writeDeadlineErr   error
}

func (d *deadlineTransport) SetReadDeadline(t time.Time) error {
	if d.beforeReadDeadline != nil {
		d.beforeReadDeadline()
	}
	err := d.Conn.SetReadDeadline(t)
	if d.afterReadDeadline != nil {
		d.afterReadDeadline()
	}
	return err
}

func (d *deadlineTransport) SetWriteDeadline(t time.Time) error {
	if d.writeDeadlineErr != nil {
		return d.writeDeadlineErr
	}
	return d.Conn.SetWriteDeadline(t)
}

func testRemote(t *testing.T) *loop.RemoteConfig {
	t.Helper()
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

// serveOneFrameThenClose reads exactly one frame from server and closes it
// when told to. The returned func blocks until the frame was consumed, then
// closes — so a row can pin the close on either side of the client's deadline
// call.
func serveOneFrameThenClose(server net.Conn) (closeNow func()) {
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		_, _ = hubwire.NewReader(server).Read()
	}()
	return func() {
		<-consumed
		_ = server.Close()
	}
}

func TestDoClassifiesADeadlineItCannotSetAsChannelLost(t *testing.T) {
	remote := testRemote(t)

	t.Run("far end closes after the request is consumed, before the read deadline is set", func(t *testing.T) {
		// Reference: the same drop landing AFTER the deadline call takes the
		// read path, which classifies. The deadline path must match it.
		ref := deadlineRowReference(t, remote)

		client, server := net.Pipe()
		closeNow := serveOneFrameThenClose(server)
		transport := &deadlineTransport{Conn: client, beforeReadDeadline: closeNow}
		s := &OneShot{transport: transport, reader: hubwire.NewReader(transport), remote: remote, agentID: "codex", nextID: 1}

		emitted := 0
		_, transmitted, err := s.do(
			hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 1}},
			nil,
			func(op.Event) error { emitted++; return nil },
			false,
			"check-ok",
		)
		if err == nil {
			t.Fatal("a channel closed before the read deadline could be set reported success")
		}
		if got := ErrorCode(err); got != "E_CHANNEL_LOST" || got != ref {
			t.Fatalf("code = %q, want E_CHANNEL_LOST (the read-path reference gave %q):\n%v", got, ref, err)
		}
		var e *Error
		if !errors.As(err, &e) || !e.Retriable {
			t.Fatalf("want a retriable hub error (nothing was streamed, so re-run is the right advice); got %#v", err)
		}
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("the transport's own error was dropped rather than carried as Cause: %v", err)
		}
		if !transmitted {
			t.Fatal("transmitted = false, but the request was fully written before the close")
		}
		if emitted != 0 {
			t.Fatalf("emitted %d events with nothing streamed", emitted)
		}
	})

	t.Run("write deadline cannot be set", func(t *testing.T) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		transport := &deadlineTransport{Conn: client, writeDeadlineErr: net.ErrClosed}
		s := &OneShot{transport: transport, reader: hubwire.NewReader(transport), remote: remote, agentID: "codex", nextID: 1}

		_, transmitted, err := s.do(
			hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 1}},
			nil,
			func(op.Event) error { return nil },
			false,
			"check-ok",
		)
		if got := ErrorCode(err); got != "E_CHANNEL_LOST" {
			t.Fatalf("code = %q, want E_CHANNEL_LOST:\n%v", got, err)
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("the transport's own error was dropped rather than carried as Cause: %v", err)
		}
		if transmitted {
			t.Fatal("transmitted = true, but nothing was ever written")
		}
	})
}

// deadlineRowReference runs the neighbouring path — same request, same drop,
// but the close lands AFTER the read deadline is set, so the read fails and is
// classified — and returns its code. It is the value the deadline path must
// agree with, computed rather than assumed.
func deadlineRowReference(t *testing.T, remote *loop.RemoteConfig) string {
	t.Helper()
	client, server := net.Pipe()
	closeNow := serveOneFrameThenClose(server)
	transport := &deadlineTransport{Conn: client, afterReadDeadline: closeNow}
	s := &OneShot{transport: transport, reader: hubwire.NewReader(transport), remote: remote, agentID: "codex", nextID: 1}
	_, _, err := s.do(
		hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 1}},
		nil,
		func(op.Event) error { return nil },
		false,
		"check-ok",
	)
	code := ErrorCode(err)
	if code == "" {
		t.Fatalf("reference read-path drop is itself unclassified: %v", err)
	}
	return code
}

func TestOpenOneShotClassifiesADeadlineItCannotSetLikeItsNeighbour(t *testing.T) {
	remote := testRemote(t)

	// Reference: hub closes after consuming hello, after the client's read
	// deadline is set — the read fails and takes the existing "connect"
	// classification.
	reference := func() string {
		client, server := net.Pipe()
		closeNow := serveOneFrameThenClose(server)
		transport := &deadlineTransport{Conn: client, afterReadDeadline: closeNow}
		_, err := OpenOneShotTransport(transport, remote, "codex", "test")
		code := ErrorCode(err)
		if code == "" {
			t.Fatalf("reference hello-phase drop is itself unclassified: %v", err)
		}
		return code
	}()

	t.Run("far end closes after hello is consumed, before the read deadline is set", func(t *testing.T) {
		client, server := net.Pipe()
		closeNow := serveOneFrameThenClose(server)
		transport := &deadlineTransport{Conn: client, beforeReadDeadline: closeNow}
		_, err := OpenOneShotTransport(transport, remote, "codex", "test")
		if err == nil {
			t.Fatal("a channel closed before the read deadline could be set reported a live session")
		}
		if got := ErrorCode(err); got != reference {
			t.Fatalf("code = %q, want the neighbouring read failure's %q:\n%v", got, reference, err)
		}
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("the transport's own error was dropped rather than carried as Cause: %v", err)
		}
	})

	t.Run("write deadline cannot be set", func(t *testing.T) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		transport := &deadlineTransport{Conn: client, writeDeadlineErr: net.ErrClosed}
		_, err := OpenOneShotTransport(transport, remote, "codex", "test")
		if got := ErrorCode(err); got != reference {
			t.Fatalf("code = %q, want the neighbouring hello-write failure's %q:\n%v", got, reference, err)
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("the transport's own error was dropped rather than carried as Cause: %v", err)
		}
	})
}
