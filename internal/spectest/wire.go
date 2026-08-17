package spectest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

// SessionFactory is the transport-neutral W harness contract. M3 supplies a
// net.Pipe factory; M6 supplies an SSH factory. The same assertions use only
// open/forced-disconnect/close and the pool-state probe below.
type SessionFactory interface {
	Open(context.Context, string) (Session, error)
	State() StateProbe
}

type Session interface {
	io.ReadWriteCloser
	ForceDisconnect() error
	Wait(context.Context) error
}

type StateProbe interface {
	Deliver(from, to, body string) error
	InboxCount(agent string) (int, error)
	ClaimedCount(agent string) (int, error)
	ReplaceLeaseToken(agent, token string) error
	StageUnreadableClaim(agent string) (restore func() error, err error)
}

type FactoryBuilder func(*testing.T) SessionFactory

func AssertWireVectors(t *testing.T, vectors []Vector, build FactoryBuilder) {
	t.Helper()
	for _, vector := range vectors {
		vector := vector
		t.Run(vector.ID+"/"+vector.Kind, func(t *testing.T) {
			factory := build(t)
			switch vector.ID {
			case "W1":
				assertW1(t, factory)
			case "W2":
				assertW2(t, factory)
			case "W3":
				assertW3(t, factory)
			case "W4":
				assertHandshakeError(t, factory, "grok", 1, hubwire.CodeIdentity)
			case "W5":
				assertHandshakeError(t, factory, "codex", 2, hubwire.CodeVersion)
			case "W6":
				assertW6(t, factory)
			default:
				t.Fatalf("unknown wire vector %s", vector.ID)
			}
		})
	}
}

func openHello(t *testing.T, factory SessionFactory, pinned, acting string, minV int) (Session, *hubwire.Reader, *hubwire.Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	session, err := factory.Open(ctx, pinned)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	r := hubwire.NewReader(session)
	w := hubwire.NewWriter(session)
	if err := w.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: max(1, minV), MinV: minV, Agent: acting, Bin: "spectest"}, nil); err != nil {
		t.Fatal(err)
	}
	return session, r, w
}

func requireHelloOK(t *testing.T, r *hubwire.Reader) {
	t.Helper()
	raw, err := r.Read()
	if err != nil || raw.T != "hello-ok" {
		t.Fatalf("hello = %s, %v", raw.T, err)
	}
}

func assertW1(t *testing.T, f SessionFactory) {
	if err := f.State().Deliver("grok", "codex", "claimed"); err != nil {
		t.Fatal(err)
	}
	s, r, w := openHello(t, f, "codex", "codex", 1)
	requireHelloOK(t, r)
	if err := w.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := r.Read()
	if err != nil || raw.T != "msg" {
		t.Fatalf("first check frame = %s, %v", raw.T, err)
	}
	if err := s.ForceDisconnect(); err != nil {
		t.Fatal(err)
	}
	waitSession(t, s)
	if n, err := f.State().ClaimedCount("codex"); err != nil || n != 1 {
		t.Fatalf("claimed residue = %d, %v", n, err)
	}

	_, r2, w2 := openHello(t, f, "codex", "codex", 1)
	requireHelloOK(t, r2)
	if err := w2.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	msg, err := r2.Read()
	if err != nil {
		t.Fatal(err)
	}
	var decoded hubwire.Message
	if err := msg.Decode(&decoded); err != nil || !decoded.Redelivered {
		t.Fatalf("redelivery = %+v, %v", decoded, err)
	}
	for {
		raw, err := r2.Read()
		if err != nil {
			t.Fatal(err)
		}
		if raw.T != "check-ok" {
			continue
		}
		var summary hubwire.CheckOK
		if err := raw.Decode(&summary); err != nil || summary.Redelivered != 1 {
			t.Fatalf("redelivery summary = %+v, %v", summary, err)
		}
		break
	}
}

func assertW2(t *testing.T, f SessionFactory) {
	s, r, w := openHello(t, f, "codex", "codex", 1)
	requireHelloOK(t, r)
	if err := w.Write(hubwire.Send{RequestBase: hubwire.RequestBase{T: "send", ID: 2}, To: "grok"}, loop.ComposeMessage("codex", "", "once")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := f.State().InboxCount("grok")
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("send never committed")
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.ForceDisconnect(); err != nil {
		t.Fatal(err)
	}
	waitSession(t, s)
	if n, err := f.State().InboxCount("grok"); err != nil || n != 1 {
		t.Fatalf("delivery count = %d, %v", n, err)
	}
}

func assertW3(t *testing.T, f SessionFactory) {
	channel, cr, cw := openHello(t, f, "codex", "codex", 1)
	requireHelloOK(t, cr)
	if err := cw.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := cr.Read()
	if err != nil {
		t.Fatal(err)
	}
	var lease hubwire.LeaseOK
	if err := raw.Decode(&lease); err != nil {
		t.Fatal(err)
	}
	if err := f.State().ReplaceLeaseToken("codex", "successor"); err != nil {
		t.Fatal(err)
	}

	_, r, w := openHello(t, f, "codex", "codex", 1)
	requireHelloOK(t, r)
	if err := w.Write(hubwire.Send{RequestBase: hubwire.RequestBase{T: "send", ID: 2}, To: "grok", ServeToken: lease.Token}, loop.ComposeMessage("codex", "", "fenced")); err != nil {
		t.Fatal(err)
	}
	raw, err = r.Read()
	if err != nil {
		t.Fatal(err)
	}
	var frame hubwire.Error
	if err := raw.Decode(&frame); err != nil || frame.Code != "E_FENCED" {
		t.Fatalf("send error = %+v, %v", frame, err)
	}
	if n, err := f.State().InboxCount("grok"); err != nil || n != 0 {
		t.Fatalf("delivery count = %d, %v", n, err)
	}
	_ = channel.ForceDisconnect()
}

func assertHandshakeError(t *testing.T, f SessionFactory, acting string, minV int, want string) {
	s, r, _ := openHello(t, f, "codex", acting, minV)
	raw, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	var frame hubwire.Error
	if err := raw.Decode(&frame); err != nil || frame.Code != want {
		t.Fatalf("handshake error = %+v, %v", frame, err)
	}
	waitSession(t, s)
}

func assertW6(t *testing.T, f SessionFactory) {
	if err := f.State().Deliver("grok", "codex", "held"); err != nil {
		t.Fatal(err)
	}
	restore, err := f.State().StageUnreadableClaim("codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restore() })
	_, r, w := openHello(t, f, "codex", "codex", 1)
	requireHelloOK(t, r)
	if err := w.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	var frame hubwire.Error
	if err := raw.Decode(&frame); err != nil || raw.T != "error" || frame.Code != "E_HUB_IO" || !frame.ClaimedHeld {
		t.Fatalf("error = %+v, %v", frame, err)
	}
}

func waitSession(t *testing.T, s Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil && err != io.EOF && !isClosed(err) {
		t.Fatalf("wait session: %v", err)
	}
}

func isClosed(err error) bool {
	return err != nil && (err == io.ErrClosedPipe || contains(err.Error(), "closed"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
