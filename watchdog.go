package main

import (
	"context"
	"errors"
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

	// Self-registration upkeep is best-effort: a setup/update reset race can
	// delete the watchdog's OWN registration (or make UpdateLastSeen fail)
	// between cycles. Returning a hard error here would permanently kill the
	// daemon loop and stop ALL peer waking — the opposite of what the watchdog
	// is for. Log-and-continue (mirroring the per-peer error handling in
	// runLivenessSweep) so a transient self-reg gap heals on the next cycle
	// while peers keep getting swept.
	selfPath := cfg.AgentRegistrationPath(opts.AgentID)
	if _, err := os.Stat(selfPath); err != nil {
		if os.IsNotExist(err) {
			logWatchdogEvent(cfg, now, "self-registration %q missing (reset race?); skipping last_seen update, continuing sweep", filepath.Base(selfPath))
		} else {
			logWatchdogEvent(cfg, now, "stat self-registration failed: %v; continuing sweep", err)
		}
	} else if err := loop.UpdateLastSeen(cfg, opts.AgentID, now); err != nil {
		logWatchdogEvent(cfg, now, "update self last_seen failed: %v; continuing sweep", err)
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
		// runner supervision probe (C lane): log unreachable runner sockets for
		// observability and self-heal awareness. (Self-heal on next attended
		// shim start; no auto poke here.)
		if reg.WakeMethod == loop.RunnerWakeMethod && reg.WakeTarget != "" {
			// Recipient-bound reachability: never dial a runner socket the peer
			// does not own (a hostile reg could name unix:/tmp/evil.sock). An
			// unowned target is reported unreachable without a dial.
			if !runnerReachableForRecipient(cfg, reg, 300*time.Millisecond) {
				logWatchdogEvent(cfg, now, "runner socket for %s unreachable (dead runner or stale reg); heals on next shim start", reg.AgentID)
			}
		}
		if err := watchdogAgentCycle(ctx, cfg, reg, now, opts); err != nil {
			logWatchdogEvent(cfg, now, "%s error: %v", reg.AgentID, err)
		}
	}
	return nil
}

func watchdogAgentCycle(ctx context.Context, cfg *loop.Config, reg *loop.Registration, now time.Time, opts watchdogOptions) error {
	inboxDir := cfg.AgentInboxDir(reg.AgentID)
	// Include SKIPPED (malformed/unparseable) files: gate blocks a peer until
	// `check` quarantines such a file, so an inbox holding ONLY malformed mail
	// is still wake-worthy work. ListInboxMessages alone treats it as empty and
	// silently declines to poke the blocked peer.
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		// Peer's inbox dir is missing: registered but never booted on this
		// host, or partially-installed setup. Skip this peer for this cycle
		// rather than failing the whole pass (codex review on the v0.2.1
		// ErrInboxMissing sentinel).
		if errors.Is(err, loop.ErrInboxMissing) {
			return nil
		}
		return err
	}
	if len(msgs) == 0 && len(skipped) == 0 {
		return nil
	}

	// Wake-age is derived from filesystem ARRIVAL time (mtime), NOT the
	// sender-encoded filename timestamp. A future/skewed filename timestamp
	// would make now.Sub(timestamp) negative, clamp to zero, and suppress the
	// poke indefinitely; and a malformed file has no parseable timestamp at
	// all. mtime reflects when the file actually landed on this host, so
	// back-dated mail still ages past the threshold and pokes.
	msgAge := oldestInboxArrivalAge(inboxDir, msgs, skipped, now)

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

	// PokeRegistration refuses an unowned runner socket (recipient-binding)
	// without dialing it; non-runner methods poke as before.
	if err := loop.PokeRegistration(ctx, cfg, reg); err != nil {
		logWatchdogEvent(cfg, now, "poke %s failed: %v", reg.AgentID, err)
		return nil
	}
	logWatchdogEvent(cfg, now, "poked %s (oldest msg age %s)", reg.AgentID, msgAge.Round(time.Second))
	return nil
}

// oldestInboxArrivalAge returns now minus the OLDEST filesystem mtime among the
// peer's observable inbox files (parsed messages and skipped/malformed files
// alike). mtime is the file's arrival observation on this host, so it is immune
// to a back-dated (future/skewed) sender-encoded filename timestamp that would
// otherwise clamp a sender-timestamp-based age to zero and silence the poke.
// A file whose mtime cannot be stat'd (raced away, permission) is ignored; if
// none can be stat'd the age is zero (nothing observably aged).
func oldestInboxArrivalAge(inboxDir string, msgs []loop.Message, skipped []string, now time.Time) time.Duration {
	var oldest time.Time
	consider := func(name string) {
		info, err := os.Stat(filepath.Join(inboxDir, name))
		if err != nil {
			return
		}
		mt := info.ModTime()
		if oldest.IsZero() || mt.Before(oldest) {
			oldest = mt
		}
	}
	for _, m := range msgs {
		consider(m.Filename)
	}
	for _, name := range skipped {
		consider(name)
	}
	if oldest.IsZero() {
		return 0
	}
	age := now.Sub(oldest)
	if age < 0 {
		// Future mtime (clock skew on disk): treat as just-arrived rather than
		// negative — never let a skewed timestamp suppress the poke.
		age = 0
	}
	return age
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
