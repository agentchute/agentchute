package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// herdrProbeBinary is the executable name for read-only herdr CLI calls made
// from the main package (rename, reachability, collision checks). Variable so
// tests can install a fake `herdr` on PATH. The wake poke itself lives in
// internal/loop/herdr.go and dispatches by stable name only.
var herdrProbeBinary = "herdr"

// currentHerdrPane returns the pane id of the herdr pane hosting this process,
// or "" when not running under herdr. This is used only to address `herdr
// agent rename`/`get` — it is NEVER stored as a wake_target (pane ids are not
// persistent across herdr restarts; the stable agent name is the wake target).
func currentHerdrPane() string {
	return strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
}

// herdrEnvActive reports whether this process runs inside a herdr pane. We
// require HERDR_PANE_ID (needed to bind the stable name) plus a corroborating
// herdr env marker.
func herdrEnvActive() bool {
	if currentHerdrPane() == "" {
		return false
	}
	return strings.TrimSpace(os.Getenv("HERDR_ENV")) != "" || strings.TrimSpace(os.Getenv("HERDR_SOCKET_PATH")) != ""
}

func herdrAvailable() bool {
	_, err := exec.LookPath(herdrProbeBinary)
	return err == nil
}

// renameCurrentHerdrAgent binds the current herdr pane to the stable agent id
// (`herdr agent rename <pane> <agentID>`) so peers can wake it by name and the
// pane is legible in the herdr UI. Best-effort: callers treat a failure as a
// warning, not a hard error, and skip registering the herdr wake.
func renameCurrentHerdrAgent(agentID string) error {
	pane := currentHerdrPane()
	agentID = strings.TrimSpace(agentID)
	if pane == "" || agentID == "" {
		return nil
	}
	if !herdrAvailable() {
		// Herdr env is set but the binary is gone: we can neither bind the
		// stable name nor poke later. Surface as an error so the caller skips
		// the herdr wake instead of registering an unreachable target.
		return fmt.Errorf("`herdr` binary not on PATH")
	}
	out, err := exec.Command(herdrProbeBinary, "agent", "rename", pane, agentID).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// herdrAgentInfo is the subset of a herdr agent record we consume (populated
// from `herdr agent list` via herdrAgentByName).
type herdrAgentInfo struct {
	Name          string
	PaneID        string
	Cwd           string
	ForegroundCwd string
	Found         bool
}

// herdrAgentByName resolves a herdr agent by its bound NAME via `herdr agent
// list`, matching on the `name` field — NOT `herdr agent get <name>`. The herdr
// *handle* can differ from the bound *name* (e.g. gemini's handle is `agy` while
// its bound name is `gemini-cli-agentchute`), so `agent get <name>` returns
// agent_not_found for a renamed/rebranded wrapper. List-and-match is robust to
// that. Returns found=false when the name is unbound or herdr is unavailable.
//
// This is the resolver WI-E2's reprove/rebind path uses; the wake target IS the
// stable name, so resolving it by name is what proves (or disproves) the binding.
func herdrAgentByName(name string) (info herdrAgentInfo, found bool) {
	return herdrAgentByNameWithin(name, time.Second)
}

func herdrAgentByNameWithin(name string, timeout time.Duration) (info herdrAgentInfo, found bool) {
	name = strings.TrimSpace(name)
	if name == "" || !herdrAvailable() {
		return herdrAgentInfo{}, false
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, herdrProbeBinary, "agent", "list").Output()
	if err != nil {
		return herdrAgentInfo{}, false
	}
	var resp struct {
		Result struct {
			Agents []struct {
				Name          string `json:"name"`
				PaneID        string `json:"pane_id"`
				Cwd           string `json:"cwd"`
				ForegroundCwd string `json:"foreground_cwd"`
			} `json:"agents"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return herdrAgentInfo{}, false
	}
	for _, a := range resp.Result.Agents {
		if strings.TrimSpace(a.Name) != name {
			continue
		}
		pane := strings.TrimSpace(a.PaneID)
		return herdrAgentInfo{
			Name:          name,
			PaneID:        pane,
			Cwd:           strings.TrimSpace(a.Cwd),
			ForegroundCwd: strings.TrimSpace(a.ForegroundCwd),
			Found:         pane != "",
		}, pane != ""
	}
	return herdrAgentInfo{}, false
}

func clearHerdrAgentName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || !herdrAvailable() {
		return nil
	}
	out, err := exec.Command(herdrProbeBinary, "agent", "rename", name, "--clear").CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// herdrAgentReachable reports whether a herdr wake target currently resolves to
// a live pane. Used by doctor wake-validity, the status REACHABLE column, and
// the recipient-liveness cache-miss live fallback (wired into
// loop.RegistrationReachable via SetHerdrReachableHook).
//
// It resolves by the bound NAME via herdrAgentByName (`herdr agent list` + match
// the `name` field), NOT by `herdr agent get <name>`: the wake
// target IS the stable name, and a herdr *handle* can differ from the bound
// *name* (gemini's handle "agy" vs name "gemini-cli-agentchute"), so `agent get
// <name>` returns agent_not_found for a renamed/rebranded wrapper while the name
// is still live in `agent list`. WI-E2's reprove path already resolves by name;
// this aligns the LIVE probe so the cache-miss fallback, status, and doctor all
// agree for the handle≠name case. Read-only; argv-only.
func herdrAgentReachable(name string) bool {
	return herdrAgentReachableWithin(name, time.Second)
}

func herdrAgentReachableWithin(name string, timeout time.Duration) bool {
	_, found := herdrAgentByNameWithin(name, timeout)
	return found
}

// herdrNameBoundToOtherPane reports whether name is already bound to a herdr
// pane other than ours. Used to refuse hijacking an explicit --as /
// AGENTCHUTE_AGENT_ID that collides with a different live pane (which would
// make `herdr agent send <name>` ambiguous).
func herdrNameBoundToOtherPane(name, pane string) bool {
	pane = strings.TrimSpace(pane)
	if pane == "" {
		return false
	}
	info, found := herdrAgentByName(name)
	return found && info.PaneID != pane
}

// herdrWakeForRegistration binds the current herdr pane to the agent id and
// returns the herdr wake (method="herdr", target=agent_id). ok=false means the
// herdr wake could not be established — the name collides with a different live
// pane (explicit identity only) or the rename failed — and the caller should
// fall through to other adapters. Any warnings are always returned to surface.
func herdrWakeForRegistration(opts registerOpts) (method, target string, warnings []string, ok bool) {
	pane := currentHerdrPane()
	agentID := strings.TrimSpace(opts.AgentID)
	if agentID == "" {
		return "", "", nil, false
	}
	if pane == "" {
		return "", "", []string{"herdr: not inside a herdr pane (HERDR_PANE_ID unset); herdr wake not registered"}, false
	}
	// Refuse to hijack an explicit identity already bound to a different pane:
	// two panes sharing one herdr name make `herdr agent send <name>`
	// ambiguous. Contextual identities are already disambiguated upstream with
	// -2/-3 suffixes, so they never collide here.
	if !opts.ContextualIdentity && herdrNameBoundToOtherPane(agentID, pane) {
		return "", "", []string{fmt.Sprintf("herdr: name %q is already bound to another pane; herdr wake not registered (use a unique --as/AGENTCHUTE_AGENT_ID)", agentID)}, false
	}
	if err := renameCurrentHerdrAgent(agentID); err != nil {
		return "", "", []string{fmt.Sprintf("herdr: agent rename failed (%v); herdr wake not registered", err)}, false
	}
	return "herdr", agentID, nil, true
}

// underAgentchuteRunner reports whether this process was launched by
// `agentchute run` (the PTY runner). When true, the runner socket is the wake
// path and auto-detection must NOT switch a registration to herdr/tmux just
// because those envs are also present.
func underAgentchuteRunner() bool {
	return os.Getenv("AGENTCHUTE_RUNNER") == "1" || strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID")) != ""
}
