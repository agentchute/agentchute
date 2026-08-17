package hubclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// Channel is the persistent client-side lease/tick session used by remote
// serve. It is deliberately not safe for concurrent use: the serve tick loop
// is the channel's only writer after startup.
type Channel struct {
	transport Transport
	reader    *hubwire.Reader
	remote    *loop.RemoteConfig
	agentID   string
	nextID    int64
	closed    bool
	warnings  []string
	hello     hubwire.HelloOK
	token     string
}

func OpenChannel(ctx context.Context, remote *loop.RemoteConfig, agentID, bin string) (*Channel, error) {
	invocation, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: agentID, Channel: true})
	if err != nil {
		return nil, err
	}
	transport, err := startSSH(ctx, invocation)
	if err != nil {
		return nil, err
	}
	c, err := OpenChannelTransport(transport, remote, agentID, bin)
	if err != nil {
		return nil, err
	}
	hubCfg, err := ReadHubConfig(remote.HubID)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if c.hello.Pool != hubCfg.Pool || c.hello.Pool12 != hubCfg.Pool12 {
		_ = c.Close()
		return nil, &Error{Code: hubwire.CodePoolMismatch, Msg: fmt.Sprintf("hub: this key now serves pool %s (id %s) on the hub, but this machine joined pool id %s (%s). The key line was re-pointed or the hub moved the pool. Re-join if the move is intended (agentchute hub join <url> --as %s), or re-authorize the key with the right --pool on the hub.", c.hello.Pool, c.hello.Pool12, hubCfg.Pool12, hubCfg.Pool, agentID)}
	}
	c.warnings = invocation.Warnings
	return c, nil
}

// OpenChannelTransport performs the production hello over an already-open
// transport. The in-process M5 harness uses this entry point; real serve uses
// OpenChannel above.
func OpenChannelTransport(transport Transport, remote *loop.RemoteConfig, agentID, bin string) (*Channel, error) {
	one, err := OpenOneShotTransport(transport, remote, agentID, bin)
	if err != nil {
		return nil, err
	}
	return &Channel{
		transport: one.transport,
		reader:    one.reader,
		remote:    one.remote,
		agentID:   one.agentID,
		nextID:    one.nextID,
		hello:     one.hello,
	}, nil
}

func (c *Channel) Hello() hubwire.HelloOK { return c.hello }

func (c *Channel) Warnings() []string { return append([]string(nil), c.warnings...) }

func (c *Channel) Token() string { return c.token }

func (c *Channel) AcquireLease(op.LeaseReq) (op.LeaseResp, error) {
	raw, err := c.do(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: c.nextID}}, "lease-ok")
	if err != nil {
		return op.LeaseResp{}, err
	}
	var resp hubwire.LeaseOK
	if err := raw.Decode(&resp); err != nil {
		_ = c.Close()
		return op.LeaseResp{}, err
	}
	c.token = resp.Token
	return op.LeaseResp{Token: resp.Token}, nil
}

func (c *Channel) Register(req op.RegisterReq) (op.RegisterResp, error) {
	// The channel owns the lease token. Callers cannot accidentally register a
	// remote runner without fencing its registration to this session.
	req.ServeToken = c.token
	raw, err := c.do(hubwire.Register{
		RequestBase: hubwire.RequestBase{T: "register", ID: c.nextID},
		Vendor:      req.Vendor, Host: req.Host, Bio: req.Bio, WorkingRepos: req.WorkingRepos,
		Announce: req.Announce, Sweep: req.Sweep, ServeToken: req.ServeToken,
	}, "register-ok")
	if err != nil {
		return op.RegisterResp{}, err
	}
	var resp hubwire.RegisterOK
	if err := raw.Decode(&resp); err != nil {
		_ = c.Close()
		return op.RegisterResp{}, err
	}
	var announce *op.AnnounceView
	if resp.Announce != nil {
		announce = &op.AnnounceView{Sent: resp.Announce.Sent, Total: resp.Announce.Total, Warnings: resp.Announce.Warnings}
	}
	return op.RegisterResp{
		Announce: announce, Pending: resp.Pending,
		Reg: op.RegistrationView{
			AgentID: resp.Reg.AgentID, ProtocolVersion: resp.Reg.ProtocolVersion,
			Vendor: resp.Reg.Vendor, ControlRepo: resp.Reg.ControlRepo,
			WorkingRepos: resp.Reg.WorkingRepos, Host: resp.Reg.Host,
			LastSeen: resp.Reg.LastSeen, Body: string(raw.Body),
		},
		InboxDir: resp.InboxDir, Refreshed: resp.Refreshed,
		ExistingFound: resp.ExistingFound, ResolvedHost: resp.ResolvedHost,
		Warnings: resp.Warnings,
	}, nil
}

func (c *Channel) Tick(op.TickReq) (op.TickResp, error) {
	raw, err := c.do(hubwire.Tick{RequestBase: hubwire.RequestBase{T: "tick", ID: c.nextID}}, "tick-ok")
	if err != nil {
		return op.TickResp{}, err
	}
	var resp hubwire.TickOK
	if err := raw.Decode(&resp); err != nil {
		_ = c.Close()
		return op.TickResp{}, err
	}
	return op.TickResp{Pending: resp.Pending, Skipped: resp.Skipped, Swept: resp.Swept, Warnings: resp.Warnings}, nil
}

func (c *Channel) ReleaseLease() error {
	if c == nil || c.closed {
		return nil
	}
	_, err := c.do(hubwire.LeaseRelease{RequestBase: hubwire.RequestBase{T: "lease-release", ID: c.nextID}}, "release-ok")
	if err != nil {
		return err
	}
	c.token = ""
	return c.Close()
}

func (c *Channel) Close() error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	return c.transport.Close()
}

func (c *Channel) do(request any, expected string) (hubwire.RawFrame, error) {
	if c.closed {
		return hubwire.RawFrame{}, &Error{Code: "E_CHANNEL_LOST", Msg: "hub: channel is closed"}
	}
	if err := c.setWriteDeadline(30 * time.Second); err != nil {
		c.closed = true
		return hubwire.RawFrame{}, classifySSHFailure(c.remote, c.agentID, "operation", err, c.transport)
	}
	if err := hubwire.NewWriter(c.transport).Write(request, nil); err != nil {
		c.closed = true
		return hubwire.RawFrame{}, classifySSHFailure(c.remote, c.agentID, "operation", err, c.transport)
	}
	if err := c.setReadDeadline(10 * time.Second); err != nil {
		c.closed = true
		return hubwire.RawFrame{}, classifySSHFailure(c.remote, c.agentID, "operation", err, c.transport)
	}
	raw, err := c.reader.Read()
	if err != nil {
		c.closed = true
		return hubwire.RawFrame{}, classifySSHFailure(c.remote, c.agentID, "operation", err, c.transport)
	}
	if raw.Re != c.nextID {
		_ = c.Close()
		return hubwire.RawFrame{}, &Error{Code: hubwire.CodeMalformedFrame, Msg: fmt.Sprintf("hub: response %q references request %d, want %d", raw.T, raw.Re, c.nextID)}
	}
	if raw.T == "error" {
		c.closed = true
		return hubwire.RawFrame{}, wireError(raw, c.transport)
	}
	if raw.T != expected {
		_ = c.Close()
		return hubwire.RawFrame{}, &Error{Code: hubwire.CodeMalformedFrame, Msg: fmt.Sprintf("hub: expected %s, got %q", expected, raw.T)}
	}
	c.nextID++
	return raw, nil
}

func (c *Channel) setReadDeadline(after time.Duration) error {
	err := c.transport.SetReadDeadline(time.Now().Add(after))
	if errors.Is(err, os.ErrNoDeadline) {
		return nil
	}
	return err
}

func (c *Channel) setWriteDeadline(after time.Duration) error {
	err := c.transport.SetWriteDeadline(time.Now().Add(after))
	if errors.Is(err, os.ErrNoDeadline) {
		return nil
	}
	return err
}
