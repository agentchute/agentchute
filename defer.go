package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// cmdDefer is the deferral path for a pending-reply obligation. The
// required companion to `gate --before finish`: without it, the gate
// could trap legitimate work the agent has consciously decided not to
// reply to right now.
//
// Spec: v0.1.1 rev3 §A.5.
//
// Behavior:
//  1. Resolve cfg, validate message_id + reason.
//  2. Look up the ledger entry for <message-id>; surface ErrLedgerEntryNotFound
//     and ErrLedgerEntryNotPending verbatim so the caller can distinguish
//     "I never owed a reply" from "I already replied/deferred."
//  3. Transition the entry to status=deferred via MarkPendingDeferred.
//  4. Compose and send an auto-acknowledgment back to the original sender
//     (task: deferred-reply, status: info, in_reply_to set) with the reason
//     in the body. Best-effort poke if the sender is pokable.
//
// Step 4 is best-effort: if the sender's registration is missing or their
// inbox dir doesn't exist (offline / never enrolled), we still committed
// the local deferral and report the send failure as a warning. The gate
// already cleared on step 3; the sender visibility is the second
// concern.
func cmdDefer(args []string) error {
	fs := flag.NewFlagSet("defer", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, messageID, reason, until, controlRepo, loopDir string
	fs.StringVar(&agentID, "as", "", "agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&messageID, "message", "", "message_id of the obligation to defer (required)")
	fs.StringVar(&reason, "reason", "", "human-readable reason for the deferral (required)")
	fs.StringVar(&until, "until", "", "optional duration (e.g. 24h) or absolute RFC3339 timestamp for the unblock")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or $AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or $AGENTCHUTE_LOOP_DIR)")

	if err := fs.Parse(args); err != nil {
		return deferUsage(err)
	}
	if fs.NArg() != 0 {
		return deferUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return deferUsage(fmt.Errorf("--message <id> is required"))
	}
	if err := rejectFrontmatterInjection("--message", messageID); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return deferUsage(fmt.Errorf("--reason is required"))
	}

	deferredUntil, err := normalizeDeferUntil(until)
	if err != nil {
		return fmt.Errorf("--until: %w", err)
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

	// Look up the entry first so we know the sender to ack and can refuse
	// gracefully if the obligation doesn't exist (rather than transitioning
	// then realizing we can't notify anyone).
	ledger, err := loop.LoadPendingLedger(cfg, agentID)
	if err != nil {
		return fmt.Errorf("load pending-reply ledger: %w", err)
	}
	entry, ok := ledger.FindByMessageID(messageID)
	if !ok {
		return fmt.Errorf("no pending-reply ledger entry for message_id %q (use agentchute pending to list)", messageID)
	}
	if entry.Status != loop.PendingReplyStatusPending {
		return fmt.Errorf("ledger entry for %q is already in status %q; cannot defer", messageID, entry.Status)
	}
	// Defense-in-depth: even though LoadPendingLedger validates From/To as
	// agent_ids and RecordPendingReply validates at write time, refuse to act
	// on an entry whose `to` doesn't name us — the ledger is recipient-owned
	// and a mismatched To means a corrupted state file, not a legitimate
	// obligation (codex review on eb58443).
	if err := loop.ValidateAgentID(entry.From); err != nil {
		return fmt.Errorf("ledger entry %q has invalid from: %w", messageID, err)
	}
	if entry.To != agentID {
		return fmt.Errorf("ledger entry %q to=%q does not match --as %q; refusing to act on a mismatched entry", messageID, entry.To, agentID)
	}

	now := time.Now().UTC()
	if err := loop.MarkPendingDeferred(cfg, agentID, messageID, reason, deferredUntil, now); err != nil {
		return fmt.Errorf("mark deferred: %w", err)
	}

	// Compose and send the deferral-acknowledgment back to the original
	// sender. Failures here are warnings, not errors: the local ledger
	// transition has already cleared our gate.
	ackBody := composeDeferralAckBody(entry, reason, deferredUntil)
	ackContent := loop.ComposeMessage(now, agentID, entry.From, "deferred-reply", "info", entry.MessageID, ackBody)

	sendWarning := ""
	senderInbox := cfg.AgentInboxDir(entry.From)
	ackMsg, err := loop.WriteInboxMessage(senderInbox, now, agentID, ackContent)
	switch {
	case err == nil:
		// Poke the sender if pokable; failures are non-fatal.
		regPath := cfg.AgentRegistrationPath(entry.From)
		if reg, regErr := loop.ReadRegistration(regPath); regErr == nil && reg.IsPokable() {
			if pokeErr := loop.PokeRegistration(context.Background(), cfg, reg); pokeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: wake poke to %s failed (%v); ack still delivered\n", entry.From, pokeErr)
			}
		}
	case os.IsNotExist(err):
		sendWarning = fmt.Sprintf("warning: sender %q has no inbox directory (unregistered?); deferral recorded locally, ack not delivered", entry.From)
	default:
		sendWarning = fmt.Sprintf("warning: failed to deliver deferral ack to %s: %v (deferral recorded locally)", entry.From, err)
	}

	fmt.Printf("Deferred %s\n", messageID)
	fmt.Printf("  from:   %s\n", entry.From)
	fmt.Printf("  task:   %s\n", entry.Task)
	fmt.Printf("  reason: %s\n", reason)
	if deferredUntil != "" {
		fmt.Printf("  until:  %s\n", deferredUntil)
	}
	if ackMsg.Filename != "" {
		fmt.Printf("  ack:    %s -> %s\n", ackMsg.Filename, entry.From)
	}
	if sendWarning != "" {
		fmt.Fprintln(os.Stderr, sendWarning)
	}
	return nil
}

// normalizeDeferUntil canonicalizes the --until value into an RFC3339 UTC
// timestamp string. Accepts either a Go duration (e.g. 24h, 90m, 7d-ish via
// composition like 168h for a week) interpreted as offset-from-now, or an
// absolute RFC3339 timestamp. Empty string passes through unchanged
// (no scheduled unblock).
func normalizeDeferUntil(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if d, derr := time.ParseDuration(raw); derr == nil {
		if d <= 0 {
			return "", fmt.Errorf("duration must be positive, got %q", raw)
		}
		return time.Now().UTC().Add(d).Format(time.RFC3339), nil
	}
	if t, terr := time.Parse(time.RFC3339, raw); terr == nil {
		// Reject past timestamps — non-positive durations are already rejected
		// above; a backdated absolute timestamp is similarly likely operator
		// error (codex review on eb58443).
		if !t.After(time.Now()) {
			return "", fmt.Errorf("timestamp must be in the future, got %q", raw)
		}
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("expected Go duration (e.g. 24h) or RFC3339 timestamp, got %q", raw)
}

// composeDeferralAckBody renders the body for the auto-acknowledgment
// message sent to the original sender. Plain-text declarative shape that
// reads well both in `agentchute check` output and to another agent.
func composeDeferralAckBody(entry loop.PendingReplyEntry, reason, deferredUntil string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your message (task: %q, message_id: %s) has been deferred by the recipient.\n\n", entry.Task, entry.MessageID)
	fmt.Fprintf(&b, "reason: %s\n", reason)
	if deferredUntil != "" {
		fmt.Fprintf(&b, "deferred_until: %s\n", deferredUntil)
	}
	b.WriteString("\nThis is an automatic acknowledgment from `agentchute defer`. The recipient's gate is no longer blocked by this obligation; they may reply later or leave the deferral in place indefinitely.\n")
	return b.String()
}

func deferUsage(err error) error {
	if err == flag.ErrHelp {
		return deferHelpErr()
	}
	return fmt.Errorf("%w\n\n%s", err, deferHelp())
}

func deferHelpErr() error {
	return fmt.Errorf("%w\n%s", flag.ErrHelp, deferHelp())
}

func deferHelp() string {
	return strings.TrimSpace(`
Usage: agentchute defer [--vendor <vendor>] [--as <id>] --message <message-id> --reason "..." [--until <duration-or-timestamp>]

Defer a pending-reply obligation. Updates the recipient's pending-reply
ledger entry to status=deferred (which no longer blocks gate --before
finish) and sends an automatic acknowledgment to the original sender.

Flags:
  --as <id>             agent id (or $AGENTCHUTE_AGENT_ID)
  --vendor <vendor>     vendor or origin for contextual identity defaults
  --message <id>        message_id of the obligation to defer (required)
  --reason "..."        human-readable reason for the deferral (required)
  --until <when>        optional Go duration (e.g. 24h, 90m) or RFC3339 timestamp
  --control-repo <p>    control repo path (or $AGENTCHUTE_CONTROL_REPO)
  --loop-dir <p>        loop dir path (or $AGENTCHUTE_LOOP_DIR)
`)
}
