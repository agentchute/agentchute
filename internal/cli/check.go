package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// oldMailBannerAfter is the age threshold (v2.5 plan A3, C18) past which
// `check` announces a message's age above its body. Decision §4: the tool
// surfaces age loudly, the reader judges relevance — no expiry, no auto-action.
const oldMailBannerAfter = 24 * time.Hour

// messageAge returns how long ago msg was sent, relative to now. Age source
// today is always msg.Timestamp (file mtime) — the filename-timestamp
// grammar doesn't exist yet (v2.5 plan B7). This is the one place plan slice
// B6 edits to add the new-format filename-timestamp branch alongside mtime.
func messageAge(msg loop.Message, now time.Time) time.Duration {
	return now.Sub(msg.Timestamp)
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var agentID, vendor, controlRepo, loopDir string
	var noArchive bool
	var limit int
	fs.StringVar(&agentID, "as", "", "agent id to act as (or $AGENTCHUTE_AGENT_ID)")
	fs.StringVar(&vendor, "vendor", "", "vendor or origin (anthropic, openai, google, xai)")
	fs.StringVar(&controlRepo, "control-repo", "", "control repo path (or AGENTCHUTE_CONTROL_REPO)")
	fs.StringVar(&loopDir, "loop-dir", "", "loop dir path (or AGENTCHUTE_LOOP_DIR)")
	fs.BoolVar(&noArchive, "no-archive", false, "dry run: suppress inbox side effects (no archive or quarantine); own last_seen still updates")
	fs.IntVar(&limit, "limit", 0, "process at most N messages this turn (0 = no limit)")

	if err := fs.Parse(args); err != nil {
		return checkUsage(err)
	}
	if fs.NArg() != 0 {
		return checkUsage(fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " ")))
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

	// v2.5 plan A7/C25: defense-in-depth self-denial. The PreToolUse guard
	// (guard.go) already denies a model from invoking `check` a second time
	// while this session holds its own unacked claimed mail; this refuses at
	// the command level too, in case check runs some other way (a stale hook
	// template, a human typing it directly) while guarded and latched. A
	// latch belonging to no session, or a foreign/dead session, never denies.
	if session := resolveGuardSession(); session != "" {
		if latch, lerr := loop.ReadGuardLatch(cfg, agentID); lerr == nil && latch.Session == session {
			return fmt.Errorf("claimed mail pending ack; finish the turn (turn-end) before checking again")
		}
	}

	now := time.Now().UTC()

	// v0.2.1 "Enforced Enrollment" (AGENTCHUTE.md §5.3): refuse to operate
	// for an unregistered agent. check is an active agent command — it
	// archives, quarantines, and sends corrective notify; all of those
	// imply the agent IS enrolled in the pool.
	selfPath := cfg.AgentRegistrationPath(agentID)
	selfExists := false
	if _, err := os.Stat(selfPath); err == nil {
		selfExists = true
		if err := loop.UpdateLastSeen(cfg, agentID, now); err != nil {
			return fmt.Errorf("update last_seen for %s: %w", agentID, err)
		}
	} else if os.IsNotExist(err) {
		return fmt.Errorf("agent %q is not registered. Run `agentchute boot --as %s --vendor <vendor>` first (AGENTCHUTE.md §5.3)", agentID, agentID)
	} else {
		return fmt.Errorf("stat own registration: %w", err)
	}

	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		return fmt.Errorf("list inbox: %w", err)
	}
	// §11 protocol enforcement: for each file that looks like a message
	// attempt but fails the §6.1 reference filename encoding, quarantine
	// it and (best-effort) notify the inferred offender. Expected noise
	// (.DS_Store, .tmp_*, dirs, symlinks) stays silent as before.
	// Enforcement is a state mutation (file moves + outgoing message), so
	// we honor --no-archive and skip it in dry-run mode.
	if !noArchive {
		for _, name := range skipped {
			srcPath := filepath.Join(inboxDir, name)
			quarantined, err := loop.QuarantineInboxFile(srcPath, cfg.MalformedDir(), agentID, now)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to quarantine %s: %v\n", name, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: quarantined %s (malformed §6.1 filename) -> %s\n",
				name, quarantined)
		}
	} else if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d non-§6.1 file(s) in inbox; --no-archive suppressed §11 enforcement:\n", len(skipped))
		for _, name := range skipped {
			fmt.Fprintf(os.Stderr, "  %s\n", name)
		}
	}
	// GATE 5 — two-phase consume (claim → commit). `check` is phase 1: it
	// CLAIMS (moves inbox -> .claimed) and DISPLAYS, but does NOT archive. The
	// separate `ack` verb is phase 2 (COMMIT/archive). The real bug this fixes:
	// the CLI prints and EXITS, then the model acts AFTER check returns — so
	// archiving DURING check (the old behavior) is at-most-once for the WORK. A
	// crash between claim and ack now RE-DELIVERS (at-least-once); handlers must
	// be idempotent.
	claimedDir := cfg.AgentClaimedDir(agentID)

	// v2.5 plan A7/C23: set the guard latch at the FIRST of (a) listing
	// non-empty .claimed residue for redelivery or (b) claiming/displaying any
	// message (including --no-archive). latchArmed makes repeat calls within
	// this one invocation cheap no-ops (maybeSetGuardLatch itself is already
	// idempotent per-session, but a large inbox would otherwise re-take the
	// state lock once per displayed message for no benefit).
	latchArmed := false
	setLatch := func() {
		if latchArmed {
			return
		}
		maybeSetGuardLatch(cfg, agentID)
		latchArmed = true
	}

	// FIRST: re-display any uncommitted residue from a crashed/un-acked prior
	// turn. These were CLAIMED but never COMMITTED (no ack). We re-deliver them
	// with a REDELIVERED banner so the agent re-acts; `ack` archives them.
	redelivered, rerr := loop.ListClaimedMessages(claimedDir)
	if rerr != nil {
		return fmt.Errorf("list claimed residue: %w", rerr)
	}
	if len(redelivered) > 0 {
		setLatch()
	}
	for _, msg := range redelivered {
		content, err := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes)
		if err != nil {
			return fmt.Errorf("read claimed message %s: %w", msg.Path, err)
		}
		displayConsumed(cfg, agentID, msg, content, true, now)
	}

	// (v2.5 plan A3, review fix): the empty-inbox line no longer returns
	// early — it must fall through to the claim loop below (a no-op when
	// msgs is empty) and on to the C19 prune offer at the very end, which
	// needs to run AFTER any obligation-discharging replies in THIS turn's
	// own claim loop have already been processed (see that comment).
	if len(msgs) == 0 && len(redelivered) == 0 {
		fmt.Println("(inbox empty)")
	}

	claimed := 0
	for _, msg := range msgs {
		if limit > 0 && claimed >= limit {
			fmt.Printf("(reached limit of %d; %d more pending)\n", limit, len(msgs)-claimed)
			break
		}
		content, err := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes)
		if err != nil {
			return fmt.Errorf("read message %s: %w", msg.Path, err)
		}

		// §11 enforcement on frontmatter: if the file has an opening `---` but
		// the block doesn't parse, quarantine + notify the (filename-known)
		// sender and skip processing. Body-only messages pass through. Quarantine
		// is a state mutation, so it (like claim) is suppressed under --no-archive.
		if err := loop.ValidateMessageFrontmatter(content); err != nil {
			if noArchive {
				fmt.Fprintf(os.Stderr, "warning: %s has malformed frontmatter (%v); --no-archive suppressed §11 enforcement\n",
					msg.Filename, err)
				claimed++
				continue
			}
			quarantined, qerr := loop.QuarantineInboxFile(msg.Path, cfg.MalformedDir(), agentID, now)
			if qerr != nil {
				fmt.Fprintf(os.Stderr, "warning: %s has malformed frontmatter but quarantine failed: %v\n", msg.Filename, qerr)
				claimed++
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: quarantined %s (malformed §6.4 frontmatter: %v) -> %s\n",
				msg.Filename, err, quarantined)
			claimed++
			continue
		}

		if noArchive {
			// Dry run: DISPLAY in place, do NOT claim/move. The asker-side owed
			// flip (ClearOwed) is a state mutation too, so displayConsumed's
			// no-side-effect display is appropriate here — we pass a read-only
			// flag below.
			setLatch()
			displayConsumedReadOnly(agentID, msg, content, now)
			claimed++
			continue
		}

		// CLAIM (phase 1): move inbox -> .claimed under the canonical name, then
		// display from the claimed copy. NO archive (that is `ack`, phase 2).
		claimedPath, cerr := loop.ClaimMessage(msg, claimedDir)
		if cerr != nil {
			return fmt.Errorf("claim message %s: %w", msg.Filename, cerr)
		}
		msg.Path = claimedPath
		setLatch()
		displayConsumed(cfg, agentID, msg, content, false, now)
		claimed++
	}

	if !noArchive && claimed > 0 {
		fmt.Println("note: messages CLAIMED (at-least-once), not yet archived. Run `agentchute ack` to commit; a crash before ack re-delivers them.")
	}

	// Update last_active per AGENTCHUTE.md §6.3 step 4 if we actually consumed.
	if !noArchive && claimed > 0 && selfExists {
		if err := loop.UpdateLastActive(cfg, agentID, now); err != nil {
			// Non-fatal: messages are claimed; only the timestamp update lost.
			fmt.Fprintf(os.Stderr, "warning: failed to update last_active (%v)\n", err)
		}
	}

	// C19 (v2.5 plan A3): offer this agent's own expired reply obligations
	// for pruning. Print-only — never auto-removes; `agentchute clean --owed`
	// (plan A4) is the explicit, human-triggered command that actually
	// prunes them. Deliberately placed AFTER the claim loop above (review
	// fix): a reply consumed by THIS turn can discharge (ClearOwed) the very
	// obligation being offered here — computing the offer any earlier would
	// print a stale obligation moments before the same turn clears it. Not
	// gated on --no-archive or on there being any mail to claim this turn
	// (the empty-inbox branch above falls through rather than returning), so
	// a returning agent with no new mail still sees a weeks-old obligation.
	if owed, oerr := loop.LoadOwedLedger(cfg, agentID); oerr != nil {
		fmt.Fprintf(os.Stderr, "warning: owed-reply ledger is corrupt or unreadable; inspect `state/%s/owed.json`\n", agentID)
	} else {
		for _, e := range owed.ExpiredOwed(now) {
			fmt.Printf("stale reply obligation (%s, expired %s ago) — prune with: agentchute clean --owed --as %s\n",
				e.Key().RefString(), now.Sub(e.By).Round(time.Second), agentID)
		}
	}

	return nil
}

// maybeSetGuardLatch sets agentID's guard latch for the current guarded
// session (v2.5 plan A7/C23), or no-ops when the guard is disabled for this
// process (resolveGuardSession returns ""). A write failure is surfaced as a
// warning, not a hard error: check's job is delivering mail, and the guard is
// defense-in-depth — a failed latch write must not itself block delivery.
func maybeSetGuardLatch(cfg *loop.Config, agentID string) {
	session := resolveGuardSession()
	if session == "" {
		return
	}
	if err := loop.SetGuardLatch(cfg, agentID, session); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to set guard latch: %v\n", err)
	}
}

// displayConsumed prints one consumed message and runs the asker-side obligation
// flip. `redelivered` toggles the REDELIVERED banner (uncommitted residue from a
// crashed/un-acked prior turn).
//
//   - in_reply_to flip: if the message is a reply that references one of OUR
//     outstanding asks (a canonical MsgID ref keyed From=us), discharge that
//     `.owed` obligation (ClearOwed). Idempotent, so re-display is safe.
//   - reply ref: if the message asks US for a reply, print the copyable ref the
//     reply must carry as --reply-to / in_reply_to so the asker can clear their
//     obligation when they consume our reply.
func displayConsumed(cfg *loop.Config, agentID string, msg loop.Message, content []byte, redelivered bool, now time.Time) {
	printConsumedBody(msg, content, redelivered, now)

	fm := loop.ParseMessageFrontmatter(content)

	// Asker-side owed flip. ClearOwed only touches OUR ledger and only removes a
	// matching key. The ref must name us as asker, and the consumed reply must
	// come from the agent that owed it; otherwise a third party could clear an
	// obligation by echoing someone else's ref.
	if ref := strings.TrimSpace(fm["in_reply_to"]); ref != "" {
		if key, ok := loop.ParseMsgIDRef(ref); ok && key.From == agentID && msg.Sender == key.To {
			if err := loop.ClearOwed(cfg, agentID, key); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clear owed obligation %s: %v\n", ref, err)
			}
		} else if key, ok := loop.ParseTsRef(ref); ok && key.From == agentID && msg.Sender == key.To {
			if err := loop.ClearOwed(cfg, agentID, key); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clear owed obligation %s: %v\n", ref, err)
			}
		}
	}

	printReplyRefIfRequired(agentID, msg, fm)
}

// displayConsumedReadOnly is the --no-archive (dry-run) display: it prints the
// body and the reply ref but performs NO state mutation (no ClearOwed).
func displayConsumedReadOnly(agentID string, msg loop.Message, content []byte, now time.Time) {
	printConsumedBody(msg, content, false, now)
	printReplyRefIfRequired(agentID, msg, loop.ParseMessageFrontmatter(content))
}

// printConsumedBody prints the C18 age banner (v2.5 plan A3) above the
// header when msg is older than oldMailBannerAfter, then the header and body
// exactly as before. The banner is program-generated text (never peer
// content), so it is printed directly and is NOT run through
// sanitizeControlBytes — only the message body needs that treatment.
func printConsumedBody(msg loop.Message, content []byte, redelivered bool, now time.Time) {
	if age := messageAge(msg, now); age > oldMailBannerAfter {
		fmt.Printf("[!] this message is %d days old (sent %s)\n", int(age.Hours()/24), msg.Timestamp.UTC().Format("2006-01-02"))
	}
	if redelivered {
		fmt.Printf("---- %s [REDELIVERED — uncommitted from a prior turn; `agentchute ack` to commit] ----\n", msg.Filename)
	} else {
		fmt.Printf("---- %s ----\n", msg.Filename)
	}
	sanitized := sanitizeControlBytes(string(content))
	fmt.Print(sanitized)
	if !strings.HasSuffix(sanitized, "\n") {
		fmt.Println()
	}
	fmt.Println()
}

// sanitizeControlBytes strips C0/C1 control code points from peer-controlled
// text before it reaches a raw terminal (N3, deep-analysis-v2): a body
// carrying ANSI/OSC escape sequences or bare C1 codes can repaint the
// operator's screen, spoof a prompt, or set the window title. Applied
// unconditionally (not just when stdout is a TTY) because message bodies are
// spec'd UTF-8 free-form text — a control sequence is never legitimate
// payload — and unconditional stripping avoids needing platform-specific
// stdout-TTY detection. \n and \t are the only control code points kept.
func sanitizeControlBytes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// drop: C0 (incl. ESC, CR), DEL, and C1 control code points.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// printReplyRefIfRequired prints the copyable in_reply_to ref a reply to msg must
// carry, when msg is reply_required. The emitted reference matches the message's
// filename identity form so it clears the asker's matching obligation.
func printReplyRefIfRequired(agentID string, msg loop.Message, fm map[string]string) {
	if !isFrontmatterReplyRequired(fm) {
		return
	}

	var ref string
	if from, seq, ok := loop.ParseSeqFilename(msg.Filename); ok {
		ref = (loop.MsgID{To: agentID, From: from, Seq: seq}).RefString()
	} else if id, ok := loop.ParseTsFilename(msg.Filename); ok {
		id.To = agentID
		ref = id.RefString()
	} else {
		return
	}
	fmt.Printf("reply-required: reply with `agentchute send --from %s --to %s --reply-to %s ...`\n\n", agentID, msg.Sender, ref)
}

func checkUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute check [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--no-archive] [--limit <n>]\n  check CLAIMS + displays (at-least-once); run `agentchute ack` to commit (archive).", err)
}

func isFrontmatterReplyRequired(fm map[string]string) bool {
	return strings.ToLower(strings.TrimSpace(fm["reply_required"])) == "true"
}
