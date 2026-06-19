//go:build windows

package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// agentLockTimeout bounds how long withAgentLock waits to acquire the per-agent
// lock before giving up, so a stuck peer never hangs a command forever.
const agentLockTimeout = 5 * time.Second

// agentLockRetryInterval is the poll cadence for the create-loop.
const agentLockRetryInterval = 25 * time.Millisecond

// agentLockStaleAfter lets a competing acquirer reclaim a lock whose holder
// crashed without releasing it. Bounds livelock if a process dies mid-section.
const agentLockStaleAfter = 30 * time.Second

// withAgentLock runs fn while holding an exclusive lock scoped to a single
// agent's local state directory. golang.org/x/sys/windows is not a dependency,
// so this uses an O_CREATE|O_EXCL create-loop with bounded retry: the existence
// of the lock file IS the lock. A stale lock (older than agentLockStaleAfter)
// is reclaimed so a crashed holder cannot wedge the agent permanently.
//
// CRITICAL — callers must never nest withAgentLock for the same agentID in one
// call stack: the create would EEXIST against the lock we already hold and spin
// until timeout. The read, mutate, and write all happen inside the single fn,
// and load/save helpers fn calls are the lock-free inner variants.
func withAgentLock(cfg *Config, agentID string, fn func() error) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	stateDir := cfg.AgentStateDir(agentID)
	if err := ensurePrivateDir(stateDir); err != nil {
		return fmt.Errorf("ensure agent state dir for lock: %w", err)
	}
	lockPath := filepath.Join(stateDir, ".lock")

	deadline := time.Now().Add(agentLockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("acquire agent lock %s: %w", lockPath, err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > agentLockStaleAfter {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for agent lock %s", agentLockTimeout, lockPath)
		}
		time.Sleep(agentLockRetryInterval)
	}
	defer func() { _ = os.Remove(lockPath) }()

	return fn()
}
