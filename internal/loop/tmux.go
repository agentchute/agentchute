package loop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// pokeSleep is the empirical delay between sending the trigger string and the
// Enter key. Chained `tmux send-keys -t %N 'check' Enter` does not reliably
// commit Enter on every CLI's input widget (per AGENTCHUTE.md §6.2 and §8).
// Two separate calls with a brief delay work consistently. Variable rather
// than constant so tests can override it.
var pokeSleep = 300 * time.Millisecond

// tmuxBinary is the executable name to invoke. Variable so tests can place a
// fake `tmux` binary on PATH and exercise the poke path without a real tmux
// server.
var tmuxBinary = "tmux"

const tmuxWakePrompt = "[agentchute:tmux] check inbox"

// PokeTargetContext sends the agentchute wake prompt + sleep + Enter to a
// tmux target, matching the AGENTCHUTE.md §8 tmux wake adapter recipe. The
// dispatcher in wake.go calls this via tmuxAdapter when a peer declares
// wake_method: tmux.
//
// Behavior:
//   - If target is empty, returns nil immediately. Non-pokable agents are
//     responsible for polling themselves; the protocol explicitly allows
//     this (AGENTCHUTE.md §6.2).
//   - Otherwise sends two `tmux send-keys -t <target>` invocations with a
//     pokeSleep delay between them. The context controls the delay and tmux
//     command timeouts; pass `context.Background()` for the standard
//     uncancellable shape.
//
// Returns an error only if a tmux invocation itself fails. The caller decides
// whether to log/skip on error or propagate it.
func PokeTargetContext(ctx context.Context, target string) error {
	if target == "" {
		return nil
	}
	if err := tmuxSendKeys(ctx, target, tmuxWakePrompt); err != nil {
		return fmt.Errorf("send wake prompt: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pokeSleep):
	}
	if err := tmuxSendKeys(ctx, target, "Enter"); err != nil {
		return fmt.Errorf("send Enter: %w", err)
	}
	return nil
}

func tmuxSendKeys(ctx context.Context, target, keys string) error {
	cmd := exec.CommandContext(ctx, tmuxBinary, "send-keys", "-t", target, keys)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
		return err
	}
	return nil
}
