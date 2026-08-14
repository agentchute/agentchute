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

// afterSendPreflightHook is a test seam for the recipient-disappears race:
// preflight has passed and the body has been read, but delivery has not begun.
var afterSendPreflightHook func()

var sendTsMessageWithCommit = loop.SendTsMessageWithCommit

var sendStdin = os.Stdin

// cmdSend writes an inbound message to a recipient's inbox. Pull-only: delivery
// is unconditional (write the inbox file); senders never poke a wake target.
// Messaging extensions (AGENTCHUTE.md §6 reply obligations):
//   - --ask:        sets reply_required: true frontmatter, prepends a `## ASK`
//     body heading if not already present, and records an ASKER-OWNED `.owed`
//     obligation (the sole reply-obligation mechanism, v0.9.0).
//   - --reply-to:   emits the `in_reply_to` frontmatter ref. When the asker
//     consumes this reply, their `.owed` obligation for the referenced
//     identity discharges (ClearOwed, check.go — both ref grammars are
//     accepted). There is NO recipient-side ledger — reply obligations are
//     asker-owned only.
//   - --json:       structured output (filename, path).
func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var fromID, toID, body, bodyFile, replyTo, controlRepo, loopDir string
	var ask, jsonOut bool
	var replyBy time.Duration
	fs.StringVar(&fromID, "from", "", "sender agent id (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&toID, "to", "", "recipient agent id")
	fs.StringVar(&body, "body", "", "message body markdown; if empty, body is read from stdin")
	fs.StringVar(&bodyFile, "body-file", "", "read the body verbatim from this file (no shell redirection; the only multi-line body form that works while the §15 guard latch is held)")
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
	var replyBySet, replyToSet, bodySet, bodyFileSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "reply-by":
			replyBySet = true
		case "reply-to":
			replyToSet = true
		case "body":
			bodySet = true
		case "body-file":
			bodyFileSet = true
		}
	})

	// Flag PRESENCE, not emptiness: `--body ""` is a legal explicit empty body,
	// so testing `body != ""` here would silently prefer the file instead of
	// reporting the contradiction the caller actually typed.
	if bodySet && bodyFileSet {
		return sendUsage(fmt.Errorf("--body and --body-file are mutually exclusive; pick one body source"))
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
	// Read --body-file BEFORE every preflight below. Discover is pure path
	// resolution (no side effects), so this is the earliest point at which the
	// loop dir is known — and the loop dir is what the state/ refusal needs.
	// Reporting a bad path here means the operator sees the mistake they
	// actually made, instead of an unrelated registration/freshness complaint
	// whose outcome depends on live pool state.
	if bodyFileSet {
		body, err = readSendBodyFile(cfg, bodyFile)
		if err != nil {
			return err
		}
	}

	fromID, err = resolveAgentID(fromID)
	if err != nil {
		return err
	}
	if err := loop.ValidateAgentID(fromID); err != nil {
		return fmt.Errorf("--from: %w", err)
	}

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): refuse invalid
	// sender or recipient state before reading stdin, so a piped body remains
	// untouched on every preflight failure.
	// B1: CLI touches no longer refresh liveness — only serve's lease-gated
	// heartbeat does (HeartbeatRegistration). This preflight only confirms
	// the sender is enrolled at all.
	selfPath := cfg.AgentRegistrationPath(fromID)
	if _, err := os.Stat(selfPath); err == nil {
		// registered; proceed.
	} else if os.IsNotExist(err) {
		return fmt.Errorf("sender %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", fromID, fromID)
	} else {
		return fmt.Errorf("stat own registration: %w", err)
	}

	// B3 (v2.5 plan): the recipient preflight is a LOCK-FREE read of to's
	// registration — never WithAgentLock(toID) here, whose ensurePrivateDir
	// side effect would manufacture state/<toID>/ for an arbitrary --to typo
	// (§4 risk; TestSendTakesNoLockForUnknownRecipient pins it). This is a
	// fast-fail optimization only, run BEFORE stdin so a piped body is never
	// touched on a doomed send (A5 ordering) — the actual enforcement is the
	// re-check inside loop.DeliverUnderRecipientLock, which a sweep or a
	// heartbeat can always race against between here and there.
	inboxDir := cfg.AgentInboxDir(toID)
	now := time.Now().UTC()
	rr, rrErr := loop.CheckRecipientReachability(cfg, toID, now)
	if rrErr != nil {
		if errors.Is(rrErr, loop.ErrRecipientUnknown) {
			return unknownRecipientError(toID, rrErr)
		}
		if errors.Is(rrErr, loop.ErrRecipientUnreadable) {
			return unreadableRecipientError(toID)
		}
		return rrErr
	}
	if !rr.Fresh {
		return staleRecipientError(toID, rr)
	}

	// `--body-file` is an explicit body source even when the file is empty: an
	// empty file must stay an empty body, never fall through and pick up
	// whatever happens to be on stdin. (`--body ""` keeps its pre-existing
	// fall-through behavior; changing that is not this flag's business.)
	if body == "" && !bodyFileSet {
		// Read stdin only when it's piped/redirected; never block waiting on a
		// human typing into an interactive terminal. If stdin is a character
		// device (TTY), send an empty body and let the caller pass --body
		// explicitly if they want content.
		if info, err := sendStdin.Stat(); err == nil && (info.Mode()&os.ModeCharDevice) == 0 {
			bodyBytes, err := io.ReadAll(sendStdin)
			if err != nil {
				return fmt.Errorf("read body from stdin: %w", err)
			}
			body = string(bodyBytes)
		}
	}
	rawBody := body

	// --ask salience polish: prepend the `## ASK` heading if not already
	// present. Pure body manipulation; the reply_required frontmatter
	// is plumbed via ComposeMessage below.
	if ask {
		// The done-when warning (v2.5 plan B9) was removed here (post-1.5.x
		// friction program, item 3): it grepped for the single literal
		// spelling "done-when", but AGENTS.md's Communication Rules rule 2
		// requires only a stated, verifiable completion condition in
		// free-form style ("no required label order... style is free") —
		// no spelling is mandated, so a one-spelling substring check
		// false-positived on every legitimate variant, including the
		// retired six-label envelope's ACCEPTANCE:. Widening the check to
		// a second hardcoded label would only move the same false positive
		// to the next spelling someone reasonably chooses; the rule itself
		// stays a prose covenant, not a mechanically checkable one.
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

	content := loop.ComposeMessage(fromID, replyTo, body)
	if ask {
		content = applyReplyRequiredFrontmatter(content)
	}

	// Land the message under a new timestamp+random-suffix identity (v2.5 plan
	// B7, Gate 4): `to` is encoded by the inbox directory; from/stamp/suffix by
	// the filename. Delivery is AT-MOST-ONCE: a sender crash between the
	// write-ahead floor commit and the link loses the minted stamp as a legal
	// gap — there is no idempotency key or resend-dedup path (deleted with the
	// old per-(from,to) seq allocator). Consume remains AT-LEAST-ONCE via the
	// existing claim/ack two-phase; handler idempotency is the covenant, not
	// delivery-side dedup.
	if afterSendPreflightHook != nil {
		afterSendPreflightHook()
	}
	// serveToken rides AGENTCHUTE_SERVE_TOKEN: a send from a child launched
	// under `agentchute serve` carries the runner's active serve-lease fence,
	// so a write from a fenced (reclaimed) agent fails closed (MintSendStamp's
	// VerifyFence -> ErrFenced). Empty env (no serve lease) => intentionally
	// unfenced.
	id, committed, sendErr := sendTsMessageWithCommit(cfg, fromID, toID, content, os.Getenv("AGENTCHUTE_SERVE_TOKEN"))
	retry := sendRetryOptions{
		Ask:        ask,
		ReplyBy:    replyBy.String(),
		ReplyBySet: replyBySet,
		ReplyTo:    replyTo,
		ReplyToSet: replyToSet,
	}
	if sendErr != nil && !committed {
		return preserveSendBody(cfg, fromID, toID, rawBody, now, retry, sendErr)
	}
	// The on-wire identity is (to,from,timestamp,suffix) (v2.5 plan B7): `to`
	// is the inbox directory, from/stamp/suffix the filename. `id` here is the
	// identity DeliverUnderRecipientLock actually committed, whose suffix may
	// differ from what was first proposed if a link collision forced a
	// fresh-suffix retry (C4). No sender-asserted message_id is emitted.
	msg := loop.Message{Filename: id.Filename(), Path: filepath.Join(inboxDir, id.Filename())}
	result := sendResult{
		Filename: msg.Filename,
		Path:     msg.Path,
		From:     fromID,
		To:       toID,
	}
	if sendErr != nil {
		if err := emitSendResult(result, jsonOut); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "WARNING: message delivered but inbox durability sync failed: %v. Do NOT resend.\n", sendErr)
	}

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
			if sendErr == nil {
				if emitErr := emitSendResult(result, jsonOut); emitErr != nil {
					return emitErr
				}
			}
			fmt.Fprintf(os.Stderr, "WARNING: reply-obligation bookkeeping failed: %v. Do NOT resend.\n", err)
			return nil
		}
	}

	// Reply obligations are asker-owned only (v0.9.0): --reply-to carries the
	// `in_reply_to` ref (emitted by ComposeMessage above) so the ASKER's `.owed`
	// obligation discharges when they consume this reply (ClearOwed, check.go).
	// There is NO recipient-side ledger to mutate here.
	if sendErr != nil {
		return nil
	}
	return emitSendResult(result, jsonOut)
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

func emitSendResult(r sendResult, jsonOut bool) error {
	if jsonOut {
		return emitSendJSON(r)
	}
	emitSendText(r)
	return nil
}

type sendRetryOptions struct {
	Ask        bool
	ReplyBy    string
	ReplyBySet bool
	ReplyTo    string
	ReplyToSet bool
}

// C29(a): never-registered. Text is literal per the v2.5 plan — "no
// registration row" (not "no inbox/registration") and an explicit "do NOT
// register on their behalf" so no reading of this text can be mistaken for
// coaching the sender to register the RECIPIENT (AGENTCHUTE.md §9).
func unknownRecipientError(to string, cause error) error {
	return unknownRecipientSendError{to: to, cause: cause}
}

type unknownRecipientSendError struct {
	to    string
	cause error
}

func (e unknownRecipientSendError) Error() string {
	return fmt.Sprintf("unknown agent %q: no registration row. Check the id (agentchute status) — do NOT register on their behalf.", e.to)
}

func (e unknownRecipientSendError) Unwrap() error {
	return e.cause
}

// C29(b): stale, caught by cmdSend's OWN lock-free preflight — a direct,
// non-racing classification (no send has been attempted yet; stdin has not
// even been read).
func staleRecipientError(to string, rr loop.RecipientReachability) error {
	return fmt.Errorf("%q was here, gone since %s (%s ago); not sending (row older than stale_after=%s). They re-register at boot.",
		to, rr.LastSeen.UTC().Format(time.RFC3339), rr.Age.Round(time.Second), rr.Threshold)
}

// C29(c): fresh-but-racing. Reached ONLY from loop.DeliverUnderRecipientLock
// returning *loop.ErrRecipientStale — which, by construction, is reachable in
// cmdSend's flow ONLY after its OWN preflight already found `to` fresh. Text
// deliberately does not repeat "stale"/"gone since": the row was here
// moments ago, so the honest read is a race (a fleet-wake storm mid-restart),
// not the same failure C29(b) describes.
func racingRecipientError(to string) error {
	return fmt.Errorf("%q was here seconds ago — likely mid-restart; retry once.", to)
}

// Malformed row: neither C29(a) (no row exists) nor C29(b)/(c) (a row exists
// and is stale/racing) — a row that fails to parse tells us nothing about
// whether `to` is reachable, so it gets its own text rather than being
// folded into either. Telling an operator "was here, gone since <time>"
// about a file that failed to parse would be actively misleading (codex/
// claude-code review, PR #95 P1).
func unreadableRecipientError(to string) error {
	return fmt.Errorf("%q's registration could not be read (malformed); not sending. Inspect agents/%s.md by hand.", to, to)
}

// classifySendFailure maps a post-stdin delivery failure to its C29 text.
// Reached only after cmdSend's own preflight already passed, so a stale
// classification here is always the racing case (c), never the direct
// stale case (b) — that one is caught earlier, before any spool/retry
// machinery even runs.
func classifySendFailure(to string, cause error) error {
	if errors.Is(cause, loop.ErrRecipientUnknown) {
		return unknownRecipientError(to, cause)
	}
	if errors.Is(cause, loop.ErrRecipientUnreadable) {
		return unreadableRecipientError(to)
	}
	if os.IsNotExist(cause) {
		// A registration can be fresh while its inbox dir is unexpectedly
		// gone (an inconsistent-state edge case, not a normal C29 branch);
		// same "unknown agent" text applies since the recipient is not
		// reachable either way.
		return unknownRecipientError(to, cause)
	}
	var staleErr *loop.ErrRecipientStale
	if errors.As(cause, &staleErr) {
		return racingRecipientError(to)
	}
	return fmt.Errorf("write inbox message: %w", cause)
}

func preserveSendBody(cfg *loop.Config, from, to, body string, now time.Time, retry sendRetryOptions, cause error) error {
	spoolPath, spoolErr := writeSendSpool(cfg, from, to, body, now)
	baseErr := classifySendFailure(to, cause)
	if spoolErr != nil {
		return fmt.Errorf("%w; body preservation failed: %v", baseErr, spoolErr)
	}
	return fmt.Errorf("%w\nbody preserved at %s; retry with: %s", baseErr, spoolPath, sendRetryCommand(from, to, spoolPath, retry))
}

func writeSendSpool(cfg *loop.Config, from, to, body string, now time.Time) (string, error) {
	spoolDir := filepath.Join(cfg.AgentStateDir(from), "spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(spoolDir, formatSendSpoolStamp(now)+"_to-"+to+".md")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := io.WriteString(f, body); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	cleanup = false
	return path, nil
}

func formatSendSpoolStamp(t time.Time) string {
	t = t.UTC()
	return t.Format("20060102T150405") + fmt.Sprintf("%06dZ", t.Nanosecond()/1000)
}

func sendRetryCommand(from, to, spoolPath string, opts sendRetryOptions) string {
	parts := []string{"agentchute", "send", "--to", to, "--from", from}
	if opts.Ask {
		parts = append(parts, "--ask")
	}
	if opts.ReplyBySet {
		parts = append(parts, "--reply-by", shellQuote(opts.ReplyBy))
	}
	if opts.ReplyToSet {
		parts = append(parts, "--reply-to", shellQuote(opts.ReplyTo))
	}
	return strings.Join(parts, " ") + " < " + shellQuote(spoolPath)
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
	// "reply_required:" anywhere in fm would false-positive when
	// in_reply_to's value contains that text — e.g. a --reply-to value of
	// "reply_required: audit" is quoted onto one line (`in_reply_to:
	// "reply_required: audit"`) but still contains the substring. Walk the
	// frontmatter line by line and check only the bare key (codex review
	// on 89ad2d9; see TestSendAskWithMisleadingReplyToValueStillSetsFrontmatter).
	for _, line := range strings.Split(fm, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "reply_required:") {
			return content
		}
	}
	return []byte("---\n" + fm + "\nreply_required: true" + body)
}

// readSendBodyFile reads a --body-file path into a message body. The binary
// opens the file itself precisely so the invocation contains no shell
// redirection: `send ... --body-file reply.md` tokenizes as plain inert words
// under the §15 guard's inert-direct-send tokenizer, which is what makes a
// real multi-line reply possible in the same turn the mail was claimed (every
// other multi-line form — `< file`, a pipe, `--body "$(cat file)"` — is
// executable syntax and is denied). guard_test.go's
// TestGuardDirectSendDataSinkException pins that tokenization; nothing in
// guard.go had to change for it.
func readSendBodyFile(cfg *loop.Config, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("--body-file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("--body-file: %s is a directory, not a file", path)
	}
	if err := rejectLoopStateBodyFile(cfg, path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--body-file: %w", err)
	}
	return string(data), nil
}

// rejectLoopStateBodyFile refuses a --body-file path that resolves inside the
// loop's `state/` tree.
//
// Why this bound exists: while the guard latch is held, a lane has no way to
// read a file at all — every read primitive is executable shell syntax the
// tokenizer denies. --body-file adds exactly one, and its output goes straight
// into another agent's inbox. `state/<id>/serve.claim` holds `serve_token`,
// the live 128-bit fence epoch (loop/lease.go), so an unbounded --body-file
// would re-create the exfiltration path the tokenizer's own comment exists to
// close — the reason `$AGENTCHUTE_SERVE_TOKEN` is rejected inside a quoted
// body there. Refusing the whole `state/` subtree costs one stat and covers
// serve.claim, guard.latch, and anything future that lands beside them.
//
// Scoped to `state/` ONLY, deliberately: inbox/, archive/, and agents/ are
// wire-protocol-public to every peer in the pool, so quoting one back to a
// peer is ordinary coordination. This is a bound on an obvious foot-gun, not a
// security boundary — the same caveat guard.go carries. An unlatched lane can
// still `cat` any file into --body, and a latched one can rename a state file
// first. It stops the accidental and the naively-injected case, nothing more.
func rejectLoopStateBodyFile(cfg *loop.Config, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("--body-file: %w", err)
	}
	// Resolve symlinks on both sides: comparing lexical paths alone would let
	// a symlink pointing into state/ walk straight around this check.
	target := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		target = resolved
	}
	loopDir, err := filepath.Abs(cfg.LoopDir)
	if err != nil {
		return fmt.Errorf("--body-file: resolve loop dir: %w", err)
	}
	stateRoot := filepath.Join(loopDir, "state")
	if resolved, err := filepath.EvalSymlinks(stateRoot); err == nil {
		stateRoot = resolved
	} else if resolvedLoop, err := filepath.EvalSymlinks(loopDir); err == nil {
		stateRoot = filepath.Join(resolvedLoop, "state")
	}

	// Both paths are absolute by construction, so Rel can only fail when they
	// sit on different volumes — which means target cannot be inside stateRoot.
	rel, err := filepath.Rel(stateRoot, target)
	if err != nil {
		return nil
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("--body-file: refusing to read %s: it is inside the loop's state/ tree, which holds serve.claim (the live serve token) — state files are never a message body", path)
	}
	return nil
}

func sendUsage(err error) error {
	return fmt.Errorf(`%w
usage: agentchute send --from <sender> --to <recipient> [--reply-to <ref>] [--ask] [--reply-by <dur>] [--body <text> | --body-file <path>] [--json] [--control-repo <path>] [--loop-dir <path>]

  Ways to provide the body (pick one):
    --body "literal text"             short replies
    --body-file body.md               multi-line body; the binary reads the file
                                      itself, so the command carries no shell
                                      syntax — the ONLY multi-line form that
                                      still works while the guard latch is held
    < body.md                          multi-line body via stdin redirection
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
