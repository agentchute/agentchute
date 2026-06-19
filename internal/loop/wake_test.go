package loop

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// loopShortSocketPath returns a unix socket path short enough for the ~104-byte
// sun_path limit (deep t.TempDir() paths overflow it on darwin). Mirror of the
// root-package shortSocketPath helper.
func loopShortSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ac-loop-evil-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// loopAcceptCounter counts accepted connections on a real unix listener so a
// test can prove whether a socket was ever dialed.
type loopAcceptCounter struct {
	ln       net.Listener
	accepted *int64
}

func (l *loopAcceptCounter) count() int64 { return atomic.LoadInt64(l.accepted) }

// loopListenCounting binds a real unix socket at path and drains accepts into a
// counter. Mirror of the root-package listenCounting helper.
func loopListenCounting(t *testing.T, path string) *loopAcceptCounter {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	var accepted int64
	l := &loopAcceptCounter{ln: ln, accepted: &accepted}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&accepted, 1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return l
}

type fakeAdapter struct {
	calls   int
	lastCtx context.Context
	lastTgt string
	err     error

	// Reachable behavior + observation. reachableCalls counts Reachable
	// invocations; reachableReg records the last reg passed; reachable is the
	// value returned.
	reachableCalls int
	reachableReg   *Registration
	reachable      bool
}

func (f *fakeAdapter) Poke(ctx context.Context, target string) error {
	f.calls++
	f.lastCtx = ctx
	f.lastTgt = target
	return f.err
}

func (f *fakeAdapter) Reachable(_ *Config, reg *Registration, _ time.Duration) bool {
	f.reachableCalls++
	f.reachableReg = reg
	return f.reachable
}

func TestPokeWakeTargetEmptyMethodIsNoop(t *testing.T) {
	if err := PokeWakeTarget("", "anything"); err != nil {
		t.Fatalf("empty method should be no-op, got %v", err)
	}
	if err := PokeWakeTarget("   ", "anything"); err != nil {
		t.Fatalf("whitespace method should be no-op, got %v", err)
	}
}

func TestPokeWakeTargetUnsupportedMethodErrors(t *testing.T) {
	// No adapter registered for "nothing-like-this".
	err := PokeWakeTarget("nothing-like-this", "ignored")
	if err == nil {
		t.Fatal("expected unsupported wake_method error")
	}
	if !strings.Contains(err.Error(), "unsupported wake_method") {
		t.Fatalf("expected unsupported wake_method error, got %v", err)
	}
}

func TestPokeWakeTargetDispatchesToRegisteredAdapter(t *testing.T) {
	fake := &fakeAdapter{}
	RegisterWakeAdapter("test-fake", fake)
	t.Cleanup(func() { RegisterWakeAdapter("test-fake", noopRemovedAdapter{}) })

	if err := PokeWakeTarget("test-fake", "%99"); err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("adapter Poke calls = %d, want 1", fake.calls)
	}
	if fake.lastTgt != "%99" {
		t.Fatalf("adapter target = %q, want %q", fake.lastTgt, "%99")
	}
}

func TestPokeWakeTargetSurfacesAdapterError(t *testing.T) {
	wantErr := errors.New("boom")
	RegisterWakeAdapter("test-fail", &fakeAdapter{err: wantErr})
	t.Cleanup(func() { RegisterWakeAdapter("test-fail", noopRemovedAdapter{}) })

	err := PokeWakeTarget("test-fail", "x")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped boom error, got %v", err)
	}
}

func TestWakeAdapterLookupReturnsNilForUnregistered(t *testing.T) {
	if a := wakeAdapterFor("none-registered-here"); a != nil {
		t.Fatalf("expected nil adapter, got %T", a)
	}
}

func TestTmuxAdapterIsRegisteredByDefault(t *testing.T) {
	if a := wakeAdapterFor("tmux"); a == nil {
		t.Fatal("expected tmux adapter to be registered via init()")
	}
}

// RegisterWakeAdapter must silently ignore empty methods and nil
// adapters: bad input from one caller should not poison a process-global
// registry. Pair-asserts via wakeAdapterFor that the registry stayed
// clean.
func TestRegisterWakeAdapterRejectsEmptyAndNil(t *testing.T) {
	// Empty method.
	RegisterWakeAdapter("", &fakeAdapter{})
	if a := wakeAdapterFor(""); a != nil {
		t.Fatal("empty method should not register an adapter")
	}
	// Whitespace-only method.
	RegisterWakeAdapter("   ", &fakeAdapter{})
	if a := wakeAdapterFor("   "); a != nil {
		t.Fatal("whitespace method should not register an adapter")
	}
	// Nil adapter.
	RegisterWakeAdapter("test-nil-adapter", nil)
	if a := wakeAdapterFor("test-nil-adapter"); a != nil {
		t.Fatal("nil adapter should not register")
	}
}

// RegisterWakeAdapter trims the method key so callers can look up the
// same adapter with or without surrounding whitespace.
func TestRegisterWakeAdapterTrimsMethodKey(t *testing.T) {
	RegisterWakeAdapter("  test-trim  ", &fakeAdapter{})
	t.Cleanup(func() { RegisterWakeAdapter("test-trim", noopRemovedAdapter{}) })

	if a := wakeAdapterFor("test-trim"); a == nil {
		t.Fatal("expected adapter registered with trimmed key")
	}
	if a := wakeAdapterFor(" test-trim "); a == nil {
		t.Fatal("expected lookup with whitespace to find trimmed adapter")
	}
}

// noopRemovedAdapter is a placeholder used when a test wants to "remove" a
// registered adapter (the registry has no Delete; overwriting with a no-op
// is the cleanup pattern). Returns nil from Poke; callers in tests don't
// invoke it.
type noopRemovedAdapter struct{}

func (noopRemovedAdapter) Poke(context.Context, string) error { return nil }

func (noopRemovedAdapter) Reachable(*Config, *Registration, time.Duration) bool { return false }

// installCountingRunnerAdapter swaps the agentchute-run adapter for one that
// counts dials, restoring the real runner adapter on cleanup. Returns the
// counter so a test can assert a refused poke never reached the dial.
func installCountingRunnerAdapter(t *testing.T) *fakeAdapter {
	t.Helper()
	fake := &fakeAdapter{}
	RegisterWakeAdapter(RunnerWakeMethod, fake)
	t.Cleanup(func() { RegisterWakeAdapter(RunnerWakeMethod, runnerWakeAdapter{}) })
	return fake
}

func TestPokeRegistration_RefusesUnownedRunnerSocket(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	dialed := installCountingRunnerAdapter(t)

	reg := &Registration{
		AgentID:    "codex",
		WakeMethod: RunnerWakeMethod,
		WakeTarget: "unix:/tmp/evil.sock", // not a path codex owns
	}
	err := PokeRegistration(context.Background(), cfg, reg)
	if err == nil {
		t.Fatal("expected refusal for unowned runner socket")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error %q should mention refusal", err)
	}
	if dialed.calls != 0 {
		t.Fatalf("unowned runner socket was dialed %d times, want 0", dialed.calls)
	}
}

func TestPokeRegistration_OwnedSocketProceeds(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	dialed := installCountingRunnerAdapter(t)

	owned := RunnerWakeTarget(cfg.RunnerSocketPath("codex"))
	reg := &Registration{
		AgentID:    "codex",
		WakeMethod: RunnerWakeMethod,
		WakeTarget: owned,
	}
	if err := PokeRegistration(context.Background(), cfg, reg); err != nil {
		t.Fatalf("owned runner socket should poke, got %v", err)
	}
	if dialed.calls != 1 {
		t.Fatalf("owned runner socket dialed %d times, want 1", dialed.calls)
	}
	if dialed.lastTgt != owned {
		t.Fatalf("dialed target = %q, want %q", dialed.lastTgt, owned)
	}
}

func TestPokeRegistration_TmuxHerdrUnaffected(t *testing.T) {
	cfg := setupAnnounceFixture(t)

	tmuxFake := &fakeAdapter{}
	RegisterWakeAdapter("tmux", tmuxFake)
	t.Cleanup(func() { RegisterWakeAdapter("tmux", tmuxAdapter{}) })

	herdrFake := &fakeAdapter{}
	RegisterWakeAdapter("herdr", herdrFake)
	t.Cleanup(func() { RegisterWakeAdapter("herdr", noopRemovedAdapter{}) })

	tmuxReg := &Registration{AgentID: "codex", WakeMethod: "tmux", WakeTarget: "%0"}
	if err := PokeRegistration(context.Background(), cfg, tmuxReg); err != nil {
		t.Fatalf("tmux poke failed: %v", err)
	}
	if tmuxFake.calls != 1 || tmuxFake.lastTgt != "%0" {
		t.Fatalf("tmux adapter calls=%d tgt=%q, want 1/%q", tmuxFake.calls, tmuxFake.lastTgt, "%0")
	}

	herdrReg := &Registration{AgentID: "codex", WakeMethod: "herdr", WakeTarget: "codex-agentchute"}
	if err := PokeRegistration(context.Background(), cfg, herdrReg); err != nil {
		t.Fatalf("herdr poke failed: %v", err)
	}
	if herdrFake.calls != 1 || herdrFake.lastTgt != "codex-agentchute" {
		t.Fatalf("herdr adapter calls=%d tgt=%q, want 1/%q", herdrFake.calls, herdrFake.lastTgt, "codex-agentchute")
	}
}

func TestPokeRegistration_NilRegIsNoop(t *testing.T) {
	cfg := setupAnnounceFixture(t)
	if err := PokeRegistration(context.Background(), cfg, nil); err != nil {
		t.Fatalf("nil reg should be a no-op, got %v", err)
	}
}

// TestRegistrationReachable_DispatchesThroughAdapter proves the dispatcher
// consults the registered adapter's Reachable method (not a hardcoded switch):
// a fake adapter is registered for a custom method and its return value is
// surfaced verbatim, and it receives the full reg.
func TestRegistrationReachable_DispatchesThroughAdapter(t *testing.T) {
	cfg := setupAnnounceFixture(t)

	fake := &fakeAdapter{reachable: true}
	RegisterWakeAdapter("test-reach", fake)
	t.Cleanup(func() { RegisterWakeAdapter("test-reach", noopRemovedAdapter{}) })

	reg := &Registration{AgentID: "codex", WakeMethod: "test-reach", WakeTarget: "tgt-1"}
	if !RegistrationReachable(cfg, reg, time.Second) {
		t.Fatal("dispatcher should surface adapter.Reachable()=true")
	}
	if fake.reachableCalls != 1 {
		t.Fatalf("adapter.Reachable calls = %d, want 1", fake.reachableCalls)
	}
	if fake.reachableReg != reg {
		t.Fatal("dispatcher did not pass the registration through to the adapter")
	}

	// Flip the fake to unreachable and confirm the dispatcher surfaces that too.
	fake.reachable = false
	if RegistrationReachable(cfg, reg, time.Second) {
		t.Fatal("dispatcher should surface adapter.Reachable()=false")
	}
}

// TestRegistrationReachable_NotReachableEdges covers the short-circuit arms that
// must report unreachable WITHOUT consulting any adapter — matching the old
// switch's empty-target short-circuit and default (unknown-method) arm exactly.
func TestRegistrationReachable_NotReachableEdges(t *testing.T) {
	cfg := setupAnnounceFixture(t)

	if RegistrationReachable(cfg, nil, time.Second) {
		t.Fatal("nil reg must be unreachable")
	}
	// Empty wake_target: unreachable, and the adapter is never consulted.
	fake := &fakeAdapter{reachable: true}
	RegisterWakeAdapter("test-empty", fake)
	t.Cleanup(func() { RegisterWakeAdapter("test-empty", noopRemovedAdapter{}) })
	if RegistrationReachable(cfg, &Registration{WakeMethod: "test-empty", WakeTarget: ""}, time.Second) {
		t.Fatal("empty wake_target must be unreachable")
	}
	if fake.reachableCalls != 0 {
		t.Fatalf("adapter consulted for empty target (%d calls); want short-circuit", fake.reachableCalls)
	}
	// Unknown method (no adapter): unreachable, like the old default arm.
	if RegistrationReachable(cfg, &Registration{WakeMethod: "no-such-method", WakeTarget: "x"}, time.Second) {
		t.Fatal("unknown wake_method must be unreachable")
	}
}

// TestRegistrationReachable_RunnerRefusesUnownedSocketNoDial is the WI-3
// regression through the NEW interface path: RegistrationReachable for a runner
// registration naming a socket the recipient does not own must report
// unreachable WITHOUT dialing — even with a live listener at that path. Mirrors
// the watchdog/runner_reachable intent of TestWatchdogReachability_DoesNotDial
// UnownedSocket, exercising the dispatcher rather than the root helper.
func TestRegistrationReachable_RunnerRefusesUnownedSocketNoDial(t *testing.T) {
	cfg := setupAnnounceFixture(t)

	// A real listening socket at a path codex does NOT own.
	evilPath := loopShortSocketPath(t, "evil.sock")
	dialed := loopListenCounting(t, evilPath)

	reg := &Registration{
		AgentID:    "codex",
		WakeMethod: RunnerWakeMethod,
		WakeTarget: RunnerWakeTarget(evilPath),
	}
	if RegistrationReachable(cfg, reg, time.Second) {
		t.Fatal("unowned runner socket reported reachable via dispatcher; want false")
	}
	time.Sleep(50 * time.Millisecond)
	if c := dialed.count(); c != 0 {
		t.Fatalf("unowned runner socket dialed %d time(s) via dispatcher; owned-check must short-circuit before any dial", c)
	}
}
