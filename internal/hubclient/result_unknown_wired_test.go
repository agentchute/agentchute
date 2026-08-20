package hubclient

import (
	"net"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// The caller row, and it exists because the unit rows above did NOT need it to
// pass: deleting the `streamed++` from the streaming loop left every assertion
// in result_unknown_test.go green. A correct-and-unwired helper looks exactly
// like a fix, so the counter is pinned where it is incremented, driving the real
// do() loop over a real transport.
func TestStreamingLoopMarksTheResultUnknownWhenTheDropFollowsOutput(t *testing.T) {
	rows := []struct {
		name        string
		streamFirst bool
		wantCode    string
	}{
		{
			// The hub committed and streamed; the terminal frame never came.
			name:        "a msg frame arrives, then the connection drops",
			streamFirst: true,
			wantCode:    "E_RESULT_UNKNOWN",
		},
		{
			// Nothing was delivered, so "nothing happened, re-run" is correct.
			name:     "the connection drops before anything is streamed",
			wantCode: "E_CHANNEL_LOST",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			client, server := net.Pipe()
			remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/pool")
			if err != nil {
				t.Fatal(err)
			}
			s := &OneShot{transport: client, reader: hubwire.NewReader(client), remote: remote, agentID: "codex", nextID: 1}

			go func() {
				reader := hubwire.NewReader(server)
				writer := hubwire.NewWriter(server)
				if _, err := reader.Read(); err != nil { // the check request
					return
				}
				if row.streamFirst {
					_ = writer.Write(hubwire.Message{
						ResponseBase: hubwire.ResponseBase{T: "msg", Re: 1},
						Filename:     "20260820T000000000000Z_from-peer_rabc.md",
						Sender:       "peer",
						Stamp:        "20260820T000000000000Z",
					}, []byte("body\n"))
				}
				// Drop without the terminal frame — the whole point.
				_ = server.Close()
			}()

			emitted := 0
			done := make(chan error, 1)
			go func() {
				_, _, doErr := s.do(
					hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: 1}},
					nil,
					func(op.Event) error { emitted++; return nil },
					false,
					"check-ok",
				)
				done <- doErr
			}()

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a drop before the terminal frame reported success")
				}
				if got := ErrorCode(err); got != row.wantCode {
					t.Fatalf("code = %s, want %s:\n%v", got, row.wantCode, err)
				}
				if row.streamFirst && emitted == 0 {
					t.Fatal("nothing reached emit, so this row did not exercise the streamed case")
				}
				if !row.streamFirst && emitted != 0 {
					t.Fatalf("emitted %d events on the nothing-streamed row", emitted)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("do() never returned")
			}
		})
	}
}
