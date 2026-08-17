package hubclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

const hubDownTTL = 30 * time.Second

type hubDownState struct {
	LastEConnect time.Time `json:"last_econnect"`
}

func HubDownPath(remote *loop.RemoteConfig) string {
	return filepath.Join(remote.HubDir, "hub-down.json")
}

func RecordConnectFailure(remote *loop.RemoteConfig, now time.Time) error {
	if remote == nil {
		return nil
	}
	if err := loop.EnsurePrivateDir(remote.HubDir); err != nil {
		return err
	}
	data, err := json.Marshal(hubDownState{LastEConnect: now.UTC()})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := HubDownPath(remote)
	tmp, err := os.CreateTemp(remote.HubDir, ".hub-down.json.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func ClearConnectFailure(remote *loop.RemoteConfig) error {
	if remote == nil {
		return nil
	}
	err := os.Remove(HubDownPath(remote))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func ConnectFailureCached(remote *loop.RemoteConfig, now time.Time) (bool, time.Duration, error) {
	if remote == nil {
		return false, 0, nil
	}
	path := HubDownPath(remote)
	data, err := loop.ReadFileLimit(path, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	var state hubDownState
	if err := json.Unmarshal(data, &state); err != nil {
		return false, 0, fmt.Errorf("parse %s: %w", path, err)
	}
	age := now.Sub(state.LastEConnect)
	if age < 0 {
		age = 0
	}
	return age < hubDownTTL, age, nil
}
