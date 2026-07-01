package cli

import "os/exec"

// Read-only wrapper-presence enumeration probes. Pull-only (simple-again Gate 6c)
// retired tmux and herdr as WAKE transports — the wake adapters, the
// wake_method/wake_target registration fields, and the herdr/tmux wake-selection
// apparatus are all gone. What survives here are the small, read-only ENUMERATION
// probes the diagnostic surfaces still need: status's "present but not enrolled"
// scan (presence_scan.go) and `setup --reset`'s per-repo agent enumeration
// (setup_reset.go). These never select or dispatch a wake; they only list what is
// running. Variables so tests can install fakes / keep the scans hermetic.
var (
	tmuxProbeBinary  = "tmux"
	herdrProbeBinary = "herdr"
)

// herdrAvailable reports whether the herdr CLI is on PATH, gating the read-only
// `herdr agent list` enumeration used by the present-but-not-enrolled scan and by
// `setup --reset`.
func herdrAvailable() bool {
	_, err := exec.LookPath(herdrProbeBinary)
	return err == nil
}
