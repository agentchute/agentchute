//go:build !windows

package loop

import (
	"fmt"
	"os"
	"syscall"
)

// openRegularNoFollow opens path read-only with O_NOFOLLOW so a symlink at the
// final path component is refused by the kernel (ELOOP) at open time, then
// fstat's the resulting fd to confirm it is a regular file. Validating the
// OPENED fd — rather than Lstat'ing the path and opening separately — closes
// the classic TOCTOU window: between an Lstat and an Open a peer could swap a
// vetted regular file for a symlink, but here the fd we read IS the object we
// checked.
//
// Note on residual TOCTOU: O_NOFOLLOW only guards the FINAL path component. A
// peer with write access to a PARENT directory in `path` could still swap an
// intermediate directory between resolution and open. agentchute's loop dirs
// are created 0700 (owner-only) precisely to keep untrusted peers out of the
// parent chain, so the realistic threat — a planted symlink at the leaf inbox/
// registration file — is fully closed; the parent-swap variant is out of scope
// for a same-uid local bus and would require a different trust model.
func openRegularNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// ELOOP is what O_NOFOLLOW returns for a symlink leaf; surface a clear
		// message matching the prior ensureRegularFile error for that case.
		if errno, ok := err.(*os.PathError); ok && errno.Err == syscall.ELOOP {
			return nil, fmt.Errorf("%s: symlink not allowed", path)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return f, nil
}
