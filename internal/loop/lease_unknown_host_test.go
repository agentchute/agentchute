package loop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// grok's #167 sweep, category 1: "an absent or unreadable thing reported as a
// satisfied condition". Here the thing is this machine's own name.
//
// AcquireServeLease discarded the os.Hostname error, so host became "". The
// claim records the host and the liveness rules read it back: a STALE claim
// whose pid is still alive is kept only when it belongs to THIS host, because a
// pid on another machine says nothing about ours. With host = "", a
// frozen-but-alive lane on this very machine looked cross-host — and its id was
// stolen out from under it. The empty value also poisons the record for every
// later acquirer.

func TestAcquireServeLeaseRefusesWhenTheHostIsUnknown(t *testing.T) {
	cfg := newTestPool(t)
	prev := leaseHostname
	leaseHostname = func() (string, error) { return "", errors.New("hostname unavailable") }
	t.Cleanup(func() { leaseHostname = prev })

	lease, err := AcquireServeLease(cfg, "alice")
	if err == nil {
		t.Fatal("acquired a lease without being able to name this host; the claim it writes would be unusable evidence")
	}
	if lease != nil {
		t.Fatal("returned a lease alongside an error")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Fatalf("the error does not say what could not be determined: %v", err)
	}
	// Nothing may be left behind: a claim carrying an empty host is exactly the
	// record that breaks the next acquirer's comparison.
	if _, statErr := os.Stat(claimPath(cfg, "alice")); statErr == nil {
		t.Fatal("a claim was written despite the refusal")
	}
	// An empty name with no error is the same problem wearing a success.
	leaseHostname = func() (string, error) { return "   ", nil }
	if _, err := AcquireServeLease(cfg, "alice"); err == nil {
		t.Fatal("a blank hostname with no error was accepted")
	}
}

// The other half, and the one that matters for pools that already exist: claims
// written by a binary that discarded the error carry Host:"". Reading that as a
// FOREIGN host is what steals a live lane, so an empty recorded host must mean
// "cannot prove it is elsewhere", not "elsewhere".
func TestStaleClaimWithAnUnknownHostIsNotStolenFromALivePID(t *testing.T) {
	cfg := newTestPool(t)
	withPidAlive(t, func(int) bool { return true })
	stale := time.Now().UTC().Add(-24 * time.Hour)
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       "", // what the discarded-error path wrote
		PID:        os.Getpid(),
		ServeToken: "tok-old",
		StartedAt:  stale,
		LastSeen:   stale,
	})

	if _, err := AcquireServeLease(cfg, "alice"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("acquire over a stale claim with an unknown host and a LIVE pid = %v, want ErrLeaseHeld", err)
	}

	// The control, or the row above would also pass for an implementation that
	// simply never steals: a stale claim from a genuinely different host, whose
	// pid means nothing here, must still be reclaimable.
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       "some-other-machine",
		PID:        os.Getpid(),
		ServeToken: "tok-old",
		StartedAt:  stale,
		LastSeen:   stale,
	})
	if _, err := AcquireServeLease(cfg, "alice"); err != nil {
		t.Fatalf("a stale claim from another host must be reclaimable: %v", err)
	}
}

func newTestPool(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{ControlRepo: root, LoopDir: filepath.Join(root, ".agentchute", "loop"), Vendor: "agentchute"}
}
