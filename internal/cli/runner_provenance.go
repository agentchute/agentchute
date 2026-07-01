package cli

import (
	"os"
	"strings"
	"time"
)

// reproveProbeTimeout bounded the runner reachability probe that register's
// runner-primary selection used before the pull-only redesign. Relocated here
// verbatim from the deleted reachability.go (simple-again Gate 6a); pull-only
// (Gate 6c) removed that probe and its caller, so nothing references this const
// now.
const reproveProbeTimeout = time.Second

// underAgentchuteRunner reports whether this process was launched by the
// agentchute PTY supervisor (the runner, evolving into `serve`). It gated the
// old wake-transport autodetect so a supervised session would not switch its
// registration to an external poke transport. Pull-only (Gate 6c) removed that
// autodetect, so this helper is now unreferenced.
//
// Relocated from herdr_state.go (Gate 0 of the simple-again/protocol-v2
// migration) so the herdr probe apparatus could be deleted without breaking
// register.go, which was then the sole caller cluster. Depends only on stdlib.
func underAgentchuteRunner() bool {
	return os.Getenv("AGENTCHUTE_RUNNER") == "1" || strings.TrimSpace(os.Getenv("AGENTCHUTE_RUNNER_PID")) != ""
}
