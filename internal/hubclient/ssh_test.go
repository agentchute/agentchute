package hubclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestBuildSSHInvocationGolden(t *testing.T) {
	hubDir, err := os.MkdirTemp("/tmp", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hubDir) })
	remote := &loop.RemoteConfig{User: "alex", Host: "hub.example", Port: 2222, HubID: "0123456789ab", HubDir: hubDir}
	// The ControlPath is the owned per-user temp path, and that is the ONLY arm.
	// This golden previously expected a hub-dir-local path and passed solely
	// because the fixture's hubDir was a short /tmp temp dir; under any real
	// home that arm exceeded the 100-byte socket budget and never ran. The
	// golden agreed with the code and both were wrong about what shipped.
	uid := currentUserID()
	got, err := BuildSSHInvocation(SSHBuildOptions{
		Remote: remote, AgentID: "codex",
		TempRoots: []string{"/tmp"}, UserID: uid,
		EnsureOwned: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(hubDir, "known_hosts"),
		"-o", "IdentitiesOnly=yes", "-i", filepath.Join(hubDir, "keys", "codex_ed25519"),
		"-o", "ClearAllForwardings=yes",
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + filepath.Join("/tmp", "ac-"+uid, muxIsolationKey(remote, "codex", filepath.Join(hubDir, "keys", "codex_ed25519")), "%C"), "-o", "ControlPersist=60s",
		"-o", "LogLevel=ERROR",
		"-p", "2222", "alex@hub.example", "agentchute-hub",
	}
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got.Args, want)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("warnings = %v", got.Warnings)
	}

	channel, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: "codex", Channel: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(channel.Args, " ")
	for _, want := range []string{"ServerAliveInterval=5", "ControlMaster=no", "ControlPath=none"} {
		if !strings.Contains(joined, want) {
			t.Errorf("channel argv missing %q: %v", want, channel.Args)
		}
	}
	if strings.Contains(joined, "ControlPersist") {
		t.Fatalf("channel argv contains ControlPersist: %v", channel.Args)
	}
}

func TestBuildSSHInvocationControlPathFallbacks(t *testing.T) {
	remote := &loop.RemoteConfig{Host: "hub", Port: 22, HubID: "0123456789ab", HubDir: "/" + strings.Repeat("deep/", 30)}
	shortRoot := "/tmp"
	got, err := BuildSSHInvocation(SSHBuildOptions{
		Remote: remote, AgentID: "codex", TempRoots: []string{shortRoot},
		EnsureOwned: func(string) error { return nil }, UserID: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	isolationKey := muxIsolationKey(remote, "codex", filepath.Join(remote.HubDir, "keys", "codex_ed25519"))
	wantPrefix := "ControlPath=" + filepath.Join(shortRoot, "ac-0", isolationKey, "%C")
	if !strings.Contains(strings.Join(got.Args, " "), wantPrefix) {
		t.Fatalf("fallback argv = %v, want %s", got.Args, wantPrefix)
	}

	disabled, err := BuildSSHInvocation(SSHBuildOptions{
		Remote: remote, AgentID: "codex", TempRoots: []string{"/" + strings.Repeat("too-long/", 30)},
		EnsureOwned: func(string) error { return errors.New("unusable") }, UserID: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(disabled.Args, " ")
	if !strings.Contains(joined, "ControlMaster=no") || !strings.Contains(joined, "ControlPath=none") {
		t.Fatalf("disabled argv = %v", disabled.Args)
	}
	if len(disabled.Warnings) != 1 || !strings.Contains(disabled.Warnings[0], "tried") {
		t.Fatalf("disabled warnings = %v, want exactly one", disabled.Warnings)
	}
}

func TestMuxIsolationKeyBindsEveryIdentityInput(t *testing.T) {
	remote := &loop.RemoteConfig{HubID: "0123456789ab"}
	base := muxIsolationKey(remote, "codex", "/keys/codex_ed25519")
	if len(base) != muxIsolationKeyWidth {
		t.Fatalf("isolation key width = %d, want %d", len(base), muxIsolationKeyWidth)
	}
	if _, err := hex.DecodeString(base); err != nil {
		t.Fatalf("isolation key %q is not hex: %v", base, err)
	}
	tests := []struct {
		name    string
		remote  *loop.RemoteConfig
		agentID string
		keyPath string
	}{
		{name: "hub", remote: &loop.RemoteConfig{HubID: "abcdef012345"}, agentID: "codex", keyPath: "/keys/codex_ed25519"},
		{name: "agent", remote: remote, agentID: "grok", keyPath: "/keys/codex_ed25519"},
		{name: "identity file", remote: remote, agentID: "codex", keyPath: "/keys/codex_ed25519.v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := muxIsolationKey(tt.remote, tt.agentID, tt.keyPath); got == base {
				t.Fatalf("changed %s retained isolation key %q", tt.name, got)
			}
		})
	}

	dir := t.TempDir()
	v1 := filepath.Join(dir, "codex_ed25519.v1")
	v2 := filepath.Join(dir, "codex_ed25519.v2")
	if err := os.WriteFile(v1, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v2, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "codex_ed25519")
	if err := os.Symlink(filepath.Base(v1), active); err != nil {
		t.Fatal(err)
	}
	before := muxIsolationKey(remote, "codex", active)
	next := active + ".tmp"
	if err := os.Symlink(filepath.Base(v2), next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, active); err != nil {
		t.Fatal(err)
	}
	if after := muxIsolationKey(remote, "codex", active); after == before {
		t.Fatalf("rotated active symlink retained isolation key %q", after)
	}
}

func TestControlPathFallbackBudgetForRealisticUserIDs(t *testing.T) {
	if controlPathByteBudget != 100 || controlPathTokenWidth != 64 {
		t.Fatalf("ControlPath constants = budget %d, token %d", controlPathByteBudget, controlPathTokenWidth)
	}
	for _, uid := range []string{"0", "501", "1001", "12345", "unknown"} {
		t.Run(uid, func(t *testing.T) {
			dir := filepath.Join("/tmp", "ac-"+uid, "0123456789ab")
			expanded := len(dir) + 1 + controlPathTokenWidth
			if expanded >= controlPathByteBudget {
				t.Fatalf("expanded ControlPath length = %d, want < %d (%s)", expanded, controlPathByteBudget, dir)
			}
			if !controlPathFits(dir) {
				t.Fatalf("controlPathFits(%q) = false at length %d", dir, expanded)
			}
		})
	}
}

func TestClassifySSHFailureCodes(t *testing.T) {
	exit127 := exec.Command("sh", "-c", "exit 127").Run()
	remote := &loop.RemoteConfig{User: "alex", Host: "hub.tail1234.ts.net", Port: 22}
	tests := []struct {
		name    string
		stage   string
		stderr  string
		waitErr error
		want    string
		wantMsg string
	}{
		{name: "changed host key", stage: "connect", stderr: "REMOTE HOST IDENTIFICATION HAS CHANGED", want: "E_HOSTKEY_CHANGED", wantMsg: "hub: HOST KEY CHANGED for hub.tail1234.ts.net — refusing to connect. If the hub was reinstalled, confirm with its operator, then run: agentchute hub join --reset-hostkey. If not, treat this as a possible interception."},
		{name: "authentication refused", stage: "connect", stderr: "Permission denied (publickey).", want: "E_UNAUTHORIZED", wantMsg: "hub: hub refused this key for alex@hub.tail1234.ts.net. Either it was never authorized or it was revoked."},
		{name: "remote binary absent on a PINNED host", stage: "connect", waitErr: exit127, want: "E_HUB_NO_BINARY", wantMsg: "hub: connected, and the hub DOES apply a forced command, but it could not run the agentchute binary that command names (remote exit 127). Reinstall agentchute on the hub, or re-authorize this key so its line points at the current binary path."},
		{name: "hello timeout", stage: "hello-timeout", want: "E_HELLO_TIMEOUT", wantMsg: "hub: connected but no protocol answer in 10s. The hub-side agentchute may be hung or broken; on the hub run: agentchute doctor"},
		{name: "connect failed", stage: "connect", want: "E_CONNECT", wantMsg: "hub: cannot reach hub.tail1234.ts.net:22 (connect failed after 5s). Check network/VPN/tailnet, then retry; `agentchute doctor` runs this same probe. (If this machine should no longer be joined to this hub, delete .agentchute-control-repo.)"},
		{name: "established channel lost", stage: "operation", want: "E_CHANNEL_LOST"},
	}
	// Exit 127 now asks the pinning probe which of its two causes this is, so the
	// table pins the PINNED arm and stubs the probe rather than shelling out to a
	// real ssh from a unit row. The unpinned arms have their own table in
	// pinning_test.go, where the verdict is the parameter.
	originalProbe := hubPinningProbe
	t.Cleanup(func() { hubPinningProbe = originalProbe })
	hubPinningProbe = func(SSHBuildOptions) (pinningVerdict, string) { return pinningPinned, "" }

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin, err := os.CreateTemp(t.TempDir(), "ssh-stdin")
			if err != nil {
				t.Fatal(err)
			}
			waitCh := make(chan error, 1)
			waitCh <- tt.waitErr
			transport := &processTransport{
				stdin: stdin, stderr: *bytes.NewBufferString(tt.stderr),
				cancel: func() {}, waitCh: waitCh, closeDone: make(chan struct{}),
			}
			got := classifySSHFailure(remote, "codex", tt.stage, errors.New("transport failed"), transport)
			if code := ErrorCode(got); code != tt.want {
				t.Fatalf("code = %q, want %q (error %v)", code, tt.want, got)
			}
			if tt.wantMsg != "" && got.Error() != tt.wantMsg {
				t.Fatalf("message = %q, want %q", got.Error(), tt.wantMsg)
			}
		})
	}
}

func TestStartSSHMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := startSSH(context.Background(), SSHInvocation{})
	if code := ErrorCode(err); code != "E_NO_SSH" {
		t.Fatalf("code = %q, want E_NO_SSH (error %v)", code, err)
	}
	want := "hub: the `ssh` binary was not found on this machine. Install the OpenSSH client (macOS: preinstalled — check PATH; Debian/Ubuntu: apt install openssh-client), then retry."
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err, want)
	}
}

func TestUnauthorizedIncludesReadyToPasteAuthorization(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/remote/pool")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHubConfig(remote.HubID, &HubConfig{Pool: "/remote/pool", Pool12: "0123456789ab"}); err != nil {
		t.Fatal(err)
	}
	keysDir := filepath.Join(remote.HubDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(keysDir, "codex_ed25519.v1")
	if err := os.WriteFile(target, []byte("private fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pubkey := "ssh-ed25519 AAAAC3NzaFixture agentchute:codex"
	if err := os.WriteFile(target+".pub", []byte(pubkey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(target), filepath.Join(keysDir, "codex_ed25519")); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.CreateTemp(t.TempDir(), "ssh-stdin")
	if err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	waitCh <- nil
	transport := &processTransport{
		stdin: stdin, stderr: *bytes.NewBufferString("Permission denied (publickey)."),
		cancel: func() {}, waitCh: waitCh, closeDone: make(chan struct{}),
	}
	got := classifySSHFailure(remote, "codex", "connect", errors.New("transport failed"), transport)
	want := "hub: hub refused this key for alex@hub.example. Either it was never authorized or it was revoked. Run this ON THE HUB, then retry here:\n  agentchute hub authorize --agent codex --pool /remote/pool --key \"" + pubkey + "\""
	if ErrorCode(got) != "E_UNAUTHORIZED" || got.Error() != want {
		t.Fatalf("unauthorized error = %q, want %q", got, want)
	}
}
