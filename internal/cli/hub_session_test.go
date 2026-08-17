package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

const fixturePoolID = "0123456789ab"

type runningHubSession struct {
	conn   net.Conn
	reader *hubwire.Reader
	writer *hubwire.Writer
	done   <-chan error
	cancel context.CancelFunc
}

func newHubPool(t *testing.T) (string, *loop.Config) {
	t.Helper()
	pool := t.TempDir()
	if err := os.WriteFile(filepath.Join(pool, "AGENTCHUTE.md"), []byte("# spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loopDir := filepath.Join(pool, ".agentchute", "loop")
	if err := os.MkdirAll(filepath.Join(loopDir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixturePoolID(t, pool)
	return pool, &loop.Config{ControlRepo: pool, LoopDir: loopDir, Vendor: "agentchute"}
}

// writeFixturePoolID is the sole M3 fixture writer. Production does not mint
// pool.id until M5.
func writeFixturePoolID(t *testing.T, poolDir string) string {
	t.Helper()
	path := filepath.Join(poolDir, ".agentchute", "loop", "state", "pool.id")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, fixturePoolID+"\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return fixturePoolID
}

func enrollHubAgent(t *testing.T, cfg *loop.Config, id string) {
	t.Helper()
	vendor := "test"
	if _, err := op.Register(cfg, op.Context{ActorID: id}, op.RegisterReq{Vendor: &vendor, Host: "laptop"}, time.Now().UTC()); err != nil {
		t.Fatalf("enroll %s: %v", id, err)
	}
}

func deliverHubMessage(t *testing.T, cfg *loop.Config, from, to, body string) loop.TsID {
	t.Helper()
	id, _, err := loop.SendTsMessageWithCommit(cfg, from, to, loop.ComposeMessage(from, "", body), "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func startHubSession(t *testing.T, pool, agent string, timing hubSessionTiming, mutate func(net.Conn) hubSessionTransport, afterAcquire func()) *runningHubSession {
	t.Helper()
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var transport hubSessionTransport = server
	if mutate != nil {
		transport = mutate(server)
	}
	go func() {
		done <- serveHubSession(ctx, transport, hubSessionOptions{Agent: agent, Pool: pool, PoolID: fixturePoolID, HubBin: "test", Timing: timing, afterAcquire: afterAcquire})
	}()
	s := &runningHubSession{conn: client, reader: hubwire.NewReader(client), writer: hubwire.NewWriter(client), done: done, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
	})
	return s
}

func helloHub(t *testing.T, s *runningHubSession, agent string, minV int) hubwire.HelloOK {
	t.Helper()
	if err := s.writer.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: 1, MinV: minV, Agent: agent, Bin: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := s.reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if raw.T == "error" {
		var e hubwire.Error
		_ = raw.Decode(&e)
		t.Fatalf("hello error: %+v", e)
	}
	var resp hubwire.HelloOK
	if err := raw.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func readUntil(t *testing.T, s *runningHubSession, terminal string) []hubwire.RawFrame {
	t.Helper()
	var frames []hubwire.RawFrame
	for {
		raw, err := s.reader.Read()
		if err != nil {
			t.Fatalf("read until %s: %v", terminal, err)
		}
		frames = append(frames, raw)
		if raw.T == terminal || raw.T == "error" {
			return frames
		}
	}
}

func TestHubSessionChannelHappyPathAndOrder(t *testing.T) {
	pool, cfg := newHubPool(t)
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	hello := helloHub(t, s, "codex", 1)
	resolvedPool, err := filepath.EvalSymlinks(pool)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Pool12 != fixturePoolID || hello.Pool != resolvedPool || !hello.Writable {
		t.Fatalf("hello = %+v", hello)
	}

	if err := s.writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := s.reader.Read()
	if err != nil || raw.T != "lease-ok" {
		t.Fatalf("lease = %s, %v", raw.T, err)
	}

	// A tick before this channel's own register is E_ORDER and the session
	// survives so the client can complete the required startup sequence.
	if err := s.writer.Write(hubwire.Tick{RequestBase: hubwire.RequestBase{T: "tick", ID: 3}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = s.reader.Read()
	if err != nil || raw.T != "error" {
		t.Fatalf("early tick = %s, %v", raw.T, err)
	}
	var order hubwire.Error
	_ = raw.Decode(&order)
	if order.Code != "E_ORDER" {
		t.Fatalf("early tick code = %s", order.Code)
	}

	vendor := "openai"
	if err := s.writer.Write(hubwire.Register{RequestBase: hubwire.RequestBase{T: "register", ID: 4}, Vendor: &vendor, Host: "m5"}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = s.reader.Read()
	if err != nil || raw.T != "register-ok" || !raw.HasBody {
		t.Fatalf("register = %s body=%v, %v", raw.T, raw.HasBody, err)
	}
	if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex")); err != nil {
		t.Fatal(err)
	}

	if err := s.writer.Write(hubwire.Tick{RequestBase: hubwire.RequestBase{T: "tick", ID: 5}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = s.reader.Read()
	if err != nil || raw.T != "tick-ok" {
		t.Fatalf("tick = %s, %v", raw.T, err)
	}
	var tick hubwire.TickOK
	_ = raw.Decode(&tick)
	if tick.Warnings == nil {
		t.Fatal("tick warnings must be present")
	}

	if err := s.writer.Write(hubwire.LeaseRelease{RequestBase: hubwire.RequestBase{T: "lease-release", ID: 6}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = s.reader.Read()
	if err != nil || raw.T != "release-ok" {
		t.Fatalf("release = %s, %v", raw.T, err)
	}
	if err := <-s.done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")); !os.IsNotExist(err) {
		t.Fatalf("claim remains: %v", err)
	}
}

func TestHubSessionRejectsUnencodableRegisterBeforeMutation(t *testing.T) {
	pool, cfg := newHubPool(t)
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	helloHub(t, s, "codex", 1)

	vendor := "openai"
	host := strings.Repeat("h", hubwire.MaxControlLine/2)
	if err := s.writer.Write(hubwire.Register{RequestBase: hubwire.RequestBase{T: "register", ID: 2}, Vendor: &vendor, Host: host}, nil); err != nil {
		t.Fatalf("the request itself must fit: %v", err)
	}
	raw, err := s.reader.Read()
	if err != nil || raw.T != "error" {
		t.Fatalf("register = %s, %v", raw.T, err)
	}
	var wireErr hubwire.Error
	if err := raw.Decode(&wireErr); err != nil {
		t.Fatal(err)
	}
	if wireErr.Code != hubwire.CodeTooLarge {
		t.Fatalf("code = %s, want %s", wireErr.Code, hubwire.CodeTooLarge)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("codex")); !os.IsNotExist(err) {
		t.Fatalf("oversize response committed a registration: %v", err)
	}
	if _, err := os.Stat(cfg.AgentInboxDir("codex")); !os.IsNotExist(err) {
		t.Fatalf("oversize response created an inbox: %v", err)
	}
	if err := <-s.done; err != nil {
		t.Fatal(err)
	}
}

func TestHubSessionEveryOneShotOp(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	enrollHubAgent(t, cfg, "grok")

	run := func(agent string, request any, body []byte, terminal string) []hubwire.RawFrame {
		t.Helper()
		s := startHubSession(t, pool, agent, hubSessionTiming{}, nil, nil)
		helloHub(t, s, agent, 1)
		if err := s.writer.Write(request, body); err != nil {
			t.Fatal(err)
		}
		frames := readUntil(t, s, terminal)
		if frames[len(frames)-1].T == "error" {
			var e hubwire.Error
			_ = frames[len(frames)-1].Decode(&e)
			t.Fatalf("%s error: %+v", terminal, e)
		}
		return frames
	}

	run("codex", hubwire.Send{RequestBase: hubwire.RequestBase{T: "send", ID: 2}, To: "grok"}, loop.ComposeMessage("codex", "", "hello"), "send-ok")
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("grok"))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("send state: %d, %v", len(msgs), err)
	}
	frames := run("grok", hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil, "check-ok")
	if frames[0].T != "msg" || !frames[0].HasBody {
		t.Fatalf("check stream = %v", frameTypes(frames))
	}
	frames = run("grok", hubwire.Ack{RequestBase: hubwire.RequestBase{T: "ack", ID: 2}}, nil, "ack-ok")
	if frames[0].T != "ack-item" {
		t.Fatalf("ack stream = %v", frameTypes(frames))
	}
	run("codex", hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: 2}}, nil, "status-ok")
	run("codex", hubwire.Gate{RequestBase: hubwire.RequestBase{T: "gate", ID: 2}, Phase: op.GatePhaseFinish}, nil, "gate-ok")
	deliverHubMessage(t, cfg, "grok", "codex", "pending")
	frames = run("codex", hubwire.Pending{RequestBase: hubwire.RequestBase{T: "pending", ID: 2}, ShowBody: true}, nil, "pending-ok")
	if frames[0].T != "msg" || !frames[0].HasBody {
		t.Fatalf("pending stream = %v", frameTypes(frames))
	}
	run("codex", hubwire.CleanOwed{RequestBase: hubwire.RequestBase{T: "clean-owed", ID: 2}}, nil, "clean-owed-ok")

	vendor := "openai"
	run("new-agent", hubwire.Register{RequestBase: hubwire.RequestBase{T: "register", ID: 2}, Vendor: &vendor, Host: "laptop"}, nil, "register-ok")
	if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath("new-agent")); err != nil {
		t.Fatal(err)
	}
}

func frameTypes(frames []hubwire.RawFrame) []string {
	out := make([]string, len(frames))
	for i := range frames {
		out[i] = frames[i].T
	}
	return out
}

func TestHubSessionUnknownTypeSurvives(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	helloHub(t, s, "codex", 1)
	if _, err := s.conn.Write([]byte("{\"t\":\"future-op\",\"id\":2,\"future\":true}\n")); err != nil {
		t.Fatal(err)
	}
	raw, err := s.reader.Read()
	if err != nil || raw.T != "error" {
		t.Fatalf("unknown response = %s, %v", raw.T, err)
	}
	var unsupported hubwire.Error
	_ = raw.Decode(&unsupported)
	if unsupported.Code != hubwire.CodeUnsupported {
		t.Fatalf("code = %s", unsupported.Code)
	}
	if err := s.writer.Write(hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: 3}}, nil); err != nil {
		t.Fatal(err)
	}
	if frames := readUntil(t, s, "status-ok"); frames[len(frames)-1].T != "status-ok" {
		t.Fatalf("session did not survive: %v", frameTypes(frames))
	}
}

func TestHubSessionStartupValidationOrderAndHandshakeFailures(t *testing.T) {
	t.Run("pool not found precedes pool id", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- serveHubSession(context.Background(), server, hubSessionOptions{Agent: "codex", Pool: missing, PoolID: fixturePoolID})
		}()
		raw, err := hubwire.NewReader(client).Read()
		if err != nil {
			t.Fatal(err)
		}
		var e hubwire.Error
		_ = raw.Decode(&e)
		if e.Code != hubwire.CodePoolNotFound {
			t.Fatalf("code = %s", e.Code)
		}
		_ = client.Close()
		<-done
	})

	t.Run("missing pool id is mismatch", func(t *testing.T) {
		pool, _ := newHubPool(t)
		if err := os.Remove(filepath.Join(pool, ".agentchute", "loop", "state", "pool.id")); err != nil {
			t.Fatal(err)
		}
		assertStartupCode(t, pool, hubwire.CodePoolMismatch)
	})

	for _, tc := range []struct {
		name string
		set  func(t *testing.T, path string)
	}{
		{"wrong mode", func(t *testing.T, path string) { t.Helper(); mustChmod(t, path, 0o644) }},
		{"wrong content", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("not-an-id\n"), 0o600) }},
		{"uppercase", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("0123456789AB\n"), 0o600) }},
		{"quote", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("0123456789a\"\n"), 0o600) }},
		{"shell syntax", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("012345678$()\n"), 0o600) }},
		{"whitespace", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("0123456789a \n"), 0o600) }},
		{"wrong length", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("0123456789a\n"), 0o600) }},
		{"oversize", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, bytesOf('x', 65), 0o600) }},
		{"embedded newline", func(t *testing.T, path string) { t.Helper(); hubMustWrite(t, path, []byte("012345\n6789ab\n"), 0o600) }},
		{"symlink", func(t *testing.T, path string) {
			t.Helper()
			target := path + ".target"
			hubMustWrite(t, target, []byte(fixturePoolID+"\n"), 0o600)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := newHubPool(t)
			path := filepath.Join(pool, ".agentchute", "loop", "state", "pool.id")
			tc.set(t, path)
			assertStartupCode(t, pool, hubwire.CodePoolIDInvalid)
		})
	}

	t.Run("valid different pool id is mismatch", func(t *testing.T) {
		pool, _ := newHubPool(t)
		path := filepath.Join(pool, ".agentchute", "loop", "state", "pool.id")
		hubMustWrite(t, path, []byte("fedcba987654\n"), 0o600)
		assertStartupCode(t, pool, hubwire.CodePoolMismatch)
	})

	t.Run("identity mismatch", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
		if err := s.writer.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: 1, MinV: 1, Agent: "grok"}, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := s.reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		var e hubwire.Error
		_ = raw.Decode(&e)
		if e.Code != hubwire.CodeIdentity {
			t.Fatalf("code = %s", e.Code)
		}
	})

	t.Run("version mismatch", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
		if err := s.writer.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: 2, MinV: 2, Agent: "codex"}, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := s.reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		var e hubwire.Error
		_ = raw.Decode(&e)
		if e.Code != hubwire.CodeVersion {
			t.Fatalf("code = %s", e.Code)
		}
	})
}

func assertStartupCode(t *testing.T, pool, want string) {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveHubSession(context.Background(), server, hubSessionOptions{Agent: "codex", Pool: pool, PoolID: fixturePoolID})
	}()
	raw, err := hubwire.NewReader(client).Read()
	if err != nil {
		t.Fatal(err)
	}
	var e hubwire.Error
	_ = raw.Decode(&e)
	if e.Code != want {
		t.Fatalf("code = %s, want %s", e.Code, want)
	}
	_ = client.Close()
	<-done
}

func hubMustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func bytesOf(b byte, n int) []byte { return []byte(strings.Repeat(string(b), n)) }

func TestHubSessionReleasesLeaseOnEveryExitPath(t *testing.T) {
	tests := []struct {
		name  string
		exit  func(t *testing.T, s *runningHubSession)
		after func()
	}{
		{"EOF", func(t *testing.T, s *runningHubSession) { t.Helper(); _ = s.conn.Close() }, nil},
		{"read deadline", func(t *testing.T, _ *runningHubSession) { t.Helper() }, nil},
		{"signal cancellation", func(t *testing.T, s *runningHubSession) { t.Helper(); s.cancel() }, nil},
		{"framing violation", func(t *testing.T, s *runningHubSession) {
			t.Helper()
			_, _ = s.conn.Write([]byte("not-json\n"))
			_, _ = s.reader.Read()
		}, nil},
		{"panic recovery", func(t *testing.T, _ *runningHubSession) { t.Helper() }, func() { panic("forced") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, cfg := newHubPool(t)
			timing := hubSessionTiming{ChannelRead: 25 * time.Millisecond, Write: 10 * time.Millisecond}
			s := startHubSession(t, pool, "codex", timing, nil, tc.after)
			helloHub(t, s, "codex", 1)
			if err := s.writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
				t.Fatal(err)
			}
			raw, err := s.reader.Read()
			if err != nil || raw.T != "lease-ok" {
				t.Fatalf("lease = %s, %v", raw.T, err)
			}
			tc.exit(t, s)
			select {
			case <-s.done:
			case <-time.After(time.Second):
				t.Fatal("session did not exit")
			}
			if _, err := os.Stat(filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")); !os.IsNotExist(err) {
				t.Fatalf("serve claim survived %s: %v", tc.name, err)
			}
		})
	}
}

type failingWriteTransport struct {
	net.Conn
	mu     sync.Mutex
	writes int
	failAt int
}

func (t *failingWriteTransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writes++
	if t.writes >= t.failAt {
		return 0, io.ErrClosedPipe
	}
	return t.Conn.Write(p)
}

func TestHubW1DisconnectAfterClaimRedelivers(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	enrollHubAgent(t, cfg, "grok")
	deliverHubMessage(t, cfg, "grok", "codex", "one")
	deliverHubMessage(t, cfg, "grok", "codex", "two")

	// hello line, first msg control, first msg body succeed; the next control
	// write fails and every later write stays failed (no terminal error frame).
	s := startHubSession(t, pool, "codex", hubSessionTiming{Write: 20 * time.Millisecond}, func(c net.Conn) hubSessionTransport {
		return &failingWriteTransport{Conn: c, failAt: 4}
	}, nil)
	helloHub(t, s, "codex", 1)
	if err := s.writer.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.reader.Read()
	if err != nil || first.T != "msg" {
		t.Fatalf("first = %s, %v", first.T, err)
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("failed session did not exit")
	}

	s2 := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	helloHub(t, s2, "codex", 1)
	if err := s2.writer.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	frames := readUntil(t, s2, "check-ok")
	redelivered := 0
	for _, f := range frames {
		if f.T == "msg" {
			var m hubwire.Message
			_ = f.Decode(&m)
			if m.Redelivered {
				redelivered++
			}
		}
	}
	var summary hubwire.CheckOK
	_ = frames[len(frames)-1].Decode(&summary)
	if redelivered != 2 || summary.Redelivered != 2 {
		t.Fatalf("redelivered frames=%d summary=%d types=%v", redelivered, summary.Redelivered, frameTypes(frames))
	}
}

func TestHubW2SendResponseFailureDoesNotReplay(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	enrollHubAgent(t, cfg, "grok")
	s := startHubSession(t, pool, "codex", hubSessionTiming{}, func(c net.Conn) hubSessionTransport {
		return &failingWriteTransport{Conn: c, failAt: 2}
	}, nil)
	helloHub(t, s, "codex", 1)
	if err := s.writer.Write(hubwire.Send{RequestBase: hubwire.RequestBase{T: "send", ID: 2}, To: "grok"}, loop.ComposeMessage("codex", "", "once")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("send session did not exit")
	}
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("grok"))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("recipient files = %d, err=%v", len(msgs), err)
	}
}

func TestHubW3ReclaimBeforeSendFenceCheckWritesNothing(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	enrollHubAgent(t, cfg, "grok")
	channel := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	helloHub(t, channel, "codex", 1)
	if err := channel.writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := channel.reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var lease hubwire.LeaseOK
	_ = raw.Decode(&lease)
	claimPath := filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")
	claimBytes, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	var claim map[string]any
	if err := json.Unmarshal(claimBytes, &claim); err != nil {
		t.Fatal(err)
	}
	claim["serve_token"] = "new-owner-token"
	claimBytes, _ = json.MarshalIndent(claim, "", "  ")
	claimBytes = append(claimBytes, '\n')
	if err := os.WriteFile(claimPath, claimBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	send := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	helloHub(t, send, "codex", 1)
	if err := send.writer.Write(hubwire.Send{RequestBase: hubwire.RequestBase{T: "send", ID: 2}, To: "grok", ServeToken: lease.Token}, loop.ComposeMessage("codex", "", "fenced")); err != nil {
		t.Fatal(err)
	}
	raw, err = send.reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var e hubwire.Error
	_ = raw.Decode(&e)
	if e.Code != "E_FENCED" {
		t.Fatalf("code = %s", e.Code)
	}
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("grok"))
	if err != nil || len(msgs) != 0 {
		t.Fatalf("recipient files = %d, err=%v", len(msgs), err)
	}
	_ = channel.conn.Close()
}

func TestHubW6UnreadableResidueSetsClaimedHeld(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	enrollHubAgent(t, cfg, "grok")
	deliverHubMessage(t, cfg, "grok", "codex", "held")
	msgs, _, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir("codex"))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("msgs=%d err=%v", len(msgs), err)
	}
	claimedPath, err := loop.ClaimMessage(msgs[0], cfg.AgentClaimedDir("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(claimedPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(claimedPath, 0o600) })

	s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
	helloHub(t, s, "codex", 1)
	if err := s.writer.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := s.reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var e hubwire.Error
	_ = raw.Decode(&e)
	if raw.T != "error" || !e.ClaimedHeld {
		t.Fatalf("error = %+v", e)
	}
}

func TestHubSessionDeadlines(t *testing.T) {
	t.Run("hello", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{Hello: 20 * time.Millisecond, Write: 10 * time.Millisecond}, nil, nil)
		select {
		case <-s.done:
		case <-time.After(time.Second):
			t.Fatal("hello deadline did not close")
		}
	})
	t.Run("one-shot idle", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{OneShotRead: 20 * time.Millisecond, Write: 10 * time.Millisecond}, nil, nil)
		helloHub(t, s, "codex", 1)
		select {
		case <-s.done:
		case <-time.After(time.Second):
			t.Fatal("one-shot idle deadline did not close")
		}
	})
	t.Run("one-shot lifetime", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{OneShotRead: time.Second, OneShotLifetime: 20 * time.Millisecond, Write: 10 * time.Millisecond}, nil, nil)
		helloHub(t, s, "codex", 1)
		select {
		case <-s.done:
		case <-time.After(time.Second):
			t.Fatal("one-shot lifetime did not close")
		}
	})
	t.Run("write", func(t *testing.T) {
		pool, cfg := newHubPool(t)
		enrollHubAgent(t, cfg, "codex")
		s := startHubSession(t, pool, "codex", hubSessionTiming{Write: 20 * time.Millisecond}, nil, nil)
		helloHub(t, s, "codex", 1)
		if err := s.writer.Write(hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: 2}}, nil); err != nil {
			t.Fatal(err)
		}
		// Do not read status-ok: net.Pipe makes the hub's response write block
		// until the configured write deadline expires.
		select {
		case <-s.done:
		case <-time.After(time.Second):
			t.Fatal("write deadline did not close")
		}
	})
}

func TestHubSubcommandHiddenFromDispatcherHelp(t *testing.T) {
	if commandHandlers["hub"] == nil {
		t.Fatal("hub command is not dispatched")
	}
	if strings.Contains(dispatchHelpText(), "hub") {
		t.Fatal("internal hub command leaked into ac help")
	}
}

func TestValidateHubPoolDoesNotConsultDiscoveryEnvironment(t *testing.T) {
	pool, _ := newHubPool(t)
	t.Setenv("AGENTCHUTE_CONTROL_REPO", filepath.Join(t.TempDir(), "wrong"))
	t.Setenv("AGENTCHUTE_LOOP_DIR", filepath.Join(t.TempDir(), "wrong-loop"))
	got, cfg, id, err := validateHubPool(pool, fixturePoolID)
	resolvedPool, resolveErr := filepath.EvalSymlinks(pool)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err != nil || got != resolvedPool || cfg.ControlRepo != resolvedPool || id != fixturePoolID {
		t.Fatalf("got=%s cfg=%+v id=%s err=%v", got, cfg, id, err)
	}
}
