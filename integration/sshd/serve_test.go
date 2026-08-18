//go:build sshd_integration

package sshd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

func TestSSHDChildEnvSendAndSupervisedRelaunchDefault(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)
	childLog := filepath.Join(h.root, "child.log")
	writeFakeCodex(t, h, childLog)
	serve := startServe(t, h, checkout, false)
	defer serve.stop()

	first := waitChildStarts(t, childLog, 1, 10*time.Second)[0]
	if first.ControlRepo != h.remote.URL || first.LoopDir != "" {
		t.Fatalf("child env control=%q loop=%q", first.ControlRepo, first.LoopDir)
	}
	if first.Agent != agentID || first.Token == "" {
		t.Fatalf("child identity/token = %+v", first)
	}
	waitInboxCount(t, h, "grok", 1, 5*time.Second)
	if err := h.State().Deliver("grok", agentID, "arm remote shadow latch"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runWithChildEnv(h, checkout, first, nil, "check")
	if err != nil || !strings.Contains(stdout, "CLAIMED") {
		t.Fatalf("remote check = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	dropServeSSH(t, serve)
	firstTerm := waitChildTerms(t, childLog, 1, 15*time.Second)[0]
	if firstTerm.PID != first.PID {
		t.Fatalf("first terminated pid = %d, want %d", firstTerm.PID, first.PID)
	}
	waitClaimAbsent(t, h.cfg, agentID, 5*time.Second)
	h.stop()
	guardCommand := "rm " + "-rf fixture"
	guardInput := []byte(fmt.Sprintf(`{"tool_name":"Bash","tool_input":{"command":%q}}`, guardCommand))
	stdout, stderr, err = runWithChildEnv(h, checkout, first, guardInput, "guard", "--pre-tool-use", "--codex-hook", "PreToolUse")
	if err != nil || !strings.Contains(stdout, `"permissionDecision":"deny"`) || stderr != "" {
		t.Fatalf("offline guard = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	h.start()

	starts := waitChildStarts(t, childLog, 2, 15*time.Second)
	second := starts[1]
	if second.PID == first.PID || second.Token == first.Token {
		t.Fatalf("relaunch reused child or token: first=%+v second=%+v", first, second)
	}
	if processExists(first.PID) {
		t.Fatalf("old child pid %d remains alive", first.PID)
	}
	if !processExists(second.PID) {
		t.Fatalf("new child pid %d is not alive", second.PID)
	}
	claim, err := loop.ReadServeClaim(h.cfg, agentID)
	if err != nil || claim.ServeToken != second.Token {
		t.Fatalf("new hub claim = %+v, %v", claim, err)
	}
}

func runLauncherPathsPreserveRemoteness(t *testing.T) {
	h := newSSHDHarness(t)
	checkout, agentID := joinNamedCodex(t, h)
	childLog := filepath.Join(h.root, "launcher-child.log")
	writeOneShotCodex(t, h, childLog)
	shimDir := filepath.Join(h.root, "launcher-shims")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dispatch := fmt.Sprintf("#!/bin/sh\nexec %s dispatch --shim-dir %s -- \"$@\"\n", shellLiteral(h.binary), shellLiteral(shimDir))
	if err := os.WriteFile(filepath.Join(shimDir, "ac"), []byte(dispatch), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf("#!/bin/sh\nexec %s shims exec --name ac-codex --shim-dir %s -- \"$@\"\n", shellLiteral(h.binary), shellLiteral(shimDir))
	if err := os.WriteFile(filepath.Join(shimDir, "ac-codex"), []byte(legacy), 0o700); err != nil {
		t.Fatal(err)
	}

	launches := []struct {
		name       string
		argv       []string
		exportedID bool
	}{
		{name: "dispatcher", argv: []string{filepath.Join(shimDir, "ac"), "serve", "codex"}},
		{name: "legacy-wrapper-shim", argv: []string{filepath.Join(shimDir, "ac-codex")}},
		{name: "direct-with-local-name-env", argv: []string{h.binary, "serve", "codex"}, exportedID: true},
		{name: "dispatcher-with-local-name-env", argv: []string{filepath.Join(shimDir, "ac"), "serve", "codex"}, exportedID: true},
	}
	for index, launch := range launches {
		t.Run(launch.name, func(t *testing.T) {
			cmd := exec.Command(launch.argv[0], launch.argv[1:]...)
			cmd.Dir = checkout
			env := h.commandEnv()
			if launch.exportedID {
				env = h.commandEnv("AGENTCHUTE_AGENT_ID=codex")
			}
			cmd.Env = prependPath(env, shimDir)
			serve := startServeCommand(t, cmd)
			events := waitChildStarts(t, childLog, index+1, 10*time.Second)
			event := events[index]
			if event.Agent != agentID || event.ControlRepo != h.remote.URL || event.LoopDir != "" {
				t.Fatalf("launcher child env = %+v", event)
			}
			if err := serve.wait(10 * time.Second); err != nil {
				t.Fatalf("launcher serve: %v\nstdout:\n%s\nstderr:\n%s", err, serve.stdout.String(), serve.stderr.String())
			}
			waitClaimAbsent(t, h.cfg, agentID, 5*time.Second)
		})
	}

	cmd := exec.Command(filepath.Join(shimDir, "ac"), "--loop-dir", filepath.Join(h.root, "bad-loop"), "serve", "codex")
	cmd.Dir = checkout
	cmd.Env = prependPath(h.commandEnv(), shimDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), "ssh control repo cannot be combined with --loop-dir") {
		t.Fatalf("explicit remote loop-dir = %v, stderr %q", err, stderr.String())
	}
}

type childEvent struct {
	Kind        string
	PID         int
	Agent       string
	Token       string
	ControlRepo string
	LoopDir     string
}

type serveProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan error
}

func joinNamedCodex(t *testing.T, h *sshdHarness) (string, string) {
	t.Helper()
	checkout := h.newCheckout()
	stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--name", "codex")
	if err != nil {
		t.Fatalf("hub join: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	remote := parseRemoteForHome(t, h)
	data, err := os.ReadFile(remote.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var config hubclient.HubConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	agentID := config.Names["codex"]
	if agentID == "" {
		t.Fatalf("join names = %v", config.Names)
	}
	return checkout, agentID
}

func writeFakeCodex(t *testing.T, h *sshdHarness, logPath string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
log=%s
printf 'start|%%s|%%s|%%s|%%s|%%s\n' "$$" "$AGENTCHUTE_AGENT_ID" "$AGENTCHUTE_SERVE_TOKEN" "$AGENTCHUTE_CONTROL_REPO" "${AGENTCHUTE_LOOP_DIR-}" >> "$log"
trap 'printf "term|%%s||||\n" "$$" >> "$log"; exit 0' TERM INT HUP
%s send --to grok --body child-send >/dev/null 2>&1 || true
while :; do sleep 1; done
`, shellLiteral(logPath), shellLiteral(h.binary))
	if err := os.WriteFile(filepath.Join(h.clientBin, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeOneShotCodex(t *testing.T, h *sshdHarness, logPath string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
printf 'start|%%s|%%s|%%s|%%s|%%s\n' "$$" "$AGENTCHUTE_AGENT_ID" "$AGENTCHUTE_SERVE_TOKEN" "$AGENTCHUTE_CONTROL_REPO" "${AGENTCHUTE_LOOP_DIR-}" >> %s
exit 0
`, shellLiteral(logPath))
	if err := os.WriteFile(filepath.Join(h.clientBin, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func shellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func startServe(t *testing.T, h *sshdHarness, checkout string, optedOut bool) *serveProcess {
	t.Helper()
	args := []string{"serve", "--interval", "5"}
	if optedOut {
		args = append(args, "--relaunch=false")
	}
	args = append(args, "codex")
	cmd := exec.Command(h.binary, args...)
	cmd.Dir = checkout
	cmd.Env = h.commandEnv()
	return startServeCommand(t, cmd)
}

func startServeCommand(t *testing.T, cmd *exec.Cmd) *serveProcess {
	t.Helper()
	p := &serveProcess{t: t, cmd: cmd, done: make(chan error, 1)}
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	return p
}

func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env))
	for _, value := range env {
		if strings.HasPrefix(value, "PATH=") {
			out = append(out, "PATH="+dir+":"+strings.TrimPrefix(value, "PATH="))
			continue
		}
		out = append(out, value)
	}
	return out
}

func runWithChildEnv(h *sshdHarness, checkout string, event childEvent, input []byte, args ...string) (string, string, error) {
	cmd := exec.Command(h.binary, args...)
	cmd.Dir = checkout
	cmd.Env = h.commandEnv(
		"AGENTCHUTE_AGENT_ID="+event.Agent,
		"AGENTCHUTE_SERVE_TOKEN="+event.Token,
		"AGENTCHUTE_GUARD=1",
		"AGENTCHUTE_CONTROL_REPO="+event.ControlRepo,
	)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (p *serveProcess) wait(timeout time.Duration) error {
	select {
	case err := <-p.done:
		return err
	case <-time.After(timeout):
		return errors.New("serve did not exit")
	}
}

func (p *serveProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if !processExists(p.cmd.Process.Pid) {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	if err := p.wait(5 * time.Second); err != nil {
		_ = p.cmd.Process.Kill()
		_ = p.wait(2 * time.Second)
	}
}

func dropServeSSH(t *testing.T, serve *serveProcess) {
	t.Helper()
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(pgrep, "-P", strconv.Itoa(serve.cmd.Process.Pid)).Output()
	if err != nil && len(out) == 0 {
		t.Fatalf("find serve children: %v", err)
	}
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			continue
		}
		command, _ := exec.Command("ps", "-p", field, "-o", "comm=").Output()
		if filepath.Base(strings.TrimSpace(string(command))) != "ssh" {
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("serve SSH child not found")
}

func waitChildStarts(t *testing.T, path string, count int, timeout time.Duration) []childEvent {
	t.Helper()
	return waitChildEvents(t, path, "start", count, timeout)
}

func waitChildTerms(t *testing.T, path string, count int, timeout time.Duration) []childEvent {
	t.Helper()
	return waitChildEvents(t, path, "term", count, timeout)
}

func waitChildEvents(t *testing.T, path, kind string, count int, timeout time.Duration) []childEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := readChildEvents(t, path, kind)
		if len(events) >= count {
			return events
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s event(s); file=%q", count, kind, readFile(path))
	return nil
}

func readChildEvents(t *testing.T, path, kind string) []childEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var events []childEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 6 || fields[0] != kind {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("child event pid %q: %v", fields[1], err)
		}
		events = append(events, childEvent{Kind: fields[0], PID: pid, Agent: fields[2], Token: fields[3], ControlRepo: fields[4], LoopDir: fields[5]})
	}
	return events
}

func waitInboxCount(t *testing.T, h *sshdHarness, id string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, err := h.State().InboxCount(id)
		if err == nil && got >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("inbox %s did not reach %d", id, want)
}

func waitClaimAbsent(t *testing.T, cfg *loop.Config, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := loop.ReadServeClaim(cfg, id)
		if os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("serve claim for %s remains after channel drop", id)
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
