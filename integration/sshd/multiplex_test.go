//go:build sshd_integration

package sshd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

func TestSSHDControlPathLengthRuleAndMuxAuthCounts(t *testing.T) {
	t.Run("preferred-within-budget", func(t *testing.T) {
		h := newSSHDHarness(t)
		preferred, err := os.MkdirTemp("/tmp", "h")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(preferred) })
		opts := hubclient.SSHBuildOptions{PreferredRoot: preferred}
		invocation := buildInvocation(t, h, opts)
		want := "ControlPath=" + filepath.Join(preferred, "mux", "%C")
		if !containsOption(invocation.Args, want) || len(invocation.Warnings) != 0 {
			t.Fatalf("preferred invocation = %v, warnings %v", invocation.Args, invocation.Warnings)
		}
		assertThreeHellosUseAuthCount(t, h, opts, 1)
	})

	t.Run("owned-temp-fallback", func(t *testing.T) {
		h := newSSHDHarness(t)
		remote := uniqueMuxRemote(h)
		deep := "/" + strings.Repeat("deep/", 30)
		opts := hubclient.SSHBuildOptions{
			Remote: remote, PreferredRoot: deep, TempRoots: []string{"/tmp"},
			UserID: "0", EnsureOwned: loop.EnsureOwnedSocketDir,
		}
		invocation := buildInvocation(t, h, opts)
		fallback := filepath.Join("/tmp", "agentchute-hub-0", remote.HubID)
		t.Cleanup(func() { _ = os.RemoveAll(fallback) })
		if !containsOption(invocation.Args, "ControlPath="+filepath.Join(fallback, "%C")) || len(invocation.Warnings) != 0 {
			t.Fatalf("fallback invocation = %v, warnings %v", invocation.Args, invocation.Warnings)
		}
		assertOwned0700(t, fallback)
		assertThreeHellosUseAuthCount(t, h, opts, 1)
	})

	t.Run("fallback-refuses-symlink", func(t *testing.T) {
		h := newSSHDHarness(t)
		remote := uniqueMuxRemote(h)
		root, err := os.MkdirTemp("/tmp", "m")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		candidate := filepath.Join(root, "agentchute-hub-0", remote.HubID)
		if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), candidate); err != nil {
			t.Fatal(err)
		}
		opts := hubclient.SSHBuildOptions{
			Remote: remote, PreferredRoot: "/" + strings.Repeat("deep/", 30),
			TempRoots: []string{root}, UserID: "0", EnsureOwned: loop.EnsureOwnedSocketDir,
		}
		invocation := buildInvocation(t, h, opts)
		if !containsOption(invocation.Args, "ControlMaster=no") || !containsOption(invocation.Args, "ControlPath=none") {
			t.Fatalf("symlink fallback was accepted: %v", invocation.Args)
		}
	})

	t.Run("all-paths-disabled", func(t *testing.T) {
		h := newSSHDHarness(t)
		remote := uniqueMuxRemote(h)
		preferred := "/" + strings.Repeat("preferred-too-long/", 20)
		temp := "/" + strings.Repeat("temp-too-long/", 20)
		opts := hubclient.SSHBuildOptions{
			Remote: remote, PreferredRoot: preferred, TempRoots: []string{temp}, UserID: "0",
			EnsureOwned: func(string) error { return errors.New("unwritable") },
		}
		invocation := buildInvocation(t, h, opts)
		if !containsOption(invocation.Args, "ControlMaster=no") || !containsOption(invocation.Args, "ControlPath=none") {
			t.Fatalf("disabled invocation = %v", invocation.Args)
		}
		if len(invocation.Warnings) != 1 || !strings.Contains(invocation.Warnings[0], filepath.Join(preferred, "mux")) || !strings.Contains(invocation.Warnings[0], filepath.Join(temp, "agentchute-hub-0", remote.HubID)) {
			t.Fatalf("disabled warnings = %v", invocation.Warnings)
		}
		assertThreeHellosUseAuthCount(t, h, opts, 3)
	})
}

func buildInvocation(t *testing.T, h *sshdHarness, opts hubclient.SSHBuildOptions) hubclient.SSHInvocation {
	t.Helper()
	if opts.Remote == nil {
		opts.Remote = h.remote
	}
	opts.AgentID = "codex"
	opts.KeyPath = h.keys["codex"]
	opts.StateDir = h.clientState
	invocation, err := hubclient.BuildSSHInvocation(opts)
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

func uniqueMuxRemote(h *sshdHarness) *loop.RemoteConfig {
	remote := *h.remote
	sum := sha256.Sum256([]byte(h.root))
	remote.HubID = hex.EncodeToString(sum[:])[:12]
	return &remote
}

func containsOption(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func assertThreeHellosUseAuthCount(t *testing.T, h *sshdHarness, opts hubclient.SSHBuildOptions, want int) {
	t.Helper()
	invocation := buildInvocation(t, h, opts)
	defer stopMuxMaster(t, h, invocation)
	before := h.authCount()
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		session, err := h.open(ctx, "codex", "discard-this-command", opts)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		client, err := hubclient.OpenOneShotTransport(session, h.remote, "codex", "sshd-integration")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := client.Close(); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.authCount()-before != want {
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.authCount() - before; got != want {
		t.Fatalf("sshd auth count = %d, want %d", got, want)
	}
}

func stopMuxMaster(t *testing.T, h *sshdHarness, invocation hubclient.SSHInvocation) {
	t.Helper()
	controlPath := ""
	for _, arg := range invocation.Args {
		if strings.HasPrefix(arg, "ControlPath=") && arg != "ControlPath=none" {
			controlPath = arg
			break
		}
	}
	if controlPath == "" {
		return
	}
	args := []string{"-F", "/dev/null", "-O", "exit", "-o", controlPath}
	if h.port != 22 {
		args = append(args, "-p", strconv.Itoa(h.port))
	}
	args = append(args, h.remote.Destination())
	cmd := exec.Command(h.ssh, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("stop SSH mux master: %v\n%s", err, out)
	}
}

func assertOwned0700(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("fallback mode = %v", info.Mode())
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(current.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uid {
		t.Fatalf("fallback uid = %v, want %d", info.Sys(), uid)
	}
}
