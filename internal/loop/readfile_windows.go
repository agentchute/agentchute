//go:build windows

package loop

import (
	"fmt"
	"os"
)

// openRegularNoFollow is the Windows fallback. It preserves the prior behavior:
// Lstat to reject a symlink leaf, then Open, then re-check the opened fd is a
// regular file. The Lstat→Open TOCTOU window is therefore
// only PARTIALLY closed on Windows (the post-open fstat catches a swap to a
// non-regular object, but a swap between two regular files is not detected).
// agentchute's loop dirs are owner-only, so an untrusted peer cannot reach the
// parent chain to exploit this; unix runners get the structural O_NOFOLLOW
// guarantee.
func openRegularNoFollow(path string) (*os.File, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: symlink not allowed", path)
	}
	f, err := os.Open(path)
	if err != nil {
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
