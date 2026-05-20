package main

import (
	"encoding/json"
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

	var agentID, controlRepo, loopDir, codexHook, staleAfter string
	var jsonOut, failIfAny, showBody bool
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.BoolVar(&failIfAny, "fail-if-any", false, "exit 2 if any unread messages")
	fs.BoolVar(&showBody, "show-body", false, "include message body in output (default: frontmatter only)")
	fs.StringVar(&staleAfter, "stale-after", "", "annotate (not filter) messages older than this duration (e.g. 5m, 1h)")
	fs.StringVar(&codexHook, "codex-hook", "", "emit codex-specific hook JSON shape for the named event (UserPromptSubmit)")

	if err := fs.Parse(args); err != nil {
		return pendingUsage(err)
	}
	if fs.NArg() != 0 {
		return pendingUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	agentID = strings.TrimSpace(firstNonEmpty(agentID, os.Getenv("AGENTCHUTE_AGENT_ID")))
	if agentID == "" {
		return fmt.Errorf("missing agent identity; pass --as or set AGENTCHUTE_AGENT_ID")
	}
	if err := loop.ValidateAgentID(agentID); err != nil {
		return err
	}

	var staleThreshold time.Duration
	if staleAfter != "" {
		d, err := time.ParseDuration(staleAfter)
		if err != nil {
			return fmt.Errorf("invalid --stale-after: %w", err)
		}
		staleThreshold = d
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

	// Strictly read-only: we do NOT update last_seen here. Pending is the
	// hook-safe peek; `boot` is the lifecycle event that ticks last_seen.
	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		return fmt.Errorf("list inbox: %w", err)
	}

	// Pending-reply ledger — surface alongside the inbox count so per-turn
	// hook context shows the full obligation picture, not just unread mail.
	// LoadPendingLedger is also strictly read-only.
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		return fmt.Errorf("load pending-reply ledger: %w", err)
	}
	pendingReplies := ledger.PendingEntries()

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

	// Emit output.
	if codexHook == "UserPromptSubmit" {
		// Codex-specific hook JSON shape. Emit even when inbox is empty so
		// the hook always produces valid JSON and exits cleanly.
		return emitCodexUserPromptSubmit(entries, pendingReplies, len(skipped))
	}
	if jsonOut {
		return emitPendingJSON(entries, pendingReplies, len(skipped))
	}
	emitPendingText(entries, pendingReplies, len(skipped))

	// Exit code: 0 by default. With --fail-if-any, exit 2 if EITHER the
	// inbox OR the ledger has unfinished obligations. Both are reasons a
	// hook should treat the turn as not-yet-done.
	if failIfAny && (len(entries) > 0 || len(pendingReplies) > 0) {
		return errFailIfAny
	}
	return nil
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
func emitPendingText(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int) {
	if len(entries) == 0 && len(replies) == 0 {
		fmt.Println("(no unread messages; no pending reply obligations)")
		if malformed > 0 {
			fmt.Printf("(%d malformed file(s) skipped; run `agentchute check` to quarantine + notify)\n", malformed)
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
		fmt.Printf("(%d malformed file(s) skipped; run `agentchute check` to quarantine + notify)\n", malformed)
	}
}

// emitPendingJSON prints the structured JSON form.
func emitPendingJSON(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int) error {
	out := struct {
		Count          int                      `json:"count"`
		Malformed      int                      `json:"malformed_skipped"`
		RepliesPending int                      `json:"replies_pending"`
		Messages       []pendingEntry           `json:"messages"`
		PendingReplies []loop.PendingReplyEntry `json:"pending_replies,omitempty"`
	}{
		Count:          len(entries),
		Malformed:      malformed,
		RepliesPending: len(replies),
		Messages:       entries,
		PendingReplies: replies,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// emitCodexUserPromptSubmit emits the codex-specific hookSpecificOutput
// JSON shape that injects the pending summary into model-visible developer
// context. Always exits 0 (returned error nil) so the hook never fails the
// turn just because mail is pending.
func emitCodexUserPromptSubmit(entries []pendingEntry, replies []loop.PendingReplyEntry, malformed int) error {
	var ctx strings.Builder
	switch {
	case len(entries) == 0 && len(replies) == 0:
		ctx.WriteString("agentchute: no unread messages; no pending reply obligations.")
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
		ctx.WriteString("Run `agentchute check` to read and archive, reply via `agentchute send --reply-to`, or defer with `agentchute defer`.")
	}
	if malformed > 0 {
		fmt.Fprintf(&ctx, "\n(%d malformed file(s) need quarantine; run `agentchute check`.)", malformed)
	}
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": ctx.String(),
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
	data, err := os.ReadFile(path)
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
Usage: agentchute pending --as <id> [flags]

List unread inbox messages without archiving, quarantining, or poking peers.
Strictly side-effect-free; safe to invoke from lifecycle hooks. Distinct from
'agentchute check' which is the consume-and-archive operation.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --control-repo <path> control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <path>     loop dir path (or $AGENTCHUTE_LOOP_DIR)
  --json                structured JSON output
  --fail-if-any         exit 2 if any unread messages
  --show-body           include message body in output
  --stale-after <dur>   annotate (not filter) messages older than this
  --codex-hook <event>  emit codex-specific hook JSON (UserPromptSubmit)
`)
}

// pendingPath is exported for tests to assert on path construction.
func pendingPath(cfg *loop.Config, agentID string) string {
	return filepath.Clean(cfg.AgentInboxDir(agentID))
}
