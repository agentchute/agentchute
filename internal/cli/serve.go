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
	"github.com/agentchute/agentchute/internal/op"
	runnerpty "github.com/agentchute/agentchute/internal/runner/pty"
)

const (
	defaultRunnerIntervalSeconds = 5
	// minServeIntervalSeconds floors --interval (v2.5 plan B5: relocated from
	// the now-deleted internal/loop/poller.go's MinPollerIntervalSeconds,
	// which this same value validated for both the detached poller and
	// serve).
	minServeIntervalSeconds = 5
	defaultRunnerIdleGrace  = 2 * time.Second
	defaultRunnerBusyGrace  = 30 * time.Second
	defaultRunnerPrompt     = "[agentchute] check inbox"

	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
	codexEnhancedEnter  = "\x1b[13;1u"
)

// recueInterval bounds how often the runner re-cues while unread mail sits in
// the inbox and the last cue hasn't been consumed (v2.5 plan A2 / decision
// §8). Package var (not a const) ONLY so tests can shrink it (same pattern as
// seqRecentWindow/leaseTimeout); production keeps 60s. Do not
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
	VendorProvided  bool
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
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "vendor" {
			opts.VendorProvided = true
		}
	})

	if opts.IntervalSeconds < minServeIntervalSeconds {
		return fmt.Errorf("--interval must be >= %d seconds", minServeIntervalSeconds)
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
	launchedWrapper := ""
	if spec, ok := wrapperSpecForName(filepath.Base(opts.WrapperArgs[0])); ok {
		if opts.Vendor == "" {
			opts.Vendor = spec.Vendor
		}
		opts.Guarded = spec.Guarded
		launchedWrapper = spec.AgentID
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
	fallbackID := ""
	if launchedWrapper != "" {
		if _, lookupErr := exec.LookPath(opts.WrapperArgs[0]); lookupErr == nil {
			fallbackID = launchedWrapper
		}
	}
	opts.AgentID, err = resolveAgentID(opts.AgentID, cfg, fallbackID)
	if err != nil {
		return err
	}
	if err := loop.ValidateAgentID(opts.AgentID); err != nil {
		return err
	}
	if cfg.Remote == nil {
		opts.Vendor = resolveAgentVendor(opts.Vendor, opts.AgentID, cfg)
	}
	if cfg.Remote == nil && opts.Vendor == "" {
		return fmt.Errorf("missing --vendor (recommended values: anthropic, openai, google)")
	}
	if opts.Vendor != "" {
		if err := loop.ValidateAgentID(opts.Vendor); err != nil {
			return fmt.Errorf("--vendor: %w", err)
		}
	}
	if err := refreshWrapperHook(cfg.ControlRepo, launchedWrapper); err != nil {
		return fmt.Errorf("serve: refresh %s hook: %w", launchedWrapper, err)
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
	// lease is the runner's own handle on the serve lease it acquired at
	// startup. The channel below ADOPTS this same lease (its adoption arm), so
	// there is one lease and two names for it: this field is what the runner's
	// shutdown path releases, and what b1_convergence_test.go:67 releases to
	// simulate a killed serve.
	lease *loop.ServeLease
	done  <-chan error
	diag  *runnerDiagnostics

	// channel is the lease/tick handle (internal/op): it holds the serve lease,
	// the no-wake heartbeat template HeartbeatRegistration refreshes every tick
	// (C13), and the 10-minute sweep throttle (C11) whose zero value makes the
	// very first tick due immediately. One handle in place of the three fields
	// those facts used to occupy — the same shape the hub session and the
	// remote serve channel inherit.
	channel *op.Channel

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

// newRunnerRuntime builds the runner's live state from ONE lease value.
//
// The lease has to reach two places — the runtime's own handle, which the
// shutdown path releases, and the op.Channel that ADOPTS it for the tick — and
// they were two independent lines in a struct literal. One of them went missing
// in the seam extraction, so every clean exit released nothing and left
// serve.claim on disk: the lane looked live to the whole pool and the next
// `serve` for that id refused to start. The suite stayed green because every
// runner test builds its runtime through newPollTestRuntime, which set the field
// (codex, PR #148 gate). A constructor taking the lease once makes the pair
// impossible to desynchronize and the field impossible to omit.
func newRunnerRuntime(cfg *loop.Config, opts runnerOptions, cwd string, lease *loop.ServeLease, tmpl loop.Registration) *runnerRuntime {
	return &runnerRuntime{
		cfg:     cfg,
		opts:    opts,
		cwd:     cwd,
		started: time.Now().UTC(),
		lease:   lease,
		channel: op.NewChannel(cfg, op.Context{ActorID: opts.AgentID}, op.ChannelOpts{Lease: lease, HeartbeatTemplate: &tmpl}),
		wakeCh:  make(chan bool, 1),
		stopCh:  make(chan struct{}),
	}
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

	rt := newRunnerRuntime(cfg, opts, cwd, lease, heartbeatTemplate(cfg, opts))
	rt.childPID = cmd.Process.Pid
	rt.cmd = cmd
	rt.ptmx = ptmx
	rt.done = done
	rt.diag = diag
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

// localHostname returns the current host's name, trimmed. Best-effort: an
// os.Hostname() failure returns "".
func localHostname() string {
	host, _ := os.Hostname()
	return strings.TrimSpace(host)
}

// withoutEnv returns env (an os.Environ()-shaped slice) with every entry
// whose key is in keys removed.
func withoutEnv(env []string, keys ...string) []string {
	blocked := make(map[string]bool, len(keys))
	for _, key := range keys {
		blocked[key] = true
	}
	filtered := env[:0]
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && blocked[key] {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

func runnerChildEnv(cfg *loop.Config, opts runnerOptions, serveToken string) []string {
	// Strip inherited authoritative values before conditionally re-adding them:
	// os.Environ() carries THIS process's discovery and guard context whenever a
	// served child launches another lane. A remote child must inherit the hub URL
	// and no loop-dir override; an unguarded child must not inherit the guard bit.
	env := withoutEnv(os.Environ(), "AGENTCHUTE_GUARD", "AGENTCHUTE_CONTROL_REPO", "AGENTCHUTE_LOOP_DIR")
	env = append(env,
		"AGENTCHUTE_AGENT_ID="+opts.AgentID,
		"AGENTCHUTE_RUNNER=1",
		"AGENTCHUTE_RUNNER_PID="+strconv.Itoa(os.Getpid()),
		// Fence the child's sends: send.go passes this serve_token to
		// MintSendStamp so a write from a fenced (reclaimed) agent fails closed
		// (protocol-v2 §6b). Empty when launched without a lease => unfenced.
		"AGENTCHUTE_SERVE_TOKEN="+serveToken,
	)
	if cfg.Remote != nil {
		env = append(env, "AGENTCHUTE_CONTROL_REPO="+cfg.Remote.URL)
	} else {
		env = append(env,
			"AGENTCHUTE_CONTROL_REPO="+cfg.ControlRepo,
			"AGENTCHUTE_LOOP_DIR="+cfg.LoopDir,
		)
	}
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
		AgentID:        opts.AgentID,
		Vendor:         opts.Vendor,
		WorkingRepos:   []string{cfg.ControlRepo},
		Host:           localHostname(),
		HostProvided:   true,
		ServeToken:     serveToken,
		VendorProvided: opts.VendorProvided,
	}, now)
	return err
}

// heartbeatTemplate builds the no-wake registration template HeartbeatRegistration
// refreshes every poll tick (v2.5 plan B1, C13), from the same fields
// registerRunner passes to performRegister for the runner's initial write.
// Built once in runWrapper and reused for the lifetime of the process: it is
// what a swept-then-recreated row comes back as, and what every tick
// re-asserts for AgentID/Vendor/ControlRepo/Host (Body and WorkingRepos come
// from the on-disk row instead — see HeartbeatRegistration).
func heartbeatTemplate(cfg *loop.Config, opts runnerOptions) loop.Registration {
	return loop.Registration{
		AgentID:         opts.AgentID,
		ProtocolVersion: loop.CurrentProtocolVersion,
		Vendor:          opts.Vendor,
		ControlRepo:     cfg.ControlRepo,
		WorkingRepos:    []string{cfg.ControlRepo},
		Host:            localHostname(),
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
	// One tick: fence verify + lease renew, lease-gated heartbeat (self-healing
	// if a sweep removed the row since the last tick), the 10-minute-throttled
	// pool sweep, and the pending-mail count — in that order, behind the seam.
	//
	// ErrFenced means we were RECLAIMED: another serve now owns this id, so we
	// must stop injecting and shut down cleanly rather than become a
	// dup-writer. It is the ONLY hard error a tick returns; every other step
	// failure comes back as a warning and the tick continues, exactly as the
	// runner has always done.
	tick, err := r.channel.Tick(op.TickReq{})
	if err != nil {
		if errors.Is(err, op.ErrFenced) {
			r.bufferFatalf("serve: this agentchute binary was fenced out (update or identity reclaim). Restart this lane: ac serve <wrapper>\n")
			r.requestShutdown(syscall.SIGTERM)
			return
		}
		r.logf("agentchute serve: tick: %v\n", err)
	}
	for _, w := range tick.Warnings {
		r.logf("%s\n", w)
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
	pending := tick.Pending > 0 || tick.Skipped > 0
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
	return op.HasPendingInboxMail(r.cfg, r.opts.AgentID)
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
