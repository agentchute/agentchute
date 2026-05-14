package loop

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// WakeAdapter dispatches a low-latency wake poke to a target.
//
// Adapters are registered in a process-global registry keyed by the
// wake_method string (AGENTCHUTE.md §5). The reference CLI ships only the
// "tmux" adapter (see AGENTCHUTE.md §8). Because this registry lives under
// Go's internal/ visibility, third-party Go packages cannot import it
// directly; the v0.1 extension model is "fork the binary, add your
// adapter source, rebuild." See EXTENSIONS.md for the protocol-level
// extensibility story (wezterm, kitty, iterm2, etc.). A plugin design
// that crosses module boundaries is out of v0.1 scope.
//
// Adapters MUST NOT shell-eval their target string. Use argv APIs and
// pass the target as a separate argument.
type WakeAdapter interface {
	Poke(ctx context.Context, target string) error
}

var (
	wakeAdaptersMu sync.RWMutex
	wakeAdapters   = make(map[string]WakeAdapter)
)

// RegisterWakeAdapter associates a wake_method name with an adapter
// implementation. The method key is trimmed for consistency with
// wakeAdapterFor / PokeWakeTarget lookups. Empty methods and nil
// adapters are silently ignored (defensive: registration is global
// state, so a bad caller should not be able to poison the registry).
// Re-registering the same method silently overwrites the prior binding
// — convenient for tests; harmless in practice since each method should
// have exactly one adapter per binary.
func RegisterWakeAdapter(method string, adapter WakeAdapter) {
	method = strings.TrimSpace(method)
	if method == "" || adapter == nil {
		return
	}
	wakeAdaptersMu.Lock()
	defer wakeAdaptersMu.Unlock()
	wakeAdapters[method] = adapter
}

// wakeAdapterFor returns the adapter registered for method, or nil if
// no such adapter is registered. Useful for tests and for callers that
// need to introspect availability before dispatching.
func wakeAdapterFor(method string) WakeAdapter {
	wakeAdaptersMu.RLock()
	defer wakeAdaptersMu.RUnlock()
	return wakeAdapters[strings.TrimSpace(method)]
}

// PokeWakeTarget dispatches a wake poke for a recipient that declared
// the given wake_method + wake_target in its registration. Returns nil
// for empty method (non-pokable; treated as no-op). Returns an
// unsupported-method error if no adapter is registered.
func PokeWakeTarget(method, target string) error {
	return PokeWakeTargetContext(context.Background(), method, target)
}

// PokeWakeTargetContext is PokeWakeTarget with a cancellable context.
func PokeWakeTargetContext(ctx context.Context, method, target string) error {
	method = strings.TrimSpace(method)
	if method == "" {
		return nil // non-pokable per AGENTCHUTE.md §10.6; caller treats as no-op
	}
	adapter := wakeAdapterFor(method)
	if adapter == nil {
		return fmt.Errorf("unsupported wake_method %q (no adapter registered)", method)
	}
	return adapter.Poke(ctx, target)
}

// tmuxAdapter wires the tmux wake convention (AGENTCHUTE.md §8) into the
// registry. Backed by the existing PokeTargetContext implementation in
// tmux.go.
type tmuxAdapter struct{}

func (tmuxAdapter) Poke(ctx context.Context, target string) error {
	return PokeTargetContext(ctx, target)
}

func init() {
	RegisterWakeAdapter("tmux", tmuxAdapter{})
}
