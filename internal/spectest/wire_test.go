package spectest

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/cli"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

const testPoolID = "0123456789ab"

type pipeFactory struct {
	pool string
	cfg  *loop.Config
}

type pipeSession struct {
	conn net.Conn
	done <-chan error
}

func (s *pipeSession) Read(p []byte) (int, error)         { return s.conn.Read(p) }
func (s *pipeSession) Write(p []byte) (int, error)        { return s.conn.Write(p) }
func (s *pipeSession) Close() error                       { return s.conn.Close() }
func (s *pipeSession) SetReadDeadline(t time.Time) error  { return s.conn.SetReadDeadline(t) }
func (s *pipeSession) SetWriteDeadline(t time.Time) error { return s.conn.SetWriteDeadline(t) }
func (s *pipeSession) ForceDisconnect() error             { return s.conn.Close() }
func (s *pipeSession) Wait(ctx context.Context) error {
	select {
	case err := <-s.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *pipeFactory) Open(ctx context.Context, pinned string) (Session, error) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- cli.ServeHubSession(ctx, server, cli.HubSessionConfig{Agent: pinned, Pool: f.pool, PoolID: testPoolID, HubBin: "spectest"})
	}()
	return &pipeSession{conn: client, done: done}, nil
}

func (f *pipeFactory) State() StateProbe { return (*pipeState)(f) }

type pipeState pipeFactory

func (s *pipeState) Deliver(from, to, body string) error {
	_, _, err := loop.SendTsMessageWithCommit(s.cfg, from, to, loop.ComposeMessage(from, "", body), "")
	return err
}

func (s *pipeState) InboxCount(agent string) (int, error) {
	msgs, _, err := loop.ListInboxMessagesWithSkipped(s.cfg.AgentInboxDir(agent))
	if errors.Is(err, loop.ErrInboxMissing) {
		return 0, nil
	}
	return len(msgs), err
}

func (s *pipeState) ClaimedCount(agent string) (int, error) {
	msgs, err := loop.ListClaimedMessages(s.cfg.AgentClaimedDir(agent))
	return len(msgs), err
}

func (s *pipeState) ReplaceLeaseToken(agent, token string) error {
	path := filepath.Join(s.cfg.AgentStateDir(agent), "serve.claim")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var claim loop.ServeClaim
	if err := json.Unmarshal(b, &claim); err != nil {
		return err
	}
	claim.ServeToken = token
	b, err = json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func (s *pipeState) StageUnreadableClaim(agent string) (func() error, error) {
	msgs, _, err := loop.ListInboxMessagesWithSkipped(s.cfg.AgentInboxDir(agent))
	if err != nil {
		return nil, err
	}
	if len(msgs) != 1 {
		return nil, errors.New("expected one inbox message")
	}
	path, err := loop.ClaimMessage(msgs[0], s.cfg.AgentClaimedDir(agent))
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0); err != nil {
		return nil, err
	}
	return func() error { return os.Chmod(path, 0o600) }, nil
}

func TestWireVectors(t *testing.T) {
	vectors, err := LoadVectors("wire.json")
	if err != nil {
		t.Fatal(err)
	}
	AssertWireVectors(t, vectors, func(t *testing.T) SessionFactory {
		pool := t.TempDir()
		if err := os.WriteFile(filepath.Join(pool, "AGENTCHUTE.md"), []byte("# spec\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loopDir := filepath.Join(pool, ".agentchute", "loop")
		if err := os.MkdirAll(filepath.Join(loopDir, "state"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(loopDir, "state", "pool.id"), []byte(testPoolID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := &loop.Config{ControlRepo: pool, LoopDir: loopDir, Vendor: "agentchute"}
		vendor := "test"
		for _, id := range []string{"codex", "grok"} {
			if _, err := op.Register(cfg, op.Context{ActorID: id}, op.RegisterReq{Vendor: &vendor, Host: "test"}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
		return &pipeFactory{pool: pool, cfg: cfg}
	})
}

func TestWireClientVectors(t *testing.T) {
	vectors, err := LoadVectors("wire.json")
	if err != nil {
		t.Fatal(err)
	}
	AssertWireClientVectors(t, vectors, newPipeFactory)
}

func newPipeFactory(t *testing.T) SessionFactory {
	pool := t.TempDir()
	if err := os.WriteFile(filepath.Join(pool, "AGENTCHUTE.md"), []byte("# spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loopDir := filepath.Join(pool, ".agentchute", "loop")
	if err := os.MkdirAll(filepath.Join(loopDir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loopDir, "state", "pool.id"), []byte(testPoolID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &loop.Config{ControlRepo: pool, LoopDir: loopDir, Vendor: "agentchute"}
	vendor := "test"
	for _, id := range []string{"codex", "grok"} {
		if _, err := op.Register(cfg, op.Context{ActorID: id}, op.RegisterReq{Vendor: &vendor, Host: "test"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	return &pipeFactory{pool: pool, cfg: cfg}
}
