package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// HubSessionTransport is the carriage contract shared by the in-process M3
// conformance driver and M6's sshd-backed driver.
type HubSessionTransport interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Close() error
}

type hubSessionTransport = HubSessionTransport

// HubSessionConfig is the forced-command identity pinned by authorized_keys.
// It intentionally contains no discovery inputs or client-selected actor.
type HubSessionConfig struct {
	Agent  string
	Pool   string
	PoolID string
	HubBin string
}

// ServeHubSession runs one forced-command session through the production
// dispatcher. The internal-only export lets transport conformance tests reuse
// the exact session implementation without copying it.
func ServeHubSession(ctx context.Context, transport HubSessionTransport, cfg HubSessionConfig) error {
	return serveHubSession(ctx, transport, hubSessionOptions{Agent: cfg.Agent, Pool: cfg.Pool, PoolID: cfg.PoolID, HubBin: cfg.HubBin})
}

type stdioHubTransport struct {
	in, out *os.File
}

func (s *stdioHubTransport) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s *stdioHubTransport) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *stdioHubTransport) SetReadDeadline(t time.Time) error {
	return s.in.SetReadDeadline(t)
}
func (s *stdioHubTransport) SetWriteDeadline(t time.Time) error {
	return s.out.SetWriteDeadline(t)
}
func (s *stdioHubTransport) Close() error {
	errIn := s.in.Close()
	errOut := s.out.Close()
	return errors.Join(errIn, errOut)
}

type hubSessionOptions struct {
	Agent  string
	Pool   string
	PoolID string
	HubBin string
	Timing hubSessionTiming

	// afterAcquire is an invocation-scoped failure-injection hook used to prove
	// panic cleanup. It is nil in production and never shared across sessions.
	afterAcquire func()
	// now is an invocation-scoped test clock. It is nil in production.
	now func() time.Time
}

type hubSessionTiming struct {
	Hello           time.Duration
	ChannelRead     time.Duration
	OneShotRead     time.Duration
	Write           time.Duration
	OneShotLifetime time.Duration
}

func (t hubSessionTiming) withDefaults() hubSessionTiming {
	if t.Hello == 0 {
		t.Hello = 10 * time.Second
	}
	if t.ChannelRead == 0 {
		t.ChannelRead = 20 * time.Second
	}
	if t.OneShotRead == 0 {
		t.OneShotRead = 30 * time.Second
	}
	if t.Write == 0 {
		t.Write = 30 * time.Second
	}
	if t.OneShotLifetime == 0 {
		t.OneShotLifetime = 10 * time.Minute
	}
	return t
}

type hubSession struct {
	// writeInFlight is true while a frame is being written. Asynchronous closes
	// read it to avoid cutting a control line in half; see closeAfterWriteIdle.
	writeInFlight atomic.Bool

	transport    hubSessionTransport
	reader       *hubwire.Reader
	writer       *hubwire.Writer
	cfg          *loop.Config
	ctx          op.Context
	channel      *op.Channel
	timing       hubSessionTiming
	lastID       int64
	mode         string
	afterAcquire func()
	now          func() time.Time
	oneShotTimer *time.Timer
}

func serveHubSession(ctx context.Context, transport hubSessionTransport, opts hubSessionOptions) (returnErr error) {
	timing := opts.Timing.withDefaults()
	now := opts.now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	s := &hubSession{transport: transport, reader: hubwire.NewReader(transport), writer: hubwire.NewWriter(transport), timing: timing, afterAcquire: opts.afterAcquire, now: now}
	defer func() {
		if s.channel != nil {
			if err := s.channel.ReleaseLease(); err != nil && !errors.Is(err, op.ErrFenced) && returnErr == nil {
				returnErr = err
			}
		}
		_ = transport.Close()
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("hub session panic: %v", recovered)
		}
	}()

	pool, cfg, pool12, err := validateHubPool(opts.Pool, opts.PoolID, opts.Agent)
	if err != nil {
		_ = s.writeError(0, err)
		return nil
	}
	if err := loop.ValidateAgentID(opts.Agent); err != nil {
		err = &hubwire.ProtocolError{Code: hubwire.CodeIdentity, Msg: fmt.Sprintf("invalid pinned agent id: %v", err)}
		_ = s.writeError(0, err)
		return nil
	}
	s.cfg = cfg
	s.ctx = op.Context{ActorID: opts.Agent}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.closeAfterWriteIdle()
		case <-done:
		}
	}()
	s.oneShotTimer = time.AfterFunc(timing.OneShotLifetime, func() { s.closeAfterWriteIdle() })
	defer s.oneShotTimer.Stop()

	raw, err := s.read(timing.Hello)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			_ = s.writeError(0, err)
		}
		return nil
	}
	if raw.T != "hello" || raw.HasBody {
		_ = s.writeError(raw.ID, &hubwire.ProtocolError{Code: hubwire.CodeMalformedFrame, Msg: "hello must be the first frame and cannot carry a body"})
		return nil
	}
	var hello hubwire.Hello
	if err := raw.Decode(&hello); err != nil {
		_ = s.writeError(raw.ID, err)
		return nil
	}
	helloOK, err := hubwire.NegotiateHello(hello, hubwire.HandshakeOptions{
		PinnedAgent: opts.Agent,
		Pool:        pool,
		Pool12:      pool12,
		HubBin:      opts.HubBin,
	})
	if err != nil {
		switch hubwire.CodeFor(err) {
		case hubwire.CodeVersion:
			clientVersion := hello.MinV
			if clientVersion == 0 {
				clientVersion = hello.V
			}
			err = hubVersionError(hubwire.Version, clientVersion)
		case hubwire.CodeIdentity:
			err = hubIdentityError(opts.Agent, hello.Agent)
		}
		_ = s.writeError(hello.ID, err)
		return nil
	}
	s.lastID = hello.ID
	if err := s.write(helloOK, nil); err != nil {
		return err
	}

	for {
		readFor := timing.OneShotRead
		if s.mode == "channel" {
			readFor = timing.ChannelRead
		}
		raw, err := s.read(readFor)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			_ = s.writeError(raw.ID, err)
			return nil
		}
		if raw.ID <= s.lastID {
			if err := s.writeError(raw.ID, op.ErrOrder); err != nil {
				return err
			}
			continue
		}
		s.lastID = raw.ID
		terminal, err := s.dispatch(raw)
		if err != nil {
			return err
		}
		if terminal {
			return nil
		}
	}
}

func (s *hubSession) dispatch(raw hubwire.RawFrame) (bool, error) {
	if raw.T == "lease-acquire" {
		if raw.HasBody || s.mode != "" {
			return false, s.writeError(raw.ID, op.ErrOrder)
		}
		var req hubwire.LeaseAcquire
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		s.mode = "channel"
		s.oneShotTimer.Stop()
		s.channel = op.NewChannel(s.cfg, s.ctx, op.ChannelOpts{HeartbeatTemplate: nil})
		resp, err := s.channel.AcquireLease(op.LeaseReq{})
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		if err := s.write(hubwire.LeaseOK{ResponseBase: hubwire.ResponseBase{T: "lease-ok", Re: raw.ID}, Token: resp.Token}, nil); err != nil {
			return true, err
		}
		if s.afterAcquire != nil {
			s.afterAcquire()
		}
		return false, nil
	}
	if raw.T == "tick" || raw.T == "lease-release" {
		if raw.HasBody || s.mode != "channel" || s.channel == nil {
			return false, s.writeError(raw.ID, op.ErrOrder)
		}
	}
	if s.mode == "channel" && raw.T != "register" && raw.T != "tick" && raw.T != "lease-release" {
		if raw.T == "hello" {
			return false, s.writeError(raw.ID, op.ErrOrder)
		}
		return false, s.unsupported(raw.ID, raw.T)
	}

	switch raw.T {
	case "send":
		if !raw.HasBody {
			return true, s.writeError(raw.ID, &hubwire.ProtocolError{Code: hubwire.CodeMalformedFrame, Msg: "send requires a body trailer"})
		}
		var req hubwire.Send
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		resp, err := op.Send(s.cfg, s.ctx, op.SendReq{To: req.To, Content: raw.Body, Ask: req.Ask, ReplyBy: time.Duration(req.ReplyByS) * time.Second, ServeToken: req.ServeToken})
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.SendOK{ResponseBase: hubwire.ResponseBase{T: "send-ok", Re: raw.ID}, Filename: resp.Filename, Ref: resp.Ref, Committed: resp.Committed, DurabilityNote: resp.DurabilityNote, OwedNote: resp.OwedNote}, nil)
	case "check":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "check cannot carry a body")
		}
		var req hubwire.Check
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		sum, err := op.Claim(s.cfg, s.ctx, op.ClaimReq{Limit: req.Limit, NoArchive: req.NoArchive}, s.emitter(raw.ID, false))
		if err != nil {
			if sum.Redelivered > 0 {
				err = &hubwire.ProtocolError{Code: hubwire.CodeFor(err), Msg: err.Error(), ClaimedHeld: true}
			}
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.CheckOK{ResponseBase: hubwire.ResponseBase{T: "check-ok", Re: raw.ID}, Claimed: sum.Claimed, Redelivered: sum.Redelivered, Quarantined: sum.Quarantined, OwedExpired: sum.OwedExpired}, nil)
	case "ack":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "ack cannot carry a body")
		}
		sum, err := op.Ack(s.cfg, s.ctx, op.AckReq{}, s.emitter(raw.ID, false))
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.AckOK{ResponseBase: hubwire.ResponseBase{T: "ack-ok", Re: raw.ID}, Acked: sum.Acked, GateClear: sum.GateClear, BlockReasons: sum.BlockReasons}, nil)
	case "register":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "register request cannot carry a body")
		}
		var req hubwire.Register
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		opReq := op.RegisterReq{Vendor: req.Vendor, Host: req.Host, Bio: req.Bio, WorkingRepos: req.WorkingRepos, Announce: req.Announce, Sweep: req.Sweep, ServeToken: req.ServeToken}
		validateResponse := func(resp op.RegisterResp) error {
			wireResp, body := registerResponse(raw.ID, resp)
			_, err := hubwire.Encode(wireResp, body)
			return err
		}
		var resp op.RegisterResp
		var err error
		if s.mode == "channel" {
			resp, err = s.channel.RegisterWithPrecommitValidation(opReq, validateResponse)
		} else {
			resp, err = op.RegisterWithPrecommitValidation(s.cfg, s.ctx, opReq, s.now(), validateResponse)
		}
		if err != nil {
			return s.mode != "channel", s.writeError(raw.ID, err)
		}
		wireResp, body := registerResponse(raw.ID, resp)
		return s.mode != "channel", s.write(wireResp, body)
	case "status":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "status cannot carry a body")
		}
		resp, err := op.Status(s.cfg, s.ctx, op.StatusReq{}, s.emitter(raw.ID, false))
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.writeStatus(raw.ID, resp)
	case "gate":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "gate cannot carry a body")
		}
		var req hubwire.Gate
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		resp, err := op.Gate(s.cfg, s.ctx, op.GateReq{Phase: req.Phase, RequireConfirm: req.RequireConfirm, AckStaleReg: req.AckStaleReg})
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.GateOK{ResponseBase: hubwire.ResponseBase{T: "gate-ok", Re: raw.ID}, GateResp: resp}, nil)
	case "pending":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "pending cannot carry a body")
		}
		var req hubwire.Pending
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		resp, err := op.Pending(s.cfg, s.ctx, op.PendingReq{ShowBody: req.ShowBody}, s.emitter(raw.ID, req.ShowBody))
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.PendingOK{ResponseBase: hubwire.ResponseBase{T: "pending-ok", Re: raw.ID}, Unread: resp.Unread, Owed: resp.Owed, Malformed: resp.Malformed, NeedsBoot: resp.NeedsBoot}, nil)
	case "clean-owed":
		if raw.HasBody {
			return true, s.malformed(raw.ID, "clean-owed cannot carry a body")
		}
		var req hubwire.CleanOwed
		if err := raw.Decode(&req); err != nil {
			return true, s.writeError(raw.ID, err)
		}
		resp, err := op.CleanOwed(s.cfg, s.ctx, op.CleanOwedReq{Apply: req.Apply})
		if err != nil {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.CleanOwedOK{ResponseBase: hubwire.ResponseBase{T: "clean-owed-ok", Re: raw.ID}, Agent: resp.Agent, Pruned: resp.Pruned, Applied: resp.Applied}, nil)
	case "tick":
		resp, err := s.channel.Tick(op.TickReq{})
		if err != nil {
			return errors.Is(err, op.ErrFenced), s.writeError(raw.ID, err)
		}
		return false, s.write(hubwire.TickOK{ResponseBase: hubwire.ResponseBase{T: "tick-ok", Re: raw.ID}, Pending: resp.Pending, Skipped: resp.Skipped, Swept: resp.Swept, Warnings: resp.Warnings}, nil)
	case "lease-release":
		err := s.channel.ReleaseLease()
		if err != nil && !errors.Is(err, op.ErrFenced) {
			return true, s.writeError(raw.ID, err)
		}
		return true, s.write(hubwire.ReleaseOK{ResponseBase: hubwire.ResponseBase{T: "release-ok", Re: raw.ID}}, nil)
	case "hello":
		return false, s.writeError(raw.ID, op.ErrOrder)
	default:
		return false, s.unsupported(raw.ID, raw.T)
	}
}

func (s *hubSession) emitter(re int64, pendingBody bool) func(op.Event) error {
	return func(event op.Event) error {
		if !event.Valid() {
			return fmt.Errorf("invalid op event union")
		}
		switch {
		case event.Message != nil:
			m := event.Message
			body := []byte(nil)
			if pendingBody || m.Body != nil {
				body = m.Body
			}
			return s.write(hubwire.Message{ResponseBase: hubwire.ResponseBase{T: "msg", Re: re}, Filename: m.Filename, Sender: m.Sender, Stamp: m.Stamp, Redelivered: m.Redelivered, ReplyRequired: m.ReplyRequired, ReplyRef: m.ReplyRef}, body)
		case event.Note != nil:
			if err := hubwire.ValidateNoteLevel(event.Note.Level); err != nil {
				return err
			}
			return s.write(hubwire.Note{ResponseBase: hubwire.ResponseBase{T: "note", Re: re}, Level: event.Note.Level, Msg: event.Note.Msg}, nil)
		case event.Owed != nil:
			o := event.Owed
			return s.write(hubwire.OwedItem{ResponseBase: hubwire.ResponseBase{T: "owed-item", Re: re}, To: o.To, From: o.From, Seq: o.Seq, Stamp: o.Stamp, Suffix: o.Suffix, By: o.By, RecordedAt: o.RecordedAt, Ref: o.Ref}, nil)
		default:
			a := event.Ack
			return s.write(hubwire.AckItem{ResponseBase: hubwire.ResponseBase{T: "ack-item", Re: re}, Filename: a.Filename, ArchivePath: a.ArchivePath}, nil)
		}
	}
}

func registerResponse(re int64, resp op.RegisterResp) (hubwire.RegisterOK, []byte) {
	warnings := append([]string(nil), resp.Warnings...)
	if warnings == nil {
		warnings = []string{}
	}
	var announce *hubwire.Announce
	if resp.Announce != nil {
		aw := append([]string(nil), resp.Announce.Warnings...)
		if aw == nil {
			aw = []string{}
		}
		announce = &hubwire.Announce{Sent: resp.Announce.Sent, Total: resp.Announce.Total, Warnings: aw}
	}
	return hubwire.RegisterOK{
		ResponseBase: hubwire.ResponseBase{T: "register-ok", Re: re},
		Announce:     announce, Pending: resp.Pending,
		Reg:      hubwire.Registration{AgentID: resp.Reg.AgentID, ProtocolVersion: resp.Reg.ProtocolVersion, Vendor: resp.Reg.Vendor, ControlRepo: resp.Reg.ControlRepo, WorkingRepos: resp.Reg.WorkingRepos, Host: resp.Reg.Host, LastSeen: resp.Reg.LastSeen},
		InboxDir: resp.InboxDir, Refreshed: resp.Refreshed, ExistingFound: resp.ExistingFound, ResolvedHost: resp.ResolvedHost, Warnings: warnings,
	}, []byte(resp.Reg.Body)
}

func (s *hubSession) malformed(re int64, msg string) error {
	return s.writeError(re, &hubwire.ProtocolError{Code: hubwire.CodeMalformedFrame, Msg: msg})
}

func (s *hubSession) unsupported(re int64, t string) error {
	return s.writeError(re, &hubwire.ProtocolError{Code: hubwire.CodeUnsupported, Msg: fmt.Sprintf("unsupported frame type %q", t)})
}

func (s *hubSession) writeError(re int64, err error) error {
	return s.write(hubwire.NewError(re, hubSessionCatalogError(s.cfg, s.ctx.ActorID, err)), nil)
}

func (s *hubSession) write(frame any, body []byte) error {
	if err := s.prepareWriteDeadline(); err != nil {
		return err
	}
	return s.watchIO(s.timing.Write, func() error { return s.writer.Write(frame, body) })
}

func (s *hubSession) writeStatus(re int64, resp op.StatusResp) error {
	if err := s.prepareWriteDeadline(); err != nil {
		return err
	}
	return s.watchIO(s.timing.Write, func() error { return s.writer.WriteStatus(re, resp) })
}

func (s *hubSession) read(after time.Duration) (hubwire.RawFrame, error) {
	err := s.transport.SetReadDeadline(time.Now().Add(after))
	if err != nil && !errors.Is(err, os.ErrNoDeadline) {
		return hubwire.RawFrame{}, err
	}
	type result struct {
		raw hubwire.RawFrame
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, readErr := s.reader.Read()
		done <- result{raw: raw, err: readErr}
	}()
	timer := time.NewTimer(after)
	defer timer.Stop()
	select {
	case got := <-done:
		return got.raw, got.err
	case <-timer.C:
		_ = s.transport.Close()
		return hubwire.RawFrame{}, os.ErrDeadlineExceeded
	}
}

func (s *hubSession) prepareWriteDeadline() error {
	err := s.transport.SetWriteDeadline(time.Now().Add(s.timing.Write))
	if errors.Is(err, os.ErrNoDeadline) {
		return nil
	}
	return err
}

// watchIO is the deadline that production SSH carriage actually enforces.
// A forced command reads and writes inherited pipe fds; os.File.SetDeadline
// returns os.ErrNoDeadline for those files, so a deadline attached to the
// blocked call is inert. This independent timer closes the transport and
// returns a deadline error even if the OS does not interrupt that call, so the
// session reaches its single defer path; that existing path remains the only
// place a held lease is released.
// hubSessionCloseGrace is how long an asynchronous close waits for an in-flight
// write to finish before closing anyway.
//
// #176: serveHubSession's ctx.Done goroutine and its one-shot timer both closed
// the transport with nothing coordinating against a write in progress, and in
// production ctx is signal.NotifyContext(SIGTERM, SIGHUP). sshd SIGHUPs a
// forced command when its channel goes away, so an ordinary disconnect at the
// wrong microsecond cut a control line in half and the peer reported a
// truncated control frame — "the hub looks broken" for a hub that is fine.
//
// Why a GRACE and not a mutex: those closers ARE the unwedging mechanism. A
// lock that makes Close wait for the write can hang a SIGTERM'd session
// forever, and the deadline that would bound it is not reliably available —
// stdioHubTransport wraps inherited os.Stdin/os.Stdout, which are typically not
// pollable, so SetWriteDeadline returns ErrNoDeadline. A bounded wait keeps the
// unwedge and buys the common case, where the write is milliseconds from done.
//
// Polling rather than signalling, deliberately: at 5ms it is at most 50 wakeups
// on a path that runs once per session, and it cannot miss a wakeup or deadlock
// the way a condition variable racing a Close can.
const (
	hubSessionCloseGrace     = 250 * time.Millisecond
	hubSessionCloseGracePoll = 5 * time.Millisecond
)

// closeAfterWriteIdle closes the transport, giving an in-flight write a bounded
// chance to finish first. It always closes.
func (s *hubSession) closeAfterWriteIdle() {
	deadline := s.now().Add(hubSessionCloseGrace)
	for s.writeInFlight.Load() && s.now().Before(deadline) {
		time.Sleep(hubSessionCloseGracePoll)
	}
	_ = s.transport.Close()
}

func (s *hubSession) watchIO(after time.Duration, operation func() error) error {
	done := make(chan error, 1)
	go func() {
		s.writeInFlight.Store(true)
		defer s.writeInFlight.Store(false)
		done <- operation()
	}()
	timer := time.NewTimer(after)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		// Deliberately the RAW close, not the graceful one: this fires because
		// the write is stuck, so waiting for it to become idle would wait for
		// exactly the thing that is not happening.
		_ = s.transport.Close()
		return os.ErrDeadlineExceeded
	}
}

var poolIDPattern = regexp.MustCompile(`^[0-9a-f]{12}\n$`)

func validateHubPool(poolArg, expectedID, agentID string) (string, *loop.Config, string, error) {
	abs, err := filepath.Abs(poolArg)
	if err != nil {
		return "", nil, "", hubPoolNotFoundError(poolArg, agentID)
	}
	pool := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(pool); err == nil {
		pool = resolved
	}
	spec, specErr := os.Stat(filepath.Join(pool, "AGENTCHUTE.md"))
	loopDir := filepath.Join(pool, ".agentchute", "loop")
	loopInfo, loopErr := os.Stat(loopDir)
	if specErr != nil || loopErr != nil || !spec.Mode().IsRegular() || !loopInfo.IsDir() {
		return "", nil, "", hubPoolNotFoundError(poolArg, agentID)
	}
	cfg := &loop.Config{ControlRepo: pool, LoopDir: loopDir, Vendor: "agentchute", ControlRepoOrigin: "hub", LoopDirOrigin: "hub"}
	identityPath := filepath.Join(loopDir, "state", "pool.id")
	data, err := loop.ReadFileLimit(identityPath, 64)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, "", hubPoolMismatchError(pool, expectedID, "<absent>", agentID)
	}
	if err != nil {
		return "", nil, "", invalidPoolID(identityPath)
	}
	info, err := os.Lstat(identityPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !poolIDPattern.Match(data) {
		return "", nil, "", invalidPoolID(identityPath)
	}
	actual := string(data[:12])
	if actual != expectedID {
		return "", nil, "", hubPoolMismatchError(pool, expectedID, actual, agentID)
	}
	return pool, cfg, actual, nil
}

func invalidPoolID(path string) error {
	return &hubwire.ProtocolError{Code: hubwire.CodePoolIDInvalid, Msg: fmt.Sprintf("hub: %s is not a valid pool identity (must be a regular 0600 file containing exactly 12 lowercase hex characters)", path)}
}
