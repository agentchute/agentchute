package cli

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

	// Asker-owned `.owed` obligations — replies WE are owed by peers (v0.9.0:
	// the sole reply-obligation surface). Surfaced alongside the inbox count so
	// per-turn hook context shows the full picture. NON-BLOCKING (asker-owned,
	// dead-recipient signal); LoadOwedLedger is strictly read-only. A corrupt
	// `.owed` is not fatal here — gate/boot own blocking, so pending stays a
	// best-effort peek and reports zero owed rather than crashing.
	var owedEntries []loop.OwedEntry
	if owed, oerr := loop.LoadOwedLedger(cfg, agentID); oerr == nil {
		owedEntries = owed.OutstandingOwed()
	}

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
			From:      msg.Sender,
			Filename:  msg.Filename,
			Timestamp: msg.Timestamp.UTC().Format(time.RFC3339Nano),
		}
		// Parse frontmatter for reply_required.
		// Body intentionally not read unless --show-body.
		fm, body, ferr := readFrontmatter(msg.Path)
		if ferr == nil {
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
		return emitClaudeUserPromptSubmit(entries, owedEntries, len(skipped), needsBoot, agentID)
	}
	if codexHook == "UserPromptSubmit" {
		return emitCodexUserPromptSubmit(entries, owedEntries, len(skipped), needsBoot, agentID)
	}
	if jsonOut {
		return emitPendingJSON(entries, owedEntries, len(skipped), needsBoot, agentID)
	}
	emitPendingText(entries, owedEntries, len(skipped), needsBoot, agentID)

	// Exit code: 0 by default. With --fail-if-any, exit 2 if the inbox has
	// unread mail OR the agent needs boot — the reasons a scheduler should wake
	// the wrapper. Owed obligations are asker-owned and NON-BLOCKING (v0.9.0):
	// they never force a wake (mirrors gate not blocking + poller not waking on
	// owed), so they are surfaced but excluded from the --fail-if-any predicate.
	if failIfAny && (needsBoot || len(entries) > 0) {
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

// pendingEntry is the structured record for a single unread message. The
// canonical (to,from,seq) identity is surfaced via Filename (`from-<from>_seq-<seq>`)
// plus From and the inbox owner (= the agent listing) — there is no sender-asserted
// message_id (v0.9.0).
type pendingEntry struct {
	From          string `json:"from"`
	Filename      string `json:"filename"`
	Timestamp     string `json:"timestamp"`
	ReplyRequired bool   `json:"reply_required,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
	Body          string `json:"body,omitempty"`
}

// errFailIfAny is the sentinel returned when --fail-if-any sees unread mail.
// main.go's error handler maps this to exit code 2.
var errFailIfAny = fmt.Errorf("agentchute-pending: unread messages exist")

// emitPendingText prints the human-readable pending summary. `owed` are the
// asker-owned obligations WE are awaiting a reply on (non-blocking).
func emitPendingText(entries []pendingEntry, owed []loop.OwedEntry, malformed int, needsBoot bool, agentID string) {
	if needsBoot {
		fmt.Println(needsBootMessage(agentID))
	}
	if len(entries) == 0 && len(owed) == 0 {
		if !needsBoot {
			fmt.Println("(no unread messages; no owed reply obligations)")
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
			if e.Stale {
				flags += " [stale]"
			}
			fmt.Printf("  %s from %s", e.Timestamp, e.From)
			fmt.Println(flags)
		}
	}
	if len(owed) > 0 {
		fmt.Printf("%d owed reply obligation(s) (awaiting a reply from a peer; non-blocking):\n", len(owed))
		for _, o := range owed {
			fmt.Printf("  %s\n", owedLine(o))
		}
	}
	if malformed > 0 {
		fmt.Printf("(%d malformed file(s) skipped; run `agentchute check --as %s` to quarantine + notify)\n", malformed, agentID)
	}
}

// owedLine renders a single asker-owned obligation: the peer we are awaiting a
// reply from and the canonical (to,from,seq) ref the reply must echo.
func owedLine(o loop.OwedEntry) string {
	return fmt.Sprintf("awaiting reply from %s — %s", o.To, o.Key().RefString())
}

// emitPendingJSON prints the structured JSON form. `owed` are the asker-owned
// obligations WE are awaiting a reply on (non-blocking).
func emitPendingJSON(entries []pendingEntry, owed []loop.OwedEntry, malformed int, needsBoot bool, agentID string) error {
	out := struct {
		Count           int              `json:"count"`
		Malformed       int              `json:"malformed_skipped"`
		OwedOutstanding int              `json:"owed_outstanding"`
		NeedsBoot       bool             `json:"needs_boot,omitempty"`
		BootHint        string           `json:"boot_hint,omitempty"`
		Messages        []pendingEntry   `json:"messages"`
		Owed            []loop.OwedEntry `json:"owed,omitempty"`
	}{
		Count:           len(entries),
		Malformed:       malformed,
		OwedOutstanding: len(owed),
		NeedsBoot:       needsBoot,
		Messages:        entries,
		Owed:            owed,
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
func buildPendingContext(entries []pendingEntry, owed []loop.OwedEntry, malformed int, needsBoot bool, agentID string) string {
	var ctx strings.Builder
	if needsBoot {
		ctx.WriteString(needsBootMessage(agentID))
		ctx.WriteString("\n")
	}
	switch {
	case len(entries) == 0 && len(owed) == 0:
		if !needsBoot {
			ctx.WriteString("agentchute: no unread messages; no owed reply obligations.")
		}
	default:
		if len(entries) > 0 {
			fmt.Fprintf(&ctx, "agentchute: %d unread message(s) in your inbox:\n", len(entries))
			for _, e := range entries {
				fmt.Fprintf(&ctx, "  - %s from %s", e.Timestamp, e.From)
				if e.ReplyRequired {
					ctx.WriteString(" [REPLY-REQUIRED]")
				}
				ctx.WriteString("\n")
			}
		}
		if len(owed) > 0 {
			fmt.Fprintf(&ctx, "agentchute: %d owed reply obligation(s) awaiting a reply from a peer (non-blocking):\n", len(owed))
			for _, o := range owed {
				fmt.Fprintf(&ctx, "  - %s\n", owedLine(o))
			}
		}
		fmt.Fprintf(&ctx, "Run `agentchute check --as %s` to read and archive, and reply via `agentchute send --from %s --to <peer> --reply-to <ref> --body \"...\"`.", agentID, agentID)
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
func emitCodexUserPromptSubmit(entries []pendingEntry, owed []loop.OwedEntry, malformed int, needsBoot bool, agentID string) error {
	return emitHookContextJSON("UserPromptSubmit", buildPendingContext(entries, owed, malformed, needsBoot, agentID))
}

// emitClaudeUserPromptSubmit emits the Claude-Code-specific hook JSON shape
// for UserPromptSubmit. Verified against code.claude.com/docs/en/hooks.md:
// Claude expects `hookSpecificOutput.additionalContext` nested (NOT
// top-level `additionalContext`), exit 0 required. Convergent with codex's
// schema; the structural difference between this and emitCodexUserPromptSubmit
// is currently zero. Kept as a distinct emitter so future wrapper-specific
// fields (Claude's `sessionTitle`, codex's `statusMessage`, etc.) can
// diverge without forcing a callsite change.
func emitClaudeUserPromptSubmit(entries []pendingEntry, owed []loop.OwedEntry, malformed int, needsBoot bool, agentID string) error {
	return emitHookContextJSON("UserPromptSubmit", buildPendingContext(entries, owed, malformed, needsBoot, agentID))
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
// message with `---   \n` no longer shows a blank reply_required just
// because of trailing whitespace on the delimiter (codex final-pass note on
// 0d468fa).
func readFrontmatter(path string) (map[string]string, string, error) {
	// Cap the read at the inbox message limit, matching the consume path
	// (check.go). pending/boot are hook-safe peek paths; a
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
