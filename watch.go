package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Default watch loop cadence. Configurable via --interval. 10s is short
// enough for the no-tmux fallback (the spec's Test 4 motivation) and long
// enough that the polling cost is negligible.
const defaultWatchInterval = 10 * time.Second

// cmdWatch is the recipient-side persistent watcher (spec rev3 §A.6).
// Polls own inbox; emits configurable actions on each *new* message.
// Non-consuming: never archives, quarantines, or pokes peers.
//
// Design notes (codex brainstorm + claude-code's read):
//   - Polling only, stdlib only. No fsnotify dependency.
//   - Dedupe by filename (the §6.1 delivery-identity tuple). Frontmatter
//     `message_id` is for reply chains, not delivery uniqueness (§6.4),
//     so two files with the same message_id fire independently.
//   - Startup sweep captures current inbox as "already seen" so the watcher
//     fires only on arrivals AFTER it started.
//   - --exec is operator-owned automation. Startup stderr warning so the
//     operator notices when it's enabled. Env vars passed to the child:
//     AGENTCHUTE_MSG_ID, AGENTCHUTE_FROM, AGENTCHUTE_TASK only — no body.
//   - At least one of --notify / --print / --exec must be set.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, controlRepo, loopDir, execCmd string
	var notify, printOnly bool
	var interval time.Duration
	fs.StringVar(&agentID, "as", "", "agent id to watch (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&notify, "notify", false, "fire an OS notification on each new message (osascript on macOS, notify-send on Linux)")
	fs.BoolVar(&printOnly, "print", false, "print a one-line summary to stdout on each new message")
	fs.StringVar(&execCmd, "exec", "", "shell command to run on each new message; receives AGENTCHUTE_MSG_ID / AGENTCHUTE_FROM / AGENTCHUTE_TASK as env vars (no body)")
	fs.DurationVar(&interval, "interval", defaultWatchInterval, "poll interval (e.g. 5s, 30s, 1m)")

	if err := fs.Parse(args); err != nil {
		return watchUsage(err)
	}
	if fs.NArg() != 0 {
		return watchUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	if !notify && !printOnly && execCmd == "" {
		return watchUsage(fmt.Errorf("watch needs at least one action: pass --notify, --print, or --exec"))
	}
	if interval < time.Second {
		return fmt.Errorf("--interval must be at least 1s, got %s", interval)
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
	agentID, err = resolveAgentID(agentID, vendor, cfg)
	if err != nil {
		return err
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}

	if execCmd != "" {
		fmt.Fprintf(os.Stderr, "warning: --exec is enabled; commands will run as %q with AGENTCHUTE_MSG_ID / _FROM / _TASK env vars on every new message\n", execCmd)
	}

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): watch is an
	// active agent surface — it polls THIS agent's inbox and fires
	// notifications/exec on its behalf. Refuse for an unregistered
	// agent rather than silently polling a non-existent directory.
	if _, err := os.Stat(cfg.AgentRegistrationPath(agentID)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("agent %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", agentID, agentID)
		}
		return fmt.Errorf("stat own registration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := watchOptions{
		Cfg:     cfg,
		AgentID: agentID,
		Notify:  notify,
		Print:   printOnly,
		ExecCmd: execCmd,
	}
	return runWatchLoop(ctx, opts, interval)
}

// watchOptions captures the surface of cmdWatch's behavior so the inner
// loop can be tested without spawning a real process or sending real
// notifications.
type watchOptions struct {
	Cfg     *loop.Config
	AgentID string
	Notify  bool
	Print   bool
	ExecCmd string

	// Hooks for testing. Production callers leave these nil; the loop
	// then dispatches to real implementations.
	NotifyFn func(title, message string) error
	PrintFn  func(title, message string)
	ExecFn   func(cmd string, env map[string]string) error
}

func (o watchOptions) notify(title, message string) error {
	if o.NotifyFn != nil {
		return o.NotifyFn(title, message)
	}
	return osNotify(title, message)
}

func (o watchOptions) print(title, message string) {
	if o.PrintFn != nil {
		o.PrintFn(title, message)
		return
	}
	fmt.Printf("[%s] %s — %s\n", time.Now().UTC().Format(time.RFC3339), title, message)
}

func (o watchOptions) exec(cmd string, env map[string]string) error {
	if o.ExecFn != nil {
		return o.ExecFn(cmd, env)
	}
	// Operator-trusted automation: --exec value comes from the operator's
	// command line, not from message content. Env vars are passed as env,
	// never interpolated into the cmd string. /bin/sh -c is the spec's
	// stated invocation.
	c := osexec.Command("/bin/sh", "-c", cmd)
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// runWatchLoop is the testable inner loop. Captures the current inbox
// as "seen", then polls at interval. Each new message (identified by
// filename) fires the configured actions exactly once and is added to
// the seen set.
func runWatchLoop(ctx context.Context, opts watchOptions, interval time.Duration) error {
	inboxDir := opts.Cfg.AgentInboxDir(opts.AgentID)

	seen, err := snapshotInbox(inboxDir)
	if err != nil {
		return fmt.Errorf("initial inbox snapshot: %w", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		entries, err := scanInbox(inboxDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: inbox scan failed: %v\n", err)
			continue
		}
		for _, e := range entries {
			if _, already := seen[e.Key]; already {
				continue
			}
			seen[e.Key] = struct{}{}
			fireActions(opts, e)
		}
	}
}

// watchEntry is the unit of new-mail dedup in the watch loop. Key is
// always the filename (the §6.1 identity tuple) — two distinct
// deliveries must dedupe independently even when they happen to share a
// frontmatter message_id, per AGENTCHUTE.md §6.4 (message_id is for
// reply chains, not delivery uniqueness). MessageID is surfaced
// separately to the --exec env var AGENTCHUTE_MSG_ID when present.
// (codex review on d73d4dd; same class as the v0.1.1 ledger bug.)
type watchEntry struct {
	Key       string // filename (delivery identity)
	MessageID string // optional frontmatter message_id for AGENTCHUTE_MSG_ID env
	From      string
	Task      string
	Filename  string
	Timestamp time.Time
}

// snapshotInbox returns the set of currently-present message keys without
// firing any actions.
func snapshotInbox(inboxDir string) (map[string]struct{}, error) {
	seen := make(map[string]struct{})
	entries, err := scanInbox(inboxDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		seen[e.Key] = struct{}{}
	}
	return seen, nil
}

// scanInbox lists the recipient's inbox and returns one watchEntry per
// valid message file. Malformed files are skipped silently (doctor /
// check surface them — watch is non-consuming).
func scanInbox(inboxDir string) ([]watchEntry, error) {
	msgs, _, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		return nil, err
	}
	out := make([]watchEntry, 0, len(msgs))
	for _, msg := range msgs {
		entry := watchEntry{
			Key:       msg.Filename, // delivery identity per §6.1
			From:      msg.Sender,
			Filename:  msg.Filename,
			Timestamp: msg.Timestamp.UTC(),
		}
		fm, _, ferr := readFrontmatter(msg.Path)
		if ferr == nil {
			// Track frontmatter message_id for AGENTCHUTE_MSG_ID env var
			// when --exec is enabled. Never used for dedup.
			entry.MessageID = strings.TrimSpace(fm["message_id"])
			entry.Task = fm["task"]
		}
		out = append(out, entry)
	}
	return out, nil
}

func fireActions(opts watchOptions, e watchEntry) {
	title := fmt.Sprintf("agentchute: new message for %s", opts.AgentID)
	summary := fmt.Sprintf("from %s", e.From)
	if e.Task != "" {
		summary += " — " + e.Task
	}
	if opts.Notify {
		if err := opts.notify(title, summary); err != nil {
			fmt.Fprintf(os.Stderr, "warning: notify failed: %v\n", err)
		}
	}
	if opts.Print {
		opts.print(title, summary)
	}
	if opts.ExecCmd != "" {
		// AGENTCHUTE_MSG_ID is the frontmatter message_id when the
		// message carries one (matches the ledger's identity convention
		// for reply threading), filename otherwise. Dedup uses filename
		// regardless — this is only the env-var presentation surface.
		msgID := e.MessageID
		if msgID == "" {
			msgID = e.Filename
		}
		env := map[string]string{
			"AGENTCHUTE_MSG_ID": msgID,
			"AGENTCHUTE_FROM":   e.From,
			"AGENTCHUTE_TASK":   e.Task,
		}
		if err := opts.exec(opts.ExecCmd, env); err != nil {
			fmt.Fprintf(os.Stderr, "warning: --exec command failed for %s: %v\n", e.Filename, err)
		}
	}
}

// osNotify dispatches an OS-level notification via the platform adapter.
// macOS: osascript. Linux: notify-send. Other platforms: best-effort
// failure (returns an error the caller logs as a warning).
//
// These are HUMAN-RELAY notifications. They wake the operator, not the
// agent. Per the v0.1.2 spec correction (codex review on 58d07d2), they
// are explicitly local-only — remote service notifications (Slack, email,
// pager, webhooks) remain a v2 non-goal.
func osNotify(title, message string) error {
	switch runtime.GOOS {
	case "darwin":
		name, args := macNotifyCommand(title, message)
		return osexec.Command(name, args...).Run()
	case "linux":
		// notify-send already receives title/message as separate argv
		// elements, so there is no script-injection surface — the shell is
		// never involved.
		return osexec.Command("notify-send", title, message).Run()
	default:
		return fmt.Errorf("--notify is not supported on %s; use --print or --exec", runtime.GOOS)
	}
}

// macNotifyCommand builds the osascript invocation for a macOS notification
// WITHOUT interpolating the (untrusted) title/message into the AppleScript
// source. The script is a fixed template that reads its values from argv —
// `osascript <script> -- <message> <title>` — so a task/sender carrying
// AppleScript metacharacters, quotes, or newlines is delivered as data, never
// executed as code. The message body is, additionally, sanitized of control
// characters and length-capped as defense in depth (a stray newline would not
// break out, but it would still render oddly).
//
// Returned as (name, args) so it can be asserted in tests without running
// osascript (unavailable in CI).
func macNotifyCommand(title, message string) (string, []string) {
	const script = `on run argv
	display notification (item 1 of argv) with title (item 2 of argv)
end run`
	return "osascript", []string{
		"-e", script,
		"--", // everything after is positional argv for the script, never flags
		sanitizeNotificationText(message),
		sanitizeNotificationText(title),
	}
}

// sanitizeNotificationText strips control characters (newlines, NUL, escape,
// etc.) and caps length. Even though the argv form already prevents script
// injection, control characters in a notification are pointless and a very long
// task could bloat the notification; this keeps the surface tidy.
func sanitizeNotificationText(s string) string {
	const maxLen = 512
	var b strings.Builder
	for _, r := range s {
		if r == '\t' || r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	return out
}

func watchUsage(err error) error {
	if err == flag.ErrHelp {
		return watchHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, watchHelp())
}

func watchHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, watchHelp())
}

func watchHelp() string {
	return strings.TrimSpace(`
Usage: agentchute watch [--vendor <vendor>] [--as <id>] [--notify] [--print] [--exec <cmd>] [--interval <dur>]

Recipient-side persistent watcher. Polls the agent's inbox at --interval
(default 10s) and fires configured actions on each NEW message. Non-
consuming: never archives, quarantines, or wakes peers.

At least one of --notify / --print / --exec must be set. Watch fires on
arrivals after it starts; existing inbox state is captured silently at
launch.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin (anthropic, openai, google, xai)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --notify              OS notification per new message (osascript / notify-send)
  --print               stdout line per new message
  --exec <cmd>          shell command per new message; receives AGENTCHUTE_MSG_ID,
                        AGENTCHUTE_FROM, AGENTCHUTE_TASK as env vars (no body)
  --interval <dur>      poll cadence (default 10s; min 1s)
`)
}
