package main

import (
	"os"
	"strings"
	"time"
)

// reproveProbeTimeout bounds the runner reachability probe used by register's
// runner-primary selection (wakeEndpointReachable). Relocated here verbatim from
// the deleted reachability.go (simple-again Gate 6a) so register.go's sole
// reference keeps compiling without an edit to the autodetect path (deferred to
// Gate 6c). The name is preserved because register.go references it.
const reproveProbeTimeout = time.Second

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
