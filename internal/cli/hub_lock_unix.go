//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

var hubLockTimeout = 5 * time.Second

const hubLockRetryInterval = 25 * time.Millisecond

// hubLockMode distinguishes the two users of these locks.
//
// SHARED is what a `serve` lane takes, for its whole lifetime, when its config is
// remote. EXCLUSIVE is what a migration takes, on BOTH hub ids. The pair is what
// makes the migration's guarantee real rather than best-effort: the hazard is a
// PROCESS holding a descriptor into the tree — runner.log lives inside it — and a
// rename moves a name, not an inode, so freezing narrows that window but cannot
// close it. Excluding the process does.
type hubLockMode int

const (
	hubLockExclusive hubLockMode = iota
	hubLockShared
)

func (m hubLockMode) flag() int {
	if m == hubLockShared {
		return syscall.LOCK_SH
	}
	return syscall.LOCK_EX
}

// acquireHubLocks takes the named hub locks and returns a release function.
//
// Acquisition is always non-blocking with a bounded retry, so a caller REFUSES
// rather than hangs; flock releases on process death, so a lane that dies never
// leaves a stale lock behind.
func acquireHubLocks(hubIDs []string, mode hubLockMode) (func(), error) {
	ids := append([]string(nil), hubIDs...)
	sort.Strings(ids) // a stable order across callers is what prevents deadlock
	var files []*os.File
	release := func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(files[i].Fd()), syscall.LOCK_UN)
			_ = files[i].Close()
		}
		files = nil
	}
	for i, hubID := range ids {
		if i > 0 && hubID == ids[i-1] {
			continue
		}
		dir, err := loop.HubDir(hubID)
		if err != nil {
			release()
			return nil, err
		}
		// LOAD-BEARING: the lock lives in <parent of HubDir>/.locks, a SIBLING of
		// the hub dirs, never inside one. A migration renames the hub dir out from
		// under everything that is running, so a lock kept inside it would be
		// renamed away mid-hold and the exclusion would silently stop working.
		// Do not "tidy" this into the hub dir.
		lockDir := filepath.Join(filepath.Dir(dir), ".locks")
		if err := loop.EnsurePrivateDir(lockDir); err != nil {
			release()
			return nil, err
		}
		lockPath := filepath.Join(lockDir, hubID+".lock")
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			release()
			return nil, err
		}
		deadline := time.Now().Add(hubLockTimeout)
		for {
			err = syscall.Flock(int(f.Fd()), mode.flag()|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
				f.Close()
				release()
				return nil, err
			}
			if time.Now().After(deadline) {
				f.Close()
				release()
				return nil, hubLockBusyError(lockPath, mode)
			}
			time.Sleep(hubLockRetryInterval)
		}
		files = append(files, f)
	}
	return release, nil
}
