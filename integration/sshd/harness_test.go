//go:build sshd_integration

package sshd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
	"github.com/agentchute/agentchute/internal/spectest"
)

const sshdPoolID = "0123456789ab"

type sshdHarness struct {
	t           *testing.T
	root        string
	repo        string
	pool        string
	cfg         *loop.Config
	remote      *loop.RemoteConfig
	binary      string
	sshd        string
	ssh         string
	keygen      string
	user        string
	port        int
	hostKey     string
	knownHosts  string
	authorized  string
	config      string
	log         string
	clientHome  string
	clientBin   string
	clientState string
	adminKey    string
	keys        map[string]string
	maxRead     int
	muxMu       sync.Mutex
	muxPaths    map[string]struct{}

	processMu sync.Mutex
	process   *exec.Cmd
	done      chan struct{}
	waitErr   error
	closeOnce sync.Once
}

func requireSSHDTest(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENTCHUTE_SSHD_TEST") != "1" {
		t.Skip("set AGENTCHUTE_SSHD_TEST=1 through tools/sshd-test.sh")
	}
	if err := validateSSHDTestEnvironment(); err != nil {
		t.Fatal(err)
	}
}

func validateSSHDTestEnvironment() error {
	for _, key := range []string{"AGENTCHUTE_CONTROL_REPO", "AGENTCHUTE_LOOP_DIR"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		if isRealPoolPath(key, value) {
			return fmt.Errorf("sshd integration refuses %s=%s: points at a real agentchute pool", key, value)
		}
	}
	return nil
}

func isRealPoolPath(key, path string) bool {
	if key == "AGENTCHUTE_CONTROL_REPO" {
		if info, err := os.Stat(filepath.Join(path, "AGENTCHUTE.md")); err == nil && info.Mode().IsRegular() {
			return true
		}
		if info, err := os.Stat(filepath.Join(path, ".agentchute", "loop")); err == nil && info.IsDir() {
			return true
		}
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func newSSHDHarness(t *testing.T) *sshdHarness {
	t.Helper()
	requireSSHDTest(t)
	root, err := os.MkdirTemp("/tmp", "agentchute-sshd-")
	if err != nil {
		t.Fatal(err)
	}
	h := &sshdHarness{t: t, root: root, repo: sshdRepoRoot(t), keys: make(map[string]string), muxPaths: make(map[string]struct{})}
	if strings.Contains(t.Name(), "/W1/") {
		// Keep the first msg frame out of bufio's read-ahead so W1 can sever
		// the real SSH transport at the vector's exact failure boundary.
		h.maxRead = 1
	}
	t.Cleanup(h.close)
	h.sshd = requireTool(t, "/usr/sbin/sshd", "sshd")
	h.ssh = requireTool(t, "", "ssh")
	h.keygen = requireTool(t, "", "ssh-keygen")
	current, err := user.Current()
	if err != nil || current.Username == "" {
		t.Fatalf("resolve current user: %v", err)
	}
	h.user = current.Username
	h.binary = filepath.Join(root, "bin", "agentchute")
	if err := os.MkdirAll(filepath.Dir(h.binary), 0o700); err != nil {
		t.Fatal(err)
	}
	runCommand(t, h.repo, "go", "build", "-o", h.binary, ".")

	h.pool = filepath.Join(root, "pool")
	loopDir := filepath.Join(h.pool, ".agentchute", "loop")
	if err := os.MkdirAll(filepath.Join(loopDir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.pool, "AGENTCHUTE.md"), []byte("# sshd integration pool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loopDir, "state", "pool.id"), []byte(sshdPoolID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.pool, err = filepath.EvalSymlinks(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	loopDir = filepath.Join(h.pool, ".agentchute", "loop")
	h.cfg = &loop.Config{ControlRepo: h.pool, LoopDir: loopDir, Vendor: "agentchute"}
	vendor := "test"
	for _, id := range []string{"codex", "grok"} {
		if _, err := op.Register(h.cfg, op.Context{ActorID: id}, op.RegisterReq{Vendor: &vendor, Host: "sshd-fixture"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	h.clientHome = filepath.Join(root, "home")
	h.clientState = filepath.Join(root, "client")
	if err := os.MkdirAll(filepath.Join(h.clientState, "keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.clientHome, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.hostKey = filepath.Join(root, "host_ed25519")
	runCommand(t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-f", h.hostKey)
	h.authorized = filepath.Join(h.clientHome, ".ssh", "authorized_keys")
	if err := os.WriteFile(h.authorized, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.addAdminKey()
	for _, id := range []string{"codex", "grok"} {
		h.addAgent(id)
	}
	h.port = freePort(t)
	h.knownHosts = filepath.Join(h.clientState, "known_hosts")
	h.writeKnownHosts()
	h.writeSSHWrapper()
	h.config = filepath.Join(root, "sshd_config")
	h.log = filepath.Join(root, "sshd.log")
	h.writeConfig()
	h.remote = &loop.RemoteConfig{
		URL:  "ssh://" + h.user + "@127.0.0.1:" + fmt.Sprint(h.port) + h.pool,
		User: h.user, Host: "127.0.0.1", Port: h.port, PoolPath: h.pool,
		HubID: sshdPoolID, HubDir: h.clientState,
		ConfigPath:    filepath.Join(h.clientState, "config.json"),
		ShadowLoopDir: filepath.Join(h.clientState, ".agentchute", "loop"),
	}
	h.start()
	return h
}

func sshdRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate sshd harness source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func requireTool(t *testing.T, preferred, name string) string {
	t.Helper()
	if preferred != "" {
		if info, err := os.Stat(preferred); err == nil && !info.IsDir() {
			return preferred
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s is required for sshd integration: %v", name, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func runCommand(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func (h *sshdHarness) addAgent(id string) {
	h.t.Helper()
	key := filepath.Join(h.clientState, "keys", id+"_ed25519")
	runCommand(h.t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "agentchute:"+id, "-f", key)
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		h.t.Fatal(err)
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		h.t.Fatalf("invalid public key for %s", id)
	}
	line := fmt.Sprintf("restrict,command=\"%s hub session --agent %s --pool %s --pool-id %s\" %s %s agentchute:%s:%s\n", h.binary, id, h.pool, sshdPoolID, fields[0], fields[1], id, sshdPoolID)
	file, err := os.OpenFile(h.authorized, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := file.WriteString(line); err != nil {
		_ = file.Close()
		h.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.keys[id] = key
}

func (h *sshdHarness) addAdminKey() {
	h.t.Helper()
	h.adminKey = filepath.Join(h.root, "admin_ed25519")
	runCommand(h.t, "", h.keygen, "-q", "-t", "ed25519", "-N", "", "-C", "agentchute:sshd-admin", "-f", h.adminKey)
	pub, err := os.ReadFile(h.adminKey + ".pub")
	if err != nil {
		h.t.Fatal(err)
	}
	file, err := os.OpenFile(h.authorized, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := file.Write(pub); err != nil {
		_ = file.Close()
		h.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *sshdHarness) writeKnownHosts() {
	h.t.Helper()
	pub, err := os.ReadFile(h.hostKey + ".pub")
	if err != nil {
		h.t.Fatal(err)
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		h.t.Fatal("invalid host public key")
	}
	line := fmt.Sprintf("[127.0.0.1]:%d %s %s\n", h.port, fields[0], fields[1])
	if err := os.WriteFile(h.knownHosts, []byte(line), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *sshdHarness) writeSSHWrapper() {
	h.t.Helper()
	h.clientBin = filepath.Join(h.root, "client-bin")
	if err := os.MkdirAll(h.clientBin, 0o700); err != nil {
		h.t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
has_identity=0
previous=
for argument in "$@"; do
  if [ "$previous" = "-i" ]; then has_identity=1; fi
  previous=$argument
done
set -- -F /dev/null -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s "$@"
if [ "$has_identity" -eq 0 ]; then set -- -i %s "$@"; fi
exec %s "$@"
`, h.knownHosts, h.adminKey, h.ssh)
	if err := os.WriteFile(filepath.Join(h.clientBin, "ssh"), []byte(script), 0o700); err != nil {
		h.t.Fatal(err)
	}
}

func (h *sshdHarness) writeConfig() {
	h.t.Helper()
	config := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
AuthenticationMethods publickey
UsePAM no
PermitRootLogin prohibit-password
AllowUsers %s
LogLevel VERBOSE
MaxAuthTries 2
MaxSessions 20
SetEnv HOME=%s PATH=%s:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
`, h.port, h.hostKey, filepath.Join(h.root, "sshd.pid"), h.authorized, h.user, h.clientHome, filepath.Dir(h.binary))
	if err := os.WriteFile(h.config, []byte(config), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *sshdHarness) start() {
	h.t.Helper()
	h.processMu.Lock()
	defer h.processMu.Unlock()
	if h.process != nil {
		h.t.Fatal("sshd already running")
	}
	check := exec.Command(h.sshd, "-t", "-f", h.config)
	if out, err := check.CombinedOutput(); err != nil {
		h.t.Fatalf("sshd config check: %v\n%s", err, out)
	}
	cmd := exec.Command(h.sshd, "-D", "-f", h.config, "-E", h.log)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.process = cmd
	h.done = make(chan struct{})
	done := h.done
	h.waitErr = nil
	go func() {
		err := cmd.Wait()
		h.processMu.Lock()
		h.waitErr = err
		h.processMu.Unlock()
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", h.port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case <-h.done:
			h.t.Fatalf("sshd exited before readiness: %v\n%s", h.waitErr, stderr.String())
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("sshd did not listen on port %d\n%s", h.port, stderr.String())
}

func (h *sshdHarness) stop() {
	h.stopMuxMasters()
	h.processMu.Lock()
	cmd, done := h.process, h.done
	h.processMu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		h.t.Errorf("sshd did not exit")
	}
	h.processMu.Lock()
	h.process = nil
	h.processMu.Unlock()
}

func (h *sshdHarness) close() {
	h.closeOnce.Do(func() {
		h.stop()
		// Dump the daemon's own account of the session BEFORE deleting the
		// fixture. sshd runs at LogLevel VERBOSE and records who authenticated,
		// when each channel opened, and — the part that matters when a stream
		// ends early — which SIDE disconnected and why. Without this the log is
		// written, read once for authCount(), and then removed with the root, so
		// a failure that only reproduces on a CI runner cannot be diagnosed from
		// the run at all: you get the client's classification ("channel to the
		// hub was lost") and nothing about the cause.
		// Normally only on failure. But dumping ONLY on failure means every
		// observation drawn from this log is selected on the failure: "it appears
		// only in failing runs" is guaranteed when failing runs are the only ones
		// we read. That is how a normal record gets promoted to a defect
		// signature, twice this milestone. Under the hammer job we take the
		// passing logs too, so 16 greens and 4 reds from one round can be diffed
		// rather than interpreted.
		//
		// It has to happen HERE, before RemoveAll below takes the log with it.
		if h.t.Failed() || os.Getenv("AGENTCHUTE_SSHD_DUMP_ALWAYS") != "" {
			h.dumpDiagnostics()
		}
		if err := os.RemoveAll(h.root); err != nil {
			h.t.Errorf("remove sshd fixture: %v", err)
		}
	})
}

// sshdLogTailLines bounds the dump: enough to cover a full session's open,
// channel and disconnect records, short enough not to bury the failure.
const sshdLogTailLines = 80

func (h *sshdHarness) dumpDiagnostics() {
	h.t.Helper()
	b, err := os.ReadFile(h.log)
	if err != nil {
		h.t.Logf("sshd log unavailable (%s): %v", h.log, err)
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		h.t.Logf("sshd log is empty (%s) — the daemon logged nothing, so the failure is client-side or before connect", h.log)
		return
	}
	dropped := 0
	if len(lines) > sshdLogTailLines {
		dropped = len(lines) - sshdLogTailLines
		lines = lines[dropped:]
	}
	header := fmt.Sprintf("sshd log tail (%s, port %d)", h.log, h.port)
	if dropped > 0 {
		header += fmt.Sprintf(", %d earlier line(s) omitted", dropped)
	}
	h.t.Logf("%s:\n%s", header, strings.Join(lines, "\n"))
}

func (h *sshdHarness) Open(ctx context.Context, pinned string) (spectest.Session, error) {
	return h.open(ctx, pinned, "agentchute-hub", hubclient.SSHBuildOptions{})
}

func (h *sshdHarness) open(ctx context.Context, pinned, requested string, opts hubclient.SSHBuildOptions) (*sshProcessSession, error) {
	key, ok := h.keys[pinned]
	if !ok {
		return nil, fmt.Errorf("no client key for %s", pinned)
	}
	if opts.Remote == nil {
		opts.Remote = h.remote
	}
	opts.AgentID = pinned
	opts.KeyPath = key
	opts.StateDir = h.clientState
	invocation, err := hubclient.BuildSSHInvocation(opts)
	if err != nil {
		return nil, err
	}
	h.rememberMuxPath(invocation)
	invocation.Args[len(invocation.Args)-1] = requested
	cmd := exec.CommandContext(ctx, h.ssh, invocation.Args...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, ok := stdinPipe.(*os.File)
	if !ok {
		return nil, errors.New("ssh stdin is not an os.File")
	}
	stdout, ok := stdoutPipe.(*os.File)
	if !ok {
		return nil, errors.New("ssh stdout is not an os.File")
	}
	session := &sshProcessSession{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan struct{}), requested: requested, warnings: invocation.Warnings, maxRead: h.maxRead}
	cmd.Stderr = &session.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		session.waitErr = cmd.Wait()
		close(session.done)
	}()
	return session, nil
}

func (h *sshdHarness) rememberMuxPath(invocation hubclient.SSHInvocation) {
	for _, arg := range invocation.Args {
		if !strings.HasPrefix(arg, "ControlPath=") || arg == "ControlPath=none" {
			continue
		}
		h.muxMu.Lock()
		h.muxPaths[arg] = struct{}{}
		h.muxMu.Unlock()
	}
}

func (h *sshdHarness) stopMuxMasters() {
	if h.remote == nil || h.ssh == "" {
		return
	}
	h.rememberJoinedMuxPaths()
	h.muxMu.Lock()
	computed := make(map[string]struct{}, len(h.muxPaths))
	paths := make([]string, 0, len(h.muxPaths))
	for path := range h.muxPaths {
		computed[path] = struct{}{}
		paths = append(paths, path)
	}
	h.muxMu.Unlock()

	// Also reap the sockets that actually exist, not only the ones this process
	// can name. The recomputed ControlPath ends in the literal `%C` token; the
	// socket on disk carries ssh's EXPANSION of it. `ssh -O exit` expands the
	// token so reaping through it works — but the two spellings can never compare
	// equal, so any "is this socket in my computed set" test answers "no" every
	// time and discriminates nothing. An earlier version of this code logged that
	// non-answer on every reap, in both passing and failing runs, which is the
	// template-versus-expansion trap it was written to detect.
	//
	// Compare DIRECTORIES, which are the same string on both sides, and report
	// only the case that is actually informative: a live socket in a directory
	// this process did not know about at all.
	known := map[string]struct{}{}
	for path := range computed {
		known[filepath.Dir(strings.TrimPrefix(path, "ControlPath="))] = struct{}{}
	}
	for _, found := range h.discoverMuxSockets() {
		if _, ok := known[filepath.Dir(strings.TrimPrefix(found, "ControlPath="))]; !ok {
			h.t.Logf("mux reap: live master in an unknown isolation dir: %s", found)
		}
		paths = append(paths, found)
	}
	for _, controlPath := range paths {
		_ = h.muxControl(controlPath, "exit")
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := h.muxControl(controlPath, "check"); err != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The positive control, and the reason this function used to report success
	// while doing nothing. `-O check` fails for a socket that does not exist and
	// for one that was just closed, so the loop above cannot tell "I reaped the
	// master" from "I addressed a name that was never there" — which is exactly
	// what it was doing: reaping through the literal `%C` token, whose expansion
	// is ssh's own connection hash and did not name the live socket.
	//
	// Assert on the filesystem instead: after a reap, no mux socket may remain.
	// This is the only assertion here that can fail, and it must stay that way.
	deadline := time.Now().Add(3 * time.Second)
	for {
		remaining := h.discoverMuxSockets()
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("mux reap did not close every master; still live: %v\n"+
				"A surviving master means the next one-shot multiplexes over it — two forced-command sessions on one connection.", remaining)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// discoverMuxSockets returns ControlPath= arguments for the mux sockets that
// actually exist in THIS harness's isolation directories. It looks at what is on
// disk rather than at what should be, which is the only way to address a master
// whose socket name this process cannot reconstruct — the name is ssh's `%C`
// expansion, not something the harness computes.
//
// Scoped to directories this harness computed, deliberately. The obvious version
// scans the whole /tmp/ac-<uid> tree, and under `-count=N` or a parallel package
// that means one iteration observing another's sockets — an instrument that
// reports failures belonging to someone else. The isolation directory already
// binds hub id, agent id and resolved key, so it is harness-specific; only the
// socket name inside it is not.
func (h *sshdHarness) discoverMuxSockets() []string {
	h.muxMu.Lock()
	dirs := make(map[string]struct{}, len(h.muxPaths))
	for path := range h.muxPaths {
		dirs[filepath.Dir(strings.TrimPrefix(path, "ControlPath="))] = struct{}{}
	}
	h.muxMu.Unlock()

	seen := map[string]struct{}{}
	var found []string
	for dir := range dirs {
		socks, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, sock := range socks {
			info, err := sock.Info()
			if err != nil || info.Mode()&os.ModeSocket == 0 {
				continue
			}
			arg := "ControlPath=" + filepath.Join(dir, sock.Name())
			if _, dup := seen[arg]; dup {
				continue
			}
			seen[arg] = struct{}{}
			found = append(found, arg)
		}
	}
	return found
}

func (h *sshdHarness) muxControl(controlPath, operation string) error {
	args := []string{"-F", "/dev/null", "-O", operation, "-o", controlPath}
	if h.port != 22 {
		args = append(args, "-p", strconv.Itoa(h.port))
	}
	args = append(args, h.remote.Destination())
	return exec.Command(h.ssh, args...).Run()
}

func (h *sshdHarness) rememberJoinedMuxPaths() {
	remote, err := loop.ParseRemoteURL(h.remote.URL)
	if err != nil {
		return
	}
	remote.HubDir = filepath.Join(h.clientHome, ".agentchute", "hub", remote.HubID)
	remote.ConfigPath = filepath.Join(remote.HubDir, "config.json")
	remote.ShadowLoopDir = filepath.Join(remote.HubDir, ".agentchute", "loop")
	entries, err := os.ReadDir(filepath.Join(remote.HubDir, "keys"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		marker := strings.Index(name, "_ed25519")
		if entry.IsDir() || marker <= 0 || strings.HasSuffix(name, ".pub") {
			continue
		}
		suffix := name[marker+len("_ed25519"):]
		if suffix != "" && !strings.HasPrefix(suffix, ".v") {
			continue
		}
		agentID := name[:marker]
		invocation, err := hubclient.BuildSSHInvocation(hubclient.SSHBuildOptions{
			Remote: remote, AgentID: agentID, KeyPath: filepath.Join(remote.HubDir, "keys", name), StateDir: remote.HubDir,
		})
		if err == nil {
			h.rememberMuxPath(invocation)
		}
	}
}

func (h *sshdHarness) State() spectest.StateProbe { return (*sshdState)(h) }

type sshProcessSession struct {
	cmd       *exec.Cmd
	stdin     *os.File
	stdout    *os.File
	stderr    bytes.Buffer
	done      chan struct{}
	waitErr   error
	requested string
	warnings  []string
	maxRead   int
	forced    atomic.Bool
	closeOnce sync.Once
}

func (s *sshProcessSession) Read(p []byte) (int, error) {
	if s.maxRead > 0 && len(p) > s.maxRead {
		p = p[:s.maxRead]
	}
	return s.stdout.Read(p)
}
func (s *sshProcessSession) Write(p []byte) (int, error)        { return s.stdin.Write(p) }
func (s *sshProcessSession) SetReadDeadline(t time.Time) error  { return s.stdout.SetReadDeadline(t) }
func (s *sshProcessSession) SetWriteDeadline(t time.Time) error { return s.stdin.SetWriteDeadline(t) }
func (s *sshProcessSession) ForceDisconnect() error {
	s.forced.Store(true)
	_ = s.stdin.Close()
	_ = s.stdout.Close()
	if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
func (s *sshProcessSession) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		if s.forced.Load() {
			return io.ErrClosedPipe
		}
		return s.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *sshProcessSession) Close() error {
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()
		select {
		case <-s.done:
		case <-time.After(time.Second):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
	if s.forced.Load() {
		return nil
	}
	return s.waitErr
}

type sshdState sshdHarness

func (s *sshdState) Deliver(from, to, body string) error {
	_, _, err := loop.SendTsMessageWithCommit(s.cfg, from, to, loop.ComposeMessage(from, "", body), "")
	return err
}

func (s *sshdState) InboxCount(agent string) (int, error) {
	msgs, _, err := loop.ListInboxMessagesWithSkipped(s.cfg.AgentInboxDir(agent))
	if errors.Is(err, loop.ErrInboxMissing) {
		return 0, nil
	}
	return len(msgs), err
}

func (s *sshdState) ClaimedCount(agent string) (int, error) {
	msgs, err := loop.ListClaimedMessages(s.cfg.AgentClaimedDir(agent))
	return len(msgs), err
}

func (s *sshdState) ReplaceLeaseToken(agent, token string) error {
	path := filepath.Join(s.cfg.AgentStateDir(agent), "serve.claim")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var claim loop.ServeClaim
	if err := json.Unmarshal(b, &claim); err != nil {
		return err
	}
	claim.ServeToken = token
	b, err = json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func (s *sshdState) StageUnreadableClaim(agent string) (func() error, error) {
	msgs, _, err := loop.ListInboxMessagesWithSkipped(s.cfg.AgentInboxDir(agent))
	if err != nil {
		return nil, err
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("expected one inbox message, got %d", len(msgs))
	}
	path, err := loop.ClaimMessage(msgs[0], s.cfg.AgentClaimedDir(agent))
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0); err != nil {
		return nil, err
	}
	return func() error { return os.Chmod(path, 0o600) }, nil
}

func (h *sshdHarness) authCount() int {
	b, _ := os.ReadFile(h.log)
	return strings.Count(string(b), "Accepted publickey for")
}

func (h *sshdHarness) commandEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+3+len(extra))
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "HOME=") || strings.HasPrefix(value, "AGENTCHUTE_") {
			continue
		}
		env = append(env, value)
	}
	env = append(env, "HOME="+h.clientHome, "PATH="+h.clientBin+":"+filepath.Dir(h.binary)+":"+os.Getenv("PATH"))
	env = append(env, extra...)
	return env
}

func (h *sshdHarness) runCLI(dir string, args ...string) (string, string, error) {
	cmd := exec.Command(h.binary, args...)
	cmd.Dir = dir
	cmd.Env = h.commandEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (h *sshdHarness) newCheckout() string {
	h.t.Helper()
	dir := filepath.Join(h.root, fmt.Sprintf("checkout-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(filepath.Join(dir, ".agentchute", "loop"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTCHUTE.md"), []byte("# client checkout\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	runCommand(h.t, dir, "git", "init", "-q")
	return dir
}

func helloOverSession(t *testing.T, h *sshdHarness, pinned string, opts hubclient.SSHBuildOptions) hubwire.HelloOK {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := h.open(ctx, pinned, "discard-this-command", opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := hubclient.OpenOneShotTransport(raw, h.remote, pinned, "sshd-integration")
	if err != nil {
		t.Fatal(err)
	}
	hello := client.Hello()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	return hello
}

func TestHarnessAuthenticatesForcesCommandAndCleansUp(t *testing.T) {
	h := newSSHDHarness(t)
	hello := helloOverSession(t, h, "codex", hubclient.SSHBuildOptions{})
	if hello.Agent != "codex" || hello.Pool != h.pool || hello.Pool12 != sshdPoolID {
		t.Fatalf("hello = %+v", hello)
	}
	if h.authCount() != 1 {
		t.Fatalf("auth entries = %d, want 1", h.authCount())
	}
	root, port := h.root, h.port
	h.close()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temp tree survived teardown: %v", err)
	}
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("sshd socket survived teardown")
	}
}

func TestHarnessRefusesRealPoolEnvironmentWithoutWrites(t *testing.T) {
	requireSSHDTest(t)
	for _, key := range []string{"AGENTCHUTE_CONTROL_REPO", "AGENTCHUTE_LOOP_DIR"} {
		t.Run(key, func(t *testing.T) {
			pool := t.TempDir()
			loopDir := filepath.Join(pool, ".agentchute", "loop")
			if err := os.MkdirAll(loopDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pool, "AGENTCHUTE.md"), []byte("sentinel\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			value := pool
			if key == "AGENTCHUTE_LOOP_DIR" {
				value = loopDir
			}
			t.Setenv(key, value)
			if err := validateSSHDTestEnvironment(); err == nil || !strings.Contains(err.Error(), "refuses") {
				t.Fatalf("containment error = %v", err)
			}
			b, err := os.ReadFile(filepath.Join(pool, "AGENTCHUTE.md"))
			if err != nil || string(b) != "sentinel\n" {
				t.Fatalf("real pool changed: %q, %v", b, err)
			}
			entries, err := os.ReadDir(loopDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("real loop changed: %v, %v", entries, err)
			}
		})
	}
}
