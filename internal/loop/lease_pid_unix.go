//go:build !windows

package loop

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether pid names a live process on THIS host, via the
// classic FindProcess + Signal(0) probe (mirrors run.go:processAlive, which
// lives in package main and is unreachable from internal/loop). EPERM means the
// process exists but is owned by another uid — still alive for our purposes.
//
// Used by the same-host arm of stale-lease reclaim: a stale claim whose pid is
// still alive is a FROZEN-BUT-ALIVE process whose id must NOT be stolen.
//
// Package var (not a plain func) so lease tests can pin it deterministically —
// a genuinely-dead pid is hard to guarantee in a test otherwise.
var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
