package hubclient_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/cli"
	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// failingWriteTransport fails every hub-side write from the failAt-th on,
// without a terminal frame: the shape of a channel that drops mid-stream.
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

// TestOneShotCheckDisconnectAfterPartialDeliveryReplaysOnRecheck is the hub
// row of the same-session re-check matrix (codex R1): the hub claims two
// messages and the channel drops after the client has received one. The
// client must not report success (it cannot know whether the second message
// was claimed), and the next check must replay everything the hub holds
// uncommitted — the second message included, which this client never saw.
// The hub keeps no latch and gains no wire field: replay is op.Claim's
// ordinary residue pass.
func TestOneShotCheckDisconnectAfterPartialDeliveryReplaysOnRecheck(t *testing.T) {
	h := newHarness(t)
	h.register("codex", "openai")
	h.register("grok", "xai")
	for _, body := range []string{"one", "two"} {
		if _, err := h.session("grok").Send(op.SendReq{To: "codex", Content: loop.ComposeMessage("grok", "", body)}); err != nil {
			t.Fatal(err)
		}
	}

	// hello-ok, the first msg control line and its body succeed; the second
	// msg's control write fails and the hub session ends without check-ok.
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- cli.ServeHubSession(ctx, &failingWriteTransport{Conn: server, failAt: 4}, cli.HubSessionConfig{Agent: "codex", Pool: h.pool, PoolID: h.poolID, HubBin: "test"})
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("dropped hub session did not exit")
		}
	})
	dropped, err := hubclient.OpenOneShotTransport(client, h.remote, "codex", "test-client")
	if err != nil {
		t.Fatal(err)
	}
	var seen []op.MessageEvent
	_, err = dropped.Check(op.ClaimReq{}, func(ev op.Event) error {
		if ev.Message != nil {
			seen = append(seen, *ev.Message)
		}
		return nil
	})
	if err == nil {
		t.Fatal("a check whose channel drops after one message must not report success")
	}
	if code := hubclient.ErrorCode(err); code != "E_RESULT_UNKNOWN" {
		t.Fatalf("dropped check code = %s (%v), want E_RESULT_UNKNOWN: one message was streamed, so the outcome is unknown", code, err)
	}
	if len(seen) != 1 || seen[0].Redelivered {
		t.Fatalf("partial delivery = %#v, want exactly one fresh message", seen)
	}

	var replay []op.MessageEvent
	sum, err := h.session("codex").Check(op.ClaimReq{}, func(ev op.Event) error {
		if ev.Message != nil {
			replay = append(replay, *ev.Message)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("re-check after the drop: %v", err)
	}
	if sum.Claimed != 0 || sum.Redelivered != 2 || len(replay) != 2 {
		t.Fatalf("re-check = %#v with %d replayed message(s), want 0 claimed / 2 redelivered", sum, len(replay))
	}
	for _, ev := range replay {
		if !ev.Redelivered {
			t.Errorf("%s replayed without the REDELIVERED marker", ev.Filename)
		}
	}
}

type harness struct {
	t      *testing.T
	pool   string
	remote *loop.RemoteConfig
	poolID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	pool := t.TempDir()
	if err := os.WriteFile(filepath.Join(pool, "AGENTCHUTE.md"), []byte("# spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(pool, ".agentchute", "loop", "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	poolID := "0123456789ab"
	if err := os.WriteFile(filepath.Join(state, "pool.id"), []byte(poolID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote, err := loop.ParseRemoteURL("ssh://hub.example/remote/pool")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, pool: pool, remote: remote, poolID: poolID}
}

func (h *harness) session(agent string) *hubclient.OneShot {
	h.t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- cli.ServeHubSession(ctx, server, cli.HubSessionConfig{Agent: agent, Pool: h.pool, PoolID: h.poolID, HubBin: "test"})
	}()
	h.t.Cleanup(func() {
		_ = client.Close()
		select {
		case err := <-done:
			if err != nil {
				h.t.Errorf("hub session: %v", err)
			}
		case <-time.After(time.Second):
			h.t.Error("hub session did not exit")
		}
	})
	s, err := hubclient.OpenOneShotTransport(client, h.remote, agent, "test-client")
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) register(agent, vendor string) op.RegisterResp {
	h.t.Helper()
	resp, err := h.session(agent).Register(op.RegisterReq{Vendor: &vendor, Host: "client-host"})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func TestOneShotRegisterStatusSendCheckAck(t *testing.T) {
	h := newHarness(t)
	reg := h.register("codex", "openai")
	if reg.Reg.Vendor != "openai" || reg.Reg.Host != "client-host" {
		t.Fatalf("register response = %#v", reg)
	}
	h.register("claude-code", "anthropic")

	status, err := h.session("codex").Status(func(op.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Agents) != 2 || status.Now.IsZero() {
		t.Fatalf("status = %#v", status)
	}

	sent, err := h.session("codex").Send(op.SendReq{To: "claude-code", Content: loop.ComposeMessage("codex", "", "body")})
	if err != nil {
		t.Fatal(err)
	}
	if !sent.Committed || sent.Filename == "" {
		t.Fatalf("send = %#v", sent)
	}

	var pendingMessages []op.MessageEvent
	pending, err := h.session("claude-code").Pending(op.PendingReq{ShowBody: true}, func(ev op.Event) error {
		if ev.Message != nil {
			pendingMessages = append(pendingMessages, *ev.Message)
		}
		return nil
	})
	if err != nil || pending.Unread != 1 || len(pendingMessages) != 1 || len(pendingMessages[0].Body) == 0 {
		t.Fatalf("pending/messages = %#v/%#v, err=%v", pending, pendingMessages, err)
	}
	gate, err := h.session("claude-code").Gate(op.GateReq{Phase: op.GatePhaseFinish})
	if err != nil || !gate.Blocked || gate.UnreadCount != 1 {
		t.Fatalf("gate = %#v, err=%v", gate, err)
	}

	var messages []op.MessageEvent
	claimed, err := h.session("claude-code").Check(op.ClaimReq{}, func(ev op.Event) error {
		if ev.Message != nil {
			messages = append(messages, *ev.Message)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Claimed != 1 || len(messages) != 1 || string(messages[0].Body) == "" {
		t.Fatalf("claim/messages = %#v/%#v", claimed, messages)
	}

	acked, err := h.session("claude-code").Ack(func(op.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if acked.Acked != 1 {
		t.Fatalf("ack = %#v", acked)
	}

	cleaned, err := h.session("codex").CleanOwed(op.CleanOwedReq{})
	if err != nil || cleaned.Agent != "codex" || cleaned.Applied || cleaned.Pruned == nil {
		t.Fatalf("clean-owed = %#v, err=%v", cleaned, err)
	}
}

func TestOneShotRegisterResolvesVendorAndAnnouncesHubSide(t *testing.T) {
	h := newHarness(t)
	h.register("codex-tiny", "openai")
	h.register("grok", "xai")

	resp, err := h.session("codex-tiny").Register(op.RegisterReq{Host: "new-host", Announce: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reg.Vendor != "openai" {
		t.Fatalf("resolved vendor = %q, want openai", resp.Reg.Vendor)
	}
	if resp.Announce == nil || resp.Announce.Sent != 1 || resp.Announce.Total != 1 {
		t.Fatalf("announce = %#v", resp.Announce)
	}
	entries, err := os.ReadDir(filepath.Join(h.pool, ".agentchute", "loop", "inbox", "grok"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".md" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("hub-side grok inbox count = %d, want 1", count)
	}
}

func TestOneShotBootRegisterSweepsHubSide(t *testing.T) {
	h := newHarness(t)
	h.register("codex", "openai")
	h.register("stale-peer", "test")
	stalePath := filepath.Join(h.pool, ".agentchute", "loop", "agents", "stale-peer.md")
	reg, err := loop.ReadRegistration(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	reg.LastSeen = time.Now().UTC().Add(-2 * loop.DefaultStaleAfter)
	if err := loop.WriteRegistration(stalePath, reg); err != nil {
		t.Fatal(err)
	}
	vendor := "openai"
	if _, err := h.session("codex").Register(op.RegisterReq{Vendor: &vendor, Host: "client-host", Sweep: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale hub registration still exists: %v", err)
	}
}
