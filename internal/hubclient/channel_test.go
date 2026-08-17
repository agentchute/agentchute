package hubclient

import (
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

func TestChannelLifecycleOrderAndToken(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	seen := make(chan []string, 1)
	go func() {
		defer server.Close()
		reader := hubwire.NewReader(server)
		writer := hubwire.NewWriter(server)
		var order []string
		for {
			raw, err := reader.Read()
			if err != nil {
				done <- err
				return
			}
			order = append(order, raw.T)
			switch raw.T {
			case "hello":
				err = writer.Write(hubwire.HelloOK{
					ResponseBase: hubwire.ResponseBase{T: "hello-ok", Re: raw.ID},
					V:            hubwire.Version, Agent: "codex", Pool: "pool", Pool12: "0123456789ab",
					Writable: true, HubBin: "test", HubTime: time.Now().UTC(),
				}, nil)
			case "lease-acquire":
				err = writer.Write(hubwire.LeaseOK{ResponseBase: hubwire.ResponseBase{T: "lease-ok", Re: raw.ID}, Token: "serve-token"}, nil)
			case "register":
				var req hubwire.Register
				if err = raw.Decode(&req); err != nil {
					done <- err
					return
				}
				if req.ServeToken != "serve-token" {
					done <- fmt.Errorf("register serve token = %q, want serve-token", req.ServeToken)
					return
				}
				err = writer.Write(hubwire.RegisterOK{
					ResponseBase: hubwire.ResponseBase{T: "register-ok", Re: raw.ID},
					Announce:     nil, Pending: 0,
					Reg:      hubwire.Registration{AgentID: "codex", Vendor: "openai"},
					Warnings: []string{},
				}, nil)
			case "tick":
				err = writer.Write(hubwire.TickOK{ResponseBase: hubwire.ResponseBase{T: "tick-ok", Re: raw.ID}, Pending: 2, Skipped: 1, Warnings: []string{}}, nil)
			case "lease-release":
				err = writer.Write(hubwire.ReleaseOK{ResponseBase: hubwire.ResponseBase{T: "release-ok", Re: raw.ID}}, nil)
				seen <- order
				done <- err
				return
			default:
				done <- fmt.Errorf("unexpected request %q", raw.T)
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()

	remote := &loop.RemoteConfig{URL: "ssh://hub.test/pool", Host: "hub.test", Port: 22}
	channel, err := OpenChannelTransport(client, remote, "codex", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channel.AcquireLease(op.LeaseReq{}); err != nil {
		t.Fatal(err)
	}
	if got := channel.Token(); got != "serve-token" {
		t.Fatalf("token = %q, want serve-token", got)
	}
	if _, err := channel.Register(op.RegisterReq{}); err != nil {
		t.Fatal(err)
	}
	tick, err := channel.Tick(op.TickReq{})
	if err != nil {
		t.Fatal(err)
	}
	if tick.Pending != 2 || tick.Skipped != 1 {
		t.Fatalf("tick counts = pending %d skipped %d, want 2/1", tick.Pending, tick.Skipped)
	}
	if err := channel.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
	if got, want := <-seen, []string{"hello", "lease-acquire", "register", "tick", "lease-release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request order = %v, want %v", got, want)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestChannelDropClassifiedAsLost(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		reader := hubwire.NewReader(server)
		writer := hubwire.NewWriter(server)
		hello, err := reader.Read()
		if err != nil {
			return
		}
		_ = writer.Write(hubwire.HelloOK{
			ResponseBase: hubwire.ResponseBase{T: "hello-ok", Re: hello.ID},
			V:            hubwire.Version, Agent: "codex", Pool: "pool", Pool12: "0123456789ab",
			Writable: true, HubBin: "test", HubTime: time.Now().UTC(),
		}, nil)
		_, _ = reader.Read()
	}()

	remote := &loop.RemoteConfig{URL: "ssh://hub.test/pool", Host: "hub.test", Port: 22}
	channel, err := OpenChannelTransport(client, remote, "codex", "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = channel.Tick(op.TickReq{})
	if got := ErrorCode(err); got != "E_CHANNEL_LOST" {
		t.Fatalf("tick error code = %q (%v), want E_CHANNEL_LOST", got, err)
	}
}
