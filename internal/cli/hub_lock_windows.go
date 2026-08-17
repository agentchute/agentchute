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

type hubWindowsLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func withHubLock(hubID string, fn func() error) error {
	return withHubLocks([]string{hubID}, fn)
}

func withHubLocks(hubIDs []string, fn func() error) error {
	ids := append([]string(nil), hubIDs...)
	sort.Strings(ids)
	var locks []hubWindowsLock
	const lockLow, lockHigh uint32 = ^uint32(0), ^uint32(0)
	const flags = windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = windows.UnlockFileEx(windows.Handle(locks[i].file.Fd()), 0, lockLow, lockHigh, &locks[i].overlapped)
			_ = locks[i].file.Close()
		}
	}()
	for i, hubID := range ids {
		if i > 0 && hubID == ids[i-1] {
			continue
		}
		dir, err := loop.HubDir(hubID)
		if err != nil {
			return err
		}
		lockDir := filepath.Join(filepath.Dir(dir), ".locks")
		if err := loop.EnsurePrivateDir(lockDir); err != nil {
			return err
		}
		lockPath := filepath.Join(lockDir, hubID+".lock")
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		lock := hubWindowsLock{file: f}
		deadline := time.Now().Add(hubLockTimeout)
		for {
			err = windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, lockLow, lockHigh, &lock.overlapped)
			if err == nil {
				break
			}
			if err != windows.ERROR_LOCK_VIOLATION && err != windows.ERROR_IO_PENDING {
				f.Close()
				return err
			}
			if time.Now().After(deadline) {
				f.Close()
				return hubJoinBusyError(lockPath)
			}
			time.Sleep(hubLockRetryInterval)
		}
		locks = append(locks, lock)
	}
	return fn()
}
