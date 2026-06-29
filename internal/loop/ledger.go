package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// pending-reply ledger (AGENTCHUTE.md §6.4).
//
// Each recipient keeps a small JSON file at <loop>/state/<agent>/pending-replies.json
// listing every inbound message with frontmatter `reply_required: true` that
// has been archived but not yet replied to or deferred. The file is the durable
// state behind `gate --before finish`: an entry with status "pending" blocks
// the gate; "replied" and "deferred" do not.
//
// The ledger is strictly recipient-owned. Peers never read another agent's
// state dir — wake-side visibility into the obligation comes from the
// `status` command, which reads only the local agent's own ledger. Anything
// a sender needs to know about its delivered messages is recorded
// sender-side (currently just the wake-attempt receipt in `send` output).

// PendingReplyStatus enumerates ledger entry lifecycle states.
type PendingReplyStatus string

const (
	// PendingReplyStatusPending — initial state, blocks gate --before finish.
	// Set by `check` when archiving a message with reply_required: true.
	PendingReplyStatusPending PendingReplyStatus = "pending"

	// PendingReplyStatusReplied — discharge via `send --reply-to <msg-id>`.
	// reply_sent_at and reply_message_id are populated at transition.
	PendingReplyStatusReplied PendingReplyStatus = "replied"

	// PendingReplyStatusDeferred — explicit punt via `agentchute defer`.
	// deferred_at, deferred_reason (and optionally deferred_until) populate
	// at transition. defer also sends an automatic deferral-ack to the
	// original sender; that send is the defer command's responsibility,
	// not the ledger's.
	PendingReplyStatusDeferred PendingReplyStatus = "deferred"
)

// MaxPendingLedgerBytes caps the on-disk ledger file size to refuse runaway
// or hand-corrupted state. 4 MiB fits thousands of entries.
const MaxPendingLedgerBytes = 4 << 20

// PendingReplyEntry is one row in the ledger. Tracks the AGENTCHUTE.md §6.4 reply obligation.
// Nullable timestamps and reason are *string so encoding/json emits literal
// JSON null when unset (matches the spec example); *string also makes the
// "did the transition happen?" check unambiguous in callers (nil vs. empty).
type PendingReplyEntry struct {
	MessageID        string             `json:"message_id"`
	From             string             `json:"from"`
	To               string             `json:"to"`
	Task             string             `json:"task"`
	OriginalFilename string             `json:"original_filename"`
	ArchivePath      string             `json:"archive_path"`
	RecordedAt       string             `json:"recorded_at"`
	Status           PendingReplyStatus `json:"status"`
	ReplySentAt      *string            `json:"reply_sent_at"`
	ReplyMessageID   *string            `json:"reply_message_id"`
	DeferredAt       *string            `json:"deferred_at"`
	DeferredUntil    *string            `json:"deferred_until"`
	DeferredReason   *string            `json:"deferred_reason"`
}

// PendingLedger is the JSON shape of pending-replies.json.
type PendingLedger struct {
	Pending []PendingReplyEntry `json:"pending"`
}

// ErrLedgerEntryNotFound is returned by MarkPendingReplied / MarkPendingDeferred
// when no entry with the requested message_id exists.
var ErrLedgerEntryNotFound = errors.New("pending-reply ledger entry not found")

// ErrLedgerEntryNotPending is returned when a transition is requested against
// an entry whose status is not "pending" — callers should treat this as a
// no-op or surface it to the operator rather than silently re-transitioning.
var ErrLedgerEntryNotPending = errors.New("pending-reply ledger entry is not in pending state")

// LoadPendingLedger reads the recipient's ledger from disk. A missing file is
// not an error — it returns an empty ledger so callers can append. Parse
// errors and oversized files are surfaced (we don't silently lose state).
func LoadPendingLedger(cfg *Config, agentID string) (*PendingLedger, error) {
	if err := ValidateAgentID(agentID); err != nil {
		return nil, err
	}
	path := cfg.PendingRepliesPath(agentID)
	data, err := ReadFileLimit(path, MaxPendingLedgerBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &PendingLedger{Pending: []PendingReplyEntry{}}, nil
		}
		return nil, fmt.Errorf("read pending-replies ledger %s: %w", path, err)
	}
	if len(data) == 0 {
		return &PendingLedger{Pending: []PendingReplyEntry{}}, nil
	}
	var ledger PendingLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parse pending-replies ledger %s: %w", path, err)
	}
	if ledger.Pending == nil {
		ledger.Pending = []PendingReplyEntry{}
	}
	// Reject non-canonical status values: a hand edit or rogue writer that
	// sets status: "frobbed" must not silently clear a blocking obligation.
	// PendingEntries() also treats unknown statuses conservatively (every
	// non-{replied,deferred} entry blocks) as defense-in-depth if Load is
	// ever bypassed (codex review on 4d34826).
	//
	// Also validate From / To as agent_ids on load — defense-in-depth on top
	// of RecordPendingReply's validation, so a hand-edited or peer-corrupted
	// state file with a path-escaping value can't reach defer / send (codex
	// review on eb58443).
	for i, e := range ledger.Pending {
		if !isCanonicalStatus(e.Status) {
			return nil, fmt.Errorf("parse pending-replies ledger %s: entry %d (message_id=%q) has non-canonical status %q",
				path, i, e.MessageID, e.Status)
		}
		if err := ValidateAgentID(e.From); err != nil {
			return nil, fmt.Errorf("parse pending-replies ledger %s: entry %d (message_id=%q) has invalid from: %w",
				path, i, e.MessageID, err)
		}
		if err := ValidateAgentID(e.To); err != nil {
			return nil, fmt.Errorf("parse pending-replies ledger %s: entry %d (message_id=%q) has invalid to: %w",
				path, i, e.MessageID, err)
		}
	}
	return &ledger, nil
}

func isCanonicalStatus(s PendingReplyStatus) bool {
	switch s {
	case PendingReplyStatusPending, PendingReplyStatusReplied, PendingReplyStatusDeferred:
		return true
	default:
		return false
	}
}

// SavePendingLedger writes the ledger atomically. The per-agent state dir is
// created with 0700 if missing. JSON is indented for hand-inspection; an
// empty `pending: []` is a valid serialized state and round-trips cleanly.
func SavePendingLedger(cfg *Config, agentID string, ledger *PendingLedger) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	if ledger == nil {
		return fmt.Errorf("SavePendingLedger: ledger is nil")
	}
	if ledger.Pending == nil {
		ledger.Pending = []PendingReplyEntry{}
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pending-replies ledger: %w", err)
	}
	data = append(data, '\n')
	path := cfg.PendingRepliesPath(agentID)
	return atomicWriteFile(path, data)
}

// RecordPendingReply appends a new pending entry. Called by `check` when it
// archives a message with frontmatter reply_required: true.
//
// The obligation's PRIMARY KEY is the canonical OriginalFilename — the inbox
// filename, which the recipient's own filesystem assigned and therefore trusts.
// message_id is sender-controlled and is NOT delivery-unique (AGENTCHUTE.md
// §6.4), so it is kept only as informational metadata on the entry, never as
// the dedup key. Keying on it would let a peer wedge `check`: two reply_required
// messages reusing a message_id would either collide-and-error (the old fatal
// ErrLedgerEntryCollision) or silently drop the second obligation. Keying on the
// trusted filename, two deliveries with a shared message_id are simply two
// distinct obligations and both are recorded.
//
// Idempotency: if an entry with the same OriginalFilename is already present
// (re-archive race or operator replay), the existing entry is left untouched and
// no duplicate is appended — the first observation wins. This is a no-op (nil),
// never an error.
//
// `now` is the recipient-side observation time used for recorded_at; passing
// it in (rather than calling time.Now) lets tests pin determinism.
func RecordPendingReply(cfg *Config, agentID string, entry PendingReplyEntry, now time.Time) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	if strings.TrimSpace(entry.MessageID) == "" {
		return fmt.Errorf("RecordPendingReply: message_id required")
	}
	// From / To are used as filesystem path components (inbox dir, archive
	// name) later by defer / send. Validate as agent_ids at record time so a
	// bad value can't reach a path-resolving caller (codex review on eb58443).
	if err := ValidateAgentID(entry.From); err != nil {
		return fmt.Errorf("RecordPendingReply: from: %w", err)
	}
	if err := ValidateAgentID(entry.To); err != nil {
		return fmt.Errorf("RecordPendingReply: to: %w", err)
	}
	if strings.TrimSpace(entry.OriginalFilename) == "" {
		return fmt.Errorf("RecordPendingReply: original_filename required")
	}
	if strings.TrimSpace(entry.ArchivePath) == "" {
		return fmt.Errorf("RecordPendingReply: archive_path required")
	}

	// Hold the per-agent lock across load->append->save so concurrent recorders
	// (e.g. two `check` invocations, or check racing a wake-triggered re-archive)
	// cannot read the same ledger and clobber each other's append (lost update).
	return withAgentLock(cfg, agentID, func() error {
		ledger, err := LoadPendingLedger(cfg, agentID)
		if err != nil {
			return err
		}
		for _, existing := range ledger.Pending {
			// Filename-keyed idempotency: the same OriginalFilename already
			// recorded ⇒ no-op (re-archive replay safety). A duplicate
			// message_id with a *new* filename falls through and is recorded as
			// a distinct obligation — it is NOT a collision (WI-2 Fix 1).
			if existing.OriginalFilename == entry.OriginalFilename {
				return nil
			}
		}

		entry.Status = PendingReplyStatusPending
		if strings.TrimSpace(entry.RecordedAt) == "" {
			entry.RecordedAt = formatLedgerTimestamp(now)
		}
		// Defensive: ensure nullable fields are nil on a fresh pending entry.
		entry.ReplySentAt = nil
		entry.ReplyMessageID = nil
		entry.DeferredAt = nil
		entry.DeferredUntil = nil
		entry.DeferredReason = nil

		ledger.Pending = append(ledger.Pending, entry)
		return SavePendingLedger(cfg, agentID, ledger)
	})
}

// MarkPendingReplied transitions EVERY pending entry that matches BOTH
// message_id AND fromSender (the original sender whose obligation we are
// discharging) to status "replied" and populates reply_sent_at +
// reply_message_id on each.
//
// SENDER-SCOPED (WI-2 follow-up): message_id is sender-controlled and NOT
// delivery-unique, so a peer can reuse another sender's message_id. Scoping the
// transition to From == fromSender prevents a reply to one sender from clearing
// an obligation owed to a DIFFERENT sender. The ledger is recipient-owned, so
// To == agentID already holds for legitimate entries; we keep that invariant by
// only ever transitioning rows that are ours (Load validates To, and the
// callers refuse mismatched-To entries before reaching here).
//
// A single (message_id, fromSender) pair may still map to MORE than one entry
// (the obligation is filename-keyed — WI-2 Fix 1; e.g. two filenames, one
// message_id, same sender). Marking ALL such pending entries is correct: it
// discharges every duplicate so none is left stranded, and it cannot
// over-discharge an unrelated thread because the match is exact on both keys.
//
// Returns ErrLedgerEntryNotFound if no entry matches (message_id, fromSender);
// ErrLedgerEntryNotPending if matches exist but NONE is in the pending state
// (idempotency hint to the caller, not a fatal protocol violation). Matching
// entries that are already terminal are skipped, never blocking the pending
// ones (no terminal-first short-circuit).
func MarkPendingReplied(cfg *Config, agentID, messageID, fromSender, replyMessageID string, replySentAt time.Time) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("MarkPendingReplied: message_id required")
	}
	if strings.TrimSpace(fromSender) == "" {
		return fmt.Errorf("MarkPendingReplied: fromSender required")
	}
	rid := strings.TrimSpace(replyMessageID)
	if rid == "" {
		// The ledger records reply_sent_at + reply_message_id together
		// on transition. Allowing an empty reply_message_id would discharge
		// the obligation without a durable reference to the reply, which
		// breaks traceability — refuse rather than persist a half-populated
		// row (codex review on 4d34826).
		return fmt.Errorf("MarkPendingReplied: reply_message_id required")
	}
	// Lock the load->mutate->save so a concurrent recorder/defer on the same
	// agent cannot drop this transition.
	return withAgentLock(cfg, agentID, func() error {
		ledger, err := LoadPendingLedger(cfg, agentID)
		if err != nil {
			return err
		}
		idxs := indicesByMessageIDFrom(ledger, messageID, fromSender)
		if len(idxs) == 0 {
			return ErrLedgerEntryNotFound
		}
		sentAt := formatLedgerTimestamp(replySentAt)
		transitioned := 0
		for _, idx := range idxs {
			if ledger.Pending[idx].Status != PendingReplyStatusPending {
				continue
			}
			ledger.Pending[idx].Status = PendingReplyStatusReplied
			ledger.Pending[idx].ReplySentAt = &sentAt
			ledger.Pending[idx].ReplyMessageID = &rid
			transitioned++
		}
		if transitioned == 0 {
			return ErrLedgerEntryNotPending
		}
		return SavePendingLedger(cfg, agentID, ledger)
	})
}

// MarkPendingDeferred transitions EVERY pending entry that matches BOTH
// message_id AND fromSender to status "deferred". The reason is required;
// `deferredUntil` is optional (empty string ⇒ nil in the JSON, meaning "no
// scheduled unblock time").
//
// SENDER-SCOPED (WI-2 follow-up), mirroring MarkPendingReplied: scoping the
// transition to From == fromSender prevents a deferral keyed on a reused
// message_id from clearing another sender's obligation. A single
// (message_id, fromSender) pair may map to several filename-keyed entries
// (WI-2 Fix 1); all PENDING such entries are transitioned so none is stranded.
//
// Returns ErrLedgerEntryNotFound if no entry matches (message_id, fromSender);
// ErrLedgerEntryNotPending if matches exist but none is pending. Already-terminal
// matching entries are skipped, never short-circuiting the pending ones.
func MarkPendingDeferred(cfg *Config, agentID, messageID, fromSender, reason, deferredUntil string, deferredAt time.Time) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("MarkPendingDeferred: message_id required")
	}
	if strings.TrimSpace(fromSender) == "" {
		return fmt.Errorf("MarkPendingDeferred: fromSender required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("MarkPendingDeferred: reason required")
	}
	// Lock the load->mutate->save so a concurrent recorder/reply on the same
	// agent cannot drop this transition.
	return withAgentLock(cfg, agentID, func() error {
		ledger, err := LoadPendingLedger(cfg, agentID)
		if err != nil {
			return err
		}
		idxs := indicesByMessageIDFrom(ledger, messageID, fromSender)
		if len(idxs) == 0 {
			return ErrLedgerEntryNotFound
		}
		at := formatLedgerTimestamp(deferredAt)
		until := strings.TrimSpace(deferredUntil)
		transitioned := 0
		for _, idx := range idxs {
			if ledger.Pending[idx].Status != PendingReplyStatusPending {
				continue
			}
			ledger.Pending[idx].Status = PendingReplyStatusDeferred
			ledger.Pending[idx].DeferredAt = &at
			r := reason
			ledger.Pending[idx].DeferredReason = &r
			if until != "" {
				u := until
				ledger.Pending[idx].DeferredUntil = &u
			}
			transitioned++
		}
		if transitioned == 0 {
			return ErrLedgerEntryNotPending
		}
		return SavePendingLedger(cfg, agentID, ledger)
	})
}

// PendingEntries returns the subset of entries that still block gate --before
// finish. Conservative: anything that isn't an explicit terminal state
// (replied, deferred) is treated as still blocking — including the canonical
// "pending" value and any unknown/corrupt value a future writer might set.
// This is defense-in-depth on top of LoadPendingLedger's status validation:
// even if a caller hand-builds a PendingLedger in memory, an unknown status
// cannot silently clear an obligation. Returned slice is a copy.
func (l *PendingLedger) PendingEntries() []PendingReplyEntry {
	out := make([]PendingReplyEntry, 0, len(l.Pending))
	for _, e := range l.Pending {
		if e.Status == PendingReplyStatusReplied || e.Status == PendingReplyStatusDeferred {
			continue
		}
		out = append(out, e)
	}
	return out
}

// FindByMessageID returns the FIRST entry with matching message_id, if any.
// Obligations are filename-keyed (WI-2 Fix 1), so a message_id may map to more
// than one entry; callers that need to act on every match (the mark/transition
// paths) use indicesByMessageIDFrom. FindByMessageID is used by send/defer only
// to READ the thread's From/To (for the cross-sender warning and the
// recipient-owned-ledger validation), never to gate the transition — the
// transition is sender-scoped and acts on every matching pending row.
func (l *PendingLedger) FindByMessageID(messageID string) (PendingReplyEntry, bool) {
	for _, e := range l.Pending {
		if e.MessageID == messageID {
			return e, true
		}
	}
	return PendingReplyEntry{}, false
}

// indicesByMessageID returns the indices of EVERY entry whose message_id
// matches. Used by send/defer only to detect whether a message_id is known at
// all (the cross-sender warning path), independent of which sender owns it.
func indicesByMessageID(l *PendingLedger, messageID string) []int {
	var idxs []int
	for i, e := range l.Pending {
		if e.MessageID == messageID {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// indicesByMessageIDFrom returns the indices of EVERY entry matching BOTH
// message_id AND from (the original sender). Used by the mark/transition paths
// so discharging a thread discharges all of that sender's filename-keyed
// obligations for that message_id — and only that sender's (WI-2 follow-up:
// message_id is sender-controlled and reusable, so the sender must be part of
// the predicate to avoid clearing a different sender's obligation).
func indicesByMessageIDFrom(l *PendingLedger, messageID, from string) []int {
	var idxs []int
	for i, e := range l.Pending {
		if e.MessageID == messageID && e.From == from {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

// PendingByMessageIDFrom returns the PENDING entries (status == pending) that
// match BOTH message_id AND from. send uses it to decide whether a
// sender-scoped reply actually has a pending obligation to discharge, without
// the first-match short-circuit that could otherwise let a terminal duplicate
// hide a still-pending one. Returned slice is a copy.
func (l *PendingLedger) PendingByMessageIDFrom(messageID, from string) []PendingReplyEntry {
	var out []PendingReplyEntry
	for _, e := range l.Pending {
		if e.MessageID == messageID && e.From == from && e.Status == PendingReplyStatusPending {
			out = append(out, e)
		}
	}
	return out
}

// EntriesByMessageIDFrom returns ALL entries (any status) matching BOTH
// message_id AND from. send uses it to distinguish "an obligation owed to this
// recipient exists but is already terminal" (non-empty result, none pending)
// from "no obligation under this message_id is owed to this recipient at all"
// (empty result) — WITHOUT ever keying any decision on FindByMessageID's first
// bare row (WI-2 follow-up rev2). Returned slice is a copy.
func (l *PendingLedger) EntriesByMessageIDFrom(messageID, from string) []PendingReplyEntry {
	var out []PendingReplyEntry
	for _, e := range l.Pending {
		if e.MessageID == messageID && e.From == from {
			out = append(out, e)
		}
	}
	return out
}

// FirstPendingByMessageID returns the FIRST entry with matching message_id whose
// status is pending. defer uses it to pick a sender that actually has a PENDING
// obligation — never the first bare row, which may be terminal and would scope
// the deferral to a sender with nothing left to defer (WI-2 follow-up rev2).
// Returns ok=false when no pending entry exists for the message_id (none at all,
// or every match is already terminal).
func (l *PendingLedger) FirstPendingByMessageID(messageID string) (PendingReplyEntry, bool) {
	for _, e := range l.Pending {
		if e.MessageID == messageID && e.Status == PendingReplyStatusPending {
			return e, true
		}
	}
	return PendingReplyEntry{}, false
}

// formatLedgerTimestamp returns RFC3339 UTC at second precision — matches
// the spec example's `recorded_at: "2026-05-19T17:54:30Z"` shape and the
// archive filename timestamp granularity.
func formatLedgerTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
