//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// singleLink reports whether a file has exactly one hard link.
func singleLink(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}
