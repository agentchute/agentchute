package main

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
	fs.BoolVar(&noArchive, "no-archive", false, "dry run: suppress inbox side effects (no archive, quarantine, or corrective sends); own last_seen still updates")
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

			// Sender inference order per §11.1: filename capture → frontmatter
			// from: → no notify.
			offender, ok := loop.InferSenderFromFilename(name)
			if !ok {
				offender, ok = loop.InferSenderFromFrontmatter(quarantined)
			}
			if !ok {
				fmt.Fprintf(os.Stderr, "  offender unidentifiable; corrective notify skipped\n")
				continue
			}
			if offender == agentID {
				fmt.Fprintf(os.Stderr, "  inferred offender is self; corrective notify skipped\n")
				continue
			}
			if _, err := loop.SendCorrective(cfg, agentID, offender,
				quarantined, "filename does not match §6.1", "§6.1", now); err != nil {
				fmt.Fprintf(os.Stderr, "  corrective send to %s failed: %v\n", offender, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "  notified %s\n", offender)
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

	// FIRST: re-display any uncommitted residue from a crashed/un-acked prior
	// turn. These were CLAIMED but never COMMITTED (no ack). We re-deliver them
	// with a REDELIVERED banner so the agent re-acts; `ack` archives them.
	redelivered, rerr := loop.ListClaimedMessages(claimedDir)
	if rerr != nil {
		return fmt.Errorf("list claimed residue: %w", rerr)
	}
	for _, msg := range redelivered {
		content, err := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes)
		if err != nil {
			return fmt.Errorf("read claimed message %s: %w", msg.Path, err)
		}
		displayConsumed(cfg, agentID, msg, content, true, now)
	}

	if len(msgs) == 0 && len(redelivered) == 0 {
		fmt.Println("(inbox empty)")
		return nil
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
			// Filename matched §6.1 so msg.Sender is authoritative.
			if msg.Sender == agentID {
				fmt.Fprintf(os.Stderr, "  inferred offender is self; corrective notify skipped\n")
				claimed++
				continue
			}
			reason := fmt.Sprintf("frontmatter block is syntactically malformed: %v", err)
			if _, serr := loop.SendCorrective(cfg, agentID, msg.Sender,
				quarantined, reason, "§6.4", now); serr != nil {
				fmt.Fprintf(os.Stderr, "  corrective send to %s failed: %v\n", msg.Sender, serr)
				claimed++
				continue
			}
			fmt.Fprintf(os.Stderr, "  notified %s\n", msg.Sender)
			claimed++
			continue
		}

		if noArchive {
			// Dry run: DISPLAY in place, do NOT claim/move. The asker-side owed
			// flip (ClearOwed) is a state mutation too, so displayConsumed's
			// no-side-effect display is appropriate here — we pass a read-only
			// flag below.
			displayConsumedReadOnly(agentID, msg, content)
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

	return nil
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
	printConsumedBody(msg, content, redelivered)

	fm := loop.ParseMessageFrontmatter(content)

	// Asker-side owed flip. ClearOwed only touches OUR ledger and only removes a
	// matching key, so the From==agentID guard (we are the asker) is the
	// authority check; a non-matching ref is a harmless no-op.
	if ref := strings.TrimSpace(fm["in_reply_to"]); ref != "" {
		if key, ok := loop.ParseMsgIDRef(ref); ok && key.From == agentID {
			if err := loop.ClearOwed(cfg, agentID, key); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to clear owed obligation %s: %v\n", ref, err)
			}
		}
	}

	printReplyRefIfRequired(agentID, msg, fm)
}

// displayConsumedReadOnly is the --no-archive (dry-run) display: it prints the
// body and the reply ref but performs NO state mutation (no ClearOwed).
func displayConsumedReadOnly(agentID string, msg loop.Message, content []byte) {
	printConsumedBody(msg, content, false)
	printReplyRefIfRequired(agentID, msg, loop.ParseMessageFrontmatter(content))
}

func printConsumedBody(msg loop.Message, content []byte, redelivered bool) {
	if redelivered {
		fmt.Printf("---- %s [REDELIVERED — uncommitted from a prior turn; `agentchute ack` to commit] ----\n", msg.Filename)
	} else {
		fmt.Printf("---- %s ----\n", msg.Filename)
	}
	fmt.Print(string(content))
	if !strings.HasSuffix(string(content), "\n") {
		fmt.Println()
	}
	fmt.Println()
}

// printReplyRefIfRequired prints the copyable in_reply_to ref a reply to msg must
// carry, when msg is reply_required. The ref is the ORIGINAL message's identity
// MsgID{To: agentID (us, the recipient), From: msg.Sender, Seq}: the asker
// recorded their `.owed` obligation under this exact tuple, so echoing it back as
// the reply's in_reply_to is what lets the asker's `check` discharge it. Legacy
// nonce messages have no seq (Seq=0 — a degenerate ref) since they predate the
// identity; they drain within one release.
func printReplyRefIfRequired(agentID string, msg loop.Message, fm map[string]string) {
	if !isFrontmatterReplyRequired(fm) {
		return
	}
	var seq uint64
	if _, s, ok := loop.ParseSeqFilename(msg.Filename); ok {
		seq = s
	}
	ref := loop.MsgID{To: agentID, From: msg.Sender, Seq: seq}.RefString()
	fmt.Printf("reply-required: reply with `agentchute send --from %s --to %s --reply-to %s ...`\n\n", agentID, msg.Sender, ref)
}

func checkUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute check [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--no-archive] [--limit <n>]\n  check CLAIMS + displays (at-least-once); run `agentchute ack` to commit (archive).", err)
}

func isFrontmatterReplyRequired(fm map[string]string) bool {
	return strings.ToLower(strings.TrimSpace(fm["reply_required"])) == "true"
}
