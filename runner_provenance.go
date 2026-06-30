package main

import (
	"os"
	"strings"
)

// underAgentchuteRunner reports whether this process was launched by the
// agentchute PTY supervisor (the runner, evolving into `serve`). When true, the
// supervisor owns the wake path and auto-detection must NOT switch a
// registration to an external poke transport just because those envs are also
// present.
//
// Relocated from herdr_state.go (Gate 0 of the simple-again/protocol-v2
// migration) so the herdr probe apparatus can be deleted without breaking
// register.go, which is the sole caller cluster. Depends only on stdlib.
func underAgentchuteRunner() bool {
	return os.Getenv("AGENTCHUTE_RUNNER") == "1" || strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID")) != ""
}
