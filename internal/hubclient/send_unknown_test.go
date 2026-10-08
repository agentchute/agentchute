package hubclient

import (
	"net"
	"testing"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// scriptedHub answers hello, consumes ONE request (with its body), and replies
// with the given frame — the shape of a hub whose terminal response is wrong.
func scriptedHub(t *testing.T, reply func(re int64) any) (*OneShot, *loop.RemoteConfig) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() {
		r, w := hubwire.NewReader(server), hubwire.NewWriter(server)
		hello, err := r.Read()
		if err != nil {
			return
		}
		_ = w.Write(hubwire.HelloOK{ResponseBase: hubwire.ResponseBase{T: "hello-ok", Re: hello.ID}, V: hubwire.Version, Agent: "codex", Pool: "/p", Pool12: "0123456789ab", Writable: true, HubBin: "test"}, nil)
		req, err := r.Read()
		if err != nil {
			return
		}
		_ = w.Write(reply(req.ID), nil)
	}()
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
	if err != nil {
		t.Fatal(err)
	}
	session, err := OpenOneShotTransport(client, remote, "codex", "test-client")
	if err != nil {
		t.Fatal(err)
	}
	return session, remote
}

// Review 2026-10-08 S11: once the send frame is on the wire, ANY failure other
// than the hub's own error frame leaves delivery unknown. A wrong-id or
// wrong-type terminal frame used to surface as E_MALFORMED_FRAME, and send.go
// then printed "retry with" — a duplicate of a send the hub may have committed.
func TestSendFailureAfterTransmissionIsDeliveryUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name  string
		reply func(re int64) any
	}{
		{"terminal frame for another request", func(re int64) any {
			return hubwire.SendOK{ResponseBase: hubwire.ResponseBase{T: "send-ok", Re: re + 7}, Committed: true}
		}},
		{"unexpected frame type", func(re int64) any {
			return hubwire.CheckOK{ResponseBase: hubwire.ResponseBase{T: "check-ok", Re: re}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, _ := scriptedHub(t, tc.reply)
			_, err := session.Send(op.SendReq{To: "grok", Content: loop.ComposeMessage("codex", "", "hi")})
			if ErrorCode(err) != "E_SEND_UNKNOWN" {
				t.Fatalf("send = %v (code %q), want E_SEND_UNKNOWN", err, ErrorCode(err))
			}
		})
	}
}

// The hub's own error frame for this request is authoritative: the hub refused
// it, nothing was delivered, and the ordinary refusal (with its retry advice)
// stands.
func TestSendRefusedByTheHubKeepsTheHubCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session, _ := scriptedHub(t, func(re int64) any {
		return hubwire.Error{ResponseBase: hubwire.ResponseBase{T: "error", Re: re}, Code: "E_RECIPIENT_STALE", Msg: "stale"}
	})
	_, err := session.Send(op.SendReq{To: "grok", Content: loop.ComposeMessage("codex", "", "hi")})
	if ErrorCode(err) != "E_RECIPIENT_STALE" {
		t.Fatalf("send = %v (code %q), want the hub's E_RECIPIENT_STALE", err, ErrorCode(err))
	}
}
