package op

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func newLocalChannel(t *testing.T, cfg *loop.Config, agentID string) (*Channel, *loop.ServeLease) {
	t.Helper()
	enroll(t, cfg, agentID)
	lease, err := loop.AcquireServeLease(cfg, agentID)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := loop.Registration{
		AgentID:         agentID,
		ProtocolVersion: loop.CurrentProtocolVersion,
		Vendor:          "test",
		ControlRepo:     cfg.ControlRepo,
		WorkingRepos:    []string{cfg.ControlRepo},
		Host:            "test-host",
	}
	ch := NewChannel(cfg, Context{ActorID: agentID}, ChannelOpts{Lease: lease, HeartbeatTemplate: &tmpl})
	return ch, lease
}

// The adoption arm: the local runner acquires its own lease and hands it to the
// channel, so the pinned startup order (lease → child → register) is untouched
// and Token() still feeds the child's AGENTCHUTE_SERVE_TOKEN.
func TestChannelAdoptsAnAlreadyHeldLease(t *testing.T) {
	cfg := newPool(t)
	ch, lease := newLocalChannel(t, cfg, "runner-test")
	defer func() { _ = ch.ReleaseLease() }()

	if ch.Token() != lease.Token || ch.Token() == "" {
		t.Fatalf("Token() = %q, want the adopted lease's token", ch.Token())
	}
}

// One tick is renew + heartbeat + throttled sweep + pending count, in that
// order. The heartbeat is what self-heals a row a sweep removed since the last
// tick, and the sweep is the pool's only hygiene path while a runner is up.
func TestChannelTickHeartbeatsSweepsAndCounts(t *testing.T) {
	cfg := newPool(t)
	ch, _ := newLocalChannel(t, cfg, "runner-test")
	defer func() { _ = ch.ReleaseLease() }()
	enroll(t, cfg, "dead-peer")
	backdate(t, cfg, "dead-peer", time.Now().UTC().Add(-100*24*time.Hour))
	enroll(t, cfg, "sender")
	deliver(t, cfg, "sender", "runner-test", "body")

	// The row is gone; the heartbeat must bring it back.
	if err := os.Remove(cfg.AgentRegistrationPath("runner-test")); err != nil {
		t.Fatal(err)
	}

	resp, err := ch.Tick(TickReq{})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("warnings = %v, want a clean tick", resp.Warnings)
	}
	if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath("runner-test")); err != nil {
		t.Fatalf("the heartbeat did not self-heal the swept row: %v", err)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("dead-peer")); !os.IsNotExist(err) {
		t.Fatalf("the first tick is due immediately and must sweep: %v", err)
	}
	if len(resp.Swept) != 1 || resp.Swept[0] != "dead-peer" {
		t.Fatalf("swept = %v, want the dead peer", resp.Swept)
	}
	if resp.Pending != 1 || resp.Skipped != 0 {
		t.Fatalf("counts = %d/%d, want 1 pending", resp.Pending, resp.Skipped)
	}
}

// The 10-minute throttle: a second tick must not re-sweep, and it must not
// re-report the first tick's sweep either.
func TestChannelTickThrottlesTheSweep(t *testing.T) {
	cfg := newPool(t)
	ch, _ := newLocalChannel(t, cfg, "runner-test")
	defer func() { _ = ch.ReleaseLease() }()

	if _, err := ch.Tick(TickReq{}); err != nil {
		t.Fatal(err)
	}
	enroll(t, cfg, "dead-peer")
	backdate(t, cfg, "dead-peer", time.Now().UTC().Add(-100*24*time.Hour))

	resp, err := ch.Tick(TickReq{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Swept) != 0 {
		t.Fatalf("swept = %v, want the throttle to hold the second tick", resp.Swept)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("dead-peer")); err != nil {
		t.Fatalf("the throttled tick swept anyway: %v", err)
	}
}

// Being FENCED is terminal: another serve now owns this id, so the tick must
// stop rather than continue as a dup-writer. It is the only hard error a tick
// returns, and it returns before the sweep.
func TestChannelTickFencedIsTerminal(t *testing.T) {
	cfg := newPool(t)
	ch, _ := newLocalChannel(t, cfg, "runner-test")
	enroll(t, cfg, "dead-peer")
	backdate(t, cfg, "dead-peer", time.Now().UTC().Add(-100*24*time.Hour))

	if _, err := loop.InvalidateAllServeLeases(cfg); err != nil {
		t.Fatal(err)
	}

	resp, err := ch.Tick(TickReq{})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("err = %v, want ErrFenced", err)
	}
	if CodeFor(err) != "E_FENCED" {
		t.Fatalf("code = %q, want E_FENCED", CodeFor(err))
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("warnings = %v, want the tick to stop at the fence", resp.Warnings)
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("dead-peer")); err != nil {
		t.Fatalf("a fenced tick must not go on to sweep: %v", err)
	}
}

// A non-fatal step failure is ONE warning carrying the exact string the runner
// log prints — the runner re-logs it verbatim, so the log stays byte-identical.
func TestChannelTickReportsStepFailuresAsWarnings(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "runner-test")
	lease, err := loop.AcquireServeLease(cfg, "runner-test")
	if err != nil {
		t.Fatal(err)
	}
	// A template missing the fields HeartbeatRegistration validates makes that
	// one step fail while everything else still runs.
	tmpl := loop.Registration{AgentID: "runner-test"}
	ch := NewChannel(cfg, Context{ActorID: "runner-test"}, ChannelOpts{Lease: lease, HeartbeatTemplate: &tmpl})
	defer func() { _ = ch.ReleaseLease() }()

	resp, err := ch.Tick(TickReq{})
	if err != nil {
		t.Fatalf("a heartbeat failure must not be a hard error: %v", err)
	}
	if len(resp.Warnings) != 1 || !strings.HasPrefix(resp.Warnings[0], "agentchute serve: heartbeat registration: ") {
		t.Fatalf("warnings = %v, want the runner's own heartbeat line", resp.Warnings)
	}
	if strings.HasSuffix(resp.Warnings[0], "\n") {
		t.Fatal("a warning carries no trailing newline; the renderer adds it")
	}
}

// Fail OPEN on a listing failure: an extra spurious cue is acceptable, a
// suppressed real one is not.
func TestChannelTickAndPendingHelperFailOpen(t *testing.T) {
	cfg := newPool(t)
	ch, _ := newLocalChannel(t, cfg, "runner-test")
	defer func() { _ = ch.ReleaseLease() }()
	if err := os.Chmod(cfg.AgentInboxDir("runner-test"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(cfg.AgentInboxDir("runner-test"), 0o700) }()

	resp, err := ch.Tick(TickReq{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Pending == 0 {
		t.Fatal("an unreadable inbox must report pending, not empty")
	}
	if !HasPendingInboxMail(cfg, "runner-test") {
		t.Fatal("the standalone check must fail open the same way")
	}
}

// A missing inbox is NOT a listing failure: it is an empty one.
func TestPendingHelperTreatsAMissingInboxAsEmpty(t *testing.T) {
	cfg := newPool(t)
	if HasPendingInboxMail(cfg, "never-booted") {
		t.Fatal("a missing inbox must read as empty, not pending")
	}
}

// The hub arm: no adopted lease, so the channel acquires one itself, and a
// second live serve is refused.
func TestChannelAcquireLeaseRefusesASecondLiveServe(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "runner-test")
	first := NewChannel(cfg, Context{ActorID: "runner-test"}, ChannelOpts{})
	if _, err := first.AcquireLease(LeaseReq{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.ReleaseLease() }()
	if first.Token() == "" {
		t.Fatal("AcquireLease did not record the token")
	}

	second := NewChannel(cfg, Context{ActorID: "runner-test"}, ChannelOpts{})
	_, err := second.AcquireLease(LeaseReq{})
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld", err)
	}
	if CodeFor(err) != "E_LEASE_HELD" {
		t.Fatalf("code = %q, want E_LEASE_HELD", CodeFor(err))
	}
}

// §6.1 step order: a channel that acquired a lease but has not registered has
// no heartbeat template, and a tick without one is refused rather than skipped
// silently.
func TestChannelTickBeforeRegisterIsAnOrderError(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "runner-test")
	ch := NewChannel(cfg, Context{ActorID: "runner-test"}, ChannelOpts{})
	if _, err := ch.AcquireLease(LeaseReq{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ch.ReleaseLease() }()

	if _, err := ch.Tick(TickReq{}); !errors.Is(err, ErrOrder) {
		t.Fatalf("err = %v, want ErrOrder", err)
	}
	if CodeFor(ErrOrder) != "E_ORDER" {
		t.Fatalf("code = %q, want E_ORDER", CodeFor(ErrOrder))
	}

	// Registering supplies the template — and injects the held token, so the
	// channel's own registration is never refused by its own lease.
	vendor := "test"
	if _, err := ch.Register(RegisterReq{Vendor: &vendor, Host: "test-host"}); err != nil {
		t.Fatalf("channel register: %v", err)
	}
	if _, err := ch.Tick(TickReq{}); err != nil {
		t.Fatalf("tick after register: %v", err)
	}
}

// The order check must not be conditional on holding a lease: a FRESH channel
// that ticks before it registers has no heartbeat template either way, and
// nesting the check under the lease branch let that case return nil and quietly
// do a subset of the work (codex, PR #148 gate).
func TestChannelFreshTickBeforeRegisterIsAnOrderError(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "runner-test")
	ch := NewChannel(cfg, Context{ActorID: "runner-test"}, ChannelOpts{})

	resp, err := ch.Tick(TickReq{})
	if !errors.Is(err, ErrOrder) {
		t.Fatalf("err = %v, want ErrOrder on a channel that never registered", err)
	}
	if len(resp.Warnings) != 0 || resp.Pending != 0 || len(resp.Swept) != 0 {
		t.Fatalf("resp = %+v, want no work done before the order check", resp)
	}
}

func TestChannelReleaseLeaseIsSafeWithoutOne(t *testing.T) {
	cfg := newPool(t)
	ch := NewChannel(cfg, Context{ActorID: "runner-test"}, ChannelOpts{})
	if err := ch.ReleaseLease(); err != nil {
		t.Fatalf("releasing a lease-less channel: %v", err)
	}
}
