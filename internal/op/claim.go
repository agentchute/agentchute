package op

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// ClaimReq is `check`'s state half. Limit 0 means no limit; NoArchive is the
// dry run — display in place, no claim, no quarantine, no owed discharge.
type ClaimReq struct {
	Limit     int  `json:"limit,omitempty"`
	NoArchive bool `json:"no_archive,omitempty"`
}

// ClaimSummary is counts only (D2). Everything unbounded left as events.
type ClaimSummary struct {
	Claimed     int `json:"claimed"`
	Redelivered int `json:"redelivered"`
	Quarantined int `json:"quarantined"`
	OwedExpired int `json:"owed_expired"`
}

// Claim is the state half of `check`: quarantine protocol violations,
// re-deliver uncommitted residue, then CLAIM (inbox -> .claimed) and emit each
// message one at a time. It never archives — that is Ack, phase 2.
//
// Order is `cmdCheck`'s exact order, and it is load-bearing: the three stdout
// status lines are emitted as NoteInfo events AT their production points
// (the limit line lands between two messages; the CLAIMED line lands before
// the expired-obligation events), so a renderer driven from this stream — local
// or remote — cannot silently reorder them the way one driven from the summary
// would.
//
// Nothing accumulates: one body is held at a time. An emit error aborts the op
// after the current event; already-claimed residue is redelivered by the next
// check (§4.5.2).
func Claim(cfg *loop.Config, ctx Context, req ClaimReq, emit func(Event) error) (ClaimSummary, error) {
	var sum ClaimSummary
	agentID := ctx.ActorID
	now := time.Now().UTC()

	// v0.2.1 enforced enrollment (§5.3): check is an active agent command — it
	// archives, quarantines and notifies, all of which imply enrollment.
	if err := requireRegistered(cfg, agentID); err != nil {
		return sum, err
	}

	inboxDir := cfg.AgentInboxDir(agentID)
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(inboxDir)
	if err != nil {
		return sum, fmt.Errorf("list inbox: %w", err)
	}

	// §11 protocol enforcement on filenames. Enforcement is a state mutation
	// (file moves), so --no-archive reports and skips it.
	if !req.NoArchive {
		for _, name := range skipped {
			quarantined, qerr := loop.QuarantineInboxFile(filepath.Join(inboxDir, name), cfg.MalformedDir(), agentID, now)
			if qerr != nil {
				if eerr := emit(NewNoteEvent(NoteWarn, fmt.Sprintf("failed to quarantine %s: %v", name, qerr))); eerr != nil {
					return sum, eerr
				}
				continue
			}
			sum.Quarantined++
			if eerr := emit(NewNoteEvent(NoteWarn, fmt.Sprintf("quarantined %s (malformed §6.1 filename) -> %s", name, quarantined))); eerr != nil {
				return sum, eerr
			}
		}
	} else if len(skipped) > 0 {
		msg := fmt.Sprintf("%d non-§6.1 file(s) in inbox; --no-archive suppressed §11 enforcement:", len(skipped))
		for _, name := range skipped {
			msg += "\n  " + name
		}
		if eerr := emit(NewNoteEvent(NoteWarn, msg)); eerr != nil {
			return sum, eerr
		}
	}

	claimedDir := cfg.AgentClaimedDir(agentID)

	// FIRST: uncommitted residue from a crashed/un-acked prior turn. These were
	// CLAIMED but never COMMITTED; re-deliver so the agent re-acts.
	redelivered, rerr := loop.ListClaimedMessages(claimedDir)
	if rerr != nil {
		return sum, fmt.Errorf("list claimed residue: %w", rerr)
	}
	for _, msg := range redelivered {
		content, err := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes)
		if err != nil {
			return sum, fmt.Errorf("read claimed message %s: %w", msg.Path, err)
		}
		sum.Redelivered++
		if err := emitMessage(emit, agentID, msg, content, true); err != nil {
			return sum, err
		}
		// The residue path discharges even under --no-archive, exactly as
		// today: the shipped loop calls displayConsumed (not the read-only
		// variant) for redelivered mail regardless of the flag.
		if err := dischargeOwed(cfg, agentID, msg, content, emit); err != nil {
			return sum, err
		}
	}

	// The empty-inbox line does NOT return early: it falls through to the
	// (no-op) claim loop and on to the expired-obligation report, which a
	// returning agent with no new mail still needs.
	if len(msgs) == 0 && len(redelivered) == 0 {
		if err := emit(NewNoteEvent(NoteInfo, "(inbox empty)")); err != nil {
			return sum, err
		}
	}

	for _, msg := range msgs {
		if req.Limit > 0 && sum.Claimed >= req.Limit {
			line := fmt.Sprintf("(reached limit of %d; %d more pending)", req.Limit, len(msgs)-sum.Claimed)
			if err := emit(NewNoteEvent(NoteInfo, line)); err != nil {
				return sum, err
			}
			break
		}
		content, err := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes)
		if err != nil {
			return sum, fmt.Errorf("read message %s: %w", msg.Path, err)
		}

		// §11 enforcement on frontmatter. Body-only messages pass through.
		// Quarantine is a state mutation, so --no-archive suppresses it.
		if verr := loop.ValidateMessageFrontmatter(content); verr != nil {
			if req.NoArchive {
				sum.Claimed++
				if eerr := emit(NewNoteEvent(NoteWarn, fmt.Sprintf("%s has malformed frontmatter (%v); --no-archive suppressed §11 enforcement", msg.Filename, verr))); eerr != nil {
					return sum, eerr
				}
				continue
			}
			quarantined, qerr := loop.QuarantineInboxFile(msg.Path, cfg.MalformedDir(), agentID, now)
			if qerr != nil {
				sum.Claimed++
				if eerr := emit(NewNoteEvent(NoteWarn, fmt.Sprintf("%s has malformed frontmatter but quarantine failed: %v", msg.Filename, qerr))); eerr != nil {
					return sum, eerr
				}
				continue
			}
			sum.Claimed++
			sum.Quarantined++
			if eerr := emit(NewNoteEvent(NoteWarn, fmt.Sprintf("quarantined %s (malformed §6.4 frontmatter: %v) -> %s", msg.Filename, verr, quarantined))); eerr != nil {
				return sum, eerr
			}
			continue
		}

		if req.NoArchive {
			// Dry run: emit in place, do NOT claim/move, and do NOT discharge
			// (ClearOwed is a state mutation too).
			sum.Claimed++
			if err := emitMessage(emit, agentID, msg, content, false); err != nil {
				return sum, err
			}
			continue
		}

		// CLAIM (phase 1): move inbox -> .claimed, then emit from the claimed
		// copy. NO archive — that is `ack`, phase 2.
		claimedPath, cerr := loop.ClaimMessage(msg, claimedDir)
		if cerr != nil {
			return sum, fmt.Errorf("claim message %s: %w", msg.Filename, cerr)
		}
		msg.Path = claimedPath
		sum.Claimed++
		if err := emitMessage(emit, agentID, msg, content, false); err != nil {
			return sum, err
		}
		if err := dischargeOwed(cfg, agentID, msg, content, emit); err != nil {
			return sum, err
		}
	}

	if !req.NoArchive && sum.Claimed > 0 {
		if err := emit(NewNoteEvent(NoteInfo, "note: messages CLAIMED (at-least-once), not yet archived. Run `agentchute ack` to commit; a crash before ack re-delivers them.")); err != nil {
			return sum, err
		}
	}

	// C19: this agent's own expired reply obligations, reported for pruning —
	// never auto-removed. Deliberately AFTER the claim loop: a reply consumed
	// by THIS turn can discharge the very obligation being offered.
	owed, oerr := loop.LoadOwedLedger(cfg, agentID)
	if oerr != nil {
		if err := emit(NewNoteEvent(NoteWarn, fmt.Sprintf("owed-reply ledger is corrupt or unreadable; inspect `state/%s/owed.json`", agentID))); err != nil {
			return sum, err
		}
		return sum, nil
	}
	for _, e := range owed.ExpiredOwed(now) {
		sum.OwedExpired++
		if err := emit(NewOwedEvent(owedEventOf(e))); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// requireRegistered is the enforced-enrollment preflight shared by the active
// actor-scoped ops. ONE sentinel; each CLI call site keeps its own wording.
func requireRegistered(cfg *loop.Config, agentID string) error {
	if _, err := os.Stat(cfg.AgentRegistrationPath(agentID)); err != nil {
		if os.IsNotExist(err) {
			return ErrNotRegistered
		}
		return fmt.Errorf("stat own registration: %w", err)
	}
	return nil
}

// emitMessage builds and emits one MessageEvent, including the precomputed
// reply ref a reply must echo when the message is reply_required.
func emitMessage(emit func(Event) error, agentID string, msg loop.Message, content []byte, redelivered bool) error {
	fm := loop.ParseMessageFrontmatter(content)
	ev := MessageEvent{
		Filename:      msg.Filename,
		Sender:        msg.Sender,
		Stamp:         msg.Timestamp.UTC().Format(time.RFC3339Nano),
		Redelivered:   redelivered,
		ReplyRequired: frontmatterReplyRequired(fm),
		Body:          content,
	}
	if ev.ReplyRequired {
		ev.ReplyRef = replyRef(agentID, msg.Filename)
	}
	return emit(NewMessageEvent(ev))
}

// dischargeOwed is the asker-side owed flip: if this message is a reply that
// references one of OUR outstanding asks, clear that obligation. ClearOwed
// only touches OUR ledger and only removes a matching key; the ref must name
// us as asker AND the reply must come from the agent that owed it, so a third
// party cannot clear an obligation by echoing someone else's ref. Idempotent,
// so re-display is safe.
func dischargeOwed(cfg *loop.Config, agentID string, msg loop.Message, content []byte, emit func(Event) error) error {
	fm := loop.ParseMessageFrontmatter(content)
	ref := strings.TrimSpace(fm["in_reply_to"])
	if ref == "" {
		return nil
	}
	if key, ok := loop.ParseMsgIDRef(ref); ok && key.From == agentID && msg.Sender == key.To {
		if err := loop.ClearOwed(cfg, agentID, key); err != nil {
			return emit(NewNoteEvent(NoteWarn, fmt.Sprintf("failed to clear owed obligation %s: %v", ref, err)))
		}
		return nil
	}
	if key, ok := loop.ParseTsRef(ref); ok && key.From == agentID && msg.Sender == key.To {
		if err := loop.ClearOwed(cfg, agentID, key); err != nil {
			return emit(NewNoteEvent(NoteWarn, fmt.Sprintf("failed to clear owed obligation %s: %v", ref, err)))
		}
	}
	return nil
}

// replyRef returns the copyable in_reply_to ref a reply to filename must
// carry, in the message's own filename identity form, or "" when the filename
// carries no parseable identity (the renderer then prints nothing).
func replyRef(agentID, filename string) string {
	if from, seq, ok := loop.ParseSeqFilename(filename); ok {
		return (loop.MsgID{To: agentID, From: from, Seq: seq}).RefString()
	}
	if id, ok := loop.ParseTsFilename(filename); ok {
		id.To = agentID
		return id.RefString()
	}
	return ""
}

func frontmatterReplyRequired(fm map[string]string) bool {
	return strings.ToLower(strings.TrimSpace(fm["reply_required"])) == "true"
}

func owedEventOf(e loop.OwedEntry) OwedEvent {
	return OwedEvent{
		To:         e.To,
		From:       e.From,
		Seq:        e.Seq,
		Stamp:      e.Stamp,
		Suffix:     e.Suffix,
		By:         e.By,
		RecordedAt: e.RecordedAt,
		Ref:        e.Key().RefString(),
	}
}
