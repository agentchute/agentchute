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
				// #166: the adopted version may be missing its public half —
				// that is the state an interrupted mint leaves. Repair before
				// the join reaches readHubPublicKey, or a re-run converges to
				// the same failure and the operator loops.
				if err := ensureHubKeyPublic(keysDir, newest); err != nil {
					return hubKeyState{}, false, err
				}
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
	// The commoner half of #166: the symlink landed, the public half did not.
	// This branch never enters the adopt path above, so repairing only there
	// would look complete and leave this state looping exactly as before.
	if err := ensureHubKeyPublic(keysDir, state.Active); err != nil {
		return hubKeyState{}, false, err
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
	// #167: this returned a hubKeyVersion naming a `.pub` it never looked for,
	// so a keygen that exited 0 without writing it was reported as a complete
	// mint — the same read-a-zero-exit-as-success shape as the rest of that
	// sweep, and the state #166 then had to recover from.
	public := private + ".pub"
	if _, err := os.Stat(public); err != nil {
		return hubKeyVersion{}, fmt.Errorf("hub join: ssh-keygen exited 0 but %s is not there: %w", displayHomePath(public), err)
	}
	if err := fsyncHubDir(keysDir); err != nil {
		return hubKeyVersion{}, err
	}
	return hubKeyVersion{Number: number, Private: private, Public: public}, nil
}

func checkHubKeyPassphraseFree(path string) error {
	_, err := hubKeyPublicFromPrivate(path)
	return err
}

// hubKeyPublicFromPrivate derives the public half from the private one and
// returns it. `ssh-keygen -y` prints exactly the contents of the `.pub` file,
// which is why the probe doubles as the repair (#166): the caller that used to
// fail on a missing `.pub` already had the material to write one.
//
// It stays the passphrase probe too, because -y is what fails on an encrypted
// key. Only the discarded stdout is new (#167): the previous version read a
// zero exit as "not passphrase-protected" and threw away the answer it had
// asked for.
func hubKeyPublicFromPrivate(path string) (string, error) {
	out, err := runHubSSHKeygen("-y", "-P", "", "-f", path)
	if err != nil {
		return "", fmt.Errorf("ssh-keygen key probe: %w: %s", err, strings.TrimSpace(string(out)))
	}
	derived := strings.TrimSpace(string(out))
	if derived == "" {
		return "", fmt.Errorf("ssh-keygen printed no public key for %s", displayHomePath(path))
	}
	return derived, nil
}

// ensureHubKeyPublic writes the `.pub` half back when it is missing, deriving it
// from the private key rather than minting a replacement.
//
// #166: ssh-keygen writes the private file and then the public one, so an
// interrupted mint leaves a valid private key with no `.pub`. prepareHubKey
// adopted it — correctly, since the hub may already have authorized it — and the
// join then died reading the file that was never written. A plain re-run
// reproduced the same state, so the obvious operator action looped forever.
//
// Regenerating is the only safe repair. Minting a replacement would strand a
// credential the hub may already have authorized and present a different one,
// which is the failure family #165 is about.
//
// A `.pub` that EXISTS is never touched: rewriting it would be an unrequested
// write on the credential path, and it would overwrite the evidence of a real
// mismatch instead of leaving it to be seen. Any stat error other than
// not-exist is returned rather than treated as absence — an unreadable file is
// not a missing one.
func ensureHubKeyPublic(keysDir string, version hubKeyVersion) error {
	if _, err := os.Stat(version.Public); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	derived, err := hubKeyPublicFromPrivate(version.Private)
	if err != nil {
		return err
	}
	if err := writeHubKeyPublic(version.Public, derived); err != nil {
		return err
	}
	return fsyncHubDir(keysDir)
}

// writeHubKeyPublic writes through a temp file in the same directory and
// renames, so a second interruption cannot leave a half-written `.pub` — the
// exact shape of the state being repaired. 0644 matches what ssh-keygen writes:
// a public key is public, and a mode nothing else uses invites a later
// permission check to disagree with reality.
func writeHubKeyPublic(path, contents string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pub.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(contents + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
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
