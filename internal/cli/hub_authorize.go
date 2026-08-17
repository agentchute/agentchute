package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

const hubAuthorizedKeysMaxBytes = 4 << 20

var (
	hubAuthorizeExecutable = os.Executable
	hubAuthorizeLink       = os.Link
	hubAuthorizeNow        = time.Now
	hubSafePathPattern     = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	hubKeyTypePattern      = regexp.MustCompile(`^[a-z0-9-]+$`)
	hubKeyBlobPattern      = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)
	hubForcedLinePattern   = regexp.MustCompile(`^restrict,command="([^ "\\]+) hub session --agent ([^ "\\]+) --pool ([^ "\\]+) --pool-id ([^ "\\]+)" ([^ ]+) ([^ ]+) (agentchute:([^: ]+):([^ ]+))$`)
)

type hubAuthorizeOptions struct {
	Agent      string
	Pool       string
	Key        string
	List       bool
	Revoke     string
	ReplaceKey bool
}

type hubAuthorizePool struct {
	Path     string
	Resolved string
	PoolID   string
	Config   *loop.Config
}

type hubPublicKey struct {
	Type string
	Blob string
	Raw  []byte
}

func cmdHubAuthorize(args []string) error {
	fs := flag.NewFlagSet("hub authorize", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts hubAuthorizeOptions
	fs.StringVar(&opts.Agent, "agent", "", "agent id pinned by this key")
	fs.StringVar(&opts.Pool, "pool", "", "absolute hub control-repo path")
	fs.StringVar(&opts.Key, "key", "", "SSH public key")
	fs.BoolVar(&opts.List, "list", false, "list and audit managed authorized keys")
	fs.StringVar(&opts.Revoke, "revoke", "", "revoke an agent from this pool")
	fs.BoolVar(&opts.ReplaceKey, "replace-key", false, "replace an existing key for this agent and pool")
	if err := fs.Parse(args); err != nil {
		return hubAuthorizeUsage(err)
	}
	if fs.NArg() != 0 {
		return hubAuthorizeUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if err := validateHubAuthorizeOptions(opts); err != nil {
		return hubAuthorizeUsage(err)
	}
	return runHubAuthorize(opts, os.Stdout)
}

func validateHubAuthorizeOptions(opts hubAuthorizeOptions) error {
	if opts.List {
		if opts.Agent != "" || opts.Pool != "" || opts.Key != "" || opts.Revoke != "" || opts.ReplaceKey {
			return fmt.Errorf("--list cannot be combined with authorization or revocation flags")
		}
		return nil
	}
	if opts.Revoke != "" {
		if opts.Pool == "" {
			return fmt.Errorf("--pool is required with --revoke")
		}
		if opts.Agent != "" || opts.Key != "" || opts.ReplaceKey {
			return fmt.Errorf("--revoke cannot be combined with --agent, --key, or --replace-key")
		}
		return loop.ValidateAgentID(opts.Revoke)
	}
	if opts.Agent == "" || opts.Pool == "" || opts.Key == "" {
		return fmt.Errorf("--agent, --pool, and --key are required")
	}
	return loop.ValidateAgentID(opts.Agent)
}

func hubAuthorizeUsage(err error) error {
	return fmt.Errorf("%w\n\nUsage:\n  agentchute hub authorize --agent <id> --pool <absolute-path> --key \"<pubkey>\" [--replace-key]\n  agentchute hub authorize --list\n  agentchute hub authorize --revoke <id> --pool <absolute-path>", err)
}

func runHubAuthorize(opts hubAuthorizeOptions, out io.Writer) error {
	if opts.List {
		return listHubAuthorizedKeys(out)
	}
	if opts.Revoke != "" {
		return revokeHubAuthorizedKey(opts.Revoke, opts.Pool, out)
	}
	return authorizeHubKey(opts, out)
}

func authorizeHubKey(opts hubAuthorizeOptions, out io.Writer) error {
	pool, err := resolveHubAuthorizePool(opts.Pool, true)
	if err != nil {
		return err
	}
	executable, err := resolveHubAuthorizeExecutable()
	if err != nil {
		return err
	}
	key, err := parseHubPublicKey(opts.Key)
	if err != nil {
		return err
	}
	marker := hubKeyMarker(opts.Agent, pool.PoolID)
	line := fmt.Sprintf("restrict,command=\"%s hub session --agent %s --pool %s --pool-id %s\" %s %s %s", executable, opts.Agent, pool.Path, pool.PoolID, key.Type, key.Blob, marker)

	var action string
	err = withHubAuthorizedKeysLock(func() error {
		path, sshDir, err := hubAuthorizedKeysPaths()
		if err != nil {
			return err
		}
		if err := ensureHubSSHDir(sshDir); err != nil {
			return err
		}
		data, info, err := readHubAuthorizedKeys(path)
		if err != nil {
			return err
		}
		lines := hubAuthorizedKeyLines(data)
		indexes := hubMarkerIndexes(lines, marker)
		if len(indexes) > 0 {
			existing, keyErr := hubKeyFromAuthorizedLine(lines[indexes[0]])
			sameKey := keyErr == nil && existing.Type == key.Type && existing.Blob == key.Blob
			if !sameKey && !opts.ReplaceKey {
				fingerprint := "unreadable key"
				if keyErr == nil {
					fingerprint = hubKeyFingerprint(existing)
				}
				added := "unknown date"
				if info != nil {
					added = info.ModTime().Format("2006-01-02")
				}
				return duplicateHubAgentError(opts.Agent, fingerprint, added)
			}
			if sameKey && len(indexes) == 1 && lines[indexes[0]] == line {
				if err := enforceHubAuthorizedKeysMode(path); err != nil {
					return err
				}
				action = "already present"
				return nil
			}
			lines = replaceHubMarkerLines(lines, indexes, line)
			if opts.ReplaceKey && !sameKey {
				action = "replaced"
			} else {
				action = "updated"
			}
		} else {
			lines = append(lines, line)
			action = "appended"
		}
		return writeHubAuthorizedKeys(path, lines)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "authorized: %s -> pool %s (canonical; marker %s)\n", opts.Agent, pool.Path, marker)
	destination := "in"
	if action == "appended" {
		destination = "to"
	}
	fmt.Fprintf(out, "key %s %s — 1 line %s %s %s\n", strings.TrimPrefix(key.Type, "ssh-"), hubKeyFingerprint(key), action, destination, displayHomePath(mustHubAuthorizedKeysPath()))
	return nil
}

func revokeHubAuthorizedKey(agentID, poolArg string, out io.Writer) error {
	pool, err := resolveHubAuthorizePool(poolArg, false)
	if err != nil {
		return err
	}
	marker := hubKeyMarker(agentID, pool.PoolID)
	removed := 0
	err = withHubAuthorizedKeysLock(func() error {
		path, sshDir, err := hubAuthorizedKeysPaths()
		if err != nil {
			return err
		}
		if err := ensureHubSSHDir(sshDir); err != nil {
			return err
		}
		data, _, err := readHubAuthorizedKeys(path)
		if err != nil {
			return err
		}
		lines := hubAuthorizedKeyLines(data)
		kept := lines[:0]
		for _, line := range lines {
			if hubLineMarker(line) == marker {
				removed++
				continue
			}
			kept = append(kept, line)
		}
		if removed == 0 {
			if _, err := os.Stat(path); err == nil {
				return enforceHubAuthorizedKeysMode(path)
			} else if !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return writeHubAuthorizedKeys(path, kept)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "revoked: %s from pool %s (%d line(s) removed)\n", agentID, pool.Path, removed)
	if claim, err := loop.ReadServeClaim(pool.Config, agentID); err == nil && !loop.ClaimIsStale(claim, hubAuthorizeNow().UTC()) && setupLocalHost(claim.Host) && claim.PID > 0 && setupProcessAlive(claim.PID) {
		fmt.Fprintf(out, "note: the live session remains active until it disconnects; for an immediate cut, inspect it and run: kill %d\n", claim.PID)
	}
	return nil
}

func listHubAuthorizedKeys(out io.Writer) error {
	path, sshDir, err := hubAuthorizedKeysPaths()
	if err != nil {
		return err
	}
	data, _, err := readHubAuthorizedKeys(path)
	if err != nil {
		return err
	}
	sshModeOK := hubPathModeIs(sshDir, os.ModeDir, 0o700)
	keysModeOK := hubPathModeIs(path, 0, 0o600)
	marked := 0
	failed := 0
	for i, line := range hubAuthorizedKeyLines(data) {
		if !strings.Contains(line, "agentchute:") {
			continue
		}
		marked++
		marker := hubLineMarker(line)
		if marker == "" {
			marker = fmt.Sprintf("line %d", i+1)
		}
		reasons := auditHubAuthorizedLine(line)
		if !sshModeOK {
			reasons = append(reasons, ".ssh must be mode 0700")
		}
		if !keysModeOK {
			reasons = append(reasons, "authorized_keys must be mode 0600")
		}
		if len(reasons) == 0 {
			fmt.Fprintf(out, "PASS %s\n", marker)
		} else {
			failed++
			fmt.Fprintf(out, "FAIL %s: %s\n", marker, strings.Join(reasons, "; "))
		}
	}
	if marked == 0 {
		fmt.Fprintln(out, "no agentchute-authorized keys")
		sshExists := pathExists(sshDir)
		keysExists := pathExists(path)
		if (sshExists && !sshModeOK) || (keysExists && !keysModeOK) {
			return fmt.Errorf("hub authorize --list: SSH key permissions are unhealthy; set .ssh to 0700 and authorized_keys to 0600")
		}
	}
	if failed > 0 {
		return fmt.Errorf("hub authorize --list: %d marked line(s) failed health checks; repair or re-authorize them before accepting joins", failed)
	}
	return nil
}

func resolveHubAuthorizePool(poolArg string, mint bool) (*hubAuthorizePool, error) {
	if !filepath.IsAbs(poolArg) {
		return nil, fmt.Errorf("hub authorize: --pool must be an absolute path: %q. Run pwd in the control repo and re-run", poolArg)
	}
	poolPath := filepath.Clean(poolArg)
	if !hubSafePathPattern.MatchString(poolPath) {
		return nil, unsafeHubAuthorizePath("pool", poolPath)
	}
	resolved, err := filepath.EvalSymlinks(poolPath)
	if err != nil {
		return nil, fmt.Errorf("hub authorize: pool %q does not resolve: %v. Check the path on the hub and re-run", poolPath, err)
	}
	resolved = filepath.Clean(resolved)
	specInfo, specErr := os.Stat(filepath.Join(resolved, "AGENTCHUTE.md"))
	loopDir := filepath.Join(resolved, ".agentchute", "loop")
	loopInfo, loopErr := os.Stat(loopDir)
	if specErr != nil || loopErr != nil || !specInfo.Mode().IsRegular() || !loopInfo.IsDir() {
		return nil, fmt.Errorf("hub authorize: %q is not an agentchute pool (expected a regular AGENTCHUTE.md and .agentchute/loop directory). Run this with the pool's absolute path", poolPath)
	}
	identityPath := filepath.Join(loopDir, "state", "pool.id")
	var poolID string
	if mint {
		poolID, err = readOrMintHubPoolID(identityPath, resolved)
	} else {
		poolID, err = readHubPoolID(identityPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("hub authorize: %s does not exist, so this pool has no identity to revoke. Authorize a key for this pool first, or verify that --pool names the original pool", identityPath)
		}
	}
	if err != nil {
		return nil, err
	}
	return &hubAuthorizePool{
		Path: poolPath, Resolved: resolved, PoolID: poolID,
		Config: &loop.Config{ControlRepo: resolved, LoopDir: loopDir, Vendor: "agentchute", ControlRepoOrigin: "hub", LoopDirOrigin: "hub"},
	}, nil
}

func readOrMintHubPoolID(path, normalizedPool string) (string, error) {
	poolID, err := readHubPoolID(path)
	if err == nil {
		return poolID, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	digest := sha256.Sum256([]byte(normalizedPool))
	candidate := hex.EncodeToString(digest[:])[:12]
	dir := filepath.Dir(path)
	if err := loop.EnsurePrivateDir(dir); err != nil {
		return "", fmt.Errorf("hub authorize: create pool identity directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp_pool.id_")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(candidate + "\n"); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := hubAuthorizeLink(tmpName, path); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
	} else if err := fsyncHubDir(dir); err != nil {
		return "", err
	}
	return readHubPoolID(path)
}

func readHubPoolID(path string) (string, error) {
	data, err := loop.ReadFileLimit(path, 64)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", invalidHubAuthorizePoolID(path)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !poolIDPattern.Match(data) {
		return "", invalidHubAuthorizePoolID(path)
	}
	return string(data[:12]), nil
}

func invalidHubAuthorizePoolID(path string) error {
	return fmt.Errorf("hub authorize: %s is not a valid pool identity (must be a regular 0600 file containing exactly 12 lowercase hex characters). Nothing was written to authorized_keys. Inspect the file; if it is corrupt, delete it and re-run authorize to mint a fresh identity (existing key lines for this pool will then need re-authorizing)", path)
}

func resolveHubAuthorizeExecutable() (string, error) {
	executable, err := hubAuthorizeExecutable()
	if err != nil {
		return "", fmt.Errorf("hub authorize: resolve the running agentchute binary: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	executable = filepath.Clean(executable)
	if !hubSafePathPattern.MatchString(executable) {
		return "", unsafeHubAuthorizePath("binary", executable)
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("hub authorize: resolved binary %q is not an executable regular file. Install agentchute at a stable absolute path and re-run", executable)
	}
	return executable, nil
}

func unsafeHubAuthorizePath(kind, path string) error {
	return fmt.Errorf("hub authorize: %s path contains characters outside the safe set [A-Za-z0-9._/+-] (spaces, quotes, and shell metacharacters are refused rather than escaped): %q. Move or symlink the %s to a plain path and re-run", kind, path, kind)
}

func parseHubPublicKey(value string) (hubPublicKey, error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) < 2 || !hubKeyTypePattern.MatchString(fields[0]) || !hubKeyBlobPattern.MatchString(fields[1]) {
		return hubPublicKey{}, fmt.Errorf("hub authorize: --key must contain a lowercase SSH key type and base64 blob. Copy the complete public key and re-run")
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(raw) == 0 {
		return hubPublicKey{}, fmt.Errorf("hub authorize: --key contains an invalid base64 blob. Copy the complete public key and re-run")
	}
	return hubPublicKey{Type: fields[0], Blob: fields[1], Raw: raw}, nil
}

func hubKeyFingerprint(key hubPublicKey) string {
	digest := sha256.Sum256(key.Raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:])
}

func hubKeyMarker(agentID, poolID string) string {
	return "agentchute:" + agentID + ":" + poolID
}

func duplicateHubAgentError(agentID, fingerprint, added string) error {
	return fmt.Errorf("hub authorize: %q already has an authorized key (%s, added %s). One key = one agent id. If this machine REPLACES the old one, re-run with --replace-key. If both machines should run, join the new one under its own id — ids are cheap, and a shared id would collide on the serve lease anyway. (Auto-derived names collide when two machines share a hostname; pick an explicit id on one of them: agentchute hub join <url> --as %s2.)", agentID, fingerprint, added, agentID)
}

func hubAuthorizedKeysPaths() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("hub authorize: resolve the SSH login user's home directory: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", "", fmt.Errorf("hub authorize: resolve the SSH login user's home directory: empty home")
	}
	sshDir := filepath.Join(home, ".ssh")
	return filepath.Join(sshDir, "authorized_keys"), sshDir, nil
}

func withHubAuthorizedKeysLock(fn func() error) error {
	err := withHubLock("authorized_keys", fn)
	if err != nil && strings.HasPrefix(err.Error(), "hub join:") {
		message := strings.TrimPrefix(err.Error(), "hub join:")
		message = strings.Replace(message, "another agentchute hub join/rotate is already running for this hub", "another agentchute hub authorize operation is already updating authorized_keys", 1)
		return fmt.Errorf("hub authorize:%s", message)
	}
	return err
}

func mustHubAuthorizedKeysPath() string {
	path, _, _ := hubAuthorizedKeysPaths()
	return path
}

func ensureHubSSHDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("hub authorize: create %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("hub authorize: %s must be a real directory, not a symlink. Replace it with the SSH login user's .ssh directory and re-run", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("hub authorize: set %s to mode 0700: %w", path, err)
	}
	return nil
}

func readHubAuthorizedKeys(path string) ([]byte, os.FileInfo, error) {
	data, err := loop.ReadFileLimit(path, hubAuthorizedKeysMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("hub authorize: read %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("hub authorize: %s must be a regular, non-symlink file", path)
	}
	return data, info, nil
}

func enforceHubAuthorizedKeysMode(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("hub authorize: set %s to mode 0600: %w", path, err)
	}
	return nil
}

func writeHubAuthorizedKeys(path string, lines []string) error {
	data := []byte{}
	if len(lines) > 0 {
		data = []byte(strings.Join(lines, "\n") + "\n")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp_authorized_keys_")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncHubDir(dir)
}

func hubAuthorizedKeyLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func hubLineMarker(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	marker := fields[len(fields)-1]
	parts := strings.Split(marker, ":")
	if len(parts) != 3 || parts[0] != "agentchute" {
		return ""
	}
	return marker
}

func hubMarkerIndexes(lines []string, marker string) []int {
	var indexes []int
	for i, line := range lines {
		if hubLineMarker(line) == marker {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func replaceHubMarkerLines(lines []string, indexes []int, replacement string) []string {
	matched := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		matched[index] = true
	}
	out := make([]string, 0, len(lines)-len(indexes)+1)
	inserted := false
	for i, line := range lines {
		if !matched[i] {
			out = append(out, line)
			continue
		}
		if !inserted {
			out = append(out, replacement)
			inserted = true
		}
	}
	return out
}

func hubKeyFromAuthorizedLine(line string) (hubPublicKey, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return hubPublicKey{}, fmt.Errorf("missing key fields")
	}
	return parseHubPublicKey(fields[len(fields)-3] + " " + fields[len(fields)-2])
}

func auditHubAuthorizedLine(line string) []string {
	matches := hubForcedLinePattern.FindStringSubmatch(line)
	if matches == nil {
		return []string{"line is not the canonical restrict forced-command form; re-authorize this key"}
	}
	binaryPath, agentID, poolPath, poolID := matches[1], matches[2], matches[3], matches[4]
	keyType, keyBlob, markerAgent, markerPoolID := matches[5], matches[6], matches[8], matches[9]
	var reasons []string
	if !filepath.IsAbs(binaryPath) || !hubSafePathPattern.MatchString(binaryPath) {
		reasons = append(reasons, "binary path is unsafe or non-absolute")
	} else if info, err := os.Stat(binaryPath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		reasons = append(reasons, "binary does not exist or is not executable")
	}
	if loop.ValidateAgentID(agentID) != nil || agentID != markerAgent {
		reasons = append(reasons, "forced-command agent and marker do not match")
	}
	if !poolIDPattern.MatchString(poolID+"\n") || poolID != markerPoolID {
		reasons = append(reasons, "forced-command pool id and marker do not match")
	}
	if _, err := parseHubPublicKey(keyType + " " + keyBlob); err != nil {
		reasons = append(reasons, "public key fields are invalid")
	}
	if !filepath.IsAbs(poolPath) || !hubSafePathPattern.MatchString(poolPath) {
		reasons = append(reasons, "pool path is unsafe or non-absolute")
	} else if pool, err := resolveHubAuthorizePool(poolPath, false); err != nil {
		reasons = append(reasons, "pool does not resolve to a healthy identified pool")
	} else if pool.PoolID != poolID {
		reasons = append(reasons, "pool identity does not match the forced command")
	}
	return reasons
}

func hubPathModeIs(path string, requiredType os.FileMode, perm os.FileMode) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if requiredType == os.ModeDir && !info.IsDir() {
		return false
	}
	if requiredType == 0 && !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm() == perm
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
