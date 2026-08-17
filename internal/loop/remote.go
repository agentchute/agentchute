package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	remoteNamePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	ErrRemoteNotJoined = errors.New("remote hub is not joined on this machine")
)

func isRemoteLocator(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), "ssh://")
}

// RemoteConfig is the pure-local derivation of an ssh:// control-repo locator.
// Pool and Pool12 come from hello-ok via config.json, never from URL text.
type RemoteConfig struct {
	URL           string
	User          string
	Host          string
	Port          int
	PoolPath      string
	HubID         string
	HubDir        string
	ConfigPath    string
	ShadowLoopDir string
}

func (r *RemoteConfig) Destination() string {
	if r == nil {
		return ""
	}
	if r.User == "" {
		return r.Host
	}
	return r.User + "@" + r.Host
}

// ParseRemoteURL validates and canonicalizes the ssh locator without dialing.
func ParseRemoteURL(raw string) (*RemoteConfig, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid ssh control repo %q: %w", raw, err)
	}
	if u.Scheme != "ssh" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return nil, fmt.Errorf("invalid ssh control repo %q: expected ssh://[user@]host[:port]/absolute/path", raw)
	}
	user := ""
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return nil, fmt.Errorf("invalid ssh control repo %q: passwords are not allowed", raw)
		}
		user = u.User.Username()
		if u.User.String() != user || !validRemoteName(user) {
			return nil, fmt.Errorf("invalid ssh control repo %q: user must match [A-Za-z0-9._-]+ and not start with '-'", raw)
		}
	}
	host := u.Hostname()
	if !validRemoteName(host) {
		return nil, fmt.Errorf("invalid ssh control repo %q: host must match [A-Za-z0-9._-]+ and not start with '-'", raw)
	}
	host = strings.ToLower(host)
	port := 22
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid ssh control repo %q: port must be 1-65535", raw)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("invalid ssh control repo %q: empty port", raw)
	}
	if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return nil, fmt.Errorf("invalid ssh control repo %q: pool path must be absolute", raw)
	}
	canonicalPath := u.EscapedPath()
	if canonicalPath == "" {
		canonicalPath = "/"
	}
	if canonicalPath != "/" {
		canonicalPath = strings.TrimRight(canonicalPath, "/")
	}
	authority := host
	if user != "" {
		authority = user + "@" + authority
	}
	if port != 22 {
		authority += ":" + strconv.Itoa(port)
	}
	canonical := "ssh://" + authority + canonicalPath
	sum := sha256.Sum256([]byte(canonical))
	hubID := hex.EncodeToString(sum[:])[:12]
	hubDir, err := HubDir(hubID)
	if err != nil {
		return nil, err
	}
	return &RemoteConfig{
		URL: canonical, User: user, Host: host, Port: port, PoolPath: u.Path,
		HubID: hubID, HubDir: hubDir, ConfigPath: filepath.Join(hubDir, "config.json"),
		ShadowLoopDir: filepath.Join(hubDir, fixedDotDir, loopDirName),
	}, nil
}

func validRemoteName(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && remoteNamePattern.MatchString(value)
}

func HubDir(hubID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for hub state: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve home directory for hub state: empty home")
	}
	return filepath.Join(home, ".agentchute", "hub", hubID), nil
}

func HubConfigPath(hubID string) (string, error) {
	dir, err := HubDir(hubID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func ShadowLoopDir(hubID string) (string, error) {
	dir, err := HubDir(hubID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fixedDotDir, loopDirName), nil
}

type RemoteNotJoinedError struct {
	URL        string
	ConfigPath string
}

func (e *RemoteNotJoinedError) Error() string {
	configPath := e.ConfigPath
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, e.ConfigPath); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			configPath = filepath.Join("~", rel)
		}
	}
	return fmt.Sprintf("hub: .agentchute-control-repo points at %s, but this machine never joined that hub (no %s). If this machine IS the hub, a joined machine probably committed its pointer file — delete .agentchute-control-repo here (and `git rm` it if tracked). If this machine should be joined, run: agentchute hub join <that-url> --as <id>", e.URL, configPath)
}

func (e *RemoteNotJoinedError) Unwrap() error { return ErrRemoteNotJoined }

func IsRemoteNotJoined(err error) bool { return errors.Is(err, ErrRemoteNotJoined) }

// RequireRemoteJoin is a stat-only check. internal/loop never parses config.json.
func RequireRemoteJoin(remote *RemoteConfig) error {
	if remote == nil {
		return nil
	}
	info, err := os.Stat(remote.ConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &RemoteNotJoinedError{URL: remote.URL, ConfigPath: remote.ConfigPath}
		}
		return fmt.Errorf("stat hub config %s: %w", remote.ConfigPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hub config %s is not a regular file", remote.ConfigPath)
	}
	return nil
}
