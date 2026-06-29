package loop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// herdrBinary is the executable name to invoke. Variable so tests can place a
// fake `herdr` binary on PATH and exercise the poke path without a real herdr
// server.
var herdrBinary = "herdr"

// herdrWakePrompt is the text injected into the recipient's herdr agent input.
// It mirrors the tmux wake convention (AGENTCHUTE.md §8); the bracketed prefix
// is machine metadata and the instruction is "check inbox".
const herdrWakePrompt = "[agentchute:herdr] check inbox"

// PokeHerdrTargetContext delivers the agentchute wake prompt to a herdr agent
// by its stable name, then submits the turn with a real Enter key event.
//
// herdr 0.7.0 splits these two operations, and conflating them is why an older
// one-shot `agent send "<text>\r"` silently failed to wake anyone:
//
//   - `herdr agent send <name> <text>` writes LITERAL text to the agent's input
//     (herdr's own help: "agent send writes literal text; use pane run when you
//     want command text plus Enter"). A trailing CR is rendered as a literal
//     character; recipient TUIs bind submit to an Enter KEY EVENT, not a raw
//     byte in the text stream, so the turn never fires.
//   - `herdr pane send-keys <pane_id> Enter` injects the Enter key event that
//     actually submits.
//
// So we mirror the tmux adapter: send the text, wait pokeSleep, send Enter as a
// separate key event. The stable agent name must be resolved to its CURRENT
// pane id (panes can move), because pane commands reject names.
//
// The dispatcher in wake.go calls this via herdrAdapter when a peer declares
// wake_method: herdr.
//
// Behavior:
//   - If target is empty, returns nil immediately. Non-pokable agents poll
//     themselves; the protocol allows this (AGENTCHUTE.md §6.2).
//   - Resolves the pane first (fail fast rather than type into a vanished
//     pane), sends the text, waits pokeSleep, then sends Enter. The context
//     controls the delay and the herdr command timeouts.
//
// The target is ALWAYS a stable herdr agent name and every argument is passed
// as a separate argv element; the adapter never shell-evaluates input.
func PokeHerdrTargetContext(ctx context.Context, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	// Re-validate at use time: a hand-written registration that bypassed
	// Validate() must not reach herdr with a slash/colon/flag-shaped name.
	if err := ValidateWakeTarget("herdr", target); err != nil {
		return err
	}
	paneID, err := herdrPaneIDForAgent(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve herdr pane for %q: %w", target, err)
	}
	if err := runHerdr(ctx, "agent", "send", target, herdrWakePrompt); err != nil {
		return fmt.Errorf("herdr agent send: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pokeSleep):
	}
	if err := runHerdr(ctx, "pane", "send-keys", paneID, "Enter"); err != nil {
		return fmt.Errorf("herdr pane send-keys Enter: %w", err)
	}
	return nil
}

// herdrPaneIDForAgent resolves a stable herdr agent name to its current pane id
// via the resolver hook the root package wires in init()
// (SetHerdrPaneResolverHook(herdrAgentPaneID)). That resolver uses `herdr agent
// list` and matches the bound NAME, so wakeability and reachability agree even
// when the herdr handle differs from the bound name.
//
// The hook is REQUIRED. It is the deliberate loop/main boundary: herdr probing
// lives in the root package (loop must not import main), and the alternative
// `herdr agent get <name>` resolver is intentionally NOT used because the herdr
// handle can differ from the bound name (handle!=name), which `agent get` cannot
// resolve. Loop-only tests (which never link the root package, so init() never
// runs) must inject a stub via SetHerdrPaneResolverHook.
//
// ctx is retained in the signature for symmetry with the poke path and so the
// resolver can grow a context-aware form without churning callers.
func herdrPaneIDForAgent(ctx context.Context, target string) (string, error) {
	if herdrPaneResolverHook == nil {
		return "", fmt.Errorf("herdr pane resolver not wired")
	}
	if paneID, ok := herdrPaneResolverHook(target); ok {
		return paneID, nil
	}
	return "", fmt.Errorf("no pane_id reported for agent %q", target)
}

func runHerdr(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, herdrBinary, args...).CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
		return err
	}
	return nil
}

// herdrAdapter wires the herdr wake convention into the registry. Backed by
// PokeHerdrTargetContext.
type herdrAdapter struct{}

func (herdrAdapter) Poke(ctx context.Context, target string) error {
	return PokeHerdrTargetContext(ctx, target)
}

// Reachable resolves the stable herdr agent name to a live pane via the
// injected root-package hook. Without the hook (loop-only
// test linkage) the name cannot be resolved, so it is reported unreachable —
// identical to a herdr lookup that found no pane under the old switch.
func (herdrAdapter) Reachable(_ *Config, reg *Registration, timeout time.Duration) bool {
	if reg == nil || herdrReachableHook == nil {
		return false
	}
	return herdrReachableHook(reg.WakeTarget, timeout)
}

func init() {
	RegisterWakeAdapter("herdr", herdrAdapter{})
}
