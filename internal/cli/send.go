package cli

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

// cmdSend writes an inbound message to a recipient's inbox. Pull-only: delivery
// is unconditional (write the inbox file); senders never poke a wake target.
// Messaging extensions (AGENTCHUTE.md §6 reply obligations):
//   - --ask:        sets reply_required: true frontmatter, prepends a `## ASK`
//     body heading if not already present, and records an ASKER-OWNED `.owed`
//     obligation (the sole reply-obligation mechanism, v0.9.0).
//   - --reply-to:   emits the `in_reply_to` frontmatter ref. When the asker
//     consumes this reply, their `.owed` obligation for the referenced
//     (to,from,seq) discharges (ClearOwed, check.go). There is NO recipient-side
//     ledger — reply obligations are asker-owned only.
//   - --json:       structured output (filename, path).
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var fromID, toID, body, replyTo, controlRepo, loopDir string
	var ask, jsonOut bool
	var replyBy time.Duration
	fs.StringVar(&fromID, "from", "", "sender agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&toID, "to", "", "recipient agent id")
	fs.StringVar(&body, "body", "", "message body markdown; if empty, body is read from stdin")
	fs.StringVar(&replyTo, "reply-to", "", "prior message ref this is replying to (emitted as in_reply_to; discharges the asker's .owed obligation when they consume it)")
	fs.BoolVar(&ask, "ask", false, "set reply_required: true and prepend `## ASK` heading to the body")
	fs.DurationVar(&replyBy, "reply-by", 0, "with --ask: override the owed-reply deadline (e.g. 1h; default 30m)")
	fs.BoolVar(&jsonOut, "json", false, "structured JSON output")
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
	// YAML-sensitive scalars. This field is meant to be compact metadata.
	for _, fld := range []struct{ name, val string }{
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
		return fmt.Errorf("missing --from; pass --from explicitly or set AGENTCHUTE_AGENT_ID")
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

	now := time.Now().UTC()

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): refuse to send
	// from an unregistered agent. The outbound message would carry a
	// `from:` field naming an agent that peers can't discover or
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

	content := loop.ComposeMessage(fromID, replyTo, body)
	if ask {
		content = applyReplyRequiredFrontmatter(content)
	}

	// Land the message under the canonical (to,from,seq) identity (Gate 4):
	// `to` is encoded by the inbox directory, (from,seq) by the filename. The
	// durable per-(from,to) seq replaces the legacy crypto/rand nonce — it makes
	// the lexicographic inbox sort exact per-sender FIFO (the live O1 fix) and
	// folds delivery-dedup into the substrate (link-EEXIST on a resend).
	inboxDir := cfg.AgentInboxDir(toID)
	// Preflight the recipient inbox BEFORE allocating a seq. AllocateSeq durably
	// bumps the sender's (from,to) counter before writeSeqMessage checks the
	// inbox, so a send to a missing/unregistered recipient would otherwise burn
	// a seq (a legal gap) and persist sender state before the os.ErrNotExist
	// surfaces. This stat keeps the old no-side-effect-on-bad-recipient behavior
	// and the same remediation message.
	if fi, statErr := os.Stat(inboxDir); statErr != nil || !fi.IsDir() {
		return fmt.Errorf("write inbox message: recipient %q is not registered; run agentchute register --as %s first (%w)", toID, toID, os.ErrNotExist)
	}
	// idempotencyKey is "": send has no stable per-message content key, so a
	// sender crash between the durable seq commit and the link loses the
	// allocated seq as a legal gap (at-most-once for this message). Acceptable
	// for the transition. serveToken rides AGENTCHUTE_SERVE_TOKEN (Gate 6b): a
	// send from a child launched under `agentchute serve` carries the runner's
	// active serve-lease fence, so a write from a fenced (reclaimed) agent fails
	// closed (AllocateSeq VerifyFence -> ErrFenced). Empty env (no serve lease) =>
	// unfenced, the transitional off-bus mode (unchanged behavior).
	id, err := loop.SendSeqMessage(cfg, fromID, toID, content, "", os.Getenv("AGENTCHUTE_SERVE_TOKEN"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("write inbox message: recipient %q is not registered; run agentchute register --as %s first (%w)", toID, toID, err)
		}
		return fmt.Errorf("write inbox message: %w", err)
	}
	// The on-wire identity is (to,from,seq): `to` is the inbox directory, (from,seq)
	// the filename. No sender-asserted message_id is emitted (v0.9.0).
	msg := loop.Message{Filename: id.Filename(), Path: filepath.Join(inboxDir, id.Filename())}

	// Asker-owned obligation (protocol-v2 / Gate 5): when we ASK for a reply,
	// record that WE are owed a reply to (to=recipient, from=us, seq) by a
	// deadline. This is the NEW obligation authority — held ASKER-side in `.owed`
	// (not the recipient's pending ledger), surfaced by our OWN gate as a
	// non-blocking dead-recipient warning. The recipient echoes id.RefString() as
	// their reply's in_reply_to; our `check` then discharges it (ClearOwed). A
	// failure here is loud: an ask without a recorded obligation is a silent leak.
	if ask {
		deadline := now.Add(loop.ReplyOwedDeadline)
		if replyBy > 0 {
			deadline = now.Add(replyBy)
		}
		if err := loop.RecordOwed(cfg, fromID, id, deadline, now); err != nil {
			return fmt.Errorf("record owed obligation for %s: %w", id.Filename(), err)
		}
	}

	// Reply obligations are asker-owned only (v0.9.0): --reply-to carries the
	// `in_reply_to` ref (emitted by ComposeMessage above) so the ASKER's `.owed`
	// obligation discharges when they consume this reply (ClearOwed, check.go).
	// There is NO recipient-side ledger to mutate here.
	result := sendResult{
		Filename: msg.Filename,
		Path:     msg.Path,
		From:     fromID,
		To:       toID,
	}

	if jsonOut {
		if err := emitSendJSON(result); err != nil {
			return err
		}
	} else {
		emitSendText(result)
	}
	return nil
}

// sendResult is the structured shape of `send`'s output (the same fields
// drive both text and --json modes).
type sendResult struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	From     string `json:"from"`
	To       string `json:"to"`
}

func emitSendText(r sendResult) {
	fmt.Printf("Sent %s\n", r.Filename)
	fmt.Printf("  from:           %s\n", r.From)
	fmt.Printf("  to:             %s\n", r.To)
	fmt.Printf("  path:           %s\n", r.Path)
}

func emitSendJSON(r sendResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
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
	// "reply_required:" anywhere in fm would false-positive if a scalar
	// value ever happened to contain that text. Walk the frontmatter line
	// by line and check only the bare key (codex review on 89ad2d9; the
	// task/status fields that originally motivated this are gone, P1, but
	// the defensive line-scoping still guards in_reply_to/idempotency_key).
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
usage: agentchute send --from <sender> --to <recipient> [--reply-to <ref>] [--ask] [--reply-by <dur>] [--body <text>] [--json] [--control-repo <path>] [--loop-dir <path>]

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
