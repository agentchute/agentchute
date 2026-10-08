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
	exited <-chan struct{}
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
	// exited is closed after the session returns. It exists because `done` is a
	// single value that tests consume; cleanup needs a signal that stays readable.
	exited := make(chan struct{})
	var transport hubSessionTransport = server
	if mutate != nil {
		transport = mutate(server)
	}
	go func() {
		defer close(exited)
		done <- serveHubSession(ctx, transport, hubSessionOptions{Agent: agent, Pool: pool, PoolID: fixturePoolID, HubBin: "test", Timing: timing, afterAcquire: afterAcquire})
	}()
	s := &runningHubSession{conn: client, reader: hubwire.NewReader(client), writer: hubwire.NewWriter(client), done: done, exited: exited, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		// JOIN the session goroutine before returning, so it cannot still be
		// running while t.TempDir removes the pool underneath it. A hub session's
		// deferred ReleaseLease re-enters withAgentLock, which RECREATES
		// state/<id>/.lock — so an unjoined session races the cleanup and the
		// cleanup loses: "TempDir RemoveAll cleanup: unlinkat .../state/codex:
		// directory not empty". internal/spectest hit exactly this and fixed it
		// by joining; this harness never got the same treatment.
		//
		// Wait on `exited`, NOT on `done`: `done` carries one value and several
		// tests consume it themselves, so a second receive here would block
		// forever. A closed channel stays readable, which is the property a
		// cleanup needs.
		select {
		case <-s.exited:
		case <-time.After(10 * time.Second):
			t.Errorf("hub session for %s did not return 10s after cancel; it may still be writing to the pool being torn down", agent)
		}
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
	if order.Msg != hubOrderError().Error() {
		t.Fatalf("early tick message = %q", order.Msg)
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
	// Issue #219: every row used to share a 25 ms read deadline and a 10 ms write
	// deadline. The write deadline also bounds hello-ok and lease-ok, which the
	// test must receive, so a reader stalled past 10 ms got EOF instead; and the
	// read deadline could end an EOF, cancellation or framing row before its own
	// exit path ran, so those rows passed without testing that path. Only the
	// read-deadline row now has a short deadline; the rest keep the production
	// defaults, so the named exit path is the only way the session ends in time.
	tests := []struct {
		name   string
		timing hubSessionTiming
		exit   func(t *testing.T, s *runningHubSession)
		after  func()
	}{
		{"EOF", hubSessionTiming{}, func(t *testing.T, s *runningHubSession) { t.Helper(); _ = s.conn.Close() }, nil},
		{"read deadline", hubSessionTiming{ChannelRead: 25 * time.Millisecond}, func(t *testing.T, _ *runningHubSession) { t.Helper() }, nil},
		{"signal cancellation", hubSessionTiming{}, func(t *testing.T, s *runningHubSession) { t.Helper(); s.cancel() }, nil},
		{"framing violation", hubSessionTiming{}, func(t *testing.T, s *runningHubSession) {
			t.Helper()
			_, _ = s.conn.Write([]byte("not-json\n"))
			_, _ = s.reader.Read()
		}, nil},
		{"panic recovery", hubSessionTiming{}, func(t *testing.T, _ *runningHubSession) { t.Helper() }, func() { panic("forced") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, cfg := newHubPool(t)
			s := startHubSession(t, pool, "codex", tc.timing, nil, tc.after)
			helloHub(t, s, "codex", 1)
			if err := s.writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
				t.Fatal(err)
			}
			// A slow reader must still get lease-ok: stand in for a scheduler stall.
			time.Sleep(hubSlowPeer)
			raw, err := s.reader.Read()
			if err != nil || raw.T != "lease-ok" {
				t.Fatalf("lease = %s, %v", raw.T, err)
			}
			tc.exit(t, s)
			select {
			case <-s.done:
			case <-time.After(hubExitWait):
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

type noDeadlineTransport struct{ net.Conn }

func (t *noDeadlineTransport) SetReadDeadline(time.Time) error  { return os.ErrNoDeadline }
func (t *noDeadlineTransport) SetWriteDeadline(time.Time) error { return os.ErrNoDeadline }

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

// hubSlowPeer stands in for a test goroutine the scheduler has not run yet (a
// loaded CI runner, -race). A frame the test must receive has to survive it.
const hubSlowPeer = 50 * time.Millisecond

// hubExitWait bounds how long a test waits for a deadline that fires after
// tens of milliseconds. It only limits a hang; a passing run never waits it out.
const hubExitWait = 10 * time.Second

// Issue #219: each row sets only the deadline it tests. The write deadline also
// bounds hello-ok and lease-ok, so a row that needs one of those to arrive keeps
// the production write bound; the write rows instead block on hello-ok itself,
// the first frame the hub writes, so no frame has to succeed under a tight bound.
func TestHubSessionDeadlines(t *testing.T) {
	t.Run("hello", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{Hello: 20 * time.Millisecond}, nil, nil)
		select {
		case <-s.done:
		case <-time.After(hubExitWait):
			t.Fatal("hello deadline did not close")
		}
	})
	t.Run("one-shot idle", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{OneShotRead: 20 * time.Millisecond}, nil, nil)
		helloHub(t, s, "codex", 1)
		select {
		case <-s.done:
		case <-time.After(hubExitWait):
			t.Fatal("one-shot idle deadline did not close")
		}
	})
	t.Run("one-shot lifetime", func(t *testing.T) {
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{OneShotRead: time.Minute, OneShotLifetime: 20 * time.Millisecond}, nil, nil)
		helloHub(t, s, "codex", 1)
		select {
		case <-s.done:
		case <-time.After(hubExitWait):
			t.Fatal("one-shot lifetime did not close")
		}
	})
	writeBlocked := func(t *testing.T, transport func(net.Conn) hubSessionTransport) {
		t.Helper()
		pool, _ := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{Write: 20 * time.Millisecond}, transport, nil)
		if err := s.writer.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: 1, MinV: 1, Agent: "codex", Bin: "test"}, nil); err != nil {
			t.Fatal(err)
		}
		// Never read hello-ok: net.Pipe blocks the hub's write until its write
		// deadline (or, without deadlines, its watchdog) closes the session.
		select {
		case <-s.done:
		case <-time.After(hubExitWait):
			t.Fatal("write deadline did not close")
		}
	}
	t.Run("write", func(t *testing.T) { writeBlocked(t, nil) })
	t.Run("pipe read watchdog releases lease", func(t *testing.T) {
		pool, cfg := newHubPool(t)
		s := startHubSession(t, pool, "codex", hubSessionTiming{ChannelRead: 20 * time.Millisecond}, func(conn net.Conn) hubSessionTransport {
			return &noDeadlineTransport{Conn: conn}
		}, nil)
		helloHub(t, s, "codex", 1)
		if err := s.writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
			t.Fatal(err)
		}
		// The CI failure: a reader not run within the old 10 ms write bound got
		// EOF here instead of lease-ok.
		time.Sleep(hubSlowPeer)
		if raw, err := s.reader.Read(); err != nil || raw.T != "lease-ok" {
			t.Fatalf("lease = %s, %v", raw.T, err)
		}
		select {
		case <-s.done:
		case <-time.After(hubExitWait):
			t.Fatal("pipe read watchdog did not close")
		}
		if _, err := os.Stat(filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")); !os.IsNotExist(err) {
			t.Fatalf("pipe read watchdog left lease: %v", err)
		}
	})
	t.Run("pipe write watchdog", func(t *testing.T) {
		writeBlocked(t, func(conn net.Conn) hubSessionTransport { return &noDeadlineTransport{Conn: conn} })
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
	got, cfg, id, err := validateHubPool(pool, fixturePoolID, "codex")
	resolvedPool, resolveErr := filepath.EvalSymlinks(pool)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err != nil || got != resolvedPool || cfg.ControlRepo != resolvedPool || id != fixturePoolID {
		t.Fatalf("got=%s cfg=%+v id=%s err=%v", got, cfg, id, err)
	}
}

// The hub's ClaimReq literal carries the frame's budget_bytes: a frame without
// it claims under the default (two of four 5,000-byte messages); -1 claims all.
func TestHubCheckFrameBudgetReachesClaim(t *testing.T) {
	for _, row := range []struct {
		name   string
		budget int
		want   int
	}{
		{"omitted = default", 0, 2},
		{"-1 = unbounded", -1, 4},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool, cfg := newHubPool(t)
			enrollHubAgent(t, cfg, "codex")
			enrollHubAgent(t, cfg, "grok")
			for i := 0; i < 4; i++ {
				deliverHubMessage(t, cfg, "grok", "codex", strings.Repeat("x", 5000))
			}
			s := startHubSession(t, pool, "codex", hubSessionTiming{}, nil, nil)
			helloHub(t, s, "codex", 1)
			if err := s.writer.Write(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}, BudgetBytes: row.budget}, nil); err != nil {
				t.Fatal(err)
			}
			frames := readUntil(t, s, "check-ok")
			msgs := 0
			for _, f := range frames {
				if f.T == "msg" {
					msgs++
				}
			}
			if msgs != row.want || frames[len(frames)-1].T != "check-ok" {
				t.Fatalf("msg frames = %d, want %d; stream = %v", msgs, row.want, frameTypes(frames))
			}
		})
	}
}

// The consume fence on the wire (review 2026-10-08, S2): the hub applies it
// against ITS serve claim for the pinned id. A check/ack frame with a
// mismatched serve_token is E_FENCED and one with an empty serve_token is
// E_LEASE_HELD while that id's serve is live — and a frame with no
// serve_token key at all (a client that predates the fence) is served as
// before, so upgrading the hub first does not strand old remote lanes.
func TestHubSessionConsumeFenceAgainstALiveLease(t *testing.T) {
	pool, cfg := newHubPool(t)
	enrollHubAgent(t, cfg, "codex")
	enrollHubAgent(t, cfg, "grok")
	lease, err := loop.AcquireServeLease(cfg, "grok")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })

	terminal := func(agent string, request any, want string) hubwire.RawFrame {
		t.Helper()
		s := startHubSession(t, pool, agent, hubSessionTiming{}, nil, nil)
		helloHub(t, s, agent, 1)
		if err := s.writer.Write(request, nil); err != nil {
			t.Fatal(err)
		}
		frames := readUntil(t, s, want)
		return frames[len(frames)-1]
	}
	errCode := func(frame hubwire.RawFrame) string {
		if frame.T != "error" {
			return frame.T
		}
		var e hubwire.Error
		_ = frame.Decode(&e)
		return e.Code
	}
	str := func(s string) *string { return &s }
	foreign := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	deliverHubMessage(t, cfg, "codex", "grok", "for the live lane")
	for _, tc := range []struct {
		name  string
		token *string
		want  string
	}{
		{"mismatched token", str(foreign), "E_FENCED"},
		{"empty token", str(""), "E_LEASE_HELD"},
	} {
		got := errCode(terminal("grok", hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}, ServeToken: tc.token}, "check-ok"))
		if got != tc.want {
			t.Fatalf("check with %s = %s, want %s", tc.name, got, tc.want)
		}
		if n := countDirFiles(t, cfg.AgentInboxDir("grok")); n != 1 {
			t.Fatalf("check with %s left %d inbox files, want the message untouched", tc.name, n)
		}
	}

	// The live lane's own token claims; a foreign ack cannot commit it.
	if got := errCode(terminal("grok", hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 2}, ServeToken: str(lease.Token)}, "check-ok")); got != "check-ok" {
		t.Fatalf("check with the live token = %s, want check-ok", got)
	}
	if got := errCode(terminal("grok", hubwire.Ack{RequestBase: hubwire.RequestBase{T: "ack", ID: 2}, ServeToken: str(foreign)}, "ack-ok")); got != "E_FENCED" {
		t.Fatalf("ack with a mismatched token = %s, want E_FENCED", got)
	}
	if n := countDirFiles(t, cfg.AgentClaimedDir("grok")); n != 1 {
		t.Fatalf(".claimed = %d files after a refused ack, want 1", n)
	}

	// A pre-fence client: no serve_token key on the frame at all.
	if got := errCode(terminal("grok", hubwire.Ack{RequestBase: hubwire.RequestBase{T: "ack", ID: 2}}, "ack-ok")); got != "ack-ok" {
		t.Fatalf("ack from a pre-fence client = %s, want ack-ok (served unfenced)", got)
	}
}
