package hubclient

import (
	"bufio"
	"context"
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
// pinnedHostAlias. Comments are dropped, and so are @revoked and
// @cert-authority lines: neither pins a host key. A file with no usable line is
// an error — an empty pin set must never read as "nothing to verify".
func pinnedHostKeyLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pinned host keys: %w", err)
	}
	defer f.Close()
	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") {
			continue
		}
		out = append(out, pinnedHostAlias+" "+fields[1]+" "+fields[2])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pinned host keys %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pinned host keys: %s holds no host key to verify against", path)
	}
	return out, nil
}
