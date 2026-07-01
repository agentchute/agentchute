package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// owed.go — the ASKER-OWNED obligation ledger, the SOLE reply-obligation
// mechanism (v0.9.0 `.owed` redesign; protocol-v2 TEAM-DECISION §3).
//
// The reply obligation is asker-owned only: "I am owed a reply to (to,from,seq)
// from <recipient> by <T>." Held as an asker-LOCAL `.owed` ledger
// (single-writer, atomic rename). The gate reads ONLY its own `.owed`; it never
// scans peers. It is NON-BLOCKING: an outstanding/expired obligation surfaces as
// a gate WARNING, never a finish blocker.
//
// v0.9.0 subtraction: the recipient-side pending-reply ledger AND the `defer`
// command were REMOVED. Recipients are NEVER blocked at finish by a
// reply_required message — delivery is best-effort pull, with no forcing
// function once delivered. Reply obligations live exclusively on the asker side.
//
// A dead recipient still surfaces TWICE OVER — the asker's expired obligation
// (here) AND the recipient's stale `.live` (live.go) — so the asker never waits
// on a corpse.
//
// KEY: the primary key is the trusted committed identity MsgID{To,From,Seq}.
// From == the asker (== the ledger owner); To == the recipient the asker is owed
// a reply by.

// MaxOwedLedgerBytes caps the on-disk `.owed` file (refuses runaway/hand-corrupted state).
const MaxOwedLedgerBytes = 4 << 20

// ReplyOwedDeadline is the default age after which an unanswered asker obligation
// is reported as EXPIRED — the asker-side dead-recipient signal the gate surfaces
// (non-blocking). Aligned with the gate's StaleRegThreshold (30m) so a recipient
// that has gone stale and one that has missed a reply deadline read as urgent on
// the same horizon. send --ask records `now + ReplyOwedDeadline` unless overridden
// by --reply-by.
const ReplyOwedDeadline = 30 * time.Minute

// OwedEntry is one outstanding obligation. From=asker (ledger owner),
// To=recipient owing the reply; key = MsgID{To,From,Seq}.
type OwedEntry struct {
	To         string    `json:"to"`
	From       string    `json:"from"`
	Seq        uint64    `json:"seq"`
	By         time.Time `json:"by"` // deadline; By < now => expired (dead-recipient signal).
	RecordedAt time.Time `json:"recorded_at"`
}

// Key returns the committed delivery identity this obligation is keyed on.
func (e OwedEntry) Key() MsgID { return MsgID{To: e.To, From: e.From, Seq: e.Seq} }

// OwedLedger is the JSON shape of <loop>/state/<asker>/owed.json.
type OwedLedger struct {
	Owed []OwedEntry `json:"owed"`
}

// owedPath returns the asker's `.owed` ledger path (asker-owned, under the
// asker's private state dir, serialized by withAgentLock(asker)).
func owedPath(cfg *Config, asker string) string {
	return filepath.Join(cfg.AgentStateDir(asker), "owed.json")
}

// LoadOwedLedger reads the asker's ledger. A missing file is not an error
// (returns an empty ledger). Parse errors, oversized files, and NON-CANONICAL
// entries (invalid agent_ids, seq==0) are surfaced — defense-in-depth so a
// hand-edited/peer-corrupted state file can't reach the gate with a
// path-escaping value.
func LoadOwedLedger(cfg *Config, asker string) (*OwedLedger, error) {
	if err := ValidateAgentID(asker); err != nil {
		return nil, err
	}
	path := owedPath(cfg, asker)
	data, err := ReadFileLimit(path, MaxOwedLedgerBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &OwedLedger{Owed: []OwedEntry{}}, nil
		}
		return nil, fmt.Errorf("read owed ledger %s: %w", path, err)
	}
	if len(data) == 0 {
		return &OwedLedger{Owed: []OwedEntry{}}, nil
	}
	var ledger OwedLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parse owed ledger %s: %w", path, err)
	}
	if ledger.Owed == nil {
		ledger.Owed = []OwedEntry{}
	}
	for i, e := range ledger.Owed {
		if err := ValidateAgentID(e.To); err != nil {
			return nil, fmt.Errorf("parse owed ledger %s: entry %d has invalid to: %w", path, i, err)
		}
		if err := ValidateAgentID(e.From); err != nil {
			return nil, fmt.Errorf("parse owed ledger %s: entry %d has invalid from: %w", path, i, err)
		}
		if e.Seq == 0 {
			return nil, fmt.Errorf("parse owed ledger %s: entry %d has zero seq", path, i)
		}
	}
	return &ledger, nil
}

// SaveOwedLedger writes the ledger atomically (single-writer, atomic rename).
func SaveOwedLedger(cfg *Config, asker string, ledger *OwedLedger) error {
	if err := ValidateAgentID(asker); err != nil {
		return err
	}
	if ledger == nil {
		return fmt.Errorf("SaveOwedLedger: ledger is nil")
	}
	if ledger.Owed == nil {
		ledger.Owed = []OwedEntry{}
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal owed ledger: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(owedPath(cfg, asker), data)
}

// RecordOwed records an obligation when asker sends a reply_required message
// keyed (To=recipient, From=asker, Seq). Idempotent on the MsgID key (re-record
// is a no-op). Runs under withAgentLock(asker). `now` is injectable for
// deterministic recorded_at.
func RecordOwed(cfg *Config, asker string, key MsgID, by, now time.Time) error {
	if err := ValidateAgentID(asker); err != nil {
		return err
	}
	if err := ValidateAgentID(key.From); err != nil {
		return fmt.Errorf("RecordOwed: from: %w", err)
	}
	if err := ValidateAgentID(key.To); err != nil {
		return fmt.Errorf("RecordOwed: to: %w", err)
	}
	if key.From != asker {
		return fmt.Errorf("RecordOwed: key.From %q must equal asker %q (the ledger owner)", key.From, asker)
	}
	if key.Seq == 0 {
		return fmt.Errorf("RecordOwed: seq must be >= 1")
	}
	return withAgentLock(cfg, asker, func() error {
		ledger, err := LoadOwedLedger(cfg, asker)
		if err != nil {
			return err
		}
		for _, e := range ledger.Owed {
			if e.Key().Equal(key) {
				return nil // idempotent: obligation already recorded.
			}
		}
		ledger.Owed = append(ledger.Owed, OwedEntry{
			To:         key.To,
			From:       key.From,
			Seq:        key.Seq,
			By:         by.UTC(),
			RecordedAt: now.UTC(),
		})
		return SaveOwedLedger(cfg, asker, ledger)
	})
}

// ClearOwed discharges the obligation keyed by `key` — called when asker
// consumes a reply whose in_reply_to references (key.To, key.From, key.Seq). A
// non-matching key removes nothing (the obligation stays). Idempotent: clearing
// an absent key is a no-op success. Runs under withAgentLock(asker).
func ClearOwed(cfg *Config, asker string, key MsgID) error {
	if err := ValidateAgentID(asker); err != nil {
		return err
	}
	return withAgentLock(cfg, asker, func() error {
		ledger, err := LoadOwedLedger(cfg, asker)
		if err != nil {
			return err
		}
		kept := make([]OwedEntry, 0, len(ledger.Owed))
		removed := false
		for _, e := range ledger.Owed {
			if e.Key().Equal(key) {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if !removed {
			return nil // nothing matched — leave the ledger untouched.
		}
		ledger.Owed = kept
		return SaveOwedLedger(cfg, asker, ledger)
	})
}

// ExpiredOwed returns the obligations whose deadline has passed (By < now) — the
// asker-side dead-recipient signal the gate surfaces. Pure; copies out.
func (l *OwedLedger) ExpiredOwed(now time.Time) []OwedEntry {
	out := make([]OwedEntry, 0)
	for _, e := range l.Owed {
		if e.By.UTC().Before(now.UTC()) {
			out = append(out, e)
		}
	}
	return out
}

// OutstandingOwed returns every obligation still on the ledger (a reply has not
// been observed yet). Pure; copies out.
func (l *OwedLedger) OutstandingOwed() []OwedEntry {
	out := make([]OwedEntry, len(l.Owed))
	copy(out, l.Owed)
	return out
}
