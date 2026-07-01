//go:build !windows

package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// agentLockTimeout bounds how long withAgentLock waits to acquire the per-agent
// flock before giving up. A blocked acquisition must never hang the runner or a
// hook-driven command forever; a timeout surfaces as an error the caller logs.
//
// It is a package var (not a const) ONLY so tests can lower it to keep the
// bounded-wait test fast; production never reassigns it, so the effective
// behavior is the same 5s timeout.
var agentLockTimeout = 5 * time.Second

// agentLockRetryInterval is the poll cadence for the non-blocking flock loop.
const agentLockRetryInterval = 25 * time.Millisecond

// withAgentLock runs fn while holding an exclusive advisory lock scoped to a
// single agent's local state directory (<loop>/state/<agent>/.lock). It
// serializes the read-modify-write sequences over an agent's registration and
// ledger files so concurrent processes (runner poll loop, hook commands)
// cannot lose updates to each other.
//
// CRITICAL — flock is per (process, file) but a second LOCK_EX from the SAME
// process on a SECOND fd for the same file deadlocks. Callers must never nest
// withAgentLock for the same agentID within a single call stack: the read,
// mutate, and write all happen inside the single fn passed here, and the
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
	fd := int(f.Fd())

	deadline := time.Now().Add(agentLockTimeout)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			return fmt.Errorf("acquire agent lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for agent lock %s", agentLockTimeout, lockPath)
		}
		time.Sleep(agentLockRetryInterval)
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()

	return fn()
}
