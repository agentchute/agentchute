package cli

import "os/exec"

// Read-only wrapper-presence enumeration probe. Pull-only (simple-again Gate 6c)
// retired tmux and herdr as WAKE transports — the wake adapters, the
// wake_method/wake_target registration fields, and the herdr/tmux wake-selection
// apparatus are all gone. What survives here is the small, read-only ENUMERATION
// probe `setup --reset`'s per-repo agent enumeration (setup_reset.go) still needs.
// It never selects or dispatches a wake; it only lists what is running. Variable
// so tests can install a fake / keep the scan hermetic.
var herdrProbeBinary = "herdr"

// herdrAvailable reports whether the herdr CLI is on PATH, gating the read-only
// `herdr agent list` enumeration used by `setup --reset`.
func herdrAvailable() bool {
	_, err := exec.LookPath(herdrProbeBinary)
	return err == nil
}
