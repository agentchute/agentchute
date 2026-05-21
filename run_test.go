package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestRunInjectsPromptOnSocketWake(t *testing.T) {
	root := setupShortRunFixture(t)
	script := filepath.Join(root, "fake-wrapper.sh")
	mustWrite(t, script, []byte("#!/bin/sh\nprintf 'READY\\n'\nIFS= read line\nprintf 'GOT:%s\\n' \"$line\"\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	withCwd(t, root, func() {
		go func() {
			errCh <- cmdRun([]string{
				"--as", "runner-test",
				"--vendor", "test",
				"--control-repo", root,
				"--loop-dir", filepath.Join(root, ".examplecorp", "loop"),
				"--interval", "5",
				"--idle-grace", "100ms",
				"--prompt", "check inbox",
				"--", script,
			})
		}()

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		target := loop.RunnerWakeTarget(cfg.RunnerSocketPath("runner-test"))
		waitForRunnerSocket(t, target, errCh)
		if err := loop.PokeWakeTarget(loop.RunnerWakeMethod, target); err != nil {
			t.Fatalf("poke runner: %v", err)
		}
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("cmdRun err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not inject prompt and exit fake wrapper")
	}
}

func TestPromptInjectionBytesDefaultUsesCarriageReturn(t *testing.T) {
	got := string(promptInjectionBytes(runnerOptions{
		AgentID:     "runner-test",
		Vendor:      "test",
		Prompt:      "check inbox",
		WrapperArgs: []string{"/tmp/fake-wrapper"},
	}))
	want := "check inbox\r"
	if got != want {
		t.Fatalf("promptInjectionBytes = %q, want %q", got, want)
	}
}

func TestDefaultRunnerPromptIsTaggedWake(t *testing.T) {
	if defaultRunnerPrompt != "[agentchute:run] check inbox" {
		t.Fatalf("defaultRunnerPrompt = %q", defaultRunnerPrompt)
	}
}

func TestPromptInjectionBytesCodexUsesBracketedPasteAndEnhancedEnter(t *testing.T) {
	got := string(promptInjectionBytes(runnerOptions{
		AgentID:     "codex",
		Vendor:      "openai",
		Prompt:      "check inbox",
		WrapperArgs: []string{"/usr/local/bin/codex"},
	}))
	want := bracketedPasteStart + "check inbox" + bracketedPasteEnd + codexEnhancedEnter
	if got != want {
		t.Fatalf("promptInjectionBytes = %q, want %q", got, want)
	}
}

func TestPromptInjectionBytesCodexWrapperUsesEnhancedEnter(t *testing.T) {
	got := string(promptInjectionBytes(runnerOptions{
		AgentID:     "custom-codex",
		Vendor:      "openai",
		Prompt:      "check inbox",
		WrapperArgs: []string{"/usr/local/bin/codex"},
	}))
	want := bracketedPasteStart + "check inbox" + bracketedPasteEnd + codexEnhancedEnter
	if got != want {
		t.Fatalf("promptInjectionBytes = %q, want %q", got, want)
	}
}

func TestRunnerMakeRawNoopsForNonTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	restore, enabled, err := runnerMakeRaw(f)
	if err != nil {
		t.Fatalf("runnerMakeRaw err = %v", err)
	}
	if enabled {
		t.Fatal("runnerMakeRaw enabled raw mode for non-terminal")
	}
	if restore == nil {
		t.Fatal("restore func is nil")
	}
	if err := restore(); err != nil {
		t.Fatalf("restore err = %v", err)
	}
}

func TestRunRefusesLiveRunnerCollision(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir("codex")); err != nil {
		t.Fatal(err)
	}
	socketPath := cfg.RunnerSocketPath("codex")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:    "codex",
		RunnerPID:  os.Getpid(),
		SocketPath: socketPath,
		StartedAt:  time.Now().UTC(),
		Status:     "active",
	}); err != nil {
		t.Fatal(err)
	}

	err = refuseLiveRunnerCollision(cfg, "codex")
	if err == nil {
		t.Fatal("expected live runner collision")
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("collision error = %v", err)
	}
}

func TestRunShutdownSocketCleansUpRunner(t *testing.T) {
	root := setupShortRunFixture(t)
	marker := filepath.Join(root, "terminated")
	ready := filepath.Join(root, "ready")
	script := filepath.Join(root, "fake-wrapper.sh")
	mustWrite(t, script, []byte("#!/bin/sh\ntrap 'echo stopped > "+marker+"; exit 0' TERM HUP INT\nprintf 'READY\\n'\necho ready > "+ready+"\nwhile :; do sleep 1; done\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	withCwd(t, root, func() {
		go func() {
			errCh <- cmdRun([]string{
				"--as", "codex",
				"--vendor", "openai",
				"--control-repo", root,
				"--loop-dir", filepath.Join(root, ".examplecorp", "loop"),
				"--interval", "5",
				"--idle-grace", "100ms",
				"--", script,
			})
		}()

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		socketPath := cfg.RunnerSocketPath("codex")
		waitForRunnerSocket(t, loop.RunnerWakeTarget(socketPath), errCh)
		waitForFile(t, ready)
		if err := runnerSocketOp(socketPath, "shutdown"); err != nil {
			t.Fatalf("shutdown runner: %v", err)
		}
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("cmdRun err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not exit after shutdown")
	}

	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Status != loop.StatusOffline {
		t.Fatalf("registration status = %s, want offline", reg.Status)
	}
	if _, err := os.Stat(cfg.RunnerSocketPath("codex")); !os.IsNotExist(err) {
		t.Fatalf("socket stat err = %v, want missing socket", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("wrapper did not receive shutdown signal: %v", err)
	}
}

func TestRunnerWakeSatisfiesRecipientLiveness(t *testing.T) {
	cfg := newDoctorCfg(t)
	if err := loop.EnsurePrivateDir(cfg.AgentStateDir("codex")); err != nil {
		t.Fatal(err)
	}
	socketPath := cfg.RunnerSocketPath("codex")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	reg := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: cfg.ControlRepo,
		Host:        localHostname(),
		WakeMethod:  loop.RunnerWakeMethod,
		WakeTarget:  loop.RunnerWakeTarget(socketPath),
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), reg); err != nil {
		t.Fatal(err)
	}
	liveness := evaluateRecipientLiveness(cfg, "codex", time.Now().UTC())
	if !liveness.OK {
		t.Fatalf("liveness OK = false, message=%q", liveness.Message)
	}
	if liveness.Via != "wake" {
		t.Fatalf("liveness Via = %q, want wake", liveness.Via)
	}
}

func runnerSocketOp(path, op string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	return json.NewEncoder(conn).Encode(map[string]string{"op": op})
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}

func setupShortRunFixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "agentchute-run-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
	return root
}

func waitForRunnerSocket(t *testing.T, target string, errCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if loop.RunnerSocketReachable(target, 100*time.Millisecond) {
			return
		}
		select {
		case err := <-errCh:
			t.Fatalf("runner exited before socket was reachable: %v", err)
		default:
		}
		_, last = loop.ParseRunnerWakeTarget(target)
		time.Sleep(50 * time.Millisecond)
	}
	if last != nil && !errors.Is(last, os.ErrNotExist) {
		t.Fatalf("runner socket never became reachable: %v", last)
	}
	t.Fatal("runner socket never became reachable")
}
