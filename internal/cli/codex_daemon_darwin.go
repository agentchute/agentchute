package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// readProcessEnvBlock returns pid's NUL-separated environment strings from
// sysctl kern.procargs2 — the same source `ps -E` prints, without the word
// splitting. Fails for another user's process, which the caller reports as an
// unreadable environment (WARN). The kernel omits the environment of platform
// (restricted) binaries; that reads as "no AGENTCHUTE_SERVE_TOKEN", and codex
// is not one.
func readProcessEnvBlock(pid int) ([]byte, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2 %d: %w", pid, err)
	}
	return parseProcArgs2Env(data)
}

// readProcessArgv returns pid's exact argv from the same kern.procargs2 buffer.
func readProcessArgv(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2 %d: %w", pid, err)
	}
	return parseProcArgs2Argv(data)
}
