package op

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestRegisterWritesTheRowAtTheInjectedInstant(t *testing.T) {
	cfg := newPool(t)
	now := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	vendor := "anthropic"

	resp, err := Register(cfg, Context{ActorID: "claude-code"}, RegisterReq{Vendor: &vendor, Host: "box-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExistingFound || !resp.Refreshed {
		t.Fatalf("resp = %+v, want a fresh write reported as refreshed", resp)
	}
	if !resp.Reg.LastSeen.Equal(now) {
		t.Fatalf("last_seen = %s, want the injected instant %s", resp.Reg.LastSeen, now)
	}
	if resp.ResolvedHost != "box-1" || resp.Reg.Host != "box-1" {
		t.Fatalf("resp = %+v, want the caller-resolved host recorded verbatim", resp)
	}
	if resp.InboxDir == "" {
		t.Fatal("resp dropped the inbox dir its callers render")
	}
	if _, err := os.Stat(resp.InboxDir); err != nil {
		t.Fatalf("the inbox must exist before the registration is visible: %v", err)
	}

	// A second call finds the existing row.
	again, err := Register(cfg, Context{ActorID: "claude-code"}, RegisterReq{Vendor: &vendor, Host: "box-1"}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !again.ExistingFound {
		t.Fatalf("resp = %+v, want the pre-write existence fact", again)
	}
}

// An explicit empty host stays empty: the hostname substitution is the CALLER's
// (D1a), so the op must never fill one in.
func TestRegisterNeverSubstitutesAHostname(t *testing.T) {
	cfg := newPool(t)
	vendor := "anthropic"
	resp, err := Register(cfg, Context{ActorID: "claude-code"}, RegisterReq{Vendor: &vendor, Host: ""}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if resp.ResolvedHost != "" || resp.Reg.Host != "" {
		t.Fatalf("resp = %+v, want an explicit empty host preserved", resp)
	}
}

// Bio presence: nil keeps whatever is on disk, non-nil sets it, and an explicit
// empty clears it. Without the pointer every re-register would clobber a
// hand-set bio.
func TestRegisterBioPresenceSemantics(t *testing.T) {
	cfg := newPool(t)
	vendor := "anthropic"
	actor := Context{ActorID: "claude-code"}
	now := time.Now().UTC()

	bio := "the original bio"
	if _, err := Register(cfg, actor, RegisterReq{Vendor: &vendor, Bio: &bio}, now); err != nil {
		t.Fatal(err)
	}
	kept, err := Register(cfg, actor, RegisterReq{Vendor: &vendor}, now)
	if err != nil {
		t.Fatal(err)
	}
	// The body round-trips through the registration file, which is
	// newline-terminated on disk; the presence semantics are what matter here.
	if strings.TrimSpace(kept.Reg.Body) != "the original bio" {
		t.Fatalf("body = %q, want an absent bio to preserve the existing one", kept.Reg.Body)
	}
	empty := ""
	cleared, err := Register(cfg, actor, RegisterReq{Vendor: &vendor, Bio: &empty}, now)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Reg.Body != "" {
		t.Fatalf("body = %q, want an explicit empty bio to clear it", cleared.Reg.Body)
	}
}

// A nil vendor is today's missing-vendor refusal, byte for byte: hub-side
// resolution lands with the client call-site branch that first produces a nil.
func TestRegisterRefusesAMissingVendor(t *testing.T) {
	cfg := newPool(t)
	for _, req := range []RegisterReq{{}, {Vendor: ptr("")}, {Vendor: ptr("   ")}} {
		_, err := Register(cfg, Context{ActorID: "claude-code"}, req, time.Now().UTC())
		if err == nil || !strings.HasPrefix(err.Error(), "missing --vendor (recommended values:") {
			t.Fatalf("err = %v, want the shipped missing-vendor refusal", err)
		}
	}
}

// A FRESH serve claim owned by another process refuses the registration; the
// lane's own token is what lets its one-shot ops through.
func TestRegisterRefusesWhenLiveElsewhereUnlessTheTokenMatches(t *testing.T) {
	cfg := newPool(t)
	vendor := "anthropic"
	actor := Context{ActorID: "claude-code"}
	if _, err := Register(cfg, actor, RegisterReq{Vendor: &vendor}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	lease, err := loop.AcquireServeLease(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loop.ReleaseLease(lease) }()

	if _, err := Register(cfg, actor, RegisterReq{Vendor: &vendor}, time.Now().UTC()); err == nil {
		t.Fatal("a tokenless register must be refused while a fresh claim owns the id")
	} else if !strings.Contains(err.Error(), "is live elsewhere") {
		t.Fatalf("err = %v, want the live-elsewhere refusal", err)
	}
	if _, err := Register(cfg, actor, RegisterReq{Vendor: &vendor, ServeToken: lease.Token}, time.Now().UTC()); err != nil {
		t.Fatalf("the lane's own token must be accepted: %v", err)
	}
}

// Sweep is boot's discriminator, and its ORDER matters (C11): register self
// first, then sweep peers, so the caller's own row is never the one swept.
func TestRegisterSweepsOnlyWhenAskedAndOnlyAfterTheWrite(t *testing.T) {
	for _, sweep := range []bool{false, true} {
		cfg := newPool(t)
		vendor := "anthropic"
		// A long-dead peer with no claim: exactly what a sweep removes.
		if _, err := Register(cfg, Context{ActorID: "dead-peer"}, RegisterReq{Vendor: &vendor}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		backdate(t, cfg, "dead-peer", time.Now().UTC().Add(-100*24*time.Hour))

		resp, err := Register(cfg, Context{ActorID: "claude-code"}, RegisterReq{Vendor: &vendor, Sweep: sweep}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Warnings) != 0 {
			t.Fatalf("warnings = %v, want none on a clean sweep", resp.Warnings)
		}
		_, statErr := os.Stat(cfg.AgentRegistrationPath("dead-peer"))
		if sweep && !os.IsNotExist(statErr) {
			t.Fatalf("Sweep:true left the dead row: %v", statErr)
		}
		if !sweep && statErr != nil {
			t.Fatalf("Sweep:false swept anyway: %v", statErr)
		}
		// The caller's own row survives its own sweep, fresh.
		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath("claude-code")); err != nil {
			t.Fatalf("the registrant's own row must survive its sweep: %v", err)
		}
	}
}

// Announce is a VIEW, not a count: the CLI prints three separate facts from it,
// and a remote lane's fan-out runs hub-side where it cannot recompute them.
func TestRegisterAnnounceReportsSentTotalAndPerPeerWarnings(t *testing.T) {
	cfg := newPool(t)
	vendor := "anthropic"
	enroll(t, cfg, "reachable-peer")
	enroll(t, cfg, "dead-peer")
	// A stale peer is skipped with a warning, exactly like any other per-peer
	// failure.
	backdate(t, cfg, "dead-peer", time.Now().UTC().Add(-100*24*time.Hour))

	resp, err := Register(cfg, Context{ActorID: "claude-code"}, RegisterReq{Vendor: &vendor, Announce: true}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Announce == nil {
		t.Fatal("Announce:true produced no view")
	}
	if resp.Announce.Total != 2 || resp.Announce.Sent != 1 {
		t.Fatalf("announce = %+v, want 1 of 2 peers reached", resp.Announce)
	}
	if len(resp.Announce.Warnings) != 1 {
		t.Fatalf("announce warnings = %v, want the undeliverable peer reported", resp.Announce.Warnings)
	}
	// The reachable peer really did get an inbox message.
	if n := countFiles(t, cfg.AgentInboxDir("reachable-peer")); n != 1 {
		t.Fatalf("peer inbox = %d files, want the announcement", n)
	}
	// Without the flag there is no view and no fan-out at all.
	quiet, err := Register(cfg, Context{ActorID: "claude-code"}, RegisterReq{Vendor: &vendor}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Announce != nil {
		t.Fatalf("announce = %+v, want nil without the flag", quiet.Announce)
	}
}

// The response's registration view must round-trip every field: `boot` and
// `self-check` render from it, and a lossy view would silently blank a row.
func TestRegistrationViewRoundTripsEveryField(t *testing.T) {
	reg := &loop.Registration{
		AgentID:         "claude-code",
		ProtocolVersion: loop.CurrentProtocolVersion,
		Vendor:          "anthropic",
		ControlRepo:     "/repo",
		WorkingRepos:    []string{"/repo", "/other"},
		Host:            "box-1",
		LastSeen:        time.Now().UTC().Truncate(time.Second),
		Body:            "a bio",
	}
	got := RegistrationViewOf(reg).Registration()
	if got.AgentID != reg.AgentID || got.ProtocolVersion != reg.ProtocolVersion || got.Vendor != reg.Vendor ||
		got.ControlRepo != reg.ControlRepo || got.Host != reg.Host || !got.LastSeen.Equal(reg.LastSeen) ||
		got.Body != reg.Body || strings.Join(got.WorkingRepos, ",") != strings.Join(reg.WorkingRepos, ",") {
		t.Fatalf("round trip = %+v, want %+v", got, reg)
	}
}

func TestRegisterRejectsAnInvalidAgentID(t *testing.T) {
	cfg := newPool(t)
	vendor := "anthropic"
	_, err := Register(cfg, Context{ActorID: "Not Valid"}, RegisterReq{Vendor: &vendor}, time.Now().UTC())
	if err == nil {
		t.Fatal("an invalid agent id must be refused before anything is written")
	}
	if errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want the id-validation error", err)
	}
}

func ptr(s string) *string { return &s }
