package loop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// herdrBinary is the executable name to invoke. Variable so tests can place a
// fake `herdr` binary on PATH and exercise the poke path without a real herdr
// server.
var herdrBinary = "herdr"

// herdrWakePrompt is the text injected into the recipient's herdr agent input.
// It mirrors the tmux wake convention (AGENTCHUTE.md §8); the bracketed prefix
// is machine metadata and the instruction is "check inbox".
const herdrWakePrompt = "[agentchute:herdr] check inbox"

// herdrSubmit is the byte appended to the wake prompt to commit the turn.
// It MUST be a carriage return (0x0d), not a line feed (0x0a): herdr's
// `agent send` writes the text bytes literally to the recipient pane, and
// standard agent TUIs submit their input on CR. A trailing LF only inserts a
// newline in a multiline editor and never fires a turn — empirically verified
// against the live herdr 0.7.0 keystroke path. This single-byte distinction is
// why a one-shot `agent send` works where a separate send-keys Enter does not.
const herdrSubmit = "\r"

// PokeHerdrTargetContext sends the agentchute wake prompt to a herdr agent by
// its stable name (the agentchute agent_id, bound to the pane via
// `herdr agent rename` at registration). One argv invocation:
//
//	herdr agent send <target> "[agentchute:herdr] check inbox\r"
//
// The dispatcher in wake.go calls this via herdrAdapter when a peer declares
// wake_method: herdr.
//
// Behavior:
//   - If target is empty, returns nil immediately. Non-pokable agents poll
//     themselves; the protocol allows this (AGENTCHUTE.md §6.2).
//   - Otherwise runs a single `herdr agent send`. Unlike the tmux adapter, no
//     second Enter call and no inter-key sleep are needed: the trailing CR in
//     the text submits the turn in one shot.
//
// The target is ALWAYS a stable herdr agent name, never an ephemeral pane id,
// and is passed as a separate argv argument. The adapter never shell-evaluates
// the target or the prompt.
func PokeHerdrTargetContext(ctx context.Context, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, herdrBinary, "agent", "send", target, herdrWakePrompt+herdrSubmit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("herdr agent send: %w: %s", err, trimmed)
		}
		return fmt.Errorf("herdr agent send: %w", err)
	}
	return nil
}

// herdrAdapter wires the herdr wake convention into the registry. Backed by
// PokeHerdrTargetContext.
type herdrAdapter struct{}

func (herdrAdapter) Poke(ctx context.Context, target string) error {
	return PokeHerdrTargetContext(ctx, target)
}

func init() {
	RegisterWakeAdapter("herdr", herdrAdapter{})
}
