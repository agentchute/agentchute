package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

var hubDirNameRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

var hubMigrationReapMux = reapHubMigrationMux

func findHubMigrationCandidate(remote *loop.RemoteConfig, opts hubJoinOptions) (string, error) {
	current, currentErr := hubclient.ReadHubConfig(remote.HubID)
	currentExists := currentErr == nil
	if currentErr != nil && !errors.Is(currentErr, hubclient.ErrHubConfigNotFound) {
		return "", currentErr
	}
	root := filepath.Dir(remote.HubDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != remote.HubID && hubDirNameRE.MatchString(entry.Name()) {
			candidates = append(candidates, entry.Name())
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	fingerprint := ""
	if currentExists {
		fingerprint = current.HostKeyFingerprint
	}
	if fingerprint == "" {
		fingerprint, err = hubJoinDiscoverFingerprint(remote)
		if err != nil {
			return "", err
		}
	}
	for _, oldID := range candidates {
		oldCfg, err := hubclient.ReadHubConfig(oldID)
		if err != nil || oldCfg.HostKeyFingerprint == "" || oldCfg.HostKeyFingerprint != fingerprint {
			continue
		}
		if currentExists {
			if current.Pool12 != "" && current.Pool12 == oldCfg.Pool12 && oldCfg.URL != remote.URL {
				return oldID, nil
			}
			continue
		}
		agentID, ok := migrationAgentID(oldCfg, opts)
		if !ok {
			continue
		}
		oldDir, err := loop.HubDir(oldID)
		if err != nil {
			return "", err
		}
		keyPath := filepath.Join(oldDir, "keys", agentID+"_ed25519")
		hello, warnings, err := hubJoinProbe(remote, agentID, keyPath)
		printHubJoinWarnings(warnings)
		if err == nil && hello.Pool12 == oldCfg.Pool12 {
			return oldID, nil
		}
	}
	return "", nil
}

func migrationAgentID(cfg *hubclient.HubConfig, opts hubJoinOptions) (string, bool) {
	if strings.TrimSpace(opts.AgentID) != "" {
		id := strings.TrimSpace(opts.AgentID)
		return id, containsString(cfg.JoinedAs, id)
	}
	spec, ok := wrapperForToken(strings.TrimSpace(opts.Name))
	if !ok {
		return "", false
	}
	id, ok := cfg.Names[spec.AgentID]
	return id, ok && containsString(cfg.JoinedAs, id)
}

func migrateHubJoinState(root, oldHubID string, remote *loop.RemoteConfig) error {
	oldDir, err := loop.HubDir(oldHubID)
	if err != nil {
		return err
	}
	oldCfg, err := hubclient.ReadHubConfig(oldHubID)
	if err != nil {
		return err
	}
	if err := refuseLiveHubMigration(root, oldDir, oldCfg); err != nil {
		return err
	}
	newDir := remote.HubDir
	partial := newDir + ".partial"
	if _, err := os.Stat(newDir); err == nil {
		// A committed new directory means this is post-rename recovery: only the
		// pointer and old-directory cleanup remain.
		if err := writeHubJoinPointer(root, remote.URL); err != nil {
			return err
		}
		hubMigrationReapMux(oldCfg, oldDir)
		if err := os.RemoveAll(oldDir); err != nil {
			return err
		}
		return fsyncHubDir(filepath.Dir(oldDir))
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(partial); err != nil {
		return err
	}
	if err := copyHubMigrationTree(oldDir, partial); err != nil {
		return err
	}
	newCfg := *oldCfg
	newCfg.URL = remote.URL
	if err := writeHubMigrationConfig(filepath.Join(partial, "config.json"), &newCfg); err != nil {
		return err
	}
	if err := fsyncHubDir(partial); err != nil {
		return err
	}
	if err := os.Rename(partial, newDir); err != nil {
		return err
	}
	if err := fsyncHubDir(filepath.Dir(newDir)); err != nil {
		return err
	}
	if err := writeHubJoinPointer(root, remote.URL); err != nil {
		return err
	}
	hubMigrationReapMux(oldCfg, oldDir)
	if err := os.RemoveAll(oldDir); err != nil {
		return err
	}
	return fsyncHubDir(filepath.Dir(oldDir))
}

func refuseLiveHubMigration(root, oldDir string, cfg *hubclient.HubConfig) error {
	shadow := filepath.Join(oldDir, ".agentchute", "loop")
	loopCfg := &loop.Config{ControlRepo: root, LoopDir: shadow}
	for _, agentID := range cfg.JoinedAs {
		state, err := loop.LoadRunnerState(loopCfg, agentID)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if errors.Is(err, loop.ErrRunnerStateAgentMismatch) {
			return fmt.Errorf("hub join: confirm lane %q is stopped, then move runner state %s to agent_id %q's state directory (or remove it if stale) and re-run. This agent_id mismatch prevents the join from proving the lane is stopped", agentID, loopCfg.RunnerStatePath(agentID), state.AgentID)
		}
		if err != nil {
			return fmt.Errorf("hub join: lane %q's runner state at %s could not be read or decoded (%v), so this join cannot prove the lane is stopped. Confirm the lane is stopped, then repair the JSON or remove the corrupt file before re-running", agentID, loopCfg.RunnerStatePath(agentID), err)
		}
		if !setupLocalHost(state.Host) || state.RunnerPID <= 0 || !setupProcessAlive(state.RunnerPID) {
			continue
		}
		cmdline := setupProcessCommandLine(state.RunnerPID)
		if remoteMigrationCommandMatches(cmdline, cfg.URL, shadow) {
			return fmt.Errorf("hub join: lane %q is still running against the old URL (serve pid %d). Stop that session first (Ctrl-C in its terminal, or end the serve process from its own supervisor), then re-run this join; the lane relaunches under the new URL afterwards", agentID, state.RunnerPID)
		}
		return fmt.Errorf("hub join: pid %d is alive but is recorded as lane %q's runner while its command line does not match this hub — possibly OS pid reuse over a stale runner.json. Refusing to migrate. Inspect with ps -p %d; if it is unrelated, remove %s and re-run this join", state.RunnerPID, agentID, state.RunnerPID, filepath.Join(oldDir, ".agentchute", "loop", "state", agentID, "runner.json"))
	}
	return nil
}

func remoteMigrationCommandMatches(cmdline, oldURL, shadow string) bool {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return false
	}
	normalized := " " + strings.Join(strings.Fields(strings.ToLower(cmdline)), " ") + " "
	serve := strings.Contains(normalized, " agentchute serve ") || strings.Contains(normalized, "/agentchute serve ")
	runAlias := strings.Contains(normalized, " agentchute run ") || strings.Contains(normalized, "/agentchute run ")
	if !serve && !runAlias {
		return false
	}
	if value := setupCommandFlagValue(cmdline, "--control-repo"); value != "" {
		parsed, err := loop.ParseRemoteURL(value)
		return err == nil && parsed.URL == oldURL
	}
	if value := setupCommandFlagValue(cmdline, "--loop-dir"); value != "" {
		return setupPathsEquivalent(value, shadow)
	}
	return true
}

func copyHubMigrationTree(src, dst string) error {
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "mux" || strings.HasPrefix(rel, "mux"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			return err
		}
		if err := in.Close(); err != nil {
			out.Close()
			return err
		}
		if err := out.Sync(); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		return err
	}
	var dirs []string
	if err := filepath.WalkDir(dst, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := fsyncHubDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeHubMigrationConfig(path string, cfg *hubclient.HubConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json.tmp-*")
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

func reapHubMigrationMux(cfg *hubclient.HubConfig, oldDir string) {
	remote, err := loop.ParseRemoteURL(cfg.URL)
	if err != nil || len(cfg.JoinedAs) == 0 {
		return
	}
	remote.HubDir = oldDir
	for _, agentID := range cfg.JoinedAs {
		keyPath := filepath.Join(oldDir, "keys", agentID+"_ed25519")
		_ = hubclient.ReapSSHMux(remote, agentID, keyPath, oldDir)
	}
}
