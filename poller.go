package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func cmdPoller(args []string) error {
	if len(args) == 0 {
		return pollerUsage(fmt.Errorf("missing subcommand"))
	}
	switch args[0] {
	case "run":
		return cmdPollerRun(args[1:])
	case "ensure":
		return cmdPollerEnsure(args[1:])
	case "status":
		return cmdPollerStatus(args[1:])
	case "-h", "--help", "help":
		fmt.Print(pollerHelp())
		return nil
	default:
		return pollerUsage(fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

type pollerCommon struct {
	AgentID     string
	Vendor      string
	ControlRepo string
	LoopDir     string
	Repo        string
	Command     string
	Interval    int
	Quiet       bool
	Launch      bool
}

func bindPollerCommon(fs *flag.FlagSet, p *pollerCommon) {
	fs.StringVar(&p.AgentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&p.Vendor, "vendor", "", "wrapper vendor (inferred for known agents)")
	fs.StringVar(&p.ControlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&p.LoopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")
	fs.StringVar(&p.Repo, "repo", "", "working directory for the poller (default: cwd)")
	fs.StringVar(&p.Command, "command", "", "override the full wrapper invocation")
	fs.IntVar(&p.Interval, "interval", loop.DefaultPollerIntervalSeconds, "poll interval in seconds")
	fs.BoolVar(&p.Quiet, "quiet", false, "suppress status text")
}

func resolvePollerCommon(p *pollerCommon) (*loop.Config, error) {
	if p.Repo == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		p.Repo = cwd
	}
	abs, err := filepath.Abs(p.Repo)
	if err != nil {
		return nil, err
	}
	p.Repo = abs

	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: p.ControlRepo,
		LoopDirFlag:     p.LoopDir,
		Cwd:             p.Repo,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return nil, err
	}

	p.AgentID, err = resolveAgentID(p.AgentID, p.Vendor, cfg)
	if err != nil {
		return nil, err
	}
	if err := loop.ValidateAgentID(p.AgentID); err != nil {
		return nil, err
	}
	p.Vendor = resolveAgentVendor(p.Vendor, p.AgentID, cfg)
	if p.Vendor != "" {
		if err := loop.ValidateAgentID(p.Vendor); err != nil {
			return nil, fmt.Errorf("--vendor %q must be a shell-safe slug (same rule as agent_id): %w", p.Vendor, err)
		}
	}
	if p.Interval < loop.MinPollerIntervalSeconds {
		return nil, fmt.Errorf("--interval must be >= %d seconds", loop.MinPollerIntervalSeconds)
	}

	return cfg, nil
}

func serviceParamsForPoller(p pollerCommon) serviceParams {
	sp := serviceParams{
		Kind:     serviceKindScript,
		AgentID:  p.AgentID,
		Vendor:   p.Vendor,
		Command:  strings.TrimSpace(p.Command),
		Interval: p.Interval,
		Repo:     p.Repo,
		Launch:   p.Launch,
	}
	if preset, ok := vendorPresets[p.AgentID]; ok {
		if sp.Vendor == "" {
			sp.Vendor = preset.Vendor
		}
		if sp.Wrapper == "" {
			sp.Wrapper = preset.Wrapper
		}
	}
	if sp.Wrapper == "" {
		sp.Wrapper = wrapperForVendor(sp.Vendor)
	}
	return sp
}

func cmdPollerRun(args []string) error {
	fs := flag.NewFlagSet("poller run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var common pollerCommon
	var once bool
	bindPollerCommon(fs, &common)
	fs.BoolVar(&once, "once", false, "run one poll tick and exit")
	fs.BoolVar(&common.Launch, "launch", false, "launch the wrapper when work is pending; default is heartbeat-only")
	if err := fs.Parse(args); err != nil {
		return pollerUsage(err)
	}
	if fs.NArg() != 0 {
		return pollerUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	cfg, err := resolvePollerCommon(&common)
	if err != nil {
		return err
	}
	params := serviceParamsForPoller(common)
	if params.Launch && params.Command == "" && params.Wrapper == "" {
		return fmt.Errorf("cannot infer wrapper command for agent %q; pass --command", common.AgentID)
	}

	rt := &pollerRuntime{}
	startedAt := time.Now().UTC()
	for {
		if err := pollerTick(cfg, params, rt, startedAt); err != nil && !common.Quiet {
			fmt.Fprintf(os.Stderr, "agentchute poller: %v\n", err)
		}
		if once {
			return nil
		}
		time.Sleep(time.Duration(common.Interval) * time.Second)
	}
}

type pollerRuntime struct {
	running *exec.Cmd
	done    chan error
}

func (r *pollerRuntime) reap() {
	if r == nil || r.done == nil {
		return
	}
	select {
	case <-r.done:
		r.running = nil
		r.done = nil
	default:
	}
}

func pollerTick(cfg *loop.Config, p serviceParams, rt *pollerRuntime, startedAt time.Time) error {
	// WI-4 Fix 2: refresh the heartbeat only AFTER a successful poll
	// computation. Previously the heartbeat was stamped first, so a poller
	// stuck/erroring inside computeSelfPollResult kept a "fresh" heartbeat
	// while consuming no mail — liveness could not tell "beating but failing"
	// from "healthy". On a compute error we record last_error WITHOUT
	// refreshing last_seen, letting the heartbeat age out while preserving the
	// diagnostic.
	result, err := computeSelfPollResult(cfg, p.AgentID)
	if err != nil {
		recordPollerHeartbeatError(cfg, p.AgentID, err)
		return err
	}
	if err := loop.SavePollerHeartbeat(cfg, loop.PollerHeartbeat{
		AgentID:         p.AgentID,
		Method:          "poller-run",
		Host:            localHostname(),
		PID:             os.Getpid(),
		IntervalSeconds: p.Interval,
		LaunchEnabled:   p.Launch,
		LastSeen:        time.Now().UTC(),
		StartedAt:       startedAt,
	}); err != nil {
		return fmt.Errorf("write poller heartbeat: %w", err)
	}
	if !result.ShouldWake {
		if rt != nil {
			rt.reap()
		}
		return nil
	}
	if !p.Launch {
		if rt != nil {
			rt.reap()
		}
		return nil
	}
	if rt != nil {
		rt.reap()
		if rt.running != nil {
			return nil
		}
	}
	cmd := exec.Command("sh", "-c", wrapperInvocation(p))
	cmd.Dir = p.Repo
	cmd.Env = pollerWrapperEnv(os.Environ(), cfg, p.AgentID)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch wrapper: %w", err)
	}
	if rt == nil {
		return cmd.Wait()
	}
	rt.running = cmd
	rt.done = make(chan error, 1)
	go func() { rt.done <- cmd.Wait() }()
	return nil
}

// recordPollerHeartbeatError stamps the most recent poll-computation failure
// onto the existing heartbeat WITHOUT advancing last_seen, so liveness sees a
// "beating but failing" poller age out while keeping the diagnostic. If no
// prior heartbeat exists there is nothing to age — a never-started poller has
// no liveness claim to preserve, so we skip. Best-effort: a write failure here
// is swallowed (the original poll error is what the caller propagates).
func recordPollerHeartbeatError(cfg *loop.Config, agentID string, pollErr error) {
	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		return
	}
	hb.LastError = pollErr.Error()
	// Preserve LastSeen as-is so PollerFreshness keeps aging; SavePollerHeartbeat
	// would only re-stamp it if zero, which it is not here.
	_ = loop.SavePollerHeartbeat(cfg, *hb)
}

func pollerWrapperEnv(env []string, cfg *loop.Config, agentID string) []string {
	env = withoutEnv(env, "AGENTCHUTE_AGENT_ID", "AGENTCHUTE_CONTROL_REPO", "AGENTCHUTE_LOOP_DIR", "AGENTCHUTE_SHIM_BYPASS")
	return append(env,
		"AGENTCHUTE_AGENT_ID="+agentID,
		"AGENTCHUTE_CONTROL_REPO="+cfg.ControlRepo,
		"AGENTCHUTE_LOOP_DIR="+cfg.LoopDir,
		"AGENTCHUTE_SHIM_BYPASS=1",
	)
}

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

func cmdPollerEnsure(args []string) error {
	fs := flag.NewFlagSet("poller ensure", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var common pollerCommon
	bindPollerCommon(fs, &common)
	fs.BoolVar(&common.Launch, "launch", false, "start a poller that launches the wrapper when work is pending; default is heartbeat-only")
	if err := fs.Parse(args); err != nil {
		return pollerUsage(err)
	}
	if fs.NArg() != 0 {
		return pollerUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	cfg, err := resolvePollerCommon(&common)
	if err != nil {
		return err
	}
	if reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(common.AgentID)); err == nil && registrationHasReachableWake(cfg, reg) {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s has reachable wake target (%s); poller not required\n", common.AgentID, reg.WakeMethod)
		}
		return nil
	}
	if pane := currentTmuxPane(); pane != "" && tmuxTargetReachable(pane) {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s is in tmux (%s); poller not required\n", common.AgentID, pane)
		}
		return nil
	} else if pane != "" && !common.Quiet {
		fmt.Printf("poller ensure: %s has unreachable TMUX_PANE=%s; starting heartbeat poller\n", common.AgentID, pane)
	}
	if !common.Launch {
		if session, err := loop.LoadActiveSession(cfg, common.AgentID); err == nil && activeSessionAlive(session) {
			if !common.Quiet {
				fmt.Printf("poller ensure: %s has active wrapper session (pid=%d); poller not required\n", common.AgentID, session.PID)
			}
			return nil
		}
	}
	if fresh, summary := pollerFreshSummary(cfg, common.AgentID, time.Now().UTC()); fresh {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s already fresh (%s)\n", common.AgentID, summary)
		}
		return nil
	}
	pollerPID, err := startDetachedPoller(cfg, common)
	if err != nil {
		return err
	}
	// WI-4 Fix 5: the detached poller's first tick runs computeSelfPollResult
	// (an inbox scan) BEFORE writing the heartbeat (Fix 2 reordering), so on a
	// slow-disk host the first heartbeat can lag the spawn by more than the
	// old 2s budget — yielding a spurious "no fresh heartbeat appeared". Wait
	// longer, and if the deadline lapses but the spawned process is still
	// alive, treat it as "starting" rather than a hard failure (a dead PID is
	// the genuine error).
	deadline := time.Now().Add(pollerFirstHeartbeatDeadline)
	for time.Now().Before(deadline) {
		if fresh, summary := pollerFreshSummary(cfg, common.AgentID, time.Now().UTC()); fresh {
			if !common.Quiet {
				fmt.Printf("poller ensure: %s started (%s)\n", common.AgentID, summary)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pollerPID > 0 && processAlive(pollerPID) {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s poller started (pid=%d) but first heartbeat is still pending (slow disk?); it should appear shortly at %s\n", common.AgentID, pollerPID, cfg.PollerHeartbeatPath(common.AgentID))
		}
		return nil
	}
	return fmt.Errorf("poller started but no fresh heartbeat appeared at %s (process pid=%d not alive)", cfg.PollerHeartbeatPath(common.AgentID), pollerPID)
}

// pollerFirstHeartbeatDeadline bounds how long `poller ensure` waits for a
// freshly-spawned detached poller's first heartbeat. With Fix 2 the first tick
// scans the inbox before writing the heartbeat, so this is generous enough to
// absorb a slow-disk first scan without spuriously failing.
const pollerFirstHeartbeatDeadline = 5 * time.Second

// startDetachedPoller spawns the detached poller and returns its PID so the
// caller can fall back to a liveness check if the first heartbeat is slow.
func startDetachedPoller(cfg *loop.Config, p pollerCommon) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	stateDir := cfg.AgentStateDir(p.AgentID)
	if err := loop.EnsurePrivateDir(stateDir); err != nil {
		return 0, err
	}
	logPath := filepath.Join(stateDir, "poller.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	args := []string{
		"poller", "run",
		"--as", p.AgentID,
		"--interval", fmt.Sprint(p.Interval),
		"--repo", p.Repo,
		"--control-repo", cfg.ControlRepo,
		"--loop-dir", cfg.LoopDir,
		"--quiet",
	}
	if p.Vendor != "" {
		args = append(args, "--vendor", p.Vendor)
	}
	if p.Launch {
		args = append(args, "--launch")
	}
	if strings.TrimSpace(p.Command) != "" {
		args = append(args, "--command", p.Command)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = p.Repo
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return pid, err
	}
	return pid, nil
}

func cmdPollerStatus(args []string) error {
	fs := flag.NewFlagSet("poller status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var common pollerCommon
	var jsonOut bool
	bindPollerCommon(fs, &common)
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	if err := fs.Parse(args); err != nil {
		return pollerUsage(err)
	}
	if fs.NArg() != 0 {
		return pollerUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	cfg, err := resolvePollerCommon(&common)
	if err != nil {
		return err
	}
	hb, err := loop.LoadPollerHeartbeat(cfg, common.AgentID)
	now := time.Now().UTC()
	status := pollerStatus{Agent: common.AgentID, Fresh: false}
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			status.Error = err.Error()
		} else {
			status.Error = "missing poller heartbeat"
		}
	} else {
		fresh, age, threshold := loop.PollerFreshness(hb, now)
		status.Fresh = fresh
		status.Method = hb.Method
		status.Host = hb.Host
		status.PID = hb.PID
		status.IntervalSeconds = hb.IntervalSeconds
		status.LastSeen = hb.LastSeen.UTC().Format(time.RFC3339)
		status.Age = age.Round(time.Second).String()
		status.Threshold = threshold.String()
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			return err
		}
	} else if status.Fresh {
		fmt.Printf("poller status: %s fresh (method=%s host=%s age=%s threshold=%s)\n", status.Agent, status.Method, status.Host, status.Age, status.Threshold)
	} else {
		fmt.Printf("poller status: %s stale (%s)\n", status.Agent, status.Error)
	}
	if !status.Fresh {
		return errBlocked
	}
	return nil
}

type pollerStatus struct {
	Agent           string `json:"agent"`
	Fresh           bool   `json:"fresh"`
	Method          string `json:"method,omitempty"`
	Host            string `json:"host,omitempty"`
	PID             int    `json:"pid,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	LastSeen        string `json:"last_seen,omitempty"`
	Age             string `json:"age,omitempty"`
	Threshold       string `json:"threshold,omitempty"`
	Error           string `json:"error,omitempty"`
}

func pollerFreshSummary(cfg *loop.Config, agentID string, now time.Time) (bool, string) {
	hb, err := loop.LoadPollerHeartbeat(cfg, agentID)
	if err != nil {
		return false, "missing heartbeat"
	}
	fresh, age, threshold := loop.PollerFreshness(hb, now)
	summary := fmt.Sprintf("method=%s host=%s age=%s threshold=%s", hb.Method, hb.Host, age.Round(time.Second), threshold)
	return fresh, summary
}

func localHostname() string {
	host, _ := os.Hostname()
	return strings.TrimSpace(host)
}

func pollerUsage(err error) error {
	if err == flag.ErrHelp {
		return pollerHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, pollerHelp())
}

func pollerHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, pollerHelp())
}

func pollerHelp() string {
	return strings.TrimSpace(`
Usage: agentchute poller <run|ensure|status> [flags]

Recipient-side poller for agents without a reachable wake adapter. By default
it is heartbeat-only: it proves this host can see the inbox but does not launch
or consume wrapper mail. Pass --launch when an off-turn wrapper launch should
process pending mail.

Subcommands:
  run      long-lived poll loop; with --launch, launches the wrapper on work
  ensure   no-op in a reachable tmux pane; otherwise start heartbeat poller
  status   report whether the poller heartbeat is fresh

Common flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <v>          wrapper vendor (inferred for claude-code/codex/gemini-cli)
  --interval <n>        poll interval in seconds (default 30, min 5)
  --repo <path>         working directory for the poller (default: cwd)
  --command <cmd>       override the full wrapper invocation
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --quiet               suppress status text
  --launch              launch the wrapper when work is pending (run/ensure)

Extra flags:
  poller run --once     run one tick and exit
  poller status --json  structured status output
`)
}
