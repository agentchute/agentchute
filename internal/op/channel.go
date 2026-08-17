package op

import (
	"errors"
	"fmt"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// sweepInterval bounds how often a channel's tick runs the pool-wide stale
// sweep (C11). lastSweep's zero value makes the very first tick due
// immediately — harmless even right after boot's own sweep (nothing is stale
// seconds later) and useful for a pool whose runner is its only hygiene path.
var sweepInterval = 10 * time.Minute

// LeaseReq / LeaseResp are the serialized lease shapes (§3.4).
type LeaseReq struct{}

type LeaseResp struct {
	Token string `json:"token"`
}

// TickReq is empty: everything a tick needs is channel state.
type TickReq struct{}

// TickResp is one tick's outcome.
//
// Warnings carries the NON-FATAL step failures in production order, one entry
// each, carrying the exact string today's runner log receives minus its
// trailing newline — so the runner log stays byte-identical. `warnings` is
// always present (it is `[]string` on the wire, never omitted).
//
// Pending/Skipped are the raw inbox counts. A transient listing failure reports
// Pending:1 — the FAIL-OPEN direction the runner has always taken (an extra
// spurious cue is acceptable, a suppressed real one is not).
type TickResp struct {
	Pending  int      `json:"pending"`
	Skipped  int      `json:"skipped"`
	Swept    []string `json:"swept,omitempty"`
	Warnings []string `json:"warnings"`
}

// ChannelOpts is a CONSTRUCTOR input, not a wire struct.
//
// Lease non-nil ⇒ the channel ADOPTS an already-held lease, and Token() returns
// its token. That is the LOCAL runner's arm, and it is what keeps C10's pinned
// startup order literal: the runner still acquires through
// refuseLiveRunnerCollision, which stays byte-unchanged. Lease nil ⇒
// AcquireLease acquires and records the token (the hub session's arm and the
// remote channel's).
//
// HeartbeatTemplate non-nil ⇒ used verbatim; the local runner passes today's
// value, so no heartbeat byte changes. Nil ⇒ Register derives and caches the
// template from its request, and a Tick before Register is ErrOrder. M1
// exercises only the non-nil arms; both shapes are pinned now so the hub
// session does not have to reshape the seam.
type ChannelOpts struct {
	HeartbeatTemplate *loop.Registration
	Lease             *loop.ServeLease
}

// Channel is the stateful lease/tick handle. Tick cannot be a free function: it
// needs the registration heartbeat template, the sweep throttle, and the lease
// token ACROSS calls. This shape is inherited by the hub session and the remote
// serve channel, so it is pinned once, here.
//
// Not safe for concurrent use: one channel belongs to one lane's serve loop.
type Channel struct {
	cfg       *loop.Config
	ctx       Context
	lease     *loop.ServeLease
	template  *loop.Registration
	lastSweep time.Time
}

func NewChannel(cfg *loop.Config, ctx Context, opts ChannelOpts) *Channel {
	return &Channel{
		cfg:      cfg,
		ctx:      ctx,
		lease:    opts.Lease,
		template: opts.HeartbeatTemplate,
	}
}

// AcquireLease takes the serve lease for this channel's actor and records the
// token. ErrLeaseHeld means another live serve owns the id: a second one must
// not start. A stale/released claim is reclaimable, so the launch proceeds.
func (c *Channel) AcquireLease(LeaseReq) (LeaseResp, error) {
	lease, err := loop.AcquireServeLease(c.cfg, c.ctx.ActorID)
	if err != nil {
		return LeaseResp{}, err
	}
	c.lease = lease
	return LeaseResp{Token: lease.Token}, nil
}

// Token is the held fence epoch, for AGENTCHUTE_SERVE_TOKEN. Empty when the
// channel holds no lease.
func (c *Channel) Token() string {
	if c.lease == nil {
		return ""
	}
	return c.lease.Token
}

// Register runs the channel's registration step, injecting the held serve token
// so the lane's own registration is never refused as "live elsewhere" by its own
// lease (§3.5). When the channel was built without a heartbeat template, this is
// also where the template comes from — the tick needs one, and a validating
// template is exactly this request's payload.
func (c *Channel) Register(req RegisterReq) (RegisterResp, error) {
	return c.register(req, nil)
}

// RegisterWithPrecommitValidation is Register with a transport-owned response
// validator that runs before the registration write.
func (c *Channel) RegisterWithPrecommitValidation(req RegisterReq, validate func(RegisterResp) error) (RegisterResp, error) {
	return c.register(req, validate)
}

func (c *Channel) register(req RegisterReq, validate func(RegisterResp) error) (RegisterResp, error) {
	req.ServeToken = c.Token()
	now := time.Now().UTC()
	resp, err := register(c.cfg, c.ctx, req, now, validate)
	if err != nil {
		return resp, err
	}
	if c.template == nil {
		tmpl := resp.Reg.Registration()
		// The heartbeat template asserts identity/provenance only; Body and
		// WorkingRepos come from the on-disk row (see HeartbeatRegistration).
		tmpl.LastSeen = time.Time{}
		tmpl.Body = ""
		c.template = tmpl
	}
	return resp, nil
}

// Tick is the periodic composite, in the runner's exact order: renew the lease
// (fence verify), heartbeat the registration, run the throttled sweep, then
// count pending mail.
//
// It returns a non-nil error ONLY for the fenced case (ErrFenced) — being
// RECLAIMED is terminal for the lane, which must stop injecting and shut down
// rather than become a dup-writer — and for ErrOrder, a tick on a channel that
// has neither a template nor a Register behind it. Every other step failure is
// ONE Warnings entry and the tick continues, because that is what the runner
// has always done; turning one into an error would change behavior.
func (c *Channel) Tick(TickReq) (TickResp, error) {
	var resp TickResp
	resp.Warnings = []string{}
	now := time.Now().UTC()

	// §6.1 step order, checked FIRST and unconditionally: a tick needs the
	// heartbeat template the channel's own Register supplies, so a tick that
	// arrives before one is refused rather than quietly doing a subset of the
	// work. Checking it inside the lease branch let a fresh channel tick and
	// return nil (codex, PR #148 gate).
	if c.template == nil {
		return resp, ErrOrder
	}

	// nil lease: the poll-only arm (no fence to verify, no token to heartbeat
	// with). HeartbeatRegistration rejects an empty token outright, so these
	// two steps are skipped rather than called with one.
	if c.lease != nil {
		if err := loop.RenewLease(c.lease); err != nil {
			if errors.Is(err, loop.ErrFenced) {
				return resp, err
			}
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("agentchute serve: renew serve lease: %v", err))
		}
		if err := loop.HeartbeatRegistration(c.cfg, *c.template, c.lease.Token); err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("agentchute serve: heartbeat registration: %v", err))
		}
	}

	// Lazy sweep (C11): bounded, 10-minute cadence. The throttle advances even
	// when the sweep failed.
	if now.Sub(c.lastSweep) >= sweepInterval {
		swept, err := loop.SweepStaleRegistrations(c.cfg, c.ctx.ActorID, now)
		if err != nil {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("agentchute serve: sweep stale registrations: %v", err))
		}
		resp.Swept = swept
		c.lastSweep = now
	}

	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(c.cfg.AgentInboxDir(c.ctx.ActorID))
	if err != nil && !errors.Is(err, loop.ErrInboxMissing) {
		// Fail OPEN: report one pending rather than none. Preserves the safe
		// failure direction the runner's own listing check has always taken.
		resp.Pending = 1
		return resp, nil
	}
	resp.Pending = len(msgs)
	resp.Skipped = len(skipped)
	return resp, nil
}

// ReleaseLease releases the held lease. ErrFenced is a no-op there by design
// (we were already reclaimed). A channel holding no lease releases nothing.
func (c *Channel) ReleaseLease() error {
	if c.lease == nil {
		return nil
	}
	return loop.ReleaseLease(c.lease)
}

// HasPendingInboxMail reports whether the raw inbox (parsed messages or
// skipped/malformed files — either needs `check` to run) currently has
// anything in it. On a transient listing error it fails OPEN.
func HasPendingInboxMail(cfg *loop.Config, agentID string) bool {
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir(agentID))
	if err != nil && !errors.Is(err, loop.ErrInboxMissing) {
		return true
	}
	return len(msgs) > 0 || len(skipped) > 0
}
