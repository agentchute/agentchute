package spectest

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
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
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	ForceDisconnect() error
	Wait(context.Context) error
}

// AssertWireClientVectors drives the three vectors whose invariant terminates
// on the client side. The factory is the same transport-neutral contract used
// by AssertWireVectors, so M6 can rerun these assertions over sshd unchanged.
func AssertWireClientVectors(t *testing.T, vectors []Vector, build FactoryBuilder) {
	t.Helper()
	for _, vector := range vectors {
		vector := vector
		if vector.ID != "W1" && vector.ID != "W2" && vector.ID != "W6" {
			continue
		}
		t.Run(vector.ID+"/client", func(t *testing.T) {
			factory := build(t)
			switch vector.ID {
			case "W1":
				assertW1Client(t, factory)
			case "W2":
				assertW2Client(t, factory)
			case "W6":
				assertW6Client(t, factory)
			}
		})
	}
}

func openClient(t *testing.T, factory SessionFactory, agent string) (*hubclient.OneShot, Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	transport, err := factory.Open(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	remote := &loop.RemoteConfig{Host: "in-process", Port: 22}
	client, err := hubclient.OpenOneShotTransport(transport, remote, agent, "spectest")
	if err != nil {
		t.Fatal(err)
	}
	return client, transport
}

func assertW1Client(t *testing.T, f SessionFactory) {
	if err := f.State().Deliver("grok", "codex", "claimed"); err != nil {
		t.Fatal(err)
	}
	client, transport := openClient(t, f, "codex")
	_, err := client.Check(op.ClaimReq{}, func(ev op.Event) error {
		if ev.Message != nil {
			return transport.ForceDisconnect()
		}
		return nil
	})
	if err == nil {
		t.Fatal("disconnect after first message returned nil error")
	}
	waitSession(t, transport)
	if n, err := f.State().ClaimedCount("codex"); err != nil || n != 1 {
		t.Fatalf("claimed residue = %d, %v", n, err)
	}
	client2, _ := openClient(t, f, "codex")
	redelivered := false
	sum, err := client2.Check(op.ClaimReq{}, func(ev op.Event) error {
		if ev.Message != nil {
			redelivered = ev.Message.Redelivered
		}
		return nil
	})
	if err != nil || !redelivered || sum.Redelivered != 1 {
		t.Fatalf("redelivery = %v, summary=%+v, err=%v", redelivered, sum, err)
	}
}

type disconnectAfterCommit struct {
	Session
	state StateProbe
	agent string
	reads int
}

func (d *disconnectAfterCommit) Read(p []byte) (int, error) {
	d.reads++
	if d.reads == 2 {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if count, countErr := d.state.InboxCount(d.agent); countErr == nil && count == 1 {
				_ = d.ForceDisconnect()
				return d.Session.Read(p)
			}
			time.Sleep(time.Millisecond)
		}
		return 0, fmt.Errorf("send did not commit before disconnect deadline")
	}
	return d.Session.Read(p)
}

func assertW2Client(t *testing.T, f SessionFactory) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := f.Open(ctx, "codex")
	if err != nil {
		t.Fatal(err)
	}
	transport := &disconnectAfterCommit{Session: raw, state: f.State(), agent: "grok"}
	client, err := hubclient.OpenOneShotTransport(transport, &loop.RemoteConfig{Host: "in-process", Port: 22}, "codex", "spectest")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Send(op.SendReq{To: "grok", Content: loop.ComposeMessage("codex", "", "once")})
	if hubclient.ErrorCode(err) != "E_SEND_UNKNOWN" {
		t.Fatalf("send error = %v (%s), want E_SEND_UNKNOWN", err, hubclient.ErrorCode(err))
	}
	waitSession(t, raw)
	if n, err := f.State().InboxCount("grok"); err != nil || n != 1 {
		t.Fatalf("delivery count = %d, %v", n, err)
	}
}

func assertW6Client(t *testing.T, f SessionFactory) {
	if err := f.State().Deliver("grok", "codex", "held"); err != nil {
		t.Fatal(err)
	}
	restore, err := f.State().StageUnreadableClaim("codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restore() })
	client, transport := openClient(t, f, "codex")
	_, err = client.Check(op.ClaimReq{}, func(op.Event) error { return nil })
	if hubclient.ErrorCode(err) != "E_HUB_IO" || !hubclient.ClaimedHeld(err) {
		t.Fatalf("check error = %v code=%s claimed_held=%v", err, hubclient.ErrorCode(err), hubclient.ClaimedHeld(err))
	}
	waitSession(t, transport)
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
	waitSession(t, channel)
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
