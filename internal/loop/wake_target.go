package loop

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Wake-target shape validators. A registration's wake_target is parsed and used
// to poke a tmux pane, a herdr agent, or a unix socket. Because any peer can
// hand-write a registration into the shared agents/ dir, an unvalidated
// wake_target is an injection vector: a hostile target string fed to
// `tmux send-keys -t <target>`, `herdr pane send-keys <target>`, or a unix
// socket dial can poke a pane/socket the registration's author does not own, or
// (via a leading dash) be misread as a flag by the underlying CLI.
//
// ValidateWakeTarget performs PURE SHAPE validation — it never touches the
// filesystem or a tmux/herdr server and needs no Config. Recipient-binding for
// the runner socket (proving the unix: path actually belongs to the named
// recipient) is a separate, stateful check in the poke path; see send.go's
// computeWakeReceipt and run.go's runner-health helpers.
var (
	// tmux targets accept the two live shapes:
	//   - a pane id:    %0, %1, %123
	//   - session:win.pane with alnum/_/- session names: main:0.0, my-session:1.2
	tmuxPaneIDRE     = regexp.MustCompile(`^%[0-9]+$`)
	tmuxSessionTgtRE = regexp.MustCompile(`^[A-Za-z0-9_-]+:[0-9]+\.[0-9]+$`)
)

// ValidateWakeTarget checks that target is a well-formed address for the given
// wake_method. It is intentionally conservative about the methods the reference
// CLI ships adapters for (tmux, herdr, agentchute-run) and PERMISSIVE about
// unknown methods so a forked binary's custom adapter is not rejected — except
// the universal argv-confusion guard below, which applies to every method.
//
// Universal rules (all methods, including unknown ones):
//   - target must be non-empty (after trimming);
//   - target must not begin with '-' (argv-confusion defense: a leading dash
//     is misread as a flag by `tmux send-keys -t <target>` / `herdr pane
//     send-keys <target>` and similar argv-based pokes).
//
// Method-specific rules:
//   - tmux: ^%[0-9]+$ OR ^[A-Za-z0-9_-]+:[0-9]+\.[0-9]+$
//   - herdr: an agent-id slug (same rule as ValidateAgentID / agentIDRE).
//   - agentchute-run: must start with "unix:" and the remainder must be a
//     clean absolute path (no NUL, no newline, filepath.IsAbs, and
//     filepath.Clean(path) == path so traversal/dot segments are rejected).
//
// Unknown method: returns nil after the universal guard. This is a deliberate
// permissive-with-comment choice so a forked binary adding e.g. a wezterm or
// kitty adapter is not blocked by the core validator; the adapter itself is
// still expected to use argv (never shell-eval) per WakeAdapter's contract.
func ValidateWakeTarget(method, target string) error {
	method = strings.TrimSpace(method)
	target = strings.TrimSpace(target)

	if target == "" {
		return fmt.Errorf("wake_target is empty")
	}
	// Argv-confusion guard for every method: a leading '-' would be parsed as a
	// flag by send-keys -t <target> and similar invocations.
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("wake_target %q must not begin with '-'", target)
	}

	switch method {
	case "tmux":
		if tmuxPaneIDRE.MatchString(target) || tmuxSessionTgtRE.MatchString(target) {
			return nil
		}
		return fmt.Errorf("tmux wake_target %q must be a pane id (%%N) or session:window.pane", target)
	case "herdr":
		// herdr targets are stable agent-id slugs; reuse the agent_id rule so a
		// slash/dot/colon/uppercase target (traversal or pane-id confusion) is
		// rejected.
		if err := ValidateAgentID(target); err != nil {
			return fmt.Errorf("herdr wake_target %q: %w", target, err)
		}
		return nil
	case RunnerWakeMethod:
		if !strings.HasPrefix(target, runnerTargetUnix) {
			return fmt.Errorf("agentchute-run wake_target %q must start with %q", target, runnerTargetUnix)
		}
		path := strings.TrimPrefix(target, runnerTargetUnix)
		if path == "" {
			return fmt.Errorf("agentchute-run wake_target has empty socket path")
		}
		if strings.ContainsAny(path, "\x00\n\r") {
			return fmt.Errorf("agentchute-run wake_target socket path contains a control character")
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("agentchute-run wake_target socket path %q must be absolute", path)
		}
		// Reject a non-clean path up front so unix:/tmp/../evil.sock,
		// unix:/a/./b, double slashes, and trailing slashes can't slip a
		// traversal past the recipient-binding owned-check (which compares
		// against cleaned, canonical paths). filepath.Clean is a pure string
		// op — no filesystem access — so this stays a shape check.
		if filepath.Clean(path) != path {
			return fmt.Errorf("agentchute-run wake_target socket path %q is not clean (want %q)", path, filepath.Clean(path))
		}
		return nil
	default:
		// Unknown method: permissive by design (see doc comment) — the universal
		// empty/leading-dash guards above still applied.
		return nil
	}
}
