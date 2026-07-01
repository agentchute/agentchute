//go:build windows

package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

// agentLockTimeout bounds how long withAgentLock waits to acquire the per-agent
// lock before giving up, so a stuck peer never hangs a command forever.
//
// It is a package var (not a const) ONLY so tests can lower it to keep the
// bounded-wait test fast; production never reassigns it, so the effective
// behavior is the same 5s timeout.
var agentLockTimeout = 5 * time.Second

// agentLockRetryInterval is the poll cadence for the non-blocking lock loop.
const agentLockRetryInterval = 25 * time.Millisecond

// withAgentLock runs fn while holding an exclusive OS advisory lock scoped to a
// single agent's local state directory (<loop>/state/<agent>/.lock). It
// serializes the read-modify-write sequences over an agent's registration and
// ledger files so concurrent processes (runner poll loop, hook commands)
// cannot lose updates to each other.
//
// The lock is acquired via LockFileEx with LOCKFILE_EXCLUSIVE_LOCK |
// LOCKFILE_FAIL_IMMEDIATELY in a bounded retry loop honoring agentLockTimeout
// (never blocks forever). The kernel releases the lock automatically when the
// handle is closed — including on process death — so there is NO age-based
// stale reclaim: a crashed holder's lock vanishes when its handle is reaped,
// and a live holder whose critical section runs long can never be stolen.
//
// CRITICAL — callers must never nest withAgentLock for the same agentID within
// a single call stack: a second LockFileEx on a SECOND handle for the same byte
// range from the same process does NOT recurse and will spin until timeout. The
// read, mutate, and write all happen inside the single fn passed here, and the
// load/save helpers fn calls must be the lock-free inner variants.
func withAgentLock(cfg *Config, agentID string, fn func() error) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	stateDir := cfg.AgentStateDir(agentID)
	if err := ensurePrivateDir(stateDir); err != nil {
		return fmt.Errorf("ensure agent state dir for lock: %w", err)
	}
	lockPath := filepath.Join(stateDir, ".lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open agent lock %s: %w", lockPath, err)
	}
	defer f.Close()
	handle := windows.Handle(f.Fd())

	// Lock the whole file. An OVERLAPPED with zero offset locks from byte 0;
	// the lock length spans the maximum range so the region is deterministic
	// regardless of file size.
	var overlapped windows.Overlapped
	const lockLow, lockHigh uint32 = ^uint32(0), ^uint32(0)
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY

	deadline := time.Now().Add(agentLockTimeout)
	for {
		err := windows.LockFileEx(handle, flags, 0, lockLow, lockHigh, &overlapped)
		if err == nil {
			break
		}
		// LOCKFILE_FAIL_IMMEDIATELY surfaces a held lock as ERROR_LOCK_VIOLATION
		// (and, on some paths, ERROR_IO_PENDING). Any other error is a real
		// failure to acquire and is returned immediately.
		if err != windows.ERROR_LOCK_VIOLATION && err != windows.ERROR_IO_PENDING {
			return fmt.Errorf("acquire agent lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for agent lock %s", agentLockTimeout, lockPath)
		}
		time.Sleep(agentLockRetryInterval)
	}
	defer func() {
		_ = windows.UnlockFileEx(handle, 0, lockLow, lockHigh, &overlapped)
	}()

	return fn()
}
