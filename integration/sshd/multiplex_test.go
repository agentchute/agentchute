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
		muxDir := controlPathDirectory(t, invocation)
		if filepath.Dir(muxDir) != filepath.Join(preferred, "mux") || len(filepath.Base(muxDir)) != 12 || len(invocation.Warnings) != 0 {
			t.Fatalf("preferred invocation = %v, warnings %v", invocation.Args, invocation.Warnings)
		}
		assertThreeHellosUseAuthCount(t, h, opts, 1)
	})

	t.Run("owned-temp-fallback", func(t *testing.T) {
		h := newSSHDHarness(t)
		remote := uniqueMuxRemote(h)
		uid := currentUID(t)
		deep := "/" + strings.Repeat("deep/", 30)
		opts := hubclient.SSHBuildOptions{
			Remote: remote, PreferredRoot: deep, TempRoots: []string{"/tmp"},
			UserID: uid, EnsureOwned: loop.EnsureOwnedSocketDir,
		}
		invocation := buildInvocation(t, h, opts)
		fallback := controlPathDirectory(t, invocation)
		t.Cleanup(func() { _ = os.RemoveAll(fallback) })
		if filepath.Dir(fallback) != filepath.Join("/tmp", "ac-"+uid) || len(filepath.Base(fallback)) != 12 || len(invocation.Warnings) != 0 {
			t.Fatalf("fallback invocation = %v, warnings %v", invocation.Args, invocation.Warnings)
		}
		assertOwned0700(t, fallback)
		assertThreeHellosUseAuthCount(t, h, opts, 1)
	})

	t.Run("fallback-refuses-symlink", func(t *testing.T) {
		h := newSSHDHarness(t)
		remote := uniqueMuxRemote(h)
		uid := currentUID(t)
		candidate := ""
		opts := hubclient.SSHBuildOptions{
			Remote: remote, PreferredRoot: "/" + strings.Repeat("deep/", 30),
			TempRoots: []string{"/tmp"}, UserID: uid,
			EnsureOwned: func(got string) error {
				candidate = got
				return errors.New("capture candidate")
			},
		}
		_ = buildInvocation(t, h, opts)
		if candidate == "" {
			t.Fatal("fallback candidate was not ownership-checked")
		}
		if err := os.MkdirAll(filepath.Dir(candidate), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), candidate); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(candidate) })
		opts.EnsureOwned = loop.EnsureOwnedSocketDir
		invocation := buildInvocation(t, h, opts)
		if !containsOption(invocation.Args, "ControlMaster=no") || !containsOption(invocation.Args, "ControlPath=none") {
			t.Fatalf("symlink fallback was accepted: %v", invocation.Args)
		}
	})

	t.Run("fallback-refuses-foreign-owned", func(t *testing.T) {
		h := newSSHDHarness(t)
		remote := uniqueMuxRemote(h)
		uid := currentUID(t)
		candidate := ""
		opts := hubclient.SSHBuildOptions{
			Remote: remote, PreferredRoot: "/" + strings.Repeat("deep/", 30),
			TempRoots: []string{"/tmp"}, UserID: uid,
			EnsureOwned: func(got string) error {
				candidate = got
				return errors.New("foreign-owned")
			},
		}
		invocation := buildInvocation(t, h, opts)
		if candidate == "" || filepath.Dir(candidate) != filepath.Join("/tmp", "ac-"+uid) || !containsOption(invocation.Args, "ControlMaster=no") || !containsOption(invocation.Args, "ControlPath=none") {
			t.Fatalf("foreign-owned fallback was accepted: %v", invocation.Args)
		}
	})

	t.Run("same-hub-agent-identities-do-not-share-master", func(t *testing.T) {
		h := newSSHDHarness(t)
		before := h.authCount()
		codex := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{})
		grok := helloOverSession(t, h, "grok", hubclient.SSHBuildOptions{})
		if codex.Agent != "codex" || grok.Agent != "grok" {
			t.Fatalf("isolated hellos = codex:%q grok:%q", codex.Agent, grok.Agent)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && h.authCount()-before != 2 {
			time.Sleep(20 * time.Millisecond)
		}
		if got := h.authCount() - before; got != 2 {
			t.Fatalf("two agent identities used %d authentication(s), want 2", got)
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
		if len(invocation.Warnings) != 1 || !strings.Contains(invocation.Warnings[0], filepath.Join(preferred, "mux")) || !strings.Contains(invocation.Warnings[0], filepath.Join(temp, "ac-0")) {
			t.Fatalf("disabled warnings = %v", invocation.Warnings)
		}
		assertThreeHellosUseAuthCount(t, h, opts, 3)
	})
}

func TestSSHDKeyRotationChangesMuxIdentityAndReauthenticates(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)
	remote := parseRemoteForHome(t, h)
	active := filepath.Join(remote.HubDir, "keys", agentID+"_ed25519")
	beforeTarget, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	beforeInvocation, err := hubclient.BuildSSHInvocation(hubclient.SSHBuildOptions{
		Remote: remote, AgentID: agentID, KeyPath: active, StateDir: remote.HubDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.rememberMuxPath(beforeInvocation)
	beforeMux := controlPathDirectory(t, beforeInvocation)

	stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex", "--rotate-key")
	if err != nil {
		t.Fatalf("rotate key: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	afterTarget, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	if afterTarget == beforeTarget {
		t.Fatalf("rotation retained symlink target %q", afterTarget)
	}
	afterInvocation, err := hubclient.BuildSSHInvocation(hubclient.SSHBuildOptions{
		Remote: remote, AgentID: agentID, KeyPath: active, StateDir: remote.HubDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.rememberMuxPath(afterInvocation)
	if afterMux := controlPathDirectory(t, afterInvocation); afterMux == beforeMux {
		t.Fatalf("rotation retained mux directory %q", afterMux)
	}

	stdout, stderr, err = h.runCLI(checkout, "status", "--as", agentID)
	if err == nil || !strings.Contains(stderr, "not registered") || strings.Contains(stderr, "identity") {
		t.Fatalf("post-rotation status did not reach the pinned agent: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	fingerprintFields := strings.Fields(string(runCommand(t, "", h.keygen, "-lf", filepath.Join(filepath.Dir(active), afterTarget+".pub"))))
	if len(fingerprintFields) < 2 || !strings.Contains(readFile(h.log), fingerprintFields[1]) {
		t.Fatalf("sshd log does not show rotated key authentication: fields=%v", fingerprintFields)
	}
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

func controlPathDirectory(t *testing.T, invocation hubclient.SSHInvocation) string {
	t.Helper()
	for _, arg := range invocation.Args {
		if !strings.HasPrefix(arg, "ControlPath=") || arg == "ControlPath=none" {
			continue
		}
		path := strings.TrimPrefix(arg, "ControlPath=")
		if filepath.Base(path) != "%C" {
			t.Fatalf("ControlPath token = %q, want %%C", filepath.Base(path))
		}
		return filepath.Dir(path)
	}
	t.Fatal("invocation has no multiplexed ControlPath")
	return ""
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

func currentUID(t *testing.T) string {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	if current.Uid == "" {
		t.Fatal("current uid is empty")
	}
	return current.Uid
}
