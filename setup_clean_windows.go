//go:build windows

package main

import "os"

// fileOwnerUID on Windows cannot cheaply resolve a POSIX uid, so it reports
// ok=false and the caller FAILS CLOSED (reports the backup, never auto-removes).
// Mirrors the degraded-but-safe posture of the other *_windows.go stubs.
func fileOwnerUID(_ os.FileInfo) (int, bool) {
	return 0, false
}
