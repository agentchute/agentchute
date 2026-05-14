package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

type watchdogOptions struct {
	AgentID             string
	LocalHost           string // for cross-host proactive skip (§10.5); empty = no filter
	Interval            time.Duration
	StaleThreshold      time.Duration
	MessageAgeThreshold time.Duration
	Now                 func() time.Time
}

func cmdWatchdog(args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID string
	var controlRepo string
	var loopDir string
	var intervalSeconds int
	var staleSeconds int
	var messageAgeSeconds int
	var once bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.IntVar(&intervalSeconds, "interval", 60, "poll cadence in seconds")
	fs.IntVar(&staleSeconds, "stale-threshold", 300, "last_seen freshness threshold in seconds")
	fs.IntVar(&messageAgeSeconds, "message-age-threshold", 90, "minimum unread message age before poking, in seconds")
	fs.BoolVar(&once, "once", false, "run one cycle and exit")

	if err := fs.Parse(args); err != nil {
		return watchdogUsage(err)
	}
	if fs.NArg() != 0 {
		return watchdogUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if intervalSeconds <= 0 || staleSeconds < 0 || messageAgeSeconds < 0 {
		return watchdogUsage(fmt.Errorf("interval must be positive; thresholds must be non-negative"))
	}

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID == "" {
		return fmt.Errorf("missing watchdog identity; pass --as or set AGENTCHUTE_AGENT_ID")
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{
		ControlRepoFlag: controlRepo,
		LoopDirFlag:     loopDir,
		Cwd:             cwd,
		EnvControlRepo:  os.Getenv("AGENTCHUTE_CONTROL_REPO"),
		EnvLoopDir:      os.Getenv("AGENTCHUTE_LOOP_DIR"),
	})
	if err != nil {
		return err
	}

	localHost, _ := os.Hostname() // empty on error is acceptable: skips host filter

	opts := watchdogOptions{
		AgentID:             agentID,
		LocalHost:           localHost,
		Interval:            time.Duration(intervalSeconds) * time.Second,
		StaleThreshold:      time.Duration(staleSeconds) * time.Second,
		MessageAgeThreshold: time.Duration(messageAgeSeconds) * time.Second,
		Now:                 time.Now,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if once {
		return runWatchdogCycle(ctx, cfg, opts)
	}

	for {
		if err := runWatchdogCycle(ctx, cfg, opts); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.Interval):
		}
	}
}

func watchdogUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute watchdog --as <agent-id> [--control-repo <path>] [--loop-dir <path>] [--interval 60] [--stale-threshold 300] [--message-age-threshold 90] [--once]", err)
}

func runWatchdogCycle(ctx context.Context, cfg *loop.Config, opts watchdogOptions) error {
	now := opts.Now().UTC()

	selfPath := cfg.AgentRegistrationPath(opts.AgentID)
	if _, err := os.Stat(selfPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("watchdog registration %q does not exist; run agentchute register first", selfPath)
		}
		return fmt.Errorf("stat watchdog registration: %w", err)
	}
	if err := loop.UpdateLastSeen(selfPath, now); err != nil {
		return fmt.Errorf("update watchdog last_seen: %w", err)
	}

	return runLivenessSweep(ctx, cfg, opts, now)
}

// runLivenessSweep walks every peer registration in the agents dir and
// applies the §10.4 watchdog algorithm. Shared between `agentchute watchdog`
// (the dedicated daemon, §10.1) and `agentchute check` (cooperative waking
// per §10.5). Per-peer errors are logged and skipped — one malformed
// registration MUST NOT abort the sweep.
//
// Cross-host proactive skip: if opts.LocalHost is non-empty and the peer
// declares a different host, the poke is skipped silently — wake adapters
// are machine-local, the message is already on the shared FS, and the
// peer's local environment handles wake (§10.5, §12).
func runLivenessSweep(ctx context.Context, cfg *loop.Config, opts watchdogOptions, now time.Time) error {
	localHost := strings.TrimSpace(opts.LocalHost)

	regs, errs := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	for _, e := range errs {
		logWatchdogEvent(cfg, now, "%s unreadable: %v", filepath.Base(e.Path), e.Err)
	}
	for _, reg := range regs {
		if reg.AgentID == opts.AgentID {
			continue
		}
		if localHost != "" && strings.TrimSpace(reg.Host) != "" && reg.Host != localHost {
			// Cross-host peer: their host is responsible for waking them.
			// Silent skip — this is expected steady-state in multi-host pools.
			continue
		}
		if err := watchdogAgentCycle(ctx, cfg, reg, now, opts); err != nil {
			logWatchdogEvent(cfg, now, "%s error: %v", reg.AgentID, err)
		}
	}
	return nil
}

func watchdogAgentCycle(ctx context.Context, cfg *loop.Config, reg *loop.Registration, now time.Time, opts watchdogOptions) error {
	msgs, err := loop.ListInboxMessages(cfg.AgentInboxDir(reg.AgentID))
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}

	oldest := msgs[0]
	msgAge := now.Sub(oldest.Timestamp)
	if msgAge < 0 {
		msgAge = 0
	}

	if reg.RestartAt != nil && reg.RestartAt.After(now) {
		logWatchdogEvent(cfg, now, "deferring %s until %s", reg.AgentID, reg.RestartAt.UTC().Format(time.RFC3339))
		return nil
	}
	if reg.Status == loop.StatusExhausted && reg.RestartAt == nil {
		logWatchdogEvent(cfg, now, "deferring %s indefinitely (status=exhausted restart_at=missing)", reg.AgentID)
		return nil
	}
	if reg.Status == loop.StatusOffline && reg.RestartAt == nil {
		logWatchdogEvent(cfg, now, "skipping %s (status=offline restart_at=missing)", reg.AgentID)
		return nil
	}

	if !reg.LastSeen.IsZero() {
		lastSeenAge := now.Sub(reg.LastSeen)
		if lastSeenAge < 0 {
			lastSeenAge = 0
		}
		if lastSeenAge < opts.StaleThreshold {
			logWatchdogEvent(cfg, now, "%s last_seen fresh (%s); skipping", reg.AgentID, lastSeenAge.Round(time.Second))
			return nil
		}
	}
	if msgAge < opts.MessageAgeThreshold {
		logWatchdogEvent(cfg, now, "%s oldest msg age %s below threshold; skipping", reg.AgentID, msgAge.Round(time.Second))
		return nil
	}
	if !reg.IsPokable() {
		if strings.TrimSpace(reg.WakeMethod) == "" {
			logWatchdogEvent(cfg, now, "no wake_method for %s; skipping poke", reg.AgentID)
		} else {
			logWatchdogEvent(cfg, now, "no wake_target for %s; skipping poke", reg.AgentID)
		}
		return nil
	}

	if err := loop.PokeWakeTargetContext(ctx, reg.WakeMethod, reg.WakeTarget); err != nil {
		logWatchdogEvent(cfg, now, "poke %s failed: %v", reg.AgentID, err)
		return nil
	}
	logWatchdogEvent(cfg, now, "poked %s (oldest msg age %s)", reg.AgentID, msgAge.Round(time.Second))
	return nil
}

// logWatchdogEvent writes one line to .<vendor>/loop/watchdog.log. The leading
// verbs are operator-facing surface (operators grep these): poked, deferring,
// skipping, error. AGENTCHUTE.md §10.7 documents the format. Renames need a
// release note; new verbs are additive.
func logWatchdogEvent(cfg *loop.Config, t time.Time, format string, args ...interface{}) {
	if err := appendWatchdogLog(cfg, t, format, args...); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write watchdog log: %v\n", err)
	}
}

func appendWatchdogLog(cfg *loop.Config, t time.Time, format string, args ...interface{}) error {
	path := cfg.WatchdogLogPath()
	if err := loop.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	line := fmt.Sprintf(format, args...)
	_, err = fmt.Fprintf(f, "%s %s\n", t.UTC().Format(time.RFC3339), line)
	return err
}
