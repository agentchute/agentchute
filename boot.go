package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// cmdBoot is the session-start ritual: one command replacing register +
// pending + a status summary. Designed for SessionStart hooks (where it
// runs in --context-only or --codex-hook mode and always exits 0) and for
// interactive enrollment at the top of a turn (where exit 2 signals "you
// still have inbox or pending-reply obligations to clear").
//
// Spec: v0.1.1 rev3 §A.1. Implementation reuses performRegister (shared
// with cmdRegister) for the registration phase and the same inbox / ledger
// reads `pending` + `gate` use.
func cmdBoot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, host, wakeMethod, wakeTarget, controlRepo, loopDir, bio, codexHook string
	var quiet, noArchive, emitPromptLine, jsonOut, contextOnly bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (e.g., anthropic, openai, local, human)")
	fs.StringVar(&host, "host", "", "host this agent runs on (defaults to OS hostname)")
	fs.StringVar(&wakeMethod, "wake-method", "", "wake adapter (e.g., tmux); leave empty for non-pokable agents")
	fs.StringVar(&wakeTarget, "wake-target", "", "wake target; auto-detected from $TMUX_PANE when wake_method is tmux")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.StringVar(&bio, "bio", "", "short self-description for the registration body (markdown allowed)")
	fs.BoolVar(&quiet, "quiet", false, "suppress success output, only emit on warnings/blockers")
	fs.BoolVar(&noArchive, "no-archive", false, "paranoia flag: boot never archives regardless")
	fs.BoolVar(&emitPromptLine, "emit-prompt-line", false, "emit a single prompt-friendly line (for wrappers without hooks)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.BoolVar(&contextOnly, "context-only", false, "hook-safe mode: emit unread/pending state as text and always exit 0 (unless command failure)")
	fs.StringVar(&codexHook, "codex-hook", "", "codex hook JSON shape for the named event (SessionStart)")

	if err := fs.Parse(args); err != nil {
		return bootUsage(err)
	}
	if fs.NArg() != 0 {
		return bootUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	// Carry --no-archive forward for documentation symmetry; boot itself
	// never archives. Reference the variable so go vet doesn't flag it as
	// unused, while keeping the flag visible for operators who set it
	// defensively on hook commands.
	_ = noArchive

	opts := registerOpts{
		Host:       host,
		WakeMethod: wakeMethod,
		WakeTarget: wakeTarget,
		Bio:        bio,
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			opts.HostProvided = true
		case "wake-method":
			opts.WakeMethodProvided = true
		case "wake-target":
			opts.WakeTargetProvided = true
		case "bio":
			opts.BioProvided = true
		}
	})

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID == "" {
		return fmt.Errorf("missing agent identity; pass --as or set AGENTCHUTE_AGENT_ID")
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}
	opts.AgentID = agentID
	opts.Vendor = strings.TrimSpace(vendor)

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

	now := time.Now().UTC()
	result, err := performRegister(cfg, opts, now)
	if err != nil {
		return err
	}

	// Inbox peek — strictly side-effect free, same path `pending` uses.
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(result.InboxDir)
	if err != nil {
		return fmt.Errorf("list inbox: %w", err)
	}
	unread := make([]pendingEntry, 0, len(msgs))
	for _, msg := range msgs {
		entry := pendingEntry{
			From:      msg.Sender,
			Filename:  msg.Filename,
			Timestamp: msg.Timestamp.UTC().Format(time.RFC3339Nano),
		}
		if fm, _, ferr := readFrontmatter(msg.Path); ferr == nil {
			entry.MessageID = fm["message_id"]
			entry.Task = fm["task"]
			entry.Priority = fm["priority"]
			if v := strings.ToLower(strings.TrimSpace(fm["reply_required"])); v == "true" {
				entry.ReplyRequired = true
			}
		}
		unread = append(unread, entry)
	}

	// Pending-reply ledger — entries the recipient still owes a reply on.
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		return fmt.Errorf("load pending-reply ledger: %w", err)
	}
	pendingReplies := ledger.PendingEntries()

	status := bootStatus{
		Agent:          agentID,
		Vendor:         opts.Vendor,
		Refreshed:      result.Refreshed,
		ExistingFound:  result.ExistingFound,
		UnreadCount:    len(unread),
		Unread:         unread,
		RepliesPending: len(pendingReplies),
		PendingReplies: pendingReplies,
		MalformedCount: len(skipped),
		Host:           result.ResolvedHost,
		WakeMethod:     result.ResolvedWakeMethod,
		WakeTarget:     result.ResolvedWakeTarget,
		Blocked:        len(unread) > 0 || len(pendingReplies) > 0,
	}

	// Output dispatch. context-only / codex-hook NEVER block; interactive
	// emits text + (in non-context modes) returns errBlocked on obligations.
	switch {
	case codexHook == "SessionStart":
		return emitBootCodexSessionStart(status)
	case contextOnly:
		return emitBootContextOnly(status)
	case jsonOut:
		if err := emitBootJSON(status); err != nil {
			return err
		}
	case emitPromptLine:
		emitBootPromptLine(status)
	default:
		emitBootText(status, quiet)
	}

	if status.Blocked {
		return errBlocked
	}
	return nil
}

// bootStatus is the cross-format shape consumed by every output emitter.
type bootStatus struct {
	Agent     string `json:"agent"`
	Vendor    string `json:"vendor"`
	Refreshed bool   `json:"refreshed"`
	// ExistingFound is internal-only (not serialized) — drives the text-mode
	// "Refreshed" vs "Registered" verb choice without affecting the spec-defined
	// JSON wire shape.
	ExistingFound  bool                     `json:"-"`
	UnreadCount    int                      `json:"unread_count"`
	Unread         []pendingEntry           `json:"unread,omitempty"`
	RepliesPending int                      `json:"replies_pending"`
	PendingReplies []loop.PendingReplyEntry `json:"pending_replies,omitempty"`
	MalformedCount int                      `json:"malformed_count,omitempty"`
	Host           string                   `json:"host,omitempty"`
	WakeMethod     string                   `json:"wake_method,omitempty"`
	WakeTarget     string                   `json:"wake_target,omitempty"`
	Blocked        bool                     `json:"blocked"`

	// StaleReg / WakeStale reserved for forward-compat with spec rev3 §A.1's
	// `stale_reg` and `wake_stale` JSON fields. Always false after a
	// successful boot since the register step freshly stamps last_seen; kept
	// in the shape so downstream parsers can rely on a stable schema.
	StaleReg  bool `json:"stale_reg"`
	WakeStale bool `json:"wake_stale"`
}

// emitBootText is the default human-readable output.
func emitBootText(s bootStatus, quiet bool) {
	if quiet && !s.Blocked && s.MalformedCount == 0 {
		return
	}
	if !quiet {
		verb := "Refreshed"
		if !s.ExistingFound {
			verb = "Registered"
		}
		fmt.Printf("%s %s (%s) — %s\n", verb, s.Agent, s.Vendor, blockedSummary(s))
		if s.WakeMethod != "" {
			fmt.Printf("  wake: %s %s\n", s.WakeMethod, s.WakeTarget)
		} else {
			fmt.Println("  (non-pokable: senders skip the wake poke; you must poll your own inbox)")
		}
	}
	if s.UnreadCount > 0 {
		fmt.Printf("  unread: %d direct message(s) — run `agentchute check` to consume\n", s.UnreadCount)
		for _, u := range s.Unread {
			flags := ""
			if u.ReplyRequired {
				flags = " [REPLY-REQUIRED]"
			}
			fmt.Printf("    %s from %s — %s%s\n", u.Timestamp, u.From, u.Task, flags)
		}
	}
	if s.RepliesPending > 0 {
		fmt.Printf("  replies_pending: %d obligation(s) (gate --before finish blocks while open)\n", s.RepliesPending)
	}
	if s.MalformedCount > 0 {
		fmt.Printf("  malformed: %d file(s) need quarantine — run `agentchute check`\n", s.MalformedCount)
	}
}

func blockedSummary(s bootStatus) string {
	switch {
	case s.UnreadCount == 0 && s.RepliesPending == 0:
		return "inbox clear, no pending replies"
	case s.UnreadCount > 0 && s.RepliesPending > 0:
		return fmt.Sprintf("%d unread / %d pending replies", s.UnreadCount, s.RepliesPending)
	case s.UnreadCount > 0:
		return fmt.Sprintf("%d unread", s.UnreadCount)
	default:
		return fmt.Sprintf("%d pending replies", s.RepliesPending)
	}
}

func emitBootJSON(s bootStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// emitBootPromptLine emits a single line designed to be appended to a
// wrapper's system prompt by an operator that hasn't installed hooks yet.
func emitBootPromptLine(s bootStatus) {
	switch {
	case s.UnreadCount == 0 && s.RepliesPending == 0:
		fmt.Println("agentchute: inbox clear; no pending replies.")
	case s.UnreadCount > 0 && s.RepliesPending > 0:
		fmt.Printf("agentchute: %d unread message(s) and %d pending reply obligation(s) — run `agentchute check` and reply via `agentchute send --reply-to`.\n",
			s.UnreadCount, s.RepliesPending)
	case s.UnreadCount > 0:
		fmt.Printf("agentchute: %d unread message(s) — run `agentchute check`.\n", s.UnreadCount)
	default:
		fmt.Printf("agentchute: %d pending reply obligation(s) — reply via `agentchute send --reply-to` or `agentchute defer`.\n", s.RepliesPending)
	}
}

// emitBootContextOnly is the generic hook-safe text output suitable for
// SessionStart developer-context injection. Never blocks — even with
// outstanding obligations — because hook stdout becomes context, not a
// turn-failure signal.
func emitBootContextOnly(s bootStatus) error {
	switch {
	case s.UnreadCount == 0 && s.RepliesPending == 0:
		fmt.Printf("agentchute: %s enrolled (%s). Inbox clear; no pending reply obligations.\n", s.Agent, s.Vendor)
	default:
		fmt.Printf("agentchute: %s enrolled (%s). %s.\n", s.Agent, s.Vendor, blockedSummary(s))
		for _, u := range s.Unread {
			flags := ""
			if u.ReplyRequired {
				flags = " [REPLY-REQUIRED]"
			}
			fmt.Printf("  - unread: %s from %s — %s%s\n", u.Timestamp, u.From, u.Task, flags)
		}
		for _, p := range s.PendingReplies {
			fmt.Printf("  - pending reply: %s from %s — %s\n", p.MessageID, p.From, p.Task)
		}
		fmt.Println()
		fmt.Println("Run `agentchute check` to consume unread; reply via `agentchute send --reply-to` or `agentchute defer`.")
	}
	if s.MalformedCount > 0 {
		fmt.Printf("agentchute: %d malformed file(s) await quarantine — run `agentchute check`.\n", s.MalformedCount)
	}
	return nil
}

// emitBootCodexSessionStart wraps the context-only text into the codex
// hookSpecificOutput JSON shape, so codex's SessionStart hook injects it
// as model-visible developer context.
func emitBootCodexSessionStart(s bootStatus) error {
	var ctx strings.Builder
	switch {
	case s.UnreadCount == 0 && s.RepliesPending == 0:
		fmt.Fprintf(&ctx, "agentchute: %s enrolled (%s). Inbox clear; no pending reply obligations.", s.Agent, s.Vendor)
	default:
		fmt.Fprintf(&ctx, "agentchute: %s enrolled (%s). %s.", s.Agent, s.Vendor, blockedSummary(s))
		for _, u := range s.Unread {
			flags := ""
			if u.ReplyRequired {
				flags = " [REPLY-REQUIRED]"
			}
			fmt.Fprintf(&ctx, "\n  - unread: %s from %s — %s%s", u.Timestamp, u.From, u.Task, flags)
		}
		for _, p := range s.PendingReplies {
			fmt.Fprintf(&ctx, "\n  - pending reply: %s from %s — %s", p.MessageID, p.From, p.Task)
		}
		ctx.WriteString("\n\nRun `agentchute check` to consume unread; reply via `agentchute send --reply-to` or `agentchute defer`.")
	}
	if s.MalformedCount > 0 {
		fmt.Fprintf(&ctx, "\nagentchute: %d malformed file(s) await quarantine.", s.MalformedCount)
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": ctx.String(),
		},
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

func bootUsage(err error) error {
	if err == flag.ErrHelp {
		return bootHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, bootHelp())
}

func bootHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, bootHelp())
}

func bootHelp() string {
	return strings.TrimSpace(`
Usage: agentchute boot --as <id> --vendor <vendor> [flags]

Session-start ritual: register/refresh + side-effect-free inbox peek + pending
reply summary, in one command. Replaces the three-step register+pending+status
sequence at the top of a turn.

Exit codes (interactive mode):
  0  registration fresh + no unread mail + no pending replies
  2  unread direct mail OR pending reply obligations present
  1  command failure (binary error, filesystem error, etc.)

Exit codes (--context-only / --codex-hook): always 0 unless command failure.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin (anthropic, openai, google, local, human)
  --host <name>         host (defaults to OS hostname)
  --wake-method <m>     wake adapter (tmux, etc.); auto-detected from $TMUX_PANE
  --wake-target <addr>  wake target; auto-detected from $TMUX_PANE when method is tmux
  --bio <text>          short self-description for the registration body
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --quiet               suppress success output (warnings/blockers still emit)
  --no-archive          paranoia flag; boot itself never archives anyway
  --emit-prompt-line    emit a single prompt-friendly line
  --json                structured JSON output
  --context-only        hook-safe mode; always exits 0 unless command failure
  --codex-hook <event>  codex hook JSON shape (SessionStart)
`)
}
