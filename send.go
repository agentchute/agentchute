package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// cmdSend writes an inbound message to a recipient's inbox and (best-effort)
// pokes their wake target. Messaging extensions (AGENTCHUTE.md §6.2/§6.4 reply
// obligations, §8 wake adapters):
//   - --ask:        sets reply_required: true frontmatter and prepends a
//     `## ASK` body heading if not already present.
//   - --reply-to:   when the message_id matches a pending entry in OUR
//     pending-reply ledger, transitions that entry to
//     "replied" with reply_sent_at + reply_message_id.
//   - --json:       structured output (filename, path, wake receipt, ledger
//     transition record).
//   - --no-wake:    explicit opt-out of the poke side effect.
//
// Always emits a wake-attempt receipt (wake_method, wake_attempted,
// wake_result) for sender-side visibility into "delivered but not poked"
// states. Independent of --json: text mode adds it; JSON mode includes it.
//
// Warns (to stderr) if the sender's OWN pending-reply ledger has any entries
// from <to> and --reply-to is not provided — catches "agent forgot to clear
// the ledger when replying" (the reply-obligation defense, AGENTCHUTE.md §6.4).
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var fromID, toID, taskField, statusField, body, replyTo, controlRepo, loopDir string
	var ask, jsonOut, noWake bool
	fs.StringVar(&fromID, "from", "", "sender agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&toID, "to", "", "recipient agent id")
	fs.StringVar(&taskField, "task", "", "short task descriptor for the message frontmatter (recommended)")
	fs.StringVar(&statusField, "status", "", "message status frontmatter field (e.g., request, signoff, info)")
	fs.StringVar(&body, "body", "", "message body markdown; if empty, body is read from stdin")
	fs.StringVar(&replyTo, "reply-to", "", "prior message_id this is replying to (clears matching pending-reply ledger entry)")
	fs.BoolVar(&ask, "ask", false, "set reply_required: true and prepend `## ASK` heading to the body")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
	fs.BoolVar(&noWake, "no-wake", false, "skip the wake poke (delivery only)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")

	if err := fs.Parse(args); err != nil {
		return sendUsage(err)
	}
	if fs.NArg() != 0 {
		return sendUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
	}

	toID = strings.TrimSpace(toID)
	if toID == "" {
		return fmt.Errorf("missing --to (recipient agent id)")
	}
	if err := loop.ValidateAgentID(toID); err != nil {
		return fmt.Errorf("--to: %w", err)
	}

	// Keep short-string flags one-line even though loop.ComposeMessage quotes
	// YAML-sensitive scalars. These fields are meant to be compact metadata.
	for _, fld := range []struct{ name, val string }{
		{"--task", taskField},
		{"--status", statusField},
		{"--reply-to", replyTo},
	} {
		if err := rejectFrontmatterInjection(fld.name, fld.val); err != nil {
			return err
		}
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
	fromID, err = resolveAgentID(fromID, "", cfg)
	if err != nil {
		return fmt.Errorf("missing --from; pass explicitly, set AGENTCHUTE_AGENT_ID, or run from a registered herdr/tmux pane")
	}
	if err := loop.ValidateAgentID(fromID); err != nil {
		return fmt.Errorf("--from: %w", err)
	}

	if body == "" {
		// Read stdin only when it's piped/redirected; never block waiting on a
		// human typing into an interactive terminal. If stdin is a character
		// device (TTY), send an empty body and let the caller pass --body
		// explicitly if they want content.
		if info, err := os.Stdin.Stat(); err == nil && (info.Mode()&os.ModeCharDevice) == 0 {
			bodyBytes, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read body from stdin: %w", err)
			}
			body = string(bodyBytes)
		}
	}

	// --ask salience polish: prepend the `## ASK` heading if not already
	// present. Pure body manipulation; the reply_required frontmatter
	// is plumbed via ComposeMessage below.
	if ask {
		body = applyAskHeading(body)
		// Self-send + --ask is a loop hazard per AGENTCHUTE.md §6.4: the
		// sender immediately owes itself a reply. The combination is
		// legitimate (e.g., a deliberate scratch obligation) so we deliver
		// the message, but emit a stderr warning so the operator pauses on
		// the unusual shape. Replies (via --reply-to) MUST NOT propagate
		// --ask — that's the protocol invariant that keeps automated
		// agents from looping. Real-bake-driven, codex review aligned.
		if fromID == toID {
			fmt.Fprintf(os.Stderr, "warning: self-send with --ask creates a self-reply obligation; per AGENTCHUTE.md §6.4 your reply MUST NOT propagate --ask\n")
		}
	}

	// Pre-send: warn if our own pending-reply ledger has entries from this
	// recipient but --reply-to wasn't passed. Best-effort signal — does not
	// block the send (AGENTCHUTE.md §6.4).
	ledgerWarning := ""
	if strings.TrimSpace(replyTo) == "" {
		if senderLedger, lerr := loop.LoadPendingLedger(cfg, fromID); lerr == nil {
			for _, e := range senderLedger.PendingEntries() {
				if e.From == toID {
					ledgerWarning = fmt.Sprintf("warning: you have %d pending reply obligation(s) from %s; consider --reply-to <msg-id> to clear them on send", countFrom(senderLedger.PendingEntries(), toID), toID)
					break
				}
			}
		}
	}

	now := time.Now().UTC()

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): refuse to send
	// from an unregistered agent. The outbound message would carry a
	// `from:` field naming an agent that peers can't discover, wake, or
	// reply to.
	selfPath := cfg.AgentRegistrationPath(fromID)
	if _, err := os.Stat(selfPath); err == nil {
		if err := loop.UpdateLastSeen(cfg, fromID, now); err != nil {
			return fmt.Errorf("update last_seen for %s: %w", fromID, err)
		}
	} else if os.IsNotExist(err) {
		return fmt.Errorf("sender %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", fromID, fromID)
	} else {
		return fmt.Errorf("stat own registration: %w", err)
	}

	messageID := loop.FormatMessageID(now)
	content := loop.ComposeMessage(now, fromID, toID, taskField, statusField, replyTo, body)
	if ask {
		content = applyReplyRequiredFrontmatter(content)
	}

	// Write to recipient's inbox via atomic temp+rename.
	inboxDir := cfg.AgentInboxDir(toID)
	msg, err := loop.WriteInboxMessage(inboxDir, now, fromID, content)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("write inbox message: recipient %q is not registered; run agentchute register --as %s first (%w)", toID, toID, err)
		}
		return fmt.Errorf("write inbox message: %w", err)
	}

	// Wake the recipient (or explicitly skip via --no-wake). Capture the
	// outcome for the sender-side receipt regardless of success.
	receipt := computeWakeReceipt(cfg, toID, noWake)

	// --reply-to ledger clearing: if our own ledger has a pending entry
	// matching this reply-to message_id AND the outbound recipient matches
	// the entry's original sender (i.e., we're actually replying TO whoever
	// asked us), transition the entry to replied. A mismatched recipient
	// (threading via a third party's msg-id while delivering elsewhere)
	// must NOT clear the obligation — the contract per AGENTCHUTE.md §6.4
	// is that the reply is owed to the sender of the reply_required message
	// (codex review on 89ad2d9).
	ledgerTransition := ""
	if strings.TrimSpace(replyTo) != "" {
		ledger, lerr := loop.LoadPendingLedger(cfg, fromID)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to read pending-reply ledger: %v\n", lerr)
		} else {
			// The obligation we may discharge is the one owed to the recipient
			// we are actually replying TO (toID — KNOWN, not inferred from the
			// ledger). Scope every decision by toID: message_id is
			// sender-controlled and reusable, so a reply to one peer must NEVER
			// clear an obligation owed to a DIFFERENT peer (WI-2 follow-up).
			//
			// CRITICAL (rev2): NOTHING here keys on FindByMessageID's first bare
			// row. EntriesByMessageIDFrom(replyTo, toID) returns exactly the rows
			// owed to us BY toID (any status), so inverse ordering — toID's row
			// not being the first bare match — can no longer short-circuit the
			// discharge. FindByMessageID is consulted ONLY to distinguish
			// "threading via another peer's id" from "no such message_id at all"
			// when toID owns nothing under this id.
			scoped := ledger.EntriesByMessageIDFrom(replyTo, toID)
			switch {
			case len(scoped) == 0:
				if other, exists := ledger.FindByMessageID(replyTo); exists {
					// A message_id entry exists but NOT from toID → threading via
					// another peer's id while delivering to toID. Do NOT clear the
					// other peer's obligation.
					fmt.Fprintf(os.Stderr, "warning: --reply-to %s names a message from %q, but this send is to %q; obligation left pending\n", replyTo, other.From, toID)
				}
				// else: no such message_id at all → silent OK. --reply-to is a
				// freeform threading hint; the agent may be threading to a
				// message that never carried reply_required: true.
			case mismatchedTo(scoped, fromID):
				// Mirror cmdDefer's recipient-owned-ledger invariant. The ledger
				// is OURS; if a corrupted state file has any scoped entry.To
				// pointing at someone other than fromID, refuse to act on it
				// (codex review on aa5f0d9 / check.go integration).
				fmt.Fprintf(os.Stderr, "warning: --reply-to %s has a ledger entry whose to does not match --from %q; refusing to clear a mismatched obligation\n", replyTo, fromID)
			case len(ledger.PendingByMessageIDFrom(replyTo, toID)) == 0:
				// toID owns ≥1 row under this message_id, but every one is
				// already terminal. Idempotent no-op note rather than a
				// re-transition.
				ledgerTransition = fmt.Sprintf("note: pending-reply ledger entry %s was already in a terminal status; not re-transitioned", replyTo)
			default:
				// toID owns ≥1 PENDING row. Discharge every pending obligation
				// scoped to (replyTo, toID); a terminal duplicate cannot strand
				// a still-pending one (MarkPendingReplied skips terminals).
				if merr := loop.MarkPendingReplied(cfg, fromID, replyTo, toID, messageID, now); merr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to update pending-reply ledger for %s: %v\n", replyTo, merr)
				} else {
					ledgerTransition = fmt.Sprintf("cleared pending-reply ledger entry %s", replyTo)
				}
			}
		}
	}

	result := sendResult{
		Filename:       msg.Filename,
		Path:           msg.Path,
		From:           fromID,
		To:             toID,
		MessageID:      messageID,
		WakeMethod:     receipt.method,
		WakeAttempted:  receipt.attempted,
		WakeResult:     receipt.result,
		ReplyToCleared: ledgerTransition,
	}

	if jsonOut {
		if err := emitSendJSON(result); err != nil {
			return err
		}
	} else {
		emitSendText(result)
	}

	if ledgerWarning != "" {
		fmt.Fprintln(os.Stderr, ledgerWarning)
	}
	return nil
}

// sendResult is the structured shape of `send`'s output (the same fields
// drive both text and --json modes).
type sendResult struct {
	Filename       string `json:"filename"`
	Path           string `json:"path"`
	From           string `json:"from"`
	To             string `json:"to"`
	MessageID      string `json:"message_id"`
	WakeMethod     string `json:"wake_method"`
	WakeAttempted  bool   `json:"wake_attempted"`
	WakeResult     string `json:"wake_result"`
	ReplyToCleared string `json:"reply_to_cleared,omitempty"`
}

type wakeReceipt struct {
	method    string
	attempted bool
	result    string // "ok" | "failed" | "skipped (no method declared)" | "skipped (--no-wake)" | "skipped (recipient unregistered)"
}

// computeWakeReceipt looks up the recipient's wake_method/target and
// (unless --no-wake) attempts the poke. Returns the receipt describing
// what was attempted and the outcome. Never returns an error — failures
// are recorded in the receipt and surfaced to the operator.
func computeWakeReceipt(cfg *loop.Config, toID string, noWake bool) wakeReceipt {
	if noWake {
		return wakeReceipt{method: "none", attempted: false, result: "skipped (--no-wake)"}
	}
	regPath := cfg.AgentRegistrationPath(toID)
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeReceipt{method: "none", attempted: false, result: "skipped (recipient unregistered)"}
		}
		return wakeReceipt{method: "none", attempted: false, result: fmt.Sprintf("skipped (registration read error: %v)", err)}
	}
	if !reg.IsPokable() {
		return wakeReceipt{method: reg.WakeMethod, attempted: false, result: "skipped (no method declared)"}
	}
	// Recipient-binding for a runner wake_target: refuse to dial a unix: socket
	// whose path the recipient does not legitimately own (e.g. a hand-written
	// registration naming unix:/tmp/evil.sock). The pure shape validator can't
	// see the recipient id; this check can.
	//
	// We keep this explicit preflight purely so the operator receipt can
	// distinguish "refused" (attempted=false) from a dial "failed"
	// (attempted=true). PokeRegistration runs the same owned-check internally
	// (see pokeEndpoint); this preflight only sharpens the receipt wording.
	if reg.WakeMethod == loop.RunnerWakeMethod {
		if err := cfg.RunnerWakeTargetOwnedBy(toID, reg.WakeTarget); err != nil {
			return wakeReceipt{method: reg.WakeMethod, attempted: false, result: fmt.Sprintf("refused (%v)", err)}
		}
	}
	if err := loop.PokeRegistration(context.Background(), cfg, reg); err != nil {
		return wakeReceipt{method: reg.WakeMethod, attempted: true, result: fmt.Sprintf("failed (%v)", err)}
	}
	return wakeReceipt{method: reg.WakeMethod, attempted: true, result: "ok"}
}

func emitSendText(r sendResult) {
	fmt.Printf("Sent %s\n", r.Filename)
	fmt.Printf("  from:           %s\n", r.From)
	fmt.Printf("  to:             %s\n", r.To)
	fmt.Printf("  path:           %s\n", r.Path)
	fmt.Printf("  wake_method:    %s\n", r.WakeMethod)
	fmt.Printf("  wake_attempted: %s\n", yesno(r.WakeAttempted))
	fmt.Printf("  wake_result:    %s\n", r.WakeResult)
	if r.ReplyToCleared != "" {
		fmt.Printf("  reply_to:       %s\n", r.ReplyToCleared)
	}
	if r.WakeMethod == loop.RunnerWakeMethod && r.WakeAttempted && !strings.HasPrefix(r.WakeResult, "ok") {
		fmt.Printf("  note: runner for %s unreachable; mail delivered to inbox; recipient will see on next start via shim\n", r.To)
	}
}

func emitSendJSON(r sendResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// countFrom is a small helper for the pre-send ledger warning. Linear scan
// over a slice that's small in practice (single-digit pending entries per
// agent).
func countFrom(entries []loop.PendingReplyEntry, from string) int {
	n := 0
	for _, e := range entries {
		if e.From == from {
			n++
		}
	}
	return n
}

// mismatchedTo reports whether ANY scoped ledger entry has a `to` that is not
// fromID. The ledger is recipient-owned, so every legitimate entry's To must
// name us (fromID); a single mismatch means a corrupted state file and the
// whole sender-scoped discharge must be refused (rev2 recipient-owned invariant,
// keyed on the scoped set rather than the first bare row).
func mismatchedTo(entries []loop.PendingReplyEntry, fromID string) bool {
	for _, e := range entries {
		if e.To != fromID {
			return true
		}
	}
	return false
}

// applyAskHeading prepends a `## ASK` heading if the body doesn't already
// start with one. Two leading newlines after the heading match the
// composed-message body shape; an empty body becomes "## ASK\n\n" so
// `agentchute pending` still surfaces the salience marker.
func applyAskHeading(body string) string {
	trimmed := strings.TrimLeft(body, "\n\r ")
	if strings.HasPrefix(trimmed, "## ASK") || strings.HasPrefix(trimmed, "##ASK") {
		return body
	}
	if trimmed == "" {
		return "## ASK\n\n"
	}
	return "## ASK\n\n" + body
}

// applyReplyRequiredFrontmatter inserts `reply_required: true` into the
// frontmatter block of an already-composed message. Splices it just before
// the closing `---` delimiter; idempotent if the field is already present.
// Operates on the byte slice produced by ComposeMessage rather than rebuilding
// the message from scratch so we don't have to thread reply_required through
// the ComposeMessage signature for one flag.
func applyReplyRequiredFrontmatter(content []byte) []byte {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return content
	}
	rest := s[4:]
	closeIdx := strings.Index(rest, "\n---")
	if closeIdx < 0 {
		return content
	}
	fm := rest[:closeIdx]
	body := rest[closeIdx:]
	// Line/key-scoped idempotence: scanning for the substring
	// "reply_required:" anywhere in fm would false-positive when a task or
	// status value happens to contain that text (e.g.
	// `task: "reply_required: audit"`). Walk the frontmatter line by line
	// and check only the bare key (codex review on 89ad2d9).
	for _, line := range strings.Split(fm, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "reply_required:") {
			return content
		}
	}
	return []byte("---\n" + fm + "\nreply_required: true" + body)
}

func sendUsage(err error) error {
	return fmt.Errorf(`%w
usage: agentchute send --from <sender> --to <recipient> [--task <text>] [--status <status>] [--reply-to <msg-id>] [--ask] [--body <text>] [--json] [--no-wake] [--control-repo <path>] [--loop-dir <path>]

  Ways to provide the body (pick one):
    --body "literal text"             short replies
    < body.md                          multi-line body from a file (preferred in restricted shells)
    cat body.md | agentchute send ...    same stdin path via pipe
    --body "$(cat body.md)"            normal shells only; blocked by some sandboxes`, err)
}

func rejectFrontmatterInjection(name, val string) error {
	if strings.ContainsAny(val, "\n\r") {
		return fmt.Errorf("%s: newlines are not allowed", name)
	}
	if strings.TrimSpace(val) == "---" {
		return fmt.Errorf("%s: frontmatter delimiter %q is not allowed", name, "---")
	}
	return nil
}
