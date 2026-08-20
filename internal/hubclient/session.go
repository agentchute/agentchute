package hubclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

type Error struct {
	Code        string
	Msg         string
	Retriable   bool
	ClaimedHeld bool
	Cause       error
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Unwrap() error {
	if e.Cause != nil {
		return e.Cause
	}
	switch e.Code {
	case "E_NOT_REGISTERED":
		return op.ErrNotRegistered
	case "E_RECIPIENT_UNKNOWN":
		return op.ErrRecipientUnknown
	case "E_RECIPIENT_UNREADABLE":
		return op.ErrRecipientUnreadable
	case "E_RECIPIENT_STALE":
		return op.ErrRecipientStale
	case "E_RECIPIENT_RACING":
		return op.ErrRecipientRacing
	case "E_FENCED":
		return op.ErrFenced
	case "E_LEASE_HELD":
		return op.ErrLeaseHeld
	case "E_ORDER":
		return op.ErrOrder
	default:
		return nil
	}
}

func ErrorCode(err error) string {
	var remoteErr *Error
	if errors.As(err, &remoteErr) {
		return remoteErr.Code
	}
	return ""
}

func ClaimedHeld(err error) bool {
	var remoteErr *Error
	return errors.As(err, &remoteErr) && remoteErr.ClaimedHeld
}

var afterSendFirstByte func()

type OneShot struct {
	transport Transport
	reader    *hubwire.Reader
	remote    *loop.RemoteConfig
	agentID   string
	nextID    int64
	closed    bool
	warnings  []string
	hello     hubwire.HelloOK
}

func OpenOneShot(ctx context.Context, remote *loop.RemoteConfig, agentID, bin string) (*OneShot, error) {
	invocation, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: agentID})
	if err != nil {
		return nil, err
	}
	transport, err := startSSH(ctx, invocation)
	if err != nil {
		return nil, err
	}
	s, err := OpenOneShotTransport(transport, remote, agentID, bin)
	if err != nil {
		return nil, err
	}
	hubCfg, err := ReadHubConfig(remote.HubID)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.hello.Pool != hubCfg.Pool || s.hello.Pool12 != hubCfg.Pool12 {
		_ = s.Close()
		return nil, &Error{Code: hubwire.CodePoolMismatch, Msg: fmt.Sprintf("hub: this key now serves pool %s (id %s) on the hub, but this machine joined pool id %s (%s). The key line was re-pointed or the hub moved the pool. Re-join if the move is intended (agentchute hub join <url> --as %s), or re-authorize the key with the right --pool on the hub.", s.hello.Pool, s.hello.Pool12, hubCfg.Pool12, hubCfg.Pool, agentID)}
	}
	s.warnings = invocation.Warnings
	return s, nil
}

// Probe opens a one-shot hello using keyPath without consulting config.json.
// Join and rotation use it before the local hub record exists, and when a
// staged version must be tested before it becomes the active symlink.
func Probe(ctx context.Context, remote *loop.RemoteConfig, agentID, bin, keyPath string) (hubwire.HelloOK, []string, error) {
	stateDir := ""
	if keyPath != "" {
		stateDir = filepath.Dir(filepath.Dir(keyPath))
	}
	invocation, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: agentID, KeyPath: keyPath, StateDir: stateDir})
	if err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	transport, err := startSSH(ctx, invocation)
	if err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	session, err := OpenOneShotTransport(transport, remote, agentID, bin)
	if err != nil {
		return hubwire.HelloOK{}, invocation.Warnings, err
	}
	hello := session.Hello()
	if err := session.Close(); err != nil {
		return hubwire.HelloOK{}, invocation.Warnings, err
	}
	return hello, invocation.Warnings, nil
}

func OpenOneShotTransport(transport Transport, remote *loop.RemoteConfig, agentID, bin string) (*OneShot, error) {
	s := &OneShot{transport: transport, reader: hubwire.NewReader(transport), remote: remote, agentID: agentID, nextID: 1}
	if err := s.setWriteDeadline(30 * time.Second); err != nil {
		_ = transport.Close()
		return nil, err
	}
	hello := hubwire.Hello{
		RequestBase: hubwire.RequestBase{T: "hello", ID: s.nextID},
		Proto:       hubwire.Protocol,
		V:           hubwire.Version,
		MinV:        hubwire.MinVersion,
		Agent:       agentID,
		Bin:         bin,
	}
	if err := hubwire.NewWriter(transport).Write(hello, nil); err != nil {
		return nil, classifySSHFailure(remote, agentID, "connect", err, transport)
	}
	if err := s.setReadDeadline(10 * time.Second); err != nil {
		_ = transport.Close()
		return nil, err
	}
	raw, err := s.reader.Read()
	if err != nil {
		stage := "connect"
		if isTimeout(err) {
			stage = "hello-timeout"
		}
		return nil, classifySSHFailure(remote, agentID, stage, err, transport)
	}
	if raw.T == "error" {
		return nil, wireError(raw, transport)
	}
	if raw.T != "hello-ok" || raw.Re != s.nextID || raw.HasBody {
		_ = transport.Close()
		return nil, &Error{Code: hubwire.CodeMalformedFrame, Msg: fmt.Sprintf("hub: expected hello-ok, got %q", raw.T)}
	}
	if err := raw.Decode(&s.hello); err != nil {
		_ = transport.Close()
		return nil, err
	}
	if s.hello.Agent != agentID {
		_ = transport.Close()
		return nil, &Error{Code: hubwire.CodeIdentity, Msg: fmt.Sprintf("hub: this key is authorized as %q but you are acting as %q. Fix --as/AGENTCHUTE_AGENT_ID, or join this machine as %s: agentchute hub join <url> --as %s", s.hello.Agent, agentID, agentID, agentID)}
	}
	s.nextID++
	return s, nil
}

func (s *OneShot) Hello() hubwire.HelloOK { return s.hello }
func (s *OneShot) Warnings() []string     { return append([]string(nil), s.warnings...) }

func (s *OneShot) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	return s.transport.Close()
}

func (s *OneShot) Send(req op.SendReq) (op.SendResp, error) {
	frame := hubwire.Send{
		RequestBase: hubwire.RequestBase{T: "send", ID: s.nextID},
		To:          req.To,
		Ask:         req.Ask,
		ReplyByS:    int64(req.ReplyBy / time.Second),
		ServeToken:  req.ServeToken,
	}
	raw, transmitted, err := s.do(frame, req.Content, nil, true, "send-ok")
	if err != nil {
		if transmitted && ErrorCode(err) == "E_CHANNEL_LOST" {
			return op.SendResp{}, &Error{Code: "E_SEND_UNKNOWN", Msg: "hub: connection lost after the send was transmitted — DELIVERY UNKNOWN", Retriable: false, Cause: err}
		}
		return op.SendResp{}, err
	}
	var wireResp hubwire.SendOK
	if err := raw.Decode(&wireResp); err != nil {
		return op.SendResp{}, &Error{Code: "E_SEND_UNKNOWN", Msg: "hub: malformed terminal response after the send was transmitted — DELIVERY UNKNOWN", Cause: err}
	}
	return op.SendResp{Filename: wireResp.Filename, Ref: wireResp.Ref, Committed: wireResp.Committed, DurabilityNote: wireResp.DurabilityNote, OwedNote: wireResp.OwedNote}, nil
}

func (s *OneShot) Check(req op.ClaimReq, emit func(op.Event) error) (op.ClaimSummary, error) {
	raw, _, err := s.do(hubwire.Check{RequestBase: hubwire.RequestBase{T: "check", ID: s.nextID}, Limit: req.Limit, NoArchive: req.NoArchive}, nil, emit, false, "check-ok")
	if err != nil {
		return op.ClaimSummary{}, err
	}
	var resp hubwire.CheckOK
	if err := raw.Decode(&resp); err != nil {
		return op.ClaimSummary{}, err
	}
	return op.ClaimSummary{Claimed: resp.Claimed, Redelivered: resp.Redelivered, Quarantined: resp.Quarantined, OwedExpired: resp.OwedExpired}, nil
}

func (s *OneShot) Ack(emit func(op.Event) error) (op.AckSummary, error) {
	raw, _, err := s.do(hubwire.Ack{RequestBase: hubwire.RequestBase{T: "ack", ID: s.nextID}}, nil, emit, false, "ack-ok")
	if err != nil {
		return op.AckSummary{}, err
	}
	var resp hubwire.AckOK
	if err := raw.Decode(&resp); err != nil {
		return op.AckSummary{}, err
	}
	return op.AckSummary{Acked: resp.Acked, GateClear: resp.GateClear, BlockReasons: resp.BlockReasons}, nil
}

func (s *OneShot) Register(req op.RegisterReq) (op.RegisterResp, error) {
	wireReq := hubwire.Register{RequestBase: hubwire.RequestBase{T: "register", ID: s.nextID}, Vendor: req.Vendor, Host: req.Host, Bio: req.Bio, WorkingRepos: req.WorkingRepos, Announce: req.Announce, Sweep: req.Sweep, ServeToken: req.ServeToken}
	raw, _, err := s.do(wireReq, nil, nil, false, "register-ok")
	if err != nil {
		return op.RegisterResp{}, err
	}
	var resp hubwire.RegisterOK
	if err := raw.Decode(&resp); err != nil {
		return op.RegisterResp{}, err
	}
	var announce *op.AnnounceView
	if resp.Announce != nil {
		announce = &op.AnnounceView{Sent: resp.Announce.Sent, Total: resp.Announce.Total, Warnings: resp.Announce.Warnings}
	}
	return op.RegisterResp{
		Announce: announce, Pending: resp.Pending,
		Reg:      op.RegistrationView{AgentID: resp.Reg.AgentID, ProtocolVersion: resp.Reg.ProtocolVersion, Vendor: resp.Reg.Vendor, ControlRepo: resp.Reg.ControlRepo, WorkingRepos: resp.Reg.WorkingRepos, Host: resp.Reg.Host, LastSeen: resp.Reg.LastSeen, Body: string(raw.Body)},
		InboxDir: resp.InboxDir, Refreshed: resp.Refreshed, ExistingFound: resp.ExistingFound, ResolvedHost: resp.ResolvedHost, Warnings: resp.Warnings,
	}, nil
}

func (s *OneShot) Status(emit func(op.Event) error) (op.StatusResp, error) {
	raw, _, err := s.do(hubwire.Status{RequestBase: hubwire.RequestBase{T: "status", ID: s.nextID}}, nil, emit, false, "status-ok")
	if err != nil {
		return op.StatusResp{}, err
	}
	var resp hubwire.StatusOK
	if err := raw.Decode(&resp); err != nil {
		return op.StatusResp{}, err
	}
	return op.StatusResp{Agents: resp.Agents, Truncated: resp.Truncated, Now: resp.Now}, nil
}

func (s *OneShot) Gate(req op.GateReq) (op.GateResp, error) {
	raw, _, err := s.do(hubwire.Gate{RequestBase: hubwire.RequestBase{T: "gate", ID: s.nextID}, Phase: req.Phase, RequireConfirm: req.RequireConfirm, AckStaleReg: req.AckStaleReg}, nil, nil, false, "gate-ok")
	if err != nil {
		return op.GateResp{}, err
	}
	var resp hubwire.GateOK
	if err := raw.Decode(&resp); err != nil {
		return op.GateResp{}, err
	}
	return resp.GateResp, nil
}

func (s *OneShot) Pending(req op.PendingReq, emit func(op.Event) error) (op.PendingSummary, error) {
	raw, _, err := s.do(hubwire.Pending{RequestBase: hubwire.RequestBase{T: "pending", ID: s.nextID}, ShowBody: req.ShowBody}, nil, emit, false, "pending-ok")
	if err != nil {
		return op.PendingSummary{}, err
	}
	var resp hubwire.PendingOK
	if err := raw.Decode(&resp); err != nil {
		return op.PendingSummary{}, err
	}
	return op.PendingSummary{Unread: resp.Unread, Owed: resp.Owed, Malformed: resp.Malformed, NeedsBoot: resp.NeedsBoot}, nil
}

func (s *OneShot) CleanOwed(req op.CleanOwedReq) (op.CleanOwedResp, error) {
	raw, _, err := s.do(hubwire.CleanOwed{RequestBase: hubwire.RequestBase{T: "clean-owed", ID: s.nextID}, Apply: req.Apply}, nil, nil, false, "clean-owed-ok")
	if err != nil {
		return op.CleanOwedResp{}, err
	}
	var resp hubwire.CleanOwedOK
	if err := raw.Decode(&resp); err != nil {
		return op.CleanOwedResp{}, err
	}
	return op.CleanOwedResp{Agent: resp.Agent, Pruned: resp.Pruned, Applied: resp.Applied}, nil
}

func (s *OneShot) do(request any, body []byte, emit func(op.Event) error, observeFirstByte bool, expected string) (hubwire.RawFrame, bool, error) {
	if s.closed {
		return hubwire.RawFrame{}, false, &Error{Code: "E_CHANNEL_LOST", Msg: "hub: one-shot session is closed"}
	}
	encoded, err := hubwire.Encode(request, body)
	if err != nil {
		return hubwire.RawFrame{}, false, err
	}
	if err := s.setWriteDeadline(30 * time.Second); err != nil {
		return hubwire.RawFrame{}, false, err
	}
	transmitted := false
	if observeFirstByte {
		n, writeErr := s.transport.Write(encoded[:1])
		if n > 0 {
			transmitted = true
			if afterSendFirstByte != nil {
				afterSendFirstByte()
			}
		}
		if writeErr == nil && n != 1 {
			writeErr = io.ErrShortWrite
		}
		if writeErr != nil {
			return hubwire.RawFrame{}, transmitted, classifySSHFailure(s.remote, s.agentID, "operation", writeErr, s.transport)
		}
		encoded = encoded[1:]
	}
	if err := writeAll(s.transport, encoded); err != nil {
		return hubwire.RawFrame{}, transmitted, classifySSHFailure(s.remote, s.agentID, "operation", err, s.transport)
	}
	if !observeFirstByte {
		transmitted = len(encoded) > 0
	}
	if err := s.setReadDeadline(30 * time.Second); err != nil {
		return hubwire.RawFrame{}, transmitted, err
	}
	streamed := 0
	for {
		raw, err := s.reader.Read()
		if err != nil {
			return hubwire.RawFrame{}, transmitted, resultUnknownIfStreamed(
				classifySSHFailure(s.remote, s.agentID, "operation", err, s.transport), streamed)
		}
		if raw.Re != s.nextID {
			return hubwire.RawFrame{}, transmitted, &Error{Code: hubwire.CodeMalformedFrame, Msg: fmt.Sprintf("hub: response %q references request %d, want %d", raw.T, raw.Re, s.nextID)}
		}
		switch raw.T {
		case "msg", "note", "owed-item", "ack-item":
			if emit == nil {
				return hubwire.RawFrame{}, transmitted, &Error{Code: hubwire.CodeMalformedFrame, Msg: fmt.Sprintf("hub: unexpected streamed frame %q", raw.T)}
			}
			event, err := eventOf(raw)
			if err != nil {
				return hubwire.RawFrame{}, transmitted, err
			}
			if err := emit(event); err != nil {
				return hubwire.RawFrame{}, transmitted, err
			}
			streamed++
		case "error":
			return hubwire.RawFrame{}, transmitted, wireError(raw, s.transport)
		default:
			if raw.T != expected {
				return hubwire.RawFrame{}, transmitted, &Error{Code: hubwire.CodeMalformedFrame, Msg: fmt.Sprintf("hub: expected %s, got %q", expected, raw.T)}
			}
			s.nextID++
			_ = s.Close()
			return raw, transmitted, nil
		}
	}
}

// resultUnknownIfStreamed re-codes a lost connection that arrived AFTER the hub
// had already streamed output.
//
// #171: the hub commits the work, streams it, the client prints all of it, and
// then the terminal frame is lost. The operator sees their messages followed by
// "channel to the hub was lost" and does the obvious thing — re-runs — and
// at-least-once re-delivers what they were just shown. Nothing is lost; the
// safety property holds exactly as designed. What is wrong is the report: two
// situations calling for OPPOSITE actions rendered identically.
//
//	nothing happened        -> re-run
//	everything happened     -> do not re-run
//
// The client genuinely cannot tell which, because the terminal frame is what
// would have told it. So it says that, rather than picking one and being wrong
// half the time. Not retriable, for the same reason E_SEND_UNKNOWN is not: the
// safe action is to look, not to repeat.
//
// A failure with NO streamed output keeps its original classification — there
// the answer is known, and "nothing happened, re-run" is the correct advice.
func resultUnknownIfStreamed(err error, streamed int) error {
	if err == nil || streamed == 0 {
		return err
	}
	return &Error{
		Code: "E_RESULT_UNKNOWN",
		Msg: fmt.Sprintf("hub: the connection dropped after the hub had already sent %d item(s) — everything printed above did happen, "+
			"but the confirmation that would say the operation finished never arrived. DO NOT assume it failed: the hub may have "+
			"committed the work, in which case re-running re-delivers what you were just shown (at-least-once). Check the current "+
			"state first (`agentchute status`, or `agentchute pending` for obligations) and re-run only if it says the work is still outstanding.", streamed),
		Retriable: false,
		Cause:     err,
	}
}

func eventOf(raw hubwire.RawFrame) (op.Event, error) {
	switch raw.T {
	case "msg":
		var frame hubwire.Message
		if err := raw.Decode(&frame); err != nil {
			return op.Event{}, err
		}
		var body []byte
		if raw.HasBody {
			body = raw.Body
		}
		return op.NewMessageEvent(op.MessageEvent{Filename: frame.Filename, Sender: frame.Sender, Stamp: frame.Stamp, Redelivered: frame.Redelivered, ReplyRequired: frame.ReplyRequired, ReplyRef: frame.ReplyRef, Body: body}), nil
	case "note":
		var frame hubwire.Note
		if err := raw.Decode(&frame); err != nil {
			return op.Event{}, err
		}
		return op.NewNoteEvent(frame.Level, frame.Msg), nil
	case "owed-item":
		var frame hubwire.OwedItem
		if err := raw.Decode(&frame); err != nil {
			return op.Event{}, err
		}
		return op.NewOwedEvent(op.OwedEvent{To: frame.To, From: frame.From, Seq: frame.Seq, Stamp: frame.Stamp, Suffix: frame.Suffix, By: frame.By, RecordedAt: frame.RecordedAt, Ref: frame.Ref}), nil
	case "ack-item":
		var frame hubwire.AckItem
		if err := raw.Decode(&frame); err != nil {
			return op.Event{}, err
		}
		return op.NewAckItemEvent(op.AckItemEvent{Filename: frame.Filename, ArchivePath: frame.ArchivePath}), nil
	default:
		return op.Event{}, fmt.Errorf("unsupported event frame %q", raw.T)
	}
}

func wireError(raw hubwire.RawFrame, transport Transport) error {
	var frame hubwire.Error
	if err := raw.Decode(&frame); err != nil {
		_ = transport.Close()
		return err
	}
	_ = transport.Close()
	return &Error{Code: frame.Code, Msg: frame.Msg, Retriable: frame.Retriable, ClaimedHeld: frame.ClaimedHeld}
}

func (s *OneShot) setReadDeadline(after time.Duration) error {
	err := s.transport.SetReadDeadline(time.Now().Add(after))
	if errors.Is(err, os.ErrNoDeadline) {
		return nil
	}
	return err
}

func (s *OneShot) setWriteDeadline(after time.Duration) error {
	err := s.transport.SetWriteDeadline(time.Now().Add(after))
	if errors.Is(err, os.ErrNoDeadline) {
		return nil
	}
	return err
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
