package hubclient

import (
	"bytes"
	"context"
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

const controlPathTokenWidth = 64

type SSHBuildOptions struct {
	Remote        *loop.RemoteConfig
	AgentID       string
	KeyPath       string
	StateDir      string
	Channel       bool
	TempRoots     []string
	EnsureOwned   func(string) error
	PreferredRoot string
	UserID        string
}

type SSHInvocation struct {
	Args     []string
	Warnings []string
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
	if opts.Channel {
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none")
	} else {
		if opts.PreferredRoot == "" {
			opts.PreferredRoot = stateDir
		}
		muxDir, attempted, err := selectMuxDir(opts)
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

func selectMuxDir(opts SSHBuildOptions) (string, []string, error) {
	ensure := opts.EnsureOwned
	if ensure == nil {
		ensure = loop.EnsureOwnedSocketDir
	}
	preferred := filepath.Join(opts.Remote.HubDir, "mux")
	if opts.PreferredRoot != "" {
		preferred = filepath.Join(opts.PreferredRoot, "mux")
	}
	attempted := []string{preferred}
	if controlPathFits(preferred) {
		if err := loop.EnsurePrivateDir(preferred); err == nil {
			return preferred, attempted, nil
		}
	}
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
		candidate := filepath.Join(root, "agentchute-hub-"+uid, opts.Remote.HubID)
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

func controlPathFits(muxDir string) bool {
	return len(muxDir)+1+controlPathTokenWidth < 100
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
	return &Error{Code: "E_CHANNEL_LOST", Msg: "hub: channel to the hub was lost", Retriable: true, Cause: cause}
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
