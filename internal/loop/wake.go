package loop

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
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
//
// Reachable reports whether the registration's wake target can currently be
// reached by this adapter's method WITHOUT enqueuing a wake. It is the single
// place per method that decides reachability, so callers do not re-implement a
// method-name switch. It receives the full registration (WakeMethod /
// WakeTarget / AgentID) and cfg, because some methods (the runner socket) MUST
// prove the recipient owns the target — via cfg.RunnerWakeTargetOwnedBy —
// BEFORE any dial, refusing an unowned socket without touching it. timeout
// bounds any probe.
type WakeAdapter interface {
	Poke(ctx context.Context, target string) error
	Reachable(cfg *Config, reg *Registration, timeout time.Duration) bool
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

// RegistrationReachable is the single recipient-bound dispatcher for
// registration-driven reachability probes. It looks up the adapter for
// reg.WakeMethod and asks it whether reg's wake target is currently reachable,
// replacing the per-call method-name switch that callers used to duplicate
// (recipient_liveness.go's registrationHasReachableWake, and the direct
// runnerReachableForRecipient call sites). One place now decides reachability
// per method.
//
// A nil reg, empty wake_method, empty wake_target, or unknown wake_method (no
// adapter registered) all report NOT reachable — matching the prior switch's
// default arm and empty-target short-circuit exactly. For the runner method the
// adapter performs cfg.RunnerWakeTargetOwnedBy BEFORE any dial, so an unowned
// socket is reported unreachable without being touched (WI-3 invariant).
func RegistrationReachable(cfg *Config, reg *Registration, timeout time.Duration) bool {
	if reg == nil {
		return false
	}
	if strings.TrimSpace(reg.WakeTarget) == "" {
		return false
	}
	adapter := wakeAdapterFor(reg.WakeMethod)
	if adapter == nil {
		return false
	}
	return adapter.Reachable(cfg, reg, timeout)
}

// Cross-package reachability probe hooks. The concrete tmux/herdr reachability
// probes live in the root (main) package — they shell out via package-level
// binary vars (tmuxProbeBinary / herdrProbeBinary) and consume root-package
// helpers — and CANNOT move into internal/loop without an import cycle (loop
// must not import main). So the loop-package tmux/herdr adapters call these
// injected hooks, which the root package wires up in init(). When a hook is
// unset (e.g. a loop-package unit test that never links the root package), the
// adapter reports unreachable — the same "cannot probe ⇒ not reachable" answer
// the old switch gave for a probe that returned false. The runner adapter needs
// no hook: its owned-check (cfg.RunnerWakeTargetOwnedBy) and dial
// (RunnerSocketReachable) both already live in this package.
var (
	tmuxReachableHook  func(target string) bool
	herdrReachableHook func(target string) bool
)

// SetTmuxReachableHook installs the tmux reachability probe. Called from the
// root package's init(); idempotent.
func SetTmuxReachableHook(fn func(target string) bool) { tmuxReachableHook = fn }

// SetHerdrReachableHook installs the herdr reachability probe. Called from the
// root package's init(); idempotent.
func SetHerdrReachableHook(fn func(target string) bool) { herdrReachableHook = fn }

// tmuxAdapter wires the tmux wake convention (AGENTCHUTE.md §8) into the
// registry. Backed by the existing PokeTargetContext implementation in
// tmux.go.
type tmuxAdapter struct{}

func (tmuxAdapter) Poke(ctx context.Context, target string) error {
	return PokeTargetContext(ctx, target)
}

// Reachable probes the tmux pane via the injected root-package hook. Without
// the hook (loop-only test linkage) the target cannot be probed, so it is
// reported unreachable — identical to a failed tmux probe under the old switch.
func (tmuxAdapter) Reachable(_ *Config, reg *Registration, _ time.Duration) bool {
	if reg == nil || tmuxReachableHook == nil {
		return false
	}
	return tmuxReachableHook(reg.WakeTarget)
}

func init() {
	RegisterWakeAdapter("tmux", tmuxAdapter{})
}
