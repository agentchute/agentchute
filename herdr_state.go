package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	return exec.Command(herdrProbeBinary, "agent", "rename", pane, agentID).Run()
}

// herdrAgentInfo is the subset of `herdr agent get` we consume.
type herdrAgentInfo struct {
	PaneID string
	Found  bool
}

// herdrAgentLookup resolves a herdr agent target (name or pane id) to its
// current pane via `herdr agent get`. Read-only; argv-only; used for doctor
// reachability and explicit-identity collision detection. Returns Found=false
// when the target is unbound or herdr is unavailable.
func herdrAgentLookup(target string) herdrAgentInfo {
	target = strings.TrimSpace(target)
	if target == "" || !herdrAvailable() {
		return herdrAgentInfo{}
	}
	out, err := exec.Command(herdrProbeBinary, "agent", "get", target).Output()
	if err != nil {
		return herdrAgentInfo{}
	}
	var resp struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &resp) != nil || resp.Error != nil {
		return herdrAgentInfo{}
	}
	pane := strings.TrimSpace(resp.Result.Agent.PaneID)
	return herdrAgentInfo{PaneID: pane, Found: pane != ""}
}

// herdrAgentReachable reports whether a herdr wake target currently resolves to
// a live pane. Used by doctor and recipient-liveness checks.
func herdrAgentReachable(name string) bool {
	return herdrAgentLookup(name).Found
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
	info := herdrAgentLookup(name)
	return info.Found && info.PaneID != pane
}

// herdrWakeForRegistration binds the current herdr pane to the agent id and
// returns the herdr wake (method="herdr", target=agent_id). ok=false means the
// herdr wake could not be established — the name collides with a different live
// pane (explicit identity only) or the rename failed — and the caller should
// fall through to other adapters. Any warnings are always returned to surface.
func herdrWakeForRegistration(opts registerOpts) (method, target string, warnings []string, ok bool) {
	pane := currentHerdrPane()
	agentID := strings.TrimSpace(opts.AgentID)
	if pane == "" || agentID == "" {
		return "", "", nil, false
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
