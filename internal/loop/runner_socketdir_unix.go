//go:build !windows

package loop

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// currentUID returns the caller's real uid as a decimal string, used to make
// the runner-socket temp directory per-user (/tmp/agentchute-run-<uid>).
func currentUID() string {
	return strconv.Itoa(os.Getuid())
}

// ensureOwnedRunnerSocketDir creates dir (0700) if needed and verifies it is
// owned by the current uid and is a real directory (not a symlink) before a
// runner binds a socket inside it. On a shared /tmp this defends against
// another local user having pre-created the predictable directory to intercept
// or DoS this user's sockets. A pre-existing directory owned by someone else is
// a hard error — we refuse to bind rather than trust a squatted path.
func ensureOwnedRunnerSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: symlink not allowed for runner socket dir", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Unknown stat backing — fail closed rather than bind into a dir we
		// can't ownership-check.
		return fmt.Errorf("%s: cannot determine runner socket dir ownership", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s: runner socket dir is owned by uid %d, not current uid %d; refusing to bind", dir, st.Uid, os.Getuid())
	}
	// Tighten perms in case MkdirAll honored a loose umask on a pre-existing dir.
	return os.Chmod(dir, 0o700)
}
