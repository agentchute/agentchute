package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// cmdPending lists unread inbox messages without archiving, quarantining,
// or poking peers. Strictly side-effect-free; safe to invoke from hooks.
// Distinct from `check` which is the consume-and-archive operation.
//
// Lifecycle hooks should prefer `pending` (or `boot --context-only`) over
// bare `check`; see AGENTCHUTE.md §6.3 and the v0.1.1 spec.
func cmdPending(args []string) error {
	fs := flag.NewFlagSet("pending", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, controlRepo, loopDir, staleAfter, codexHook, claudeHook string
	var jsonOut, failIfAny, showBody bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")

	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.BoolVar(&failIfAny, "fail-if-any", false, "exit 2 if any unread messages")
	fs.BoolVar(&showBody, "show-body", false, "include message body in output (default: frontmatter only)")
	fs.StringVar(&staleAfter, "stale-after", "", "annotate (not filter) messages older than this duration (e.g. 5m, 1h)")
	fs.StringVar(&codexHook, "codex-hook", "", "emit codex-specific hook JSON shape for the named event (UserPromptSubmit)")
	fs.StringVar(&claudeHook, "claude-hook", "", "emit Claude-Code-specific hook JSON shape for the named event (UserPromptSubmit)")

	if err := fs.Parse(args); err != nil {
		return pendingUsage(err)
	}
	if fs.NArg() != 0 {
		return pendingUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
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

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): pending stays
	// read-only, so it does NOT hard-refuse on missing registration like
	// the active commands. Instead it surfaces a `needs_boot` reason in
	// every output mode (text, --json, --claude-hook, --codex-hook). The
	// only exit-2 path is --fail-if-any (codex review: needs_boot IS
	// actionable work).
	needsBoot := false
	if _, err := os.Stat(cfg.AgentRegistrationPath(agentID)); err != nil {
		if os.IsNotExist(err) {
			needsBoot = true
		} else {
			return fmt.Errorf("stat own registration: %w", err)
		}
	}

	// Strictly read-only: we do NOT update last_seen here. Pending is the
	// hook-safe peek; `boot` is the lifecycle event that ticks last_seen.
	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		if errors.Is(err, loop.ErrInboxMissing) {
			needsBoot = true
			msgs, skipped = nil, nil
		} else {
			return fmt.Errorf("list inbox: %w", err)
		}
	}

	// Pending-reply ledger — surface alongside the inbox count so per-turn
	// hook context shows the full obligation picture, not just unread mail.
	// LoadPendingLedger is also strictly read-only.
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		return fmt.Errorf("load pending-reply ledger: %w", err)
	}
	pendingReplies := ledger.PendingEntries()

	var staleThreshold time.Duration
	if staleAfter != "" {
		d, err := time.ParseDuration(staleAfter)
		if err != nil {
			return fmt.Errorf("invalid --stale-after: %w", err)
		}
		staleThreshold = d
	}

	now := time.Now().UTC()
	entries := make([]pendingEntry, 0, len(msgs))
	for _, msg := range msgs {
		entry := pendingEntry{
			MessageID: "",
			From:      msg.Sender,
			Filename:  msg.Filename,
			Timestamp: msg.Timestamp.UTC().Format(time.RFC3339Nano),
		}
		// Parse frontmatter for reply_required / priority / task / message_id.
		// Body intentionally not read unless --show-body.
		fm, body, ferr := readFrontmatter(msg.Path)
		if ferr == nil {
			entry.MessageID = fm["message_id"]
			entry.Task = fm["task"]
			entry.Priority = fm["priority"]
			if v := strings.ToLower(strings.TrimSpace(fm["reply_required"])); v == "true" {
				entry.ReplyRequired = true
			}
			if showBody {
				entry.Body = body
			}
		}
		if staleThreshold > 0 && now.Sub(msg.Timestamp.UTC()) > staleThreshold {
			entry.Stale = true
		}
		entries = append(entries, entry)
	}

	// Emit output. The two --*-hook modes emit wrapper-specific JSON shapes
	// designed to inject `additionalContext` into the model's developer
	// context for that hook event. They never exit nonzero — the hook is
	// for information, not lifecycle gating.
	if claudeHook == "UserPromptSubmit" {
		return emitClaudeUserPromptSubmit(entries, pendingReplies, len(skipped), needsBoot, agentID)
	}
	if codexHook == "UserPromptSubmit" {
		return emitCodexUserPromptSubmit(entries, pendingReplies, len(skipped), needsBoot, agentID)
	}
	if jsonOut {
		return emitPendingJSON(entries, pendingReplies, len(skipped), needsBoot, agentID)
	}
	emitPendingText(entries, pendingReplies, len(skipped), needsBoot, agentID)

	// Exit code: 0 by default. With --fail-if-any, exit 2 if EITHER the
	// inbox OR the ledger has unfinished obligations, OR the agent needs
	// boot — all three are reasons a scheduler should wake the wrapper.
	if failIfAny && (needsBoot || len(entries) > 0 || len(pendingReplies) > 0) {
		return errFailIfAny
	}
	return nil
}

// needsBootMessage is the model-facing line surfaced when pending detects
// that the agent has no registration on disk. Plain-text only (no shell-
// special chars) so it can pass through the hook-envelope additionalContext
// layers without further escaping.
func needsBootMessage(agentID string) string {
	return fmt.Sprintf("agentchute: agent %q is not registered yet. Run agentchute boot --as %s --vendor <vendor> before processing mail (AGENTCHUTE.md §5.3).", agentID, agentID)
}

// pendingEntry is the structured record for a single unread message.
type pendingEntry struct {
	MessageID     string `json:"message_id,omitempty"`
	From          string `json:"from"`
	Task          string `json:"task,omitempty"`
	Filename      string `json:"filename"`
	Timestamp     string `json:"timestamp"`
	Priority      string `json:"priority,omitempty"`
	ReplyRequired bool   `json:"reply_required,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
	Body          string `json:"body,omitempty"`
}

// errFailIfAny is the sentinel returned when --fail-if-any sees unread mail.
// main.go's error handler maps this to exit code 2.
var errFailIfAny = fmt.Errorf("agentchute-pending: unread messages exist")

// emitPendingText prints the human-readable pending summary.
func emitPendingText(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int, needsBoot bool, agentID string) {
	if needsBoot {
		fmt.Println(needsBootMessage(agentID))
	}
	if len(entries) == 0 && len(replies) == 0 {
		if !needsBoot {
			fmt.Println("(no unread messages; no pending reply obligations)")
		}
		if malformed > 0 {
			fmt.Printf("(%d malformed file(s) skipped; run `agentchute check --as %s` to quarantine + notify)\n", malformed, agentID)
		}
		return
	}
	if len(entries) > 0 {
		fmt.Printf("%d unread message(s):\n", len(entries))
		for _, e := range entries {
			flags := ""
			if e.ReplyRequired {
				flags += " [REPLY-REQUIRED]"
			}
			if e.Priority != "" && e.Priority != "normal" {
				flags += " [priority:" + e.Priority + "]"
			}
			if e.Stale {
				flags += " [stale]"
			}
			fmt.Printf("  %s from %s", e.Timestamp, e.From)
			if e.Task != "" {
				fmt.Printf(" — %s", e.Task)
			}
			fmt.Println(flags)
		}
	}
	if len(replies) > 0 {
		fmt.Printf("%d pending reply obligation(s):\n", len(replies))
		for _, r := range replies {
			fmt.Printf("  %s from %s — %s\n", r.MessageID, r.From, r.Task)
		}
	}
	if malformed > 0 {
		fmt.Printf("(%d malformed file(s) skipped; run `agentchute check --as %s` to quarantine + notify)\n", malformed, agentID)
	}
}

// emitPendingJSON prints the structured JSON form.
func emitPendingJSON(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int, needsBoot bool, agentID string) error {
	out := struct {
		Count          int                      `json:"count"`
		Malformed      int                      `json:"malformed_skipped"`
		RepliesPending int                      `json:"replies_pending"`
		NeedsBoot      bool                     `json:"needs_boot,omitempty"`
		BootHint       string                   `json:"boot_hint,omitempty"`
		Messages       []pendingEntry           `json:"messages"`
		PendingReplies []loop.PendingReplyEntry `json:"pending_replies,omitempty"`
	}{
		Count:          len(entries),
		Malformed:      malformed,
		RepliesPending: len(replies),
		NeedsBoot:      needsBoot,
		Messages:       entries,
		PendingReplies: replies,
	}
	if needsBoot {
		out.BootHint = needsBootMessage(agentID)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// buildPendingContext renders the shared developer-context text used by both
// the --codex-hook and --claude-hook UserPromptSubmit emitters. The text is
// the same regardless of wrapper — only the surrounding JSON envelope
// differs (and even that converged: Claude Code and codex-cli both accept
// the same `hookSpecificOutput.additionalContext` nested shape).
func buildPendingContext(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int, needsBoot bool, agentID string) string {
	var ctx strings.Builder
	if needsBoot {
		ctx.WriteString(needsBootMessage(agentID))
		ctx.WriteString("\n")
	}
	switch {
	case len(entries) == 0 && len(replies) == 0:
		if !needsBoot {
			ctx.WriteString("agentchute: no unread messages; no pending reply obligations.")
		}
	default:
		if len(entries) > 0 {
			fmt.Fprintf(&ctx, "agentchute: %d unread message(s) in your inbox:\n", len(entries))
			for _, e := range entries {
				fmt.Fprintf(&ctx, "  - %s from %s", e.Timestamp, e.From)
				if e.Task != "" {
					fmt.Fprintf(&ctx, " — %s", e.Task)
				}
				if e.ReplyRequired {
					ctx.WriteString(" [REPLY-REQUIRED]")
				}
				ctx.WriteString("\n")
			}
		}
		if len(replies) > 0 {
			fmt.Fprintf(&ctx, "agentchute: %d pending reply obligation(s):\n", len(replies))
			for _, r := range replies {
				fmt.Fprintf(&ctx, "  - %s from %s — %s\n", r.MessageID, r.From, r.Task)
			}
		}
		fmt.Fprintf(&ctx, "Run `agentchute check --as %s` to read and archive, reply via `agentchute send --from %s --to <peer> --reply-to <message-id>`, or defer with `agentchute defer --as %s --message <message-id>`.", agentID, agentID, agentID)
	}
	if malformed > 0 {
		fmt.Fprintf(&ctx, "\n(%d malformed file(s) need quarantine; run `agentchute check --as %s`.)", malformed, agentID)
	}
	return ctx.String()
}

// emitCodexUserPromptSubmit emits the codex-specific hookSpecificOutput
// JSON shape that injects the pending summary into model-visible developer
// context. Always exits 0 (returned error nil) so the hook never fails the
// turn just because mail is pending.
func emitCodexUserPromptSubmit(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int, needsBoot bool, agentID string) error {
	return emitHookContextJSON("UserPromptSubmit", buildPendingContext(entries, replies, malformed, needsBoot, agentID))
}

// emitClaudeUserPromptSubmit emits the Claude-Code-specific hook JSON shape
// for UserPromptSubmit. Verified against code.claude.com/docs/en/hooks.md:
// Claude expects `hookSpecificOutput.additionalContext` nested (NOT
// top-level `additionalContext`), exit 0 required. Convergent with codex's
// schema; the structural difference between this and emitCodexUserPromptSubmit
// is currently zero. Kept as a distinct emitter so future wrapper-specific
// fields (Claude's `sessionTitle`, codex's `statusMessage`, etc.) can
// diverge without forcing a callsite change.
func emitClaudeUserPromptSubmit(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int, needsBoot bool, agentID string) error {
	return emitHookContextJSON("UserPromptSubmit", buildPendingContext(entries, replies, malformed, needsBoot, agentID))
}

// emitHookContextJSON writes the canonical hookSpecificOutput envelope to
// stdout. Used by both wrapper-specific UserPromptSubmit emitters.
func emitHookContextJSON(event, additionalContext string) error {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": additionalContext,
		},
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}

// readFrontmatter parses the leading frontmatter block from a message
// file via loop.ParseMessageFrontmatter + loop.ExtractMessageBody. Both
// helpers use the trimmed-delimiter semantics shared with
// loop.ValidateMessageFrontmatter, so `pending` / `boot` hook context
// surfaces the same fields the consume path records — a hand-protocol
// message with `---   \n` no longer shows blank message_id / task /
// reply_required just because of trailing whitespace on the delimiter
// (codex final-pass note on 0d468fa).
func readFrontmatter(path string) (map[string]string, string, error) {
	// Cap the read at the inbox message limit, matching the consume path
	// (check.go). pending/boot/self-poll/watch are hook-safe peek paths; a
	// peer could plant a validly named but oversized inbox file, and reading
	// it unbounded just to inspect frontmatter would let that peer OOM the
	// consumer.
	data, err := loop.ReadFileLimit(path, loop.MaxInboxMessageBytes)
	if err != nil {
		return nil, "", err
	}
	return loop.ParseMessageFrontmatter(data), loop.ExtractMessageBody(data), nil
}

// pendingUsage prints the pending command help and returns ErrHelp.
func pendingUsage(err error) error {
	if err == flag.ErrHelp {
		return pendingHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, pendingHelp())
}

func pendingHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, pendingHelp())
}

func pendingHelp() string {
	return strings.TrimSpace(`
Usage: agentchute pending [--vendor <vendor>] [--as <id>] [flags]

List unread inbox messages without archiving, quarantining, or poking peers.
Strictly side-effect-free; safe to invoke from lifecycle hooks. Distinct from
'agentchute check' which is the consume-and-archive operation.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin (anthropic, openai, google, xai)
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)

  --loop-dir <path>     loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output
  --fail-if-any         exit 2 if any unread messages
  --show-body           include message body in output
  --stale-after <dur>   annotate (not filter) messages older than this
  --codex-hook <event>  emit codex-specific hook JSON (UserPromptSubmit)
  --claude-hook <event> emit Claude-Code-specific hook JSON (UserPromptSubmit)
`)
}

// pendingPath is exported for tests to assert on path construction.
func pendingPath(cfg *loop.Config, agentID string) string {
	return filepath.Clean(cfg.AgentInboxDir(agentID))
}
