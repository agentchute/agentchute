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
	if params.Command == "" && params.Wrapper == "" {
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
	if err := loop.SavePollerHeartbeat(cfg, loop.PollerHeartbeat{
		AgentID:         p.AgentID,
		Method:          "poller-run",
		Host:            localHostname(),
		PID:             os.Getpid(),
		IntervalSeconds: p.Interval,
		LastSeen:        time.Now().UTC(),
		StartedAt:       startedAt,
	}); err != nil {
		return fmt.Errorf("write poller heartbeat: %w", err)
	}
	result, err := computeSelfPollResult(cfg, p.AgentID)
	if err != nil {
		return err
	}
	if !result.ShouldWake {
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

func cmdPollerEnsure(args []string) error {
	fs := flag.NewFlagSet("poller ensure", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var common pollerCommon
	bindPollerCommon(fs, &common)
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
	if reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(common.AgentID)); err == nil && registrationHasReachableWake(reg) {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s has reachable wake target (%s); background poller not required\n", common.AgentID, reg.WakeMethod)
		}
		return nil
	}
	if pane := currentTmuxPane(); pane != "" && tmuxTargetReachable(pane) {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s is in tmux (%s); background poller not required\n", common.AgentID, pane)
		}
		return nil
	} else if pane != "" && !common.Quiet {
		fmt.Printf("poller ensure: %s has unreachable TMUX_PANE=%s; starting background poller\n", common.AgentID, pane)
	}
	if fresh, summary := pollerFreshSummary(cfg, common.AgentID, time.Now().UTC()); fresh {
		if !common.Quiet {
			fmt.Printf("poller ensure: %s already fresh (%s)\n", common.AgentID, summary)
		}
		return nil
	}
	if err := startDetachedPoller(cfg, common); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fresh, summary := pollerFreshSummary(cfg, common.AgentID, time.Now().UTC()); fresh {
			if !common.Quiet {
				fmt.Printf("poller ensure: %s started (%s)\n", common.AgentID, summary)
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("poller started but no fresh heartbeat appeared at %s", cfg.PollerHeartbeatPath(common.AgentID))
}

func startDetachedPoller(cfg *loop.Config, p pollerCommon) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	stateDir := cfg.AgentStateDir(p.AgentID)
	if err := loop.EnsurePrivateDir(stateDir); err != nil {
		return err
	}
	logPath := filepath.Join(stateDir, "poller.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
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
		return err
	}
	return cmd.Process.Release()
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

Recipient-side poller for agents without a reachable wake adapter. Senders
only deliver to the inbox; recipients prove they are reading by keeping a
fresh state/<agent>/poller.json heartbeat.

Subcommands:
  run      long-lived poll loop; launches the wrapper when self-poll finds work
  ensure   no-op in a reachable tmux pane; otherwise start poller run if stale
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

Extra flags:
  poller run --once     run one tick and exit
  poller status --json  structured status output
`)
}
