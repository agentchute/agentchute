package loop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeAdapter struct {
	calls   int
	lastCtx context.Context
	lastTgt string
	err     error
}

func (f *fakeAdapter) Poke(ctx context.Context, target string) error {
	f.calls++
	f.lastCtx = ctx
	f.lastTgt = target
	return f.err
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
