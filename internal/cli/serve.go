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

// recueInterval bounds how often the runner re-cues while unread mail sits in
// the inbox and the last cue hasn't been consumed (v2.5 plan A2 / decision
// §8). Package var (not a const) ONLY so tests can shrink it (same pattern as
// seqRecentWindow/liveWindow/leaseTimeout); production keeps 60s. Do not
// shrink below ~10s in production: it must stay well above IdleGrace so
// re-cues never chase their own echo.
var recueInterval = 60 * time.Second

// sweepInterval bounds how often the runner's poll tick runs
// SweepStaleRegistrations (v2.5 plan B1, C11). Package var (same pattern as
// recueInterval/leaseTimeout) so tests can shrink it; production keeps 10m —
// hygiene stays lazy but bounded, and boot already covers a pool whose only
// runner is offline.
var sweepInterval = 10 * time.Minute

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
	ShimName        string // ac-* launcher shim that started this lane (provenance).
	Guarded         bool   // wrapper's hooks can clear the guard latch (v2.5 A7/C22); serve exports AGENTCHUTE_GUARD=1 only when true.
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
	if spec, ok := wrapperSpecForName(filepath.Base(opts.WrapperArgs[0])); ok {
		if opts.Vendor == "" {
			opts.Vendor = spec.Vendor
		}
		opts.Guarded = spec.Guarded
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
	opts.AgentID, err = resolveAgentID(opts.AgentID)
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
	diag     *runnerDiagnostics

	// regTemplate is the no-wake registration template HeartbeatRegistration
	// refreshes every tick (v2.5 plan B1, C13). Built once from the same
	// fields registerRunner used for the initial write, so every heartbeat
	// re-asserts the same AgentID/Vendor/ControlRepo/Host/provenance.
	regTemplate loop.Registration
	// lastSweep is the last time this runner ran SweepStaleRegistrations
	// (v2.5 plan B1, C11). Zero value means "never yet" — pollOnce treats
	// that as due immediately, matching boot's "sweep once, early" behavior
	// for a pool whose only hygiene path is this runner.
	lastSweep time.Time

	mu                 sync.Mutex
	ptmxMu             sync.Mutex
	stopOnce           sync.Once
	pollWG             sync.WaitGroup
	shutdownRequested  atomic.Bool
	pendingWake        bool
	injectedThisPeriod bool // true once a cue has SUCCEEDED for the current pending period (A2/C16-C17, review fix)
	lastInjection      time.Time
	lastPoll           time.Time
	lastOutputUnixNano atomic.Int64
	lastInputUnixNano  atomic.Int64

	wakeCh chan bool // true = re-cue (retry), false = first cue of a pending period
	stopCh chan struct{}
}

type runnerDiagnostics struct {
	mu         sync.Mutex
	file       *os.File
	dropped    int
	fatalLines []string
}

func newRunnerDiagnostics(cfg *loop.Config, agentID string) *runnerDiagnostics {
	d := &runnerDiagnostics{}
	stateDir := cfg.AgentStateDir(agentID)
	if err := loop.EnsurePrivateDir(stateDir); err != nil {
		d.dropped++
		return d
	}
	logFile, err := os.OpenFile(filepath.Join(stateDir, "runner.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		d.dropped++
		return d
	}
	d.file = logFile
	return d
}

func (d *runnerDiagnostics) logf(format string, args ...any) {
	if d == nil {
		return
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	d.write(timestamp + " " + fmt.Sprintf(format, args...))
}

func (d *runnerDiagnostics) bufferFatalf(format string, args ...any) {
	if d == nil {
		return
	}
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	d.logf("%s\n", msg)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fatalLines = append(d.fatalLines, msg)
}

func (d *runnerDiagnostics) printBufferedFatal(w io.Writer) {
	if d == nil {
		return
	}
	d.mu.Lock()
	lines := append([]string(nil), d.fatalLines...)
	d.fatalLines = nil
	d.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "%s\n", strings.Join(lines, "; "))
}

func (d *runnerDiagnostics) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
}

func (d *runnerDiagnostics) write(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		d.dropped++
		return
	}
	if _, err := io.WriteString(d.file, msg); err != nil {
		d.dropped++
		_ = d.file.Close()
		d.file = nil
	}
}

func (r *runnerRuntime) logf(format string, args ...any) {
	if r == nil || r.diag == nil {
		return
	}
	r.diag.logf(format, args...)
}

func (r *runnerRuntime) bufferFatalf(format string, args ...any) {
	if r == nil || r.diag == nil {
		return
	}
	r.diag.bufferFatalf(format, args...)
}

func restoreRunnerTerminal(restoreTerminal func() error, diag *runnerDiagnostics, stderr io.Writer) {
	if err := restoreTerminal(); err != nil {
		diag.bufferFatalf("agentchute serve: restore terminal: %v\n", err)
	}
	diag.printBufferedFatal(stderr)
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

	if err := registerRunner(cfg, opts, lease.Token, time.Now().UTC()); err != nil {
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
		_ = loop.ReleaseLease(lease)
		return fmt.Errorf("set stdin raw mode: %w", err)
	}
	diag := newRunnerDiagnostics(cfg, opts.AgentID)
	defer diag.close()
	if rawEnabled {
		defer restoreRunnerTerminal(restoreTerminal, diag, os.Stderr)
	} else {
		defer diag.printBufferedFatal(os.Stderr)
	}

	rt := &runnerRuntime{
		cfg:         cfg,
		opts:        opts,
		cwd:         cwd,
		started:     time.Now().UTC(),
		childPID:    cmd.Process.Pid,
		cmd:         cmd,
		ptmx:        ptmx,
		lease:       lease,
		done:        done,
		diag:        diag,
		regTemplate: heartbeatTemplate(cfg, opts),
		wakeCh:      make(chan bool, 1),
		stopCh:      make(chan struct{}),
	}
	nowUnix := time.Now().UnixNano()
	rt.lastOutputUnixNano.Store(nowUnix)
	rt.lastInputUnixNano.Store(nowUnix)
	if err := rt.saveState(); err != nil {
		rt.logf("agentchute serve: write runner state: %v\n", err)
	}

	defer func() {
		rt.stopLoops()
		// B1: a quitting agent does nothing to its registration row — no more
		// Status=offline write here. The row simply stops heartbeating and
		// either ages past StaleAfter (swept by boot/serve elsewhere) or gets
		// refreshed again if this same process relaunches. Still wait for the
		// poll loop to fully exit before proceeding: rt.pollWG.Wait() below
		// keeps saveStateWithStatus("offline") (runner.json, not the
		// registration) from racing a pollOnce still in flight.
		rt.pollWG.Wait()
		rt.closePTY()
		_ = rt.saveStateWithStatus("offline")
		// Release the serve lease last. ErrFenced => we were already reclaimed
		// (another serve owns the id); releasing would be a no-op and must not
		// delete the new owner's claim, so it is not an error to report.
		if err := loop.ReleaseLease(rt.lease); err != nil && !errors.Is(err, loop.ErrFenced) {
			rt.logf("agentchute serve: release serve lease: %v\n", err)
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
	// Strip any inherited AGENTCHUTE_GUARD before conditionally re-adding it
	// below: os.Environ() carries THIS process's own env, which is nonempty
	// whenever a guarded session itself launches `ac serve <other-wrapper>`
	// (e.g. `ac serve grok` run from inside a guarded claude-code session).
	// Without stripping, an unguarded wrapper's child would inherit the bit
	// from its guarded parent and appear armed with no hook able to ever
	// clear the latch (codex review, PR #89 finding #2).
	env := withoutEnv(os.Environ(), "AGENTCHUTE_GUARD")
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
	if opts.Guarded {
		// v2.5 A7/C22: only a wrapper whose installed hooks can clear the
		// guard latch (via turn-end) may have it armed. Unguarded wrappers
		// (grok: hookless) never see this bit, so guard.go's session
		// resolution always allows them.
		env = append(env, "AGENTCHUTE_GUARD=1")
	}
	return env
}

func registerRunner(cfg *loop.Config, opts runnerOptions, serveToken string, now time.Time) error {
	// Pull-only (Gate 6c): the runner publishes no wake target — it owns the wake
	// path via the PTY supervisor, not a registration field. The registration is
	// a plain no-wake record.
	_, err := performRegister(cfg, registerOpts{
		AgentID:      opts.AgentID,
		Vendor:       opts.Vendor,
		WorkingRepos: []string{cfg.ControlRepo},
		Host:         localHostname(),
		HostProvided: true,
		ServeToken:   serveToken,
		// WI-E3 provenance: the runner owns this lane. ShimName is threaded from
		// the launcher shim (cmdShimsExec passes --shim-name) when present.
		LaunchedBy: loop.LaunchedByRunner,
		ShimName:   opts.ShimName,
	}, now)
	return err
}

// heartbeatTemplate builds the no-wake registration template HeartbeatRegistration
// refreshes every poll tick (v2.5 plan B1, C13), from the same fields
// registerRunner passes to performRegister for the runner's initial write.
// Built once in runWrapper and reused for the lifetime of the process: it is
// what a swept-then-recreated row comes back as, and what every tick
// re-asserts for AgentID/Vendor/ControlRepo/Host/provenance (Body and
// WorkingRepos come from the on-disk row instead — see HeartbeatRegistration).
func heartbeatTemplate(cfg *loop.Config, opts runnerOptions) loop.Registration {
	return loop.Registration{
		AgentID:         opts.AgentID,
		ProtocolVersion: loop.CurrentProtocolVersion,
		Vendor:          opts.Vendor,
		ControlRepo:     cfg.ControlRepo,
		WorkingRepos:    []string{cfg.ControlRepo},
		Host:            localHostname(),
		Status:          loop.StatusActive,
		LaunchedBy:      loop.LaunchedByRunner,
		ShimName:        opts.ShimName,
	}
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
	r.pollOnce()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.pollOnce()
		}
	}
}

func (r *runnerRuntime) pollOnce() {
	now := time.Now().UTC()
	// Fence verify + heartbeat the serve lease (protocol-v2 §6b). ErrFenced means
	// we were RECLAIMED — another serve now owns this id — so we must stop
	// injecting and shut down cleanly rather than become a dup-writer. (nil lease
	// in the poll-only unit-test runtime: skip.)
	if r.lease != nil {
		if err := loop.RenewLease(r.lease); err != nil {
			if errors.Is(err, loop.ErrFenced) {
				r.bufferFatalf("serve: this agentchute binary was fenced out (update or identity reclaim). Restart this lane: ac serve <wrapper>\n")
				r.requestShutdown(syscall.SIGTERM)
				return
			}
			r.logf("agentchute serve: renew serve lease: %v\n", err)
		}
	}
	// Lease-gated heartbeat (v2.5 plan B1, C13): unconditional refresh of the
	// registration row, self-healing if a sweep (ours or a peer's) removed it
	// since the last tick. nil lease in the poll-only unit-test runtime: skip
	// rather than pass an empty token, which HeartbeatRegistration rejects.
	if r.lease != nil {
		if err := loop.HeartbeatRegistration(r.cfg, r.regTemplate, r.lease.Token); err != nil {
			r.logf("agentchute serve: heartbeat registration: %v\n", err)
		}
	}

	// Lazy sweep (C11): bounded, 10-minute cadence. lastSweep's zero value
	// makes the very first tick due immediately — harmless even right after
	// boot's own sweep (nothing is stale seconds later) and useful for a pool
	// whose runner is its only hygiene path.
	if now.Sub(r.lastSweep) >= sweepInterval {
		if _, err := loop.SweepStaleRegistrations(r.cfg, r.opts.AgentID, now); err != nil {
			r.logf("agentchute serve: sweep stale registrations: %v\n", err)
		}
		r.lastSweep = now
	}

	// Re-cue predicate (v2.5 plan A2, C16/C17): cue whenever mail is pending —
	// including mail already present at startup, and skipped/malformed files
	// (gate blocks until `check` quarantines them). No per-filename seen-set:
	// injectedThisPeriod tracks whether a cue has SUCCEEDED yet for the
	// current pending period — NOT merely been attempted (review fix: a
	// failed first injection must not downgrade the period to recue=true,
	// or a continuously busy wrapper under after-grace/always could suppress
	// the escalation retry indefinitely, since recue=true always waits for
	// idle regardless of --interrupt-policy). The period resets (ready to
	// treat the next arrival as genuinely new) the moment the inbox is
	// observed empty — here, and in injectIfPending's drained-mail skip path.
	// Once a wake is already in flight (pendingWake), we do not re-enqueue —
	// enqueueWake itself guards that.
	pending := r.hasPendingInboxMail()
	r.mu.Lock()
	if !pending {
		r.injectedThisPeriod = false
	}
	injected := r.injectedThisPeriod
	lastInjection := r.lastInjection
	r.mu.Unlock()
	if pending && (!injected || now.Sub(lastInjection) >= recueInterval) {
		r.enqueueWake(injected)
	}

	r.mu.Lock()
	r.lastPoll = now
	r.mu.Unlock()
	if err := r.saveState(); err != nil {
		r.logf("agentchute serve: write runner state: %v\n", err)
	}
}

// enqueueWake queues a wake for injectLoop. recue=false is the first cue of a
// pending period (the configured --interrupt-policy applies); recue=true is a
// retry of still-unread mail (always waits for idle, C17).
//
// Gated on pendingWake (not just the channel's own non-blocking send): once a
// wake is queued, pollOnce must not re-enqueue while it is still in flight —
// including the window where injectLoop has already RECEIVED the value from
// wakeCh (so the channel itself is empty again) but is still blocked inside
// waitForInjectionWindow waiting for idle. A bare non-blocking send would
// succeed and queue a redundant second wake during that window; gating on
// pendingWake closes it and, as a side effect, also means a queued recue=false
// can never be overwritten by a later recue=true (nothing gets queued at all
// while pendingWake is set).
func (r *runnerRuntime) enqueueWake(recue bool) {
	if r.shutdownRequested.Load() {
		return
	}
	r.mu.Lock()
	if r.pendingWake {
		r.mu.Unlock()
		return
	}
	r.pendingWake = true
	r.mu.Unlock()
	select {
	case r.wakeCh <- recue:
	default:
	}
	_ = r.saveState()
}

func (r *runnerRuntime) injectLoop() {
	for {
		select {
		case <-r.stopCh:
			return
		case recue := <-r.wakeCh:
			if r.waitForInjectionWindow(recue) {
				r.injectIfPending()
			}
		}
	}
}

// injectIfPending re-checks the inbox immediately before injecting (M4,
// deep-analysis-v2 addendum): a wake can be enqueued for mail that the
// agent's own `check` — triggered by an earlier cue in the same batch —
// already claimed (moved inbox -> .claimed) by the time this cue reaches the
// front of injectLoop, producing a spurious "check inbox" prompt into an
// already-empty inbox. Single call site (the only caller of injectPrompt).
//
// The skip path clears pendingWake (v2.5 plan A2): today's code left it
// stuck true forever when the inbox drained out from under a queued wake —
// runner.json would report pending_wake:true against an empty inbox
// indefinitely, since only a successful injectPrompt cleared it.
//
// It also resets injectedThisPeriod (review fix): this is an observed-empty
// transition exactly like pollOnce's own, so mail arriving before the next
// poll tick must be treated as a genuinely NEW period (recue=false) rather
// than misclassified as a continuation of the just-ended one, which could
// wait out up to recueInterval before cuing.
func (r *runnerRuntime) injectIfPending() {
	if !r.hasPendingInboxMail() {
		r.mu.Lock()
		r.pendingWake = false
		r.injectedThisPeriod = false
		r.mu.Unlock()
		_ = r.saveState()
		return
	}
	r.injectPrompt()
}

// hasPendingInboxMail reports whether the raw inbox (parsed messages or
// skipped/malformed files — either needs `check` to run) currently has
// anything in it. On a transient listing error, fails OPEN (reports
// pending) to preserve the safe failure direction: an extra spurious cue is
// acceptable, a suppressed real one is not.
func (r *runnerRuntime) hasPendingInboxMail() bool {
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(r.cfg.AgentInboxDir(r.opts.AgentID))
	if err != nil && !errors.Is(err, loop.ErrInboxMissing) {
		return true
	}
	return len(msgs) > 0 || len(skipped) > 0
}

// waitForInjectionWindow blocks until it is safe to inject, or false if the
// runner is shutting down. recue=true (a retry of an already-cued, still
// unread pending period) always waits for idle regardless of
// --interrupt-policy (C17): the Ctrl-C escalation of after-grace/always
// applies only to the first injection of a pending period — repeated Ctrl-C
// every recueInterval would abuse a busy wrapper.
func (r *runnerRuntime) waitForInjectionWindow(recue bool) bool {
	started := time.Now()
	for {
		if r.shutdownRequested.Load() {
			return false
		}
		if r.isIdle() {
			return true
		}
		if !recue {
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
		r.logf("agentchute serve: inject prompt: %v\n", err)
		// Clear pendingWake even on failure (code review fix): enqueueWake's
		// pendingWake gate (A2) means a stuck-true flag here would silently
		// suppress every future wake for the runner's lifetime — including
		// brand-new mail arriving later — defeating the whole point of this
		// slice. lastInjection is deliberately NOT set: nothing was actually
		// delivered, so the recueInterval countdown must not start.
		// injectedThisPeriod is deliberately left UNTOUCHED (review fix): a
		// failed attempt must not be treated as a delivered first cue (the
		// next poll should still retry with the configured --interrupt-policy
		// escalation, not downgrade to a silent recue=true), but if a PRIOR
		// attempt this period already succeeded, that earlier success must
		// still stand — only pollOnce/injectIfPending's observed-empty resets
		// clear this flag.
		r.mu.Lock()
		r.pendingWake = false
		r.mu.Unlock()
		_ = r.saveState()
		return
	}
	now := time.Now().UTC()
	r.mu.Lock()
	r.pendingWake = false
	r.lastInjection = now
	r.injectedThisPeriod = true
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
				r.logf("agentchute serve: write stdout: %v\n", werr)
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

// afterSaveStateSnapshotHook, when non-nil, fires inside saveStateWithStatus
// AFTER the snapshot is taken but BEFORE the durable write — while r.mu is
// STILL HELD. Test-only seam proving the snapshot-plus-write is one atomic
// critical section (a concurrent caller cannot mutate state or start its own
// write until this one fully completes, so a slow writer's stale snapshot can
// never "lap" a fresher concurrent write onto disk). nil in production.
var afterSaveStateSnapshotHook func()

// saveStateWithStatus snapshots the runtime's state AND performs the durable
// write under the SAME r.mu critical section (review fix): pollOnce,
// enqueueWake, injectIfPending, and injectPrompt all call this concurrently
// from two different goroutines (poll loop, inject loop). Releasing the lock
// between snapshot and write let two concurrent callers' writes land in
// EITHER order regardless of which snapshot was actually fresher — a slow
// writer holding a stale (e.g. pendingWake=true) snapshot could rename its
// file AFTER a faster, fresher (pendingWake=false) writer, resurrecting the
// exact stale runner.json state this slice exists to prevent. Holding the
// lock for the full duration serializes every write in true snapshot order.
func (r *runnerRuntime) saveStateWithStatus(status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if afterSaveStateSnapshotHook != nil {
		afterSaveStateSnapshotHook()
	}
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
