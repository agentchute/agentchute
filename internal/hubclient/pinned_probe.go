package hubclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

// pinnedHostAlias is the one name the pinned known_hosts file is written under,
// and the HostKeyAlias the pinned probe looks it up by: the pinned keys belong
// to the OLD hub URL, and the probe dials a NEW one, so neither real host name
// would match.
const pinnedHostAlias = "agentchute-pinned-hub"

// errNoPinnedHostKey: the file exists but pins nothing.
var errNoPinnedHostKey = errors.New("holds no host key to verify against")

// ProbeWithPinnedHostKey is Probe with no trust on first use: the server must
// prove possession of a host key already pinned in pinnedKnownHosts (the old
// hub's known_hosts) during key exchange, or ssh refuses before any agentchute
// frame is sent.
//
// A hub migration used to decide "same hub" from an ssh-keyscan fingerprint,
// which a server can present without holding the key, and then probed the new
// URL with accept-new, which trusts whatever key the new host offers (review
// 2026-10-08, S11). A successful pinned probe is the proof both of those
// assumed.
func ProbeWithPinnedHostKey(ctx context.Context, remote *loop.RemoteConfig, agentID, bin, keyPath, pinnedKnownHosts string) (hubwire.HelloOK, []string, error) {
	lines, err := pinnedHostKeyLines(pinnedKnownHosts)
	if err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	tmp, err := os.CreateTemp("", "agentchute-pinned-known-hosts-*")
	if err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		_ = tmp.Close()
		return hubwire.HelloOK{}, nil, err
	}
	if err := tmp.Close(); err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	stateDir := ""
	if keyPath != "" {
		stateDir = filepath.Dir(filepath.Dir(keyPath))
	}
	invocation, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: agentID, KeyPath: keyPath, StateDir: stateDir, PinnedKnownHosts: tmp.Name()})
	if err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	transport, err := startSSH(ctx, invocation)
	if err != nil {
		return hubwire.HelloOK{}, nil, err
	}
	session, err := OpenOneShotTransport(transport, remote, agentID, bin)
	if err != nil {
		return hubwire.HelloOK{}, invocation.Warnings, err
	}
	hello := session.Hello()
	if err := session.Close(); err != nil {
		return hubwire.HelloOK{}, invocation.Warnings, err
	}
	return hello, invocation.Warnings, nil
}

// pinnedHostKeyLines rewrites every usable key line of a known_hosts file under
// pinnedHostAlias. Comments are dropped. An @revoked key is removed from the
// set even where an ordinary line pins the same key — sshd's own rule
// (sshd(8), SSH_KNOWN_HOSTS FILE FORMAT), and the pin must not be weaker than
// plain ssh on the same file (PR #216 gate, codex P1). @cert-authority lines
// pin no specific host key and are not carried. A set with nothing left is an
// error: an empty pin must never read as "nothing to verify".
func pinnedHostKeyLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pinned host keys: %w", err)
	}
	defer f.Close()
	var keys []string
	revoked := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if fields[0] == "@revoked" {
			if len(fields) >= 4 {
				revoked[fields[2]+" "+fields[3]] = true
			}
			continue
		}
		if len(fields) < 3 || strings.HasPrefix(fields[0], "@") {
			continue
		}
		keys = append(keys, fields[1]+" "+fields[2])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pinned host keys %s: %w", path, err)
	}
	var out []string
	for _, key := range keys {
		if !revoked[key] {
			out = append(out, pinnedHostAlias+" "+key)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pinned host keys: %s %w", path, errNoPinnedHostKey)
	}
	return out, nil
}

// CarryHostKeyPin pins every host key in knownHostsPath for hostSpec as well,
// after a migration PROVED (ProbeWithPinnedHostKey) that the host behind
// hostSpec holds them. Keys already pinned for hostSpec are not repeated. A file
// with no key carries nothing (a pinned probe could not have passed against it).
func CarryHostKeyPin(knownHostsPath, hostSpec string) error {
	if _, err := os.Stat(knownHostsPath); os.IsNotExist(err) {
		return nil
	}
	keys, err := knownHostKeys(knownHostsPath)
	if err != nil {
		if errors.Is(err, errNoPinnedHostKey) {
			return nil
		}
		return err
	}
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var add []string
	for _, key := range keys {
		if line := hostSpec + " " + key; !have[line] {
			add = append(add, line)
			have[line] = true
		}
	}
	if len(add) == 0 {
		return nil
	}
	f, err := os.OpenFile(knownHostsPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(strings.Join(add, "\n") + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// knownHostKeys returns "<type> <blob>" for every usable key line.
func knownHostKeys(path string) ([]string, error) {
	lines, err := pinnedHostKeyLines(path)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(lines))
	for i, line := range lines {
		keys[i] = strings.TrimPrefix(line, pinnedHostAlias+" ")
	}
	return keys, nil
}
