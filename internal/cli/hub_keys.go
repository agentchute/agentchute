package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

var hubKeyVersionRE = regexp.MustCompile(`^[1-9][0-9]*$`)

var runHubSSHKeygen = func(args ...string) ([]byte, error) {
	return exec.Command("ssh-keygen", args...).CombinedOutput()
}

var hubJoinNow = func() time.Time { return time.Now().UTC() }

type hubKeyVersion struct {
	Number  int
	Private string
	Public  string
}

type hubKeyState struct {
	Active   hubKeyVersion
	Versions []hubKeyVersion
	Staged   *hubKeyVersion
}

func prepareHubKey(remoteDir, agentID string) (hubKeyState, bool, error) {
	keysDir := filepath.Join(remoteDir, "keys")
	if err := loop.EnsurePrivateDir(keysDir); err != nil {
		return hubKeyState{}, false, err
	}
	base := filepath.Join(keysDir, agentID+"_ed25519")
	_ = os.Remove(base + ".tmp")
	versions, err := scanHubKeyVersions(keysDir, agentID)
	if err != nil {
		return hubKeyState{}, false, err
	}

	info, err := os.Lstat(base)
	if os.IsNotExist(err) {
		if len(versions) > 0 {
			newest := versions[len(versions)-1]
			if err := checkHubKeyPassphraseFree(newest.Private); err == nil {
				if err := os.Symlink(filepath.Base(newest.Private), base); err != nil {
					return hubKeyState{}, false, err
				}
				return classifyHubKeyState(base, versions)
			}
			if err := retireHubKeyVersion(newest); err != nil {
				return hubKeyState{}, false, err
			}
			versions = versions[:len(versions)-1]
		}
		minted, err := mintHubKey(keysDir, agentID, nextHubKeyVersion(versions))
		if err != nil {
			return hubKeyState{}, false, err
		}
		if err := os.Symlink(filepath.Base(minted.Private), base); err != nil {
			return hubKeyState{}, false, err
		}
		versions = append(versions, minted)
		state, _, err := classifyHubKeyState(base, versions)
		return state, true, err
	}
	if err != nil {
		return hubKeyState{}, false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return hubKeyState{}, false, fmt.Errorf("hub join: %s exists but is not the required active-key symlink", displayHomePath(base))
	}
	state, _, err := classifyHubKeyState(base, versions)
	if err != nil {
		return hubKeyState{}, false, err
	}
	if err := checkHubKeyPassphraseFree(state.Active.Private); err != nil {
		return hubKeyState{}, false, fmt.Errorf("hub join: active key %s is corrupt or passphrase-protected; remove the passphrase or re-run with --rotate-key", displayHomePath(state.Active.Private))
	}
	return state, false, nil
}

func scanHubKeyVersions(keysDir, agentID string) ([]hubKeyVersion, error) {
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		return nil, err
	}
	prefix := agentID + "_ed25519.v"
	var versions []hubKeyVersion
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".pub") || strings.Contains(name, ".invalid.") {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if !hubKeyVersionRE.MatchString(suffix) {
			return nil, hubJoinKeyVersionError(name)
		}
		n, err := strconv.Atoi(suffix)
		if err != nil {
			return nil, fmt.Errorf("hub join: key version %s is too large", name)
		}
		path := filepath.Join(keysDir, name)
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("hub join: key version %s is not a regular file", displayHomePath(path))
		}
		versions = append(versions, hubKeyVersion{Number: n, Private: path, Public: path + ".pub"})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Number < versions[j].Number })
	return versions, nil
}

func classifyHubKeyState(activePath string, versions []hubKeyVersion) (hubKeyState, bool, error) {
	target, err := os.Readlink(activePath)
	if err != nil {
		return hubKeyState{}, false, err
	}
	if filepath.Base(target) != target {
		return hubKeyState{}, false, fmt.Errorf("hub join: active key symlink %s must name a version in the same keys directory", displayHomePath(activePath))
	}
	var active *hubKeyVersion
	for i := range versions {
		if filepath.Base(versions[i].Private) == target {
			active = &versions[i]
			break
		}
	}
	if active == nil {
		return hubKeyState{}, false, fmt.Errorf("hub join: active key symlink %s points at missing or invalid version %q", displayHomePath(activePath), target)
	}
	state := hubKeyState{Active: *active, Versions: versions}
	if newest := versions[len(versions)-1]; newest.Number > active.Number {
		copy := newest
		state.Staged = &copy
	}
	return state, false, nil
}

func nextHubKeyVersion(versions []hubKeyVersion) int {
	if len(versions) == 0 {
		return 1
	}
	return versions[len(versions)-1].Number + 1
}

func mintHubKey(keysDir, agentID string, number int) (hubKeyVersion, error) {
	private := filepath.Join(keysDir, fmt.Sprintf("%s_ed25519.v%d", agentID, number))
	if _, err := os.Lstat(private); !os.IsNotExist(err) {
		if err == nil {
			return hubKeyVersion{}, fmt.Errorf("hub join: refusing to overwrite existing key version %s", displayHomePath(private))
		}
		return hubKeyVersion{}, err
	}
	out, err := runHubSSHKeygen("-q", "-t", "ed25519", "-N", "", "-C", "agentchute:"+agentID, "-f", private)
	if err != nil {
		return hubKeyVersion{}, fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := fsyncHubDir(keysDir); err != nil {
		return hubKeyVersion{}, err
	}
	return hubKeyVersion{Number: number, Private: private, Public: private + ".pub"}, nil
}

func checkHubKeyPassphraseFree(path string) error {
	out, err := runHubSSHKeygen("-y", "-P", "", "-f", path)
	if err != nil {
		return fmt.Errorf("ssh-keygen key probe: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readHubPublicKey(version hubKeyVersion) (string, error) {
	data, err := os.ReadFile(version.Public)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", fmt.Errorf("public key %s is empty or malformed", displayHomePath(version.Public))
	}
	return key, nil
}

func mintStagedHubKey(remoteDir, agentID string, state hubKeyState) (hubKeyVersion, error) {
	version, err := mintHubKey(filepath.Join(remoteDir, "keys"), agentID, nextHubKeyVersion(state.Versions))
	return version, err
}

func promoteHubKey(remoteDir, agentID string, version hubKeyVersion) error {
	active := filepath.Join(remoteDir, "keys", agentID+"_ed25519")
	tmp := active + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(version.Private), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, active); err != nil {
		return err
	}
	return fsyncHubDir(filepath.Dir(active))
}

func pruneOlderHubKeys(state hubKeyState) error {
	for _, version := range state.Versions {
		if version.Number >= state.Active.Number {
			continue
		}
		if err := os.Remove(version.Private); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(version.Public); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return fsyncHubDir(filepath.Dir(state.Active.Private))
}

func retireHubKeyVersion(version hubKeyVersion) error {
	stampTime := hubJoinNow()
	for {
		stamp := loop.FormatStamp(stampTime)
		privateDst := version.Private + ".invalid." + stamp
		publicDst := version.Public + ".invalid." + stamp
		if _, err := os.Lstat(privateDst); os.IsNotExist(err) {
			if err := os.Rename(version.Private, privateDst); err != nil {
				return err
			}
			if _, err := os.Lstat(version.Public); err == nil {
				if err := os.Rename(version.Public, publicDst); err != nil {
					return err
				}
			}
			return fsyncHubDir(filepath.Dir(version.Private))
		}
		stampTime = stampTime.Add(time.Microsecond)
	}
}

func fsyncHubDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
