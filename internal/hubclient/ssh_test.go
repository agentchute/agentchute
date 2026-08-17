package hubclient

import (
	"bytes"
	"context"
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
	got, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: "codex", EnsureOwned: func(string) error { return nil }})
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
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + filepath.Join(hubDir, "mux", "%C"), "-o", "ControlPersist=60s",
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
	wantPrefix := "ControlPath=" + filepath.Join(shortRoot, "agentchute-hub-0", remote.HubID, "%C")
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

func TestClassifySSHFailureCodes(t *testing.T) {
	exit127 := exec.Command("sh", "-c", "exit 127").Run()
	remote := &loop.RemoteConfig{User: "alex", Host: "hub.example", Port: 22}
	tests := []struct {
		name    string
		stage   string
		stderr  string
		waitErr error
		want    string
	}{
		{name: "changed host key", stage: "connect", stderr: "REMOTE HOST IDENTIFICATION HAS CHANGED", want: "E_HOSTKEY_CHANGED"},
		{name: "authentication refused", stage: "connect", stderr: "Permission denied (publickey).", want: "E_UNAUTHORIZED"},
		{name: "remote binary absent", stage: "connect", waitErr: exit127, want: "E_HUB_NO_BINARY"},
		{name: "hello timeout", stage: "hello-timeout", want: "E_HELLO_TIMEOUT"},
		{name: "connect failed", stage: "connect", want: "E_CONNECT"},
		{name: "established channel lost", stage: "operation", want: "E_CHANNEL_LOST"},
	}
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
		})
	}
}

func TestStartSSHMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := startSSH(context.Background(), SSHInvocation{})
	if code := ErrorCode(err); code != "E_NO_SSH" {
		t.Fatalf("code = %q, want E_NO_SSH (error %v)", code, err)
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
	want := "Run this ON THE HUB, then retry here:\n  agentchute hub authorize --agent codex --pool /remote/pool --key \"" + pubkey + "\""
	if ErrorCode(got) != "E_UNAUTHORIZED" || !strings.Contains(got.Error(), want) {
		t.Fatalf("unauthorized error = %q, want complete authorization command %q", got, want)
	}
}
