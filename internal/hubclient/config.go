package hubclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentchute/agentchute/internal/loop"
)

const maxHubConfigBytes = 64 << 10

var ErrHubConfigNotFound = errors.New("hub config not found")

type HubConfig struct {
	URL      string            `json:"url"`
	JoinedAs []string          `json:"joined_as"`
	Names    map[string]string `json:"names"`
	Pool     string            `json:"pool"`
	Pool12   string            `json:"pool12"`
}

func ReadHubConfig(hubID string) (*HubConfig, error) {
	path, err := loop.HubConfigPath(hubID)
	if err != nil {
		return nil, err
	}
	data, err := loop.ReadFileLimit(path, maxHubConfigBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrHubConfigNotFound, path)
		}
		return nil, fmt.Errorf("read hub config %s: %w", path, err)
	}
	var cfg HubConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse hub config %s: %w", path, err)
	}
	return &cfg, nil
}

func WriteHubConfig(hubID string, cfg *HubConfig) error {
	if cfg == nil {
		return fmt.Errorf("write hub config: nil config")
	}
	path, err := loop.HubConfigPath(hubID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := loop.EnsurePrivateDir(dir); err != nil {
		return fmt.Errorf("create hub config directory %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hub config: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".config.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary hub config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temporary hub config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary hub config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temporary hub config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary hub config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit hub config %s: %w", path, err)
	}
	return nil
}
