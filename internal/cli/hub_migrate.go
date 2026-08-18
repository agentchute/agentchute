package cli

import (
	"bytes"
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
		// The new directory exists. That is post-rename recovery ONLY if this
		// migration is the thing that created it (issue #165): existence alone
		// is also what a failed fresh join and a completed separate join leave
		// behind, and neither of those carries the old directory's contents.
		// Taking the recovery path against one of those deleted the old hub dir
		// — including the authorized key — without copying anything first.
		if err := checkHubMigrationProvenance(newDir, oldHubID); err != nil {
			return err
		}
		return finishHubMigration(root, remote, oldDir, oldCfg)
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
	// Written BEFORE the rename, so the directory carries its own provenance the
	// moment it exists under its final name. A marker written afterwards would
	// leave exactly the window this fix is about.
	if err := os.WriteFile(filepath.Join(partial, hubMigrationMarker), []byte(oldHubID+"\n"), 0o600); err != nil {
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
	return finishHubMigration(root, remote, oldDir, oldCfg)
}

// hubMigrationMarker records, inside the new directory, which old hub directory
// this one was renamed out of. It is the rename provenance the recovery branch
// keys on, and it is removed once the migration is complete.
const hubMigrationMarker = ".migrated-from"

// checkHubMigrationProvenance answers one question: did THIS migration create
// newDir? Only a marker naming oldHubID says yes. Anything else — no marker, an
// unreadable one, or one naming a different hub — means the directory came from
// somewhere else and its contents are not the old directory's, so the migration
// refuses instead of deleting state it cannot reproduce.
func checkHubMigrationProvenance(newDir, oldHubID string) error {
	data, err := os.ReadFile(filepath.Join(newDir, hubMigrationMarker))
	if err == nil && strings.TrimSpace(string(data)) == oldHubID {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return fmt.Errorf("hub join: %s already exists and was not created by this migration, so migrating into it would delete %s — including its authorized key — without copying anything. Nothing was changed. If that directory is left over from a failed join to this same hub, remove it and re-run; if it is a real join in its own right, finish or undo that one first", newDir, hubDirForMessage(oldHubID))
}

// hubDirForMessage renders a hub id for an error message, falling back to the
// bare id when the path cannot be built — a message is never worth failing a
// refusal over. Not named oldDir: that is a local variable throughout this file,
// and a function it shadows in some scopes but not others is a trap.
func hubDirForMessage(hubID string) string {
	if dir, err := loop.HubDir(hubID); err == nil {
		return dir
	}
	return hubID
}

// finishHubMigration performs the steps that are shared by the first pass and by
// post-rename recovery: prove the new directory really carries the old one's
// contents, then point at it, reap the old mux sockets and delete the old tree.
//
// verifyHubMigrationCopy is deliberately INDEPENDENT of the provenance check
// above. Provenance says who created the directory; this says what is actually
// in it. The RemoveAll is irreversible, so it is gated on the second question
// rather than inferred from the first.
func finishHubMigration(root string, remote *loop.RemoteConfig, oldDir string, oldCfg *hubclient.HubConfig) error {
	newDir := remote.HubDir
	if err := verifyHubMigrationCopy(oldDir, newDir); err != nil {
		return err
	}
	if err := writeHubJoinPointer(root, remote.URL); err != nil {
		return err
	}
	hubMigrationReapMux(oldCfg, oldDir)
	if err := os.RemoveAll(oldDir); err != nil {
		return err
	}
	if err := fsyncHubDir(filepath.Dir(oldDir)); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(newDir, hubMigrationMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return fsyncHubDir(newDir)
}

// verifyHubMigrationCopy fails unless every path under oldDir is present under
// newDir with the same content. config.json is exempt because the migration
// rewrites its URL by design, and mux is exempt because it is sockets.
func verifyHubMigrationCopy(oldDir, newDir string) error {
	return filepath.WalkDir(oldDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(oldDir, path)
		if err != nil {
			return err
		}
		if rel == "." || rel == "config.json" {
			return nil
		}
		if rel == "mux" || strings.HasPrefix(rel, "mux"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(newDir, rel)
		missing := fmt.Errorf("hub join: refusing to delete %s: %s was not copied to %s. Nothing was changed", oldDir, path, target)
		if entry.Type()&os.ModeSymlink != 0 {
			was, err := os.Readlink(path)
			if err != nil {
				return err
			}
			now, err := os.Readlink(target)
			if err != nil || now != was {
				return missing
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if stat, err := os.Lstat(target); err != nil || !stat.IsDir() {
				return missing
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		was, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		now, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(was, now) {
			return missing
		}
		return nil
	})
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
