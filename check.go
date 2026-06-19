package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Default thresholds for cooperative waking, matching the watchdog
// defaults (AGENTCHUTE.md §10.1). These are NOT configurable via `check`
// flags — operators who need tuning run the dedicated watchdog.
const (
	cooperativeStaleThreshold      = 5 * time.Minute
	cooperativeMessageAgeThreshold = 90 * time.Second
)

// recordReplyObligationFn is the seam check uses to record a pending-reply
// obligation. It is a package var (not a direct call) only so tests can inject
// a record/lock failure and assert the message is left in the inbox un-archived.
var recordReplyObligationFn = recordReplyObligation

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
	fs.BoolVar(&noArchive, "no-archive", false, "dry run: suppress inbox/cooperation side effects (no archive, quarantine, corrective sends, or cooperative pokes); own last_seen still updates")
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
	// archives, quarantines, sends corrective notify, and runs cooperative
	// waking; all of those imply the agent IS enrolled in the pool.
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
	if len(msgs) == 0 {
		fmt.Println("(inbox empty)")
		// Cooperation still runs even when our inbox is empty —
		// see AGENTCHUTE.md §10.2 and codex's chunk-2 review.
		if !noArchive {
			runCooperativeWaking(cfg, agentID, time.Now().UTC())
		}
		return nil
	}

	processed := 0
	for _, msg := range msgs {
		if limit > 0 && processed >= limit {
			fmt.Printf("(reached limit of %d; %d more pending)\n", limit, len(msgs)-processed)
			break
		}
		content, err := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes)
		if err != nil {
			return fmt.Errorf("read message %s: %w", msg.Path, err)
		}

		// §11 enforcement on frontmatter: if the file has an opening `---` but
		// the block doesn't parse, quarantine + notify the (filename-known)
		// sender and skip processing. Body-only messages pass through.
		if err := loop.ValidateMessageFrontmatter(content); err != nil {
			if noArchive {
				fmt.Fprintf(os.Stderr, "warning: %s has malformed frontmatter (%v); --no-archive suppressed §11 enforcement\n",
					msg.Filename, err)
				processed++
				continue
			}
			quarantined, qerr := loop.QuarantineInboxFile(msg.Path, cfg.MalformedDir(), agentID, now)
			if qerr != nil {
				fmt.Fprintf(os.Stderr, "warning: %s has malformed frontmatter but quarantine failed: %v\n", msg.Filename, qerr)
				processed++
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: quarantined %s (malformed §6.4 frontmatter: %v) -> %s\n",
				msg.Filename, err, quarantined)
			// Filename matched §6.1 so msg.Sender is authoritative.
			if msg.Sender == agentID {
				fmt.Fprintf(os.Stderr, "  inferred offender is self; corrective notify skipped\n")
				processed++
				continue
			}
			reason := fmt.Sprintf("frontmatter block is syntactically malformed: %v", err)
			if _, serr := loop.SendCorrective(cfg, agentID, msg.Sender,
				quarantined, reason, "§6.4", now); serr != nil {
				fmt.Fprintf(os.Stderr, "  corrective send to %s failed: %v\n", msg.Sender, serr)
				processed++
				continue
			}
			fmt.Fprintf(os.Stderr, "  notified %s\n", msg.Sender)
			processed++
			continue
		}
		fmt.Printf("---- %s ----\n", msg.Filename)
		fmt.Print(string(content))
		if !strings.HasSuffix(string(content), "\n") {
			fmt.Println()
		}
		fmt.Println()

		if !noArchive {
			// Record-before-archive (Fix C / gemini finding): the message must
			// not leave the inbox until its reply obligation is durably recorded.
			// If recording fails (ledger error OR the per-agent lock timeout),
			// archiving first would move the message out of the inbox with no
			// ledger entry — a silently dropped obligation. We compute the archive
			// path the move WILL produce (deterministic in `now`+filename) so the
			// ledger entry's archive_path is correct, record the obligation, and
			// only then archive. On record failure we return WITHOUT archiving so
			// the message stays in the inbox and the next `check` re-processes it;
			// the re-record is idempotent on (message_id, original_filename), so a
			// record-success-then-archive-fail next cycle does not double-record.
			archivePath := loop.ArchiveMessageDest(msg, cfg.ArchiveDir(), agentID, now)
			if err := recordReplyObligationFn(cfg, agentID, msg, archivePath, content, now); err != nil {
				return fmt.Errorf("record reply obligation for %s: %w", msg.Filename, err)
			}
			if _, err := loop.ArchiveMessage(msg, cfg.ArchiveDir(), agentID, now); err != nil {
				return fmt.Errorf("archive message %s: %w", msg.Path, err)
			}
		}
		processed++
	}

	// Update last_active per AGENTCHUTE.md §6.3 step 4 if we actually consumed.
	if !noArchive && processed > 0 && selfExists {
		if err := loop.UpdateLastActive(cfg, agentID, now); err != nil {
			// Non-fatal: messages are archived; only the timestamp update lost.
			fmt.Fprintf(os.Stderr, "warning: failed to update last_active (%v)\n", err)
		}
	}

	// §6.3 step 5 / §10.2: cooperative waking. After own-inbox work and
	// timestamp updates, contribute best-effort liveness for peers that are
	// stale with unread mail and reachable from this host. --no-archive
	// suppresses inbox/cooperation side effects so dry-runs do not move,
	// quarantine, or poke anything (own last_seen still updates).
	// `now` is re-taken here in case the inbox-processing loop ran long
	// enough to make the earlier timestamp stale for threshold checks.
	if !noArchive {
		runCooperativeWaking(cfg, agentID, time.Now().UTC())
	}

	return nil
}

// runCooperativeWaking performs AGENTCHUTE.md §10.2: every recipient flow
// MAY run the §10.1 watchdog algorithm opportunistically during its
// `check` cycle. The reference CLI always does. Per-peer errors warn or
// log; cooperation MUST NOT make check exit nonzero. The message has
// already been delivered to the recipient's inbox before this call (in
// this reference CLI, via the shared filesystem).
func runCooperativeWaking(cfg *loop.Config, agentID string, now time.Time) {
	localHost, _ := os.Hostname() // empty = no host filter; tolerable

	opts := watchdogOptions{
		AgentID:             agentID,
		LocalHost:           localHost,
		StaleThreshold:      cooperativeStaleThreshold,
		MessageAgeThreshold: cooperativeMessageAgeThreshold,
		Now:                 func() time.Time { return now },
	}
	// Errors from runLivenessSweep are already logged per-peer via the
	// watchdog log; the outer return is informational and only fires on a
	// catastrophic agents-dir read failure (which the lenient iterator
	// already surfaces as a per-file error line). Swallow to keep check
	// non-fatal per §10.2 contract.
	_ = runLivenessSweep(context.Background(), cfg, opts, now)
}

func checkUsage(err error) error {
	return fmt.Errorf("%w\nusage: agentchute check [--as <agent-id>] [--vendor <v>] [--control-repo <path>] [--loop-dir <path>] [--no-archive] [--limit <n>]", err)
}

// recordReplyObligation parses the just-archived message's frontmatter
// for `reply_required: true` and, when present, appends a pending entry
// to the recipient's pending-reply ledger. No-op when the field is
// absent or false. Errors propagate to the caller — per codex's ledger
// integration note, an archived obligation without a ledger entry is a
// silent protocol leak, so failures must be loud rather than swallowed.
//
// Uses loop.ParseMessageFrontmatter so the recorder honors the same
// lenient delimiter semantics (whitespace-tolerant `---` lines per
// §6.4) as loop.ValidateMessageFrontmatter — fixes the codex-flagged
// gap where the validator accepted a message but the recorder dropped
// it on a stricter parse (codex final review on 5320c08).
func recordReplyObligation(cfg *loop.Config, agentID string, msg loop.Message, archivePath string, content []byte, now time.Time) error {
	fm := loop.ParseMessageFrontmatter(content)
	if !isFrontmatterReplyRequired(fm) {
		return nil
	}
	messageID := strings.TrimSpace(fm["message_id"])
	if messageID == "" {
		// Per spec rev3 A.9 schema, message_id is the ledger's primary key.
		// Frontmatter that sets reply_required: true without a message_id is
		// malformed; refuse rather than fabricate.
		return fmt.Errorf("reply_required: true set but message_id missing in frontmatter")
	}
	entry := loop.PendingReplyEntry{
		MessageID:        messageID,
		From:             msg.Sender,
		To:               agentID,
		Task:             strings.TrimSpace(fm["task"]),
		OriginalFilename: msg.Filename,
		ArchivePath:      archivePath,
	}
	return loop.RecordPendingReply(cfg, agentID, entry, now)
}

func isFrontmatterReplyRequired(fm map[string]string) bool {
	return strings.ToLower(strings.TrimSpace(fm["reply_required"])) == "true"
}
