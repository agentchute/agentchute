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

// PokeRegistration is the single recipient-bound entry point for every
// registration-driven wake poke. It dispatches the poke declared by reg, but
// for the runner method (agentchute-run) it FIRST proves the unix: socket path
// is one the recipient legitimately owns (cfg.RunnerWakeTargetOwnedBy) and
// REFUSES — never dialing — if it does not.
//
// This closes the recipient-binding gap: a hand-written peer registration that
// names, e.g., unix:/tmp/evil.sock for an innocent peer id would otherwise be
// dialed by the announce/corrective/watchdog/defer poke sites. recipientID is
// always reg.AgentID — the socket must be owned by the agent the registration
// is for; there is no scenario where a registration's runner socket should be
// owned by anyone else.
//
// For non-runner methods (tmux/herdr/unknown) there is no socket-ownership
// concept, so PokeRegistration simply pokes via PokeWakeTargetContext,
// preserving prior behavior exactly. cfg is unused for those methods (and may
// be nil for them, though callers always have one).
//
// A nil reg returns nil (treated as a no-op, consistent with IsPokable).
func PokeRegistration(ctx context.Context, cfg *Config, reg *Registration) error {
	if reg == nil {
		return nil
	}
	if strings.TrimSpace(reg.WakeMethod) == RunnerWakeMethod {
		if cfg == nil {
			return fmt.Errorf("refused: cannot verify runner socket ownership without config")
		}
		if err := cfg.RunnerWakeTargetOwnedBy(reg.AgentID, reg.WakeTarget); err != nil {
			return fmt.Errorf("refused: unowned runner socket: %w", err)
		}
	}
	return PokeWakeTargetContext(ctx, reg.WakeMethod, reg.WakeTarget)
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
