package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

const (
	controlPathByteBudget = 100
	controlPathTokenWidth = 64
	muxIsolationKeyWidth  = 12
)

type SSHBuildOptions struct {
	Remote      *loop.RemoteConfig
	AgentID     string
	KeyPath     string
	StateDir    string
	Channel     bool
	TempRoots   []string
	EnsureOwned func(string) error
	UserID      string
}

type SSHInvocation struct {
	Args     []string
	Warnings []string
}

// ReapSSHMux closes this identity's live one-shot master, if any. Callers use
// it after a local authorization change that must force the next operation to
// authenticate again; channel sessions never multiplex and are unaffected.
func ReapSSHMux(remote *loop.RemoteConfig, agentID, keyPath, stateDir string) error {
	invocation, err := BuildSSHInvocation(SSHBuildOptions{Remote: remote, AgentID: agentID, KeyPath: keyPath, StateDir: stateDir})
	if err != nil {
		return err
	}
	controlPath := ""
	for _, arg := range invocation.Args {
		if strings.HasPrefix(arg, "ControlPath=") && arg != "ControlPath=none" {
			controlPath = arg
			break
		}
	}
	if controlPath == "" {
		return nil
	}
	args := []string{"-O", "exit", "-o", controlPath}
	if remote.Port != 22 {
		args = append(args, "-p", strconv.Itoa(remote.Port))
	}
	args = append(args, remote.Destination())
	out, err := exec.Command("ssh", args...).CombinedOutput()
	if err == nil || strings.Contains(strings.ToLower(string(out)), "control socket connect") {
		return nil
	}
	return fmt.Errorf("reap SSH multiplex master: %w: %s", err, strings.TrimSpace(string(out)))
}

func BuildSSHInvocation(opts SSHBuildOptions) (SSHInvocation, error) {
	if opts.Remote == nil {
		return SSHInvocation{}, fmt.Errorf("build ssh invocation: missing remote config")
	}
	if err := loop.ValidateAgentID(opts.AgentID); err != nil {
		return SSHInvocation{}, fmt.Errorf("build ssh invocation: %w", err)
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = opts.Remote.HubDir
	}
	key := opts.KeyPath
	if key == "" {
		key = filepath.Join(stateDir, "keys", opts.AgentID+"_ed25519")
	}
	args := []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
	}
	if opts.Channel {
		args = append(args, "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2")
	} else {
		args = append(args, "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2")
	}
	args = append(args,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+filepath.Join(stateDir, "known_hosts"),
		"-o", "IdentitiesOnly=yes", "-i", key,
		"-o", "ClearAllForwardings=yes",
	)
	var warnings []string
	// Every invocation sharing a mux key must keep the same connection-affecting
	// options: an attached session inherits the master's forwarding, host-key,
	// and route decisions. User ssh_config alias changes are deliberately not
	// reimplemented here; a live master retains its resolved route until close.
	if opts.Channel {
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none")
	} else {
		muxDir, attempted, err := selectMuxDir(opts, muxIsolationKey(opts.Remote, opts.AgentID, key))
		if err != nil {
			args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none")
			warnings = append(warnings, fmt.Sprintf("ssh multiplexing disabled: no owned ControlPath directory fits the 100-byte socket budget (tried %s)", strings.Join(attempted, ", ")))
		} else {
			args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(muxDir, "%C"), "-o", "ControlPersist=60s")
		}
	}
	args = append(args, "-o", "LogLevel=ERROR")
	if opts.Remote.Port != 22 {
		args = append(args, "-p", strconv.Itoa(opts.Remote.Port))
	}
	args = append(args, opts.Remote.Destination(), "agentchute-hub")
	return SSHInvocation{Args: args, Warnings: warnings}, nil
}

// selectMuxDir chooses the ControlPath directory. There is ONE arm: an owned
// per-user directory under a temp root.
//
// A hub-dir-local arm used to be tried first, and it was DEAD — unreachable for
// every possible home, including an empty one. The budget
// (controlPathFits) leaves 34 bytes for the whole directory, while
// `<home>/.agentchute/hub/<12hex>/mux/<12hex>` spends 46 before a single byte of
// $HOME. It looked healthy only because the fallback covers it on every run:
// the same shape as the uid arm that no real user could reach, one arm over.
// No naming scheme rescues it — dropping the hub-id segment still leaves room
// for a one-byte home — so the arm is gone rather than shortened or documented
// as vestigial, which would only invite its restoration.
//
// The honest cost, stated because it is a real consequence and not a wrinkle:
// mux sockets live OUTSIDE the hub dir, so removing a hub dir does not remove
// them. That was already true — the preferred arm never ran — so this changes
// nothing operationally; it only stops the code claiming a locality it did not
// have. Stale sockets are harmless (ssh reconnects when a socket is dead) and a
// migration reaps them explicitly before removing the directory, by
// re-deriving the live path rather than assuming one.
func selectMuxDir(opts SSHBuildOptions, isolationKey string) (string, []string, error) {
	ensure := opts.EnsureOwned
	if ensure == nil {
		ensure = loop.EnsureOwnedSocketDir
	}
	var attempted []string
	roots := opts.TempRoots
	if roots == nil {
		roots = []string{os.TempDir(), "/tmp"}
	}
	uid := opts.UserID
	if uid == "" {
		uid = currentUserID()
	}
	seen := map[string]bool{}
	for _, root := range roots {
		candidate := filepath.Join(root, "ac-"+uid, isolationKey)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		attempted = append(attempted, candidate)
		if !controlPathFits(candidate) {
			continue
		}
		if err := ensure(candidate); err != nil {
			continue
		}
		return candidate, attempted, nil
	}
	return "", attempted, errors.New("no usable mux directory")
}

// muxIsolationKey is an opaque connection-isolation key, not a hub id. HubID
// already hashes the canonical URL (user, host, port, and pool path). SSH's %C
// token omits IdentityFile, so this also binds the acting agent and resolved
// key version; rotating the stable active-key symlink therefore opens a new
// master instead of reusing one authenticated with the retired key.
func muxIsolationKey(remote *loop.RemoteConfig, agentID, keyPath string) string {
	resolvedKey := filepath.Clean(keyPath)
	if resolved, err := filepath.EvalSymlinks(keyPath); err == nil {
		resolvedKey = resolved
	}
	sum := sha256.Sum256([]byte(remote.HubID + "\x00" + agentID + "\x00" + resolvedKey))
	return hex.EncodeToString(sum[:])[:muxIsolationKeyWidth]
}

func controlPathFits(muxDir string) bool {
	return len(muxDir)+1+controlPathTokenWidth < controlPathByteBudget
}

func currentUserID() string {
	u, err := user.Current()
	if err == nil && u.Uid != "" {
		return u.Uid
	}
	return "unknown"
}

type Transport interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

type processTransport struct {
	cmd       *exec.Cmd
	stdin     *os.File
	stdout    *os.File
	stderr    bytes.Buffer
	cancel    context.CancelFunc
	waitCh    chan error
	closeOnce sync.Once
	closeDone chan struct{}
	err       error
}

func startSSH(ctx context.Context, invocation SSHInvocation) (Transport, error) {
	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, "ssh", invocation.Args...)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdin, ok := stdinPipe.(*os.File)
	if !ok {
		cancel()
		return nil, fmt.Errorf("ssh stdin is not an os.File")
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, ok := stdoutPipe.(*os.File)
	if !ok {
		cancel()
		return nil, fmt.Errorf("ssh stdout is not an os.File")
	}
	p := &processTransport{cmd: cmd, stdin: stdin, stdout: stdout, cancel: cancel, waitCh: make(chan error, 1), closeDone: make(chan struct{})}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &Error{Code: "E_NO_SSH", Msg: "hub: the `ssh` binary was not found on this machine. Install the OpenSSH client (macOS: preinstalled — check PATH; Debian/Ubuntu: apt install openssh-client), then retry."}
		}
		return nil, err
	}
	go func() { p.waitCh <- cmd.Wait() }()
	return p, nil
}

func (p *processTransport) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *processTransport) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *processTransport) SetReadDeadline(t time.Time) error {
	return p.stdout.SetReadDeadline(t)
}
func (p *processTransport) SetWriteDeadline(t time.Time) error { return p.stdin.SetWriteDeadline(t) }
func (p *processTransport) Close() error {
	p.closeOnce.Do(func() {
		defer close(p.closeDone)
		_ = p.stdin.Close()
		select {
		case p.err = <-p.waitCh:
		case <-time.After(time.Second):
			p.cancel()
			p.err = <-p.waitCh
		}
		p.cancel()
	})
	<-p.closeDone
	return p.err
}
func (p *processTransport) diagnostics() (string, error) {
	return p.stderr.String(), p.err
}

func classifySSHFailure(remote *loop.RemoteConfig, agentID, stage string, cause error, transport Transport) error {
	stderr := ""
	waitErr := error(nil)
	if p, ok := transport.(*processTransport); ok {
		_ = p.Close()
		stderr, waitErr = p.diagnostics()
	} else {
		_ = transport.Close()
	}
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "remote host identification has changed") || strings.Contains(lower, "host key verification failed") {
		return &Error{Code: "E_HOSTKEY_CHANGED", Msg: fmt.Sprintf("hub: HOST KEY CHANGED for %s — refusing to connect. If the hub was reinstalled, confirm with its operator, then run: agentchute hub join --reset-hostkey. If not, treat this as a possible interception.", remote.Host)}
	}
	if strings.Contains(lower, "permission denied") {
		msg := fmt.Sprintf("hub: hub refused this key for %s. Either it was never authorized or it was revoked.", remote.Destination())
		if hubCfg, err := ReadHubConfig(remote.HubID); err == nil {
			if pubkey, err := readActivePublicKey(remote, agentID); err == nil {
				msg += fmt.Sprintf(" Run this ON THE HUB, then retry here:\n  agentchute hub authorize --agent %s --pool %s --key %q", agentID, hubCfg.Pool, pubkey)
			}
		}
		return &Error{Code: "E_UNAUTHORIZED", Msg: msg}
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 127 {
		return &Error{Code: "E_HUB_NO_BINARY", Msg: "hub: connected, but the hub could not run agentchute (remote exit 127 — command not found at /usr/local/bin/agentchute). Reinstall agentchute on the hub, or re-authorize this key so its line points at the current binary path."}
	}
	if stage == "hello-timeout" {
		return &Error{Code: "E_HELLO_TIMEOUT", Msg: "hub: connected but no protocol answer in 10s. The hub-side agentchute may be hung or broken; on the hub run: agentchute doctor"}
	}
	if stage == "connect" {
		return &Error{Code: "E_CONNECT", Msg: fmt.Sprintf("hub: cannot reach %s:%d (connect failed after 5s). Check network/VPN/tailnet, then retry; `agentchute doctor` runs this same probe. (If this machine should no longer be joined to this hub, delete .agentchute-control-repo.)", remote.Host, remote.Port), Retriable: true, Cause: cause}
	}
	return &Error{Code: "E_CHANNEL_LOST", Msg: "hub: channel to the hub was lost" + channelLostDetail(waitErr, stderr, cause), Retriable: true, Cause: cause}
}

// channelLostDetail says what actually failed, for the one arm that otherwise
// cannot. E_CHANNEL_LOST is the fallback: host-key and permission-denied are
// matched on stderr text, hello-timeout and connect on stage, and exactly one
// exit code (127) is read out of waitErr. Everything else lands here.
//
// Three signals are in hand at that point and two were being discarded. `Cause`
// is the READ error — it renders as "EOF", which only restates what "channel
// lost" already means. The two that discriminate are:
//
//   - waitErr: ssh's exit status. This is the value that separates "ssh died"
//     from "ssh faithfully propagated a non-zero exit from the remote hub
//     session", and "signal: killed" from either. Only exit 127 was ever read.
//   - stderr: ssh's own account of why it gave up, captured and then consulted
//     only for two substring matches.
//
// The read error is kept too — "EOF" versus "connection reset by peer" is a real
// distinction — but it is reported alongside the other two rather than as the
// whole story, which is what it was.
//
// stderr can carry remote paths, so only the last non-empty line is included and
// it is length-capped. The exit status carries no such risk and is unconditional.
func channelLostDetail(waitErr error, stderr string, cause error) string {
	var parts []string
	if cause != nil {
		parts = append(parts, "read: "+cause.Error())
	}
	if waitErr != nil {
		parts = append(parts, waitErr.Error())
	}
	if line := lastNonEmptyLine(stderr); line != "" {
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		parts = append(parts, "ssh: "+line)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func readActivePublicKey(remote *loop.RemoteConfig, agentID string) (string, error) {
	active := filepath.Join(remote.HubDir, "keys", agentID+"_ed25519")
	target, err := filepath.EvalSymlinks(active)
	if err != nil {
		return "", err
	}
	data, err := loop.ReadFileLimit(target+".pub", 64<<10)
	if err != nil {
		return "", err
	}
	pubkey := strings.TrimSpace(string(data))
	if pubkey == "" {
		return "", fmt.Errorf("active public key is empty")
	}
	return pubkey, nil
}
