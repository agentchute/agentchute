//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
	"golang.org/x/sys/windows"
)

var hubLockTimeout = 5 * time.Second

const hubLockRetryInterval = 25 * time.Millisecond

// hubLockMode — see the doc on the unix implementation; the semantics are the
// same and the two must not drift.
type hubLockMode int

const (
	hubLockExclusive hubLockMode = iota
	hubLockShared
)

// flags returns the LockFileEx flags. A SHARED lock is simply the absence of
// LOCKFILE_EXCLUSIVE_LOCK; FAIL_IMMEDIATELY is kept in both modes so the caller
// refuses rather than hangs, matching the unix side.
func (m hubLockMode) flags() uint32 {
	if m == hubLockShared {
		return windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	return windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
}

type hubWindowsLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireHubLocks(hubIDs []string, mode hubLockMode) (func(), error) {
	ids := append([]string(nil), hubIDs...)
	sort.Strings(ids)
	var locks []hubWindowsLock
	const lockLow, lockHigh uint32 = ^uint32(0), ^uint32(0)
	release := func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = windows.UnlockFileEx(windows.Handle(locks[i].file.Fd()), 0, lockLow, lockHigh, &locks[i].overlapped)
			_ = locks[i].file.Close()
		}
		locks = nil
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
		// LOAD-BEARING: a SIBLING of the hub dirs, never inside one — see the
		// unix implementation. A migration renames the hub dir out from under
		// everything running; a lock kept inside it would be renamed away
		// mid-hold and the exclusion would silently stop working.
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
		lock := hubWindowsLock{file: f}
		deadline := time.Now().Add(hubLockTimeout)
		for {
			err = windows.LockFileEx(windows.Handle(f.Fd()), mode.flags(), 0, lockLow, lockHigh, &lock.overlapped)
			if err == nil {
				break
			}
			if err != windows.ERROR_LOCK_VIOLATION && err != windows.ERROR_IO_PENDING {
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
		locks = append(locks, lock)
	}
	return release, nil
}
