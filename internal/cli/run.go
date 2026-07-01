package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
	runnerpty "github.com/agentchute/agentchute/internal/runner/pty"
)

const (
	defaultRunnerIntervalSeconds = 5
	defaultRunnerIdleGrace       = 2 * time.Second
	defaultRunnerBusyGrace       = 30 * time.Second
	defaultRunnerPrompt          = "[agentchute] check inbox"

	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
	codexEnhancedEnter  = "\x1b[13;1u"
)

type interruptPolicy string

const (
	interruptAfterIdle  interruptPolicy = "after-idle"
	interruptAfterGrace interruptPolicy = "after-grace"
	interruptAlways     interruptPolicy = "always"
)

type runnerOptions struct {
	AgentID         string
	Vendor          string
	ControlRepo     string
	LoopDir         string
	IntervalSeconds int
	InterruptPolicy interruptPolicy
	Prompt          string
	IdleGrace       time.Duration
	BusyGrace       time.Duration
	WrapperArgs     []string
	ContextualID    bool
	ContextualBase  string
	ShimName        string // ac-* launcher shim that started this lane (provenance).
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var opts runnerOptions
	var idleGrace, busyGrace time.Duration
	fs.StringVar(&opts.AgentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&opts.Vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, google, xai)")
	fs.StringVar(&opts.ControlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&opts.LoopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.IntVar(&opts.IntervalSeconds, "interval", defaultRunnerIntervalSeconds, "inbox poll interval in seconds")
	fs.Var((*interruptPolicyFlag)(&opts.InterruptPolicy), "interrupt-policy", "after-idle|after-grace|always")
	fs.StringVar(&opts.Prompt, "prompt", defaultRunnerPrompt, "prompt injected when mail arrives")
	fs.DurationVar(&idleGrace, "idle-grace", defaultRunnerIdleGrace, "quiet period before a wrapper is considered idle")
	fs.DurationVar(&busyGrace, "busy-grace", defaultRunnerBusyGrace, "busy period before after-grace sends Ctrl-C")
	fs.StringVar(&opts.ShimName, "shim-name", "", "ac-* launcher shim that started this lane (provenance; set by the shim)")
	if err := fs.Parse(args); err != nil {
		return runUsage(err)
	}

	if opts.IntervalSeconds < loop.MinPollerIntervalSeconds {
		return fmt.Errorf("--interval must be >= %d seconds", loop.MinPollerIntervalSeconds)
	}
	if opts.InterruptPolicy == "" {
		opts.InterruptPolicy = interruptAfterIdle
	}
	if !validInterruptPolicy(opts.InterruptPolicy) {
		return fmt.Errorf("--interrupt-policy must be one of after-idle, after-grace, always")
	}
	opts.Prompt = strings.TrimSpace(opts.Prompt)
	if opts.Prompt == "" {
		return fmt.Errorf("--prompt must not be empty")
	}
	if idleGrace <= 0 {
		return fmt.Errorf("--idle-grace must be > 0")
	}
	if busyGrace <= 0 {
		return fmt.Errorf("--busy-grace must be > 0")
	}
	opts.IdleGrace = idleGrace
	opts.BusyGrace = busyGrace
	opts.WrapperArgs = fs.Args()
	if len(opts.WrapperArgs) == 0 {
		return runUsage(fmt.Errorf("missing wrapper command after --"))
	}
	opts.Vendor = strings.TrimSpace(opts.Vendor)
	if opts.Vendor == "" {
		if spec, ok := wrapperSpecForName(filepath.Base(opts.WrapperArgs[0])); ok {
			opts.Vendor = spec.Vendor
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: opts.ControlRepo,
		LoopDirFlag:     opts.LoopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return err
	}
	contextualBase, contextual, err := contextualIdentityBase(opts.AgentID, opts.Vendor)
	if err != nil {
		return err
	}
	opts.AgentID, err = resolveAgentID(opts.AgentID, opts.Vendor, cfg)
	if err != nil {
		return err
	}
	if err := loop.ValidateAgentID(opts.AgentID); err != nil {
		return err
	}
	opts.Vendor = resolveAgentVendor(opts.Vendor, opts.AgentID, cfg)
	if opts.Vendor == "" {
		return fmt.Errorf("missing --vendor (recommended values: anthropic, openai, google)")
	}
	if err := loop.ValidateAgentID(opts.Vendor); err != nil {
		return fmt.Errorf("--vendor: %w", err)
	}
	opts.ContextualID = contextual
	opts.ContextualBase = contextualBase
	return runWrapper(cfg, opts, cwd)
}

type interruptPolicyFlag interruptPolicy

func (p *interruptPolicyFlag) String() string {
	return string(*p)
}

func (p *interruptPolicyFlag) Set(v string) error {
	policy := interruptPolicy(strings.TrimSpace(v))
	if !validInterruptPolicy(policy) {
		return fmt.Errorf("invalid interrupt policy %q", v)
	}
	*p = interruptPolicyFlag(policy)
	return nil
}

func validInterruptPolicy(p interruptPolicy) bool {
	switch p {
	case interruptAfterIdle, interruptAfterGrace, interruptAlways:
		return true
	default:
		return false
	}
}

func runUsage(err error) error {
	if err == flag.ErrHelp {
		return runHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, runHelp())
}

func runHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, runHelp())
}

func runHelp() string {
	return strings.TrimSpace(`
Usage: agentchute serve --vendor <vendor> [--as <id>] [flags] -- <wrapper> [args...]

Launch an interactive wrapper under agentchute's PTY runner. The runner owns
registration, last_seen heartbeat updates, the serve lease (id-uniqueness +
fence), inbox polling, and prompt injection when mail arrives.

Flags:
  --as <id>                  agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>          vendor or origin (anthropic, openai, google, xai)
  --interval <seconds>       inbox poll interval (minimum 5; default 5)
  --interrupt-policy <mode>  after-idle|after-grace|always (default after-idle; idle is heuristic)
  --prompt <text>            prompt injected on wake (default "[agentchute] check inbox")
  --idle-grace <duration>    quiet period before prompt injection (default 2s)
  --busy-grace <duration>    grace before Ctrl-C in after-grace mode (default 30s)
  --control-repo <p>         control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>             loop dir path (or $AGENTCHUTE_LOOP_DIR)
`)
}

type runnerRuntime struct {
	cfg      *loop.Config
	opts     runnerOptions
	cwd      string
	started  time.Time
	childPID int
	cmd      *exec.Cmd
	ptmx     *os.File
	lease    *loop.ServeLease
	done     <-chan error

	mu                 sync.Mutex
	ptmxMu             sync.Mutex
	stopOnce           sync.Once
	pollWG             sync.WaitGroup
	shutdownRequested  atomic.Bool
	pendingWake        bool
	lastInjection      time.Time
	lastPoll           time.Time
	seenInboxFiles     map[string]struct{}
	lastOutputUnixNano atomic.Int64
	lastInputUnixNano  atomic.Int64

	wakeCh chan struct{}
	stopCh chan struct{}
}

func runWrapper(cfg *loop.Config, opts runnerOptions, cwd string) error {
	stateDir := cfg.AgentStateDir(opts.AgentID)
	if err := loop.EnsurePrivateDir(stateDir); err != nil {
		return err
	}
	// id-uniqueness + fence: the serve lease REPLACES the old socket-dial
	// collision guard. ErrLeaseHeld => another live serve owns this id; refuse to
	// start. The returned lease is held for the runner's lifetime: renewed each
	// poll tick (fence verify) and released on exit. Pull-only (Gate 6c): nothing
	// binds a socket and the registration publishes no wake target, so no socket
	// path is computed.
	lease, err := refuseLiveRunnerCollision(cfg, opts.AgentID)
	if err != nil {
		return err
	}

	cmd := exec.Command(opts.WrapperArgs[0], opts.WrapperArgs[1:]...)
	cmd.Dir = cwd
	cmd.Env = runnerChildEnv(cfg, opts, lease.Token)
	// Size the child's PTY from our own terminal before the child starts —
	// a TUI that reads a 0x0 winsize on first draw renders a blank screen.
	ptmx, err := runnerpty.StartInheritSize(cmd, os.Stdin)
	if err != nil {
		_ = loop.ReleaseLease(lease)
		return fmt.Errorf("start wrapper under PTY: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	if err := registerRunner(cfg, opts, time.Now().UTC()); err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		<-done
		_ = saveRunnerOfflineState(cfg, opts.AgentID, cmd.Process.Pid, time.Now().UTC())
		_ = loop.ReleaseLease(lease)
		return err
	}

	restoreTerminal, rawEnabled, err := runnerMakeRaw(os.Stdin)
	if err != nil {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		<-done
		_ = saveRunnerOfflineState(cfg, opts.AgentID, cmd.Process.Pid, time.Now().UTC())
		_ = markRunnerOffline(cfg, opts.AgentID)
		_ = loop.ReleaseLease(lease)
		return fmt.Errorf("set stdin raw mode: %w", err)
	}
	if rawEnabled {
		defer func() {
			if err := restoreTerminal(); err != nil {
				fmt.Fprintf(os.Stderr, "agentchute serve: restore terminal: %v\n", err)
			}
		}()
	}

	rt := &runnerRuntime{
		cfg:            cfg,
		opts:           opts,
		cwd:            cwd,
		started:        time.Now().UTC(),
		childPID:       cmd.Process.Pid,
		cmd:            cmd,
		ptmx:           ptmx,
		lease:          lease,
		done:           done,
		wakeCh:         make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		seenInboxFiles: make(map[string]struct{}),
	}
	nowUnix := time.Now().UnixNano()
	rt.lastOutputUnixNano.Store(nowUnix)
	rt.lastInputUnixNano.Store(nowUnix)
	if err := rt.saveState(); err != nil {
		fmt.Fprintf(os.Stderr, "agentchute serve: write runner state: %v\n", err)
	}

	defer func() {
		rt.stopLoops()
		// Wait for the poll loop to fully exit BEFORE marking offline. stopLoops
		// only closes stopCh; a pollOnce already in flight would otherwise run
		// its UpdateLastSeen after markRunnerOffline and resurrect Status=active.
		rt.pollWG.Wait()
		rt.closePTY()
		_ = rt.saveStateWithStatus("offline")
		_ = markRunnerOffline(cfg, opts.AgentID)
		// Release the serve lease last. ErrFenced => we were already reclaimed
		// (another serve owns the id); releasing would be a no-op and must not
		// delete the new owner's claim, so it is not an error to report.
		if err := loop.ReleaseLease(rt.lease); err != nil && !errors.Is(err, loop.ErrFenced) {
			fmt.Fprintf(os.Stderr, "agentchute serve: release serve lease: %v\n", err)
		}
	}()

	rt.pollWG.Add(1)
	go rt.pollLoop()
	go rt.injectLoop()
	go rt.copyPTYOutput()
	go rt.copyInput()
	go rt.resizeLoop()
	go rt.shutdownSignalLoop()

	err = <-done
	rt.stopLoops()
	if err != nil && !rt.shutdownRequested.Load() {
		return fmt.Errorf("wrapper exited: %w", err)
	}
	return nil
}

func runnerChildEnv(cfg *loop.Config, opts runnerOptions, serveToken string) []string {
	env := os.Environ()
	env = append(env,
		"AGENTCHUTE_AGENT_ID="+opts.AgentID,
		"AGENTCHUTE_CONTROL_REPO="+cfg.ControlRepo,
		"AGENTCHUTE_LOOP_DIR="+cfg.LoopDir,
		"AGENTCHUTE_RUNNER=1",
		"AGENTCHUTE_RUNNER_PID="+strconv.Itoa(os.Getpid()),
		// Fence the child's sends: send.go passes this serve_token to
		// AllocateSeq so a write from a fenced (reclaimed) agent fails closed
		// (protocol-v2 §6b). Empty when launched without a lease => unfenced.
		"AGENTCHUTE_SERVE_TOKEN="+serveToken,
	)
	return env
}

func registerRunner(cfg *loop.Config, opts runnerOptions, now time.Time) error {
	// Pull-only (Gate 6c): the runner publishes no wake target — it owns the wake
	// path via the PTY supervisor, not a registration field. The registration is
	// a plain no-wake record.
	_, err := performRegister(cfg, registerOpts{
		AgentID:            opts.AgentID,
		Vendor:             opts.Vendor,
		WorkingRepos:       []string{cfg.ControlRepo},
		Host:               localHostname(),
		HostProvided:       true,
		ContextualIdentity: opts.ContextualID,
		ContextualBaseID:   opts.ContextualBase,
		// WI-E3 provenance: the runner owns this lane. ShimName is threaded from
		// the launcher shim (cmdShimsExec passes --shim-name) when present.
		LaunchedBy: loop.LaunchedByRunner,
		ShimName:   opts.ShimName,
	}, now)
	return err
}

func markRunnerOffline(cfg *loop.Config, agentID string) error {
	// Serialize against a concurrent pollOnce -> UpdateLastSeen so the offline
	// status write is not clobbered by a stale-read last_seen refresh that would
	// resurrect Status=active after shutdown. (The caller also joins the poll
	// loop before invoking this, so by here no poller should still be running;
	// the lock is belt-and-suspenders against any other writer.)
	return loop.WithAgentLock(cfg, agentID, func() error {
		regPath := cfg.AgentRegistrationPath(agentID)
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			return err
		}
		reg.Status = loop.StatusOffline
		reg.LastSeen = time.Now().UTC()
		return loop.WriteRegistration(regPath, reg)
	})
}

// refuseLiveRunnerCollision enforces id-uniqueness via the serve lease
// (protocol-v2 §6b) instead of the retired socket dial. AcquireServeLease fails
// closed with ErrLeaseHeld when a FRESH valid claim already owns the id — a
// second live serve must not start (the same refusal the old socket-ping guard
// produced). A stale/released claim is reclaimable, so the launch proceeds. The
// returned lease is held by the runner for its lifetime (renew/release).
func refuseLiveRunnerCollision(cfg *loop.Config, agentID string) (*loop.ServeLease, error) {
	lease, err := loop.AcquireServeLease(cfg, agentID)
	if err != nil {
		if errors.Is(err, loop.ErrLeaseHeld) {
			return nil, fmt.Errorf("runner for %s is already active (serve lease held)", agentID)
		}
		return nil, fmt.Errorf("acquire serve lease for %s: %w", agentID, err)
	}
	return lease, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (r *runnerRuntime) pollLoop() {
	defer r.pollWG.Done()
	ticker := time.NewTicker(time.Duration(r.opts.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	r.pollOnce(false)
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.pollOnce(true)
		}
	}
}

func (r *runnerRuntime) pollOnce(enqueueNew bool) {
	now := time.Now().UTC()
	// Fence verify + heartbeat the serve lease (protocol-v2 §6b). ErrFenced means
	// we were RECLAIMED — another serve now owns this id — so we must stop
	// injecting and shut down cleanly rather than become a dup-writer. (nil lease
	// in the poll-only unit-test runtime: skip.)
	if r.lease != nil {
		if err := loop.RenewLease(r.lease); err != nil {
			if errors.Is(err, loop.ErrFenced) {
				fmt.Fprintf(os.Stderr, "agentchute serve: serve lease reclaimed (fenced); shutting down\n")
				r.requestShutdown(syscall.SIGTERM)
				return
			}
			fmt.Fprintf(os.Stderr, "agentchute serve: renew serve lease: %v\n", err)
		}
	}
	if err := loop.UpdateLastSeen(r.cfg, r.opts.AgentID, now); err != nil {
		fmt.Fprintf(os.Stderr, "agentchute serve: update last_seen: %v\n", err)
	}
	// Track a SEEN-filename snapshot across BOTH parsed messages and skipped
	// (malformed/unparseable) files. Lexicographic-newest tracking misses two
	// real cases: (1) malformed files never matched a Message at all yet gate
	// blocks until `check` quarantines them, and (2) a valid message whose
	// sender-encoded filename timestamp sorts BEFORE the last observed name
	// (clock skew, back-dated send) would never become the "newest". Any
	// filename not already in the set is unseen mail and must enqueue a wake.
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(r.cfg.AgentInboxDir(r.opts.AgentID))
	if err != nil && !errors.Is(err, loop.ErrInboxMissing) {
		fmt.Fprintf(os.Stderr, "agentchute serve: list inbox: %v\n", err)
	}
	r.mu.Lock()
	if r.seenInboxFiles == nil {
		r.seenInboxFiles = make(map[string]struct{})
	}
	hasUnseen := false
	for _, m := range msgs {
		if _, ok := r.seenInboxFiles[m.Filename]; !ok {
			r.seenInboxFiles[m.Filename] = struct{}{}
			hasUnseen = true
		}
	}
	for _, name := range skipped {
		if _, ok := r.seenInboxFiles[name]; !ok {
			r.seenInboxFiles[name] = struct{}{}
			hasUnseen = true
		}
	}
	r.mu.Unlock()
	if hasUnseen && enqueueNew {
		r.enqueueWake()
	}
	r.mu.Lock()
	r.lastPoll = now
	r.mu.Unlock()
	if err := r.saveState(); err != nil {
		fmt.Fprintf(os.Stderr, "agentchute serve: write runner state: %v\n", err)
	}
}

func (r *runnerRuntime) enqueueWake() {
	if r.shutdownRequested.Load() {
		return
	}
	r.mu.Lock()
	r.pendingWake = true
	r.mu.Unlock()
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
	_ = r.saveState()
}

func (r *runnerRuntime) injectLoop() {
	for {
		select {
		case <-r.stopCh:
			return
		case <-r.wakeCh:
			if r.waitForInjectionWindow() {
				r.injectPrompt()
			}
		}
	}
}

func (r *runnerRuntime) waitForInjectionWindow() bool {
	started := time.Now()
	for {
		if r.shutdownRequested.Load() {
			return false
		}
		if r.isIdle() {
			return true
		}
		switch r.opts.InterruptPolicy {
		case interruptAfterIdle:
			// Keep waiting.
		case interruptAfterGrace:
			if time.Since(started) >= r.opts.BusyGrace {
				_ = r.writePTY([]byte{0x03})
				time.Sleep(300 * time.Millisecond)
				return true
			}
		case interruptAlways:
			_ = r.writePTY([]byte{0x03})
			time.Sleep(300 * time.Millisecond)
			return true
		}
		select {
		case <-r.stopCh:
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (r *runnerRuntime) isIdle() bool {
	lastOutput := r.lastOutputUnixNano.Load()
	lastInput := r.lastInputUnixNano.Load()
	last := lastOutput
	if lastInput > last {
		last = lastInput
	}
	return time.Since(time.Unix(0, last)) >= r.opts.IdleGrace
}

func (r *runnerRuntime) injectPrompt() {
	if err := r.writePTY(promptInjectionBytes(r.opts)); err != nil {
		fmt.Fprintf(os.Stderr, "agentchute serve: inject prompt: %v\n", err)
		return
	}
	now := time.Now().UTC()
	r.mu.Lock()
	r.pendingWake = false
	r.lastInjection = now
	r.mu.Unlock()
	_ = r.saveState()
}

func promptInjectionBytes(opts runnerOptions) []byte {
	if shouldUseCodexSubmitSequence(opts) {
		return []byte(bracketedPasteStart + opts.Prompt + bracketedPasteEnd + codexEnhancedEnter)
	}
	return []byte(opts.Prompt + "\r")
}

func shouldUseCodexSubmitSequence(opts runnerOptions) bool {
	if strings.EqualFold(opts.AgentID, "codex") {
		return true
	}
	if len(opts.WrapperArgs) == 0 {
		return false
	}
	return filepath.Base(opts.WrapperArgs[0]) == "codex"
}

func (r *runnerRuntime) copyPTYOutput() {
	buf := make([]byte, 32*1024)
	for {
		r.ptmxMu.Lock()
		ptmx := r.ptmx
		r.ptmxMu.Unlock()
		if ptmx == nil {
			return
		}
		n, err := ptmx.Read(buf)
		if n > 0 {
			r.lastOutputUnixNano.Store(time.Now().UnixNano())
			if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
				fmt.Fprintf(os.Stderr, "agentchute serve: write stdout: %v\n", werr)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *runnerRuntime) copyInput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			r.lastInputUnixNano.Store(time.Now().UnixNano())
			if werr := r.writePTY(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *runnerRuntime) resizeLoop() {
	stdin := os.Stdin
	if runnerIsTerminal(stdin) {
		_ = runnerpty.InheritSize(stdin, r.ptmx)
	}
	ch := make(chan os.Signal, 1)
	signalNotifyResize(ch)
	defer signalStopResize(ch)
	for {
		select {
		case <-r.stopCh:
			return
		case <-ch:
			if runnerIsTerminal(stdin) {
				_ = runnerpty.InheritSize(stdin, r.ptmx)
			}
		}
	}
}

func (r *runnerRuntime) shutdownSignalLoop() {
	ch := make(chan os.Signal, 2)
	signalNotifyShutdown(ch)
	defer signalStopShutdown(ch)
	select {
	case <-r.stopCh:
		return
	case sig := <-ch:
		if s, ok := sig.(syscall.Signal); ok {
			r.requestShutdown(s)
		} else {
			r.requestShutdown(syscall.SIGTERM)
		}
	}
}

func (r *runnerRuntime) requestShutdown(sig syscall.Signal) {
	if !r.shutdownRequested.CompareAndSwap(false, true) {
		return
	}
	r.stopLoops()
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(sig)
		go func() {
			time.Sleep(300 * time.Millisecond)
			r.closePTY()
			time.Sleep(2 * time.Second)
			if processAlive(r.cmd.Process.Pid) {
				_ = r.cmd.Process.Kill()
			}
		}()
	}
}

func (r *runnerRuntime) stopLoops() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}

func (r *runnerRuntime) writePTY(p []byte) error {
	if r.shutdownRequested.Load() {
		return os.ErrClosed
	}
	r.ptmxMu.Lock()
	defer r.ptmxMu.Unlock()
	if r.ptmx == nil {
		return os.ErrClosed
	}
	_, err := r.ptmx.Write(p)
	return err
}

func (r *runnerRuntime) closePTY() {
	r.ptmxMu.Lock()
	defer r.ptmxMu.Unlock()
	if r.ptmx != nil {
		_ = r.ptmx.Close()
		r.ptmx = nil
	}
}

func (r *runnerRuntime) saveState() error {
	return r.saveStateWithStatus("active")
}

func (r *runnerRuntime) saveStateWithStatus(status string) error {
	r.mu.Lock()
	st := loop.RunnerState{
		AgentID:       r.opts.AgentID,
		Host:          localHostname(),
		RunnerPID:     os.Getpid(),
		ChildPID:      r.childPID,
		StartedAt:     r.started,
		LastPoll:      r.lastPoll,
		LastInjection: r.lastInjection,
		PendingWake:   r.pendingWake,
		Status:        status,
	}
	r.mu.Unlock()
	return loop.SaveRunnerState(r.cfg, st)
}

func saveRunnerOfflineState(cfg *loop.Config, agentID string, childPID int, now time.Time) error {
	return loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:   agentID,
		Host:      localHostname(),
		RunnerPID: os.Getpid(),
		ChildPID:  childPID,
		StartedAt: now,
		Status:    "offline",
	})
}
