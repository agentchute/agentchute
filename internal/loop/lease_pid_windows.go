//go:build windows

package loop

// pidAlive on Windows is a best-effort stub (mirrors filelock_windows.go's
// degraded posture under the POSIX-only runtime contract). We cannot cheaply
// prove a foreign pid is alive here, so we report not-alive and let the
// freshness/timeout rule govern stale-lease reclaim. The Windows file lock
// already auto-releases on process death, so a crashed holder is reaped
// independently of this probe.
//
// Package var so it has the same shape (and test-override hook) as the unix
// implementation.
var pidAlive = func(pid int) bool {
	return false
}
