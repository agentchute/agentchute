//go:build windows

package loop

import (
	"fmt"
	"os"
	"os/user"
)

// currentUID returns a per-user identifier for the runner-socket temp dir.
// Windows has no uid; use the current user's SID (uniquely per-account), or
// fall back to the username. This keeps the directory per-user.
func currentUID() string {
	if u, err := user.Current(); err == nil {
		if u.Uid != "" {
			return u.Uid // SID on Windows
		}
		if u.Username != "" {
			return u.Username
		}
	}
	return "user"
}

// ensureOwnedRunnerSocketDir creates dir (0700) if needed and verifies it is a
// real directory (not a symlink). This fallback does not perform a Windows
// ACL/SID owner check; ownership is approximated by the per-user
// (SID-suffixed) directory name, and the symlink and directory checks still
// apply. Unix runners get the full uid ownership verification.
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
	return nil
}
