package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// pending-reply ledger (AGENTCHUTE.md §6.4 / v0.1.1 spec rev3 A.9).
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

// PendingReplyEntry is one row in the ledger. Schema per spec rev3 Part A.9.
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

// ErrLedgerEntryCollision is returned by RecordPendingReply when an entry
// with the same message_id but a *different* original_filename already
// exists. AGENTCHUTE.md §6.4.1 says message_id is not delivery-unique, so
// the recipient's filesystem (filename) is the canonical identity; two
// distinct deliveries sharing a message_id must be surfaced, not silently
// merged — otherwise the second obligation would vanish.
var ErrLedgerEntryCollision = errors.New("pending-reply ledger collision: message_id already recorded with different original_filename")

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
// archives a message with frontmatter reply_required: true. If an entry with
// the same message_id already exists (re-archive race or operator replay),
// the existing entry is left untouched and no duplicate is appended — the
// archive_path of the first observation wins, since it's the canonical record.
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

	ledger, err := LoadPendingLedger(cfg, agentID)
	if err != nil {
		return err
	}
	for _, existing := range ledger.Pending {
		if existing.MessageID != entry.MessageID {
			continue
		}
		// Same message_id: idempotent only when original_filename also matches.
		// Distinct filename ⇒ two real deliveries with a shared message_id,
		// which would otherwise silently drop the second obligation. Surface
		// as a typed error so the caller decides (operator alert, retry with
		// a re-issued message_id, etc.) — see codex review on 4d34826.
		if existing.OriginalFilename == entry.OriginalFilename {
			return nil
		}
		return fmt.Errorf("%w: message_id=%q first_filename=%q second_filename=%q",
			ErrLedgerEntryCollision, entry.MessageID, existing.OriginalFilename, entry.OriginalFilename)
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
}

// MarkPendingReplied transitions a pending entry to status "replied" and
// populates reply_sent_at + reply_message_id. Returns ErrLedgerEntryNotFound
// if no entry matches; ErrLedgerEntryNotPending if the entry exists but has
// already left the pending state (idempotency hint to the caller, not a
// fatal protocol violation).
func MarkPendingReplied(cfg *Config, agentID, messageID, replyMessageID string, replySentAt time.Time) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("MarkPendingReplied: message_id required")
	}
	rid := strings.TrimSpace(replyMessageID)
	if rid == "" {
		// Spec rev3 A.9 records reply_sent_at + reply_message_id together
		// on transition. Allowing an empty reply_message_id would discharge
		// the obligation without a durable reference to the reply, which
		// breaks traceability — refuse rather than persist a half-populated
		// row (codex review on 4d34826).
		return fmt.Errorf("MarkPendingReplied: reply_message_id required")
	}
	ledger, err := LoadPendingLedger(cfg, agentID)
	if err != nil {
		return err
	}
	idx, ok := indexByMessageID(ledger, messageID)
	if !ok {
		return ErrLedgerEntryNotFound
	}
	if ledger.Pending[idx].Status != PendingReplyStatusPending {
		return ErrLedgerEntryNotPending
	}
	sentAt := formatLedgerTimestamp(replySentAt)
	ledger.Pending[idx].Status = PendingReplyStatusReplied
	ledger.Pending[idx].ReplySentAt = &sentAt
	ledger.Pending[idx].ReplyMessageID = &rid
	return SavePendingLedger(cfg, agentID, ledger)
}

// MarkPendingDeferred transitions a pending entry to status "deferred". The
// reason is required; `deferredUntil` is optional (empty string ⇒ nil in
// the JSON, meaning "no scheduled unblock time").
func MarkPendingDeferred(cfg *Config, agentID, messageID, reason, deferredUntil string, deferredAt time.Time) error {
	if err := ValidateAgentID(agentID); err != nil {
		return err
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("MarkPendingDeferred: message_id required")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("MarkPendingDeferred: reason required")
	}
	ledger, err := LoadPendingLedger(cfg, agentID)
	if err != nil {
		return err
	}
	idx, ok := indexByMessageID(ledger, messageID)
	if !ok {
		return ErrLedgerEntryNotFound
	}
	if ledger.Pending[idx].Status != PendingReplyStatusPending {
		return ErrLedgerEntryNotPending
	}
	at := formatLedgerTimestamp(deferredAt)
	ledger.Pending[idx].Status = PendingReplyStatusDeferred
	ledger.Pending[idx].DeferredAt = &at
	r := reason
	ledger.Pending[idx].DeferredReason = &r
	if until := strings.TrimSpace(deferredUntil); until != "" {
		ledger.Pending[idx].DeferredUntil = &until
	}
	return SavePendingLedger(cfg, agentID, ledger)
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

// FindByMessageID returns the entry with matching message_id, if any.
func (l *PendingLedger) FindByMessageID(messageID string) (PendingReplyEntry, bool) {
	for _, e := range l.Pending {
		if e.MessageID == messageID {
			return e, true
		}
	}
	return PendingReplyEntry{}, false
}

func indexByMessageID(l *PendingLedger, messageID string) (int, bool) {
	for i, e := range l.Pending {
		if e.MessageID == messageID {
			return i, true
		}
	}
	return 0, false
}

// formatLedgerTimestamp returns RFC3339 UTC at second precision — matches
// the spec example's `recorded_at: "2026-05-19T17:54:30Z"` shape and the
// archive filename timestamp granularity.
func formatLedgerTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
