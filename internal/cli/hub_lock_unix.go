//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

var hubLockTimeout = 5 * time.Second

const hubLockRetryInterval = 25 * time.Millisecond

func withHubLock(hubID string, fn func() error) error {
	return withHubLocks([]string{hubID}, fn)
}

func withHubLocks(hubIDs []string, fn func() error) error {
	ids := append([]string(nil), hubIDs...)
	sort.Strings(ids)
	var files []*os.File
	defer func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(files[i].Fd()), syscall.LOCK_UN)
			_ = files[i].Close()
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
		deadline := time.Now().Add(hubLockTimeout)
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
				f.Close()
				return err
			}
			if time.Now().After(deadline) {
				f.Close()
				return fmt.Errorf("hub join: another agentchute hub join/rotate is already running for this hub (lock %s). Wait for it to finish and re-run", displayHomePath(lockPath))
			}
			time.Sleep(hubLockRetryInterval)
		}
		files = append(files, f)
	}
	return fn()
}
