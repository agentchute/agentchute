//go:build unix

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubwire"
)

// Issue #219: sshd runs `hub session` with its stdout on a pipe. When the
// client vanishes, sshd's end of that pipe closes, and the next frame the
// session writes — a tick-ok already in flight, say — hits a broken pipe on
// fd 1. A Go program that has not asked for SIGPIPE is KILLED by it there, so
// the session's deferred lease release never ran and the hub's serve claim
// outlived the channel (TestSSHDChildIsNotRelaunchedWhenOptedOut: "serve claim
// remains after channel drop"). The session must instead see the write fail
// and release.
func TestHubSessionReleasesLeaseWhenItsStdoutBreaks(t *testing.T) {
	pool, cfg := newHubPool(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHubSessionStdioHelper$")
	cmd.Env = append(hubHelperBaseEnv(),
		"AGENTCHUTE_HUB_SESSION_HELPER=1",
		"AGENTCHUTE_HUB_SESSION_HELPER_POOL="+pool,
		"SSH_ORIGINAL_COMMAND=agentchute-hub",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	writer, reader := hubwire.NewWriter(stdin), hubwire.NewReader(stdout)
	if err := writer.Write(hubwire.Hello{RequestBase: hubwire.RequestBase{T: "hello", ID: 1}, Proto: hubwire.Protocol, V: 1, MinV: 1, Agent: "codex", Bin: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if raw, err := reader.Read(); err != nil || raw.T != "hello-ok" {
		t.Fatalf("hello = %s, %v\nstderr: %s", raw.T, err, stderr.String())
	}
	if err := writer.Write(hubwire.LeaseAcquire{RequestBase: hubwire.RequestBase{T: "lease-acquire", ID: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	if raw, err := reader.Read(); err != nil || raw.T != "lease-ok" {
		t.Fatalf("lease = %s, %v\nstderr: %s", raw.T, err, stderr.String())
	}
	claim := filepath.Join(cfg.AgentStateDir("codex"), "serve.claim")
	if _, err := os.Stat(claim); err != nil {
		t.Fatalf("no serve claim after lease-ok: %v", err)
	}

	// The client goes away: our end of the session's stdout closes. Then a tick
	// that was already on its way makes the session write.
	_ = stdout.Close()
	if err := writer.Write(hubwire.Tick{RequestBase: hubwire.RequestBase{T: "tick", ID: 3}}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		exited <- err
		if state := cmd.ProcessState; state != nil && strings.Contains(state.String(), "broken pipe") {
			t.Errorf("hub session was killed by SIGPIPE (%s) instead of ending its session", state)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hub session still running 10s after its stdout broke")
	}
	if _, err := os.Stat(claim); !os.IsNotExist(err) {
		t.Fatalf("serve claim survived a broken stdout: %v\nstderr: %s", err, stderr.String())
	}
}

// TestHubSessionStdioHelper is the child process for the row above: the real
// `hub session` command over this process's own stdin and stdout.
func TestHubSessionStdioHelper(t *testing.T) {
	if os.Getenv("AGENTCHUTE_HUB_SESSION_HELPER") != "1" {
		t.Skip("helper process for TestHubSessionReleasesLeaseWhenItsStdoutBreaks")
	}
	err := cmdHubSession([]string{"--agent", "codex", "--pool", os.Getenv("AGENTCHUTE_HUB_SESSION_HELPER_POOL"), "--pool-id", fixturePoolID})
	if err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// hubHelperBaseEnv is the parent's environment without any AGENTCHUTE_*
// variable, so the child cannot reach a live pool.
func hubHelperBaseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AGENTCHUTE_") && !strings.HasPrefix(kv, "SSH_ORIGINAL_COMMAND=") {
			env = append(env, kv)
		}
	}
	return env
}
