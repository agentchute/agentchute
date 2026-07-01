//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// fileOwnerUID extracts the owning uid from a FileInfo. ok=false when the
// platform stat shape is unavailable, in which case the caller FAILS CLOSED
// (treats the file as not-current-user and reports rather than removes).
func fileOwnerUID(info os.FileInfo) (int, bool) {
	if info == nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return int(st.Uid), true
}
