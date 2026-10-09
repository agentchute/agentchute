package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A lane's processes carry its identity, fence token and guard session in
// AGENTCHUTE_* variables. A wrapper or serve started from INSIDE a lane — a
// command the lane's model types in its shell, a script it runs — inherits
// them, and so ran as the parent lane: a nested `codex exec` whose Stop hook
// ran turn-end archived the parent's claimed mail and cleared its latch, and
// a nested `ac serve` resolved the parent's id (opus-xhigh S7).
//
// serve starts its wrapper as a DIRECT child (pty.Start), and the `ac`
// dispatcher and the wrapper shims are scripts that exec, so the runner's own
// launch always has the runner as its parent. Any other parent is a launch
// inside the lane.
//
// Choice: strip, not refuse. A lane running another CLI as a tool (`codex
// exec "review this"`) is ordinary work that a refusal would break; what must
// not happen is that process acting as the lane. Stripped of the lane's
// identity, fence token, guard bit and runner markers, it runs as a plain,
// un-enrolled wrapper: its hooks find no identity and do nothing to the
// parent's mail.

// laneIdentityKeys are the variables that make a process act as a lane.
var laneIdentityKeys = []string{
	"AGENTCHUTE_AGENT_ID",
	"AGENTCHUTE_SERVE_TOKEN",
	"AGENTCHUTE_GUARD",
	"AGENTCHUTE_RUNNER",
	"AGENTCHUTE_RUNNER_PID",
}

// laneParentPID is os.Getppid; a variable so tests can pose as either parent.
var laneParentPID = os.Getppid

// nestedLaneLaunch reports whether this process carries a lane's runner pid
// but was not started by that runner. Without a runner pid (a hand-run
// command, an older serve) there is nothing to compare, and it reports false.
func nestedLaneLaunch() bool {
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID")))
	if err != nil || pid <= 0 {
		return false
	}
	return laneParentPID() != pid
}

// wrapperBypassEnv is the environment for a wrapper exec'd without serve
// (AGENTCHUTE_SHIM_BYPASS, or a lane's runner env present): the runner's own
// launch keeps the lane's identity, a launch from inside the lane does not.
func wrapperBypassEnv(command string) []string {
	env := os.Environ()
	if !nestedLaneLaunch() {
		return env
	}
	fmt.Fprintf(os.Stderr, "agentchute %s: started inside lane %s, not by its runner; running without the lane's identity, fence token or guard\n", command, os.Getenv("AGENTCHUTE_AGENT_ID"))
	return withoutEnv(env, laneIdentityKeys...)
}

// dropNestedLaneIdentity unsets the lane's identity in this process when it
// was started inside a lane, so a nested `agentchute serve` resolves its own
// id from its flags instead of the parent's.
func dropNestedLaneIdentity(command string) {
	if !nestedLaneLaunch() {
		return
	}
	fmt.Fprintf(os.Stderr, "agentchute %s: started inside lane %s, not by its runner; not inheriting the lane's identity, fence token or guard\n", command, os.Getenv("AGENTCHUTE_AGENT_ID"))
	for _, k := range laneIdentityKeys {
		_ = os.Unsetenv(k)
	}
}
