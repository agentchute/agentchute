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
// KEY: the primary key is the trusted committed old or new message identity.
// From == the asker (== the ledger owner); To == the recipient the asker is
// owed a reply by.

// MaxOwedLedgerBytes caps the on-disk `.owed` file (refuses runaway/hand-corrupted state).
const MaxOwedLedgerBytes = 4 << 20

// ReplyOwedDeadline is the default age after which an unanswered asker obligation
// is reported as EXPIRED — the asker-side dead-recipient signal the gate surfaces
// (non-blocking). Aligned with the gate's StaleRegThreshold (30m) so a recipient
// that has gone stale and one that has missed a reply deadline read as urgent on
// the same horizon. send --ask records `now + ReplyOwedDeadline` unless overridden
// by --reply-by.
const ReplyOwedDeadline = 30 * time.Minute

// OwedIdentity is either supported committed message identity.
type OwedIdentity interface {
	OwedKey() OwedKey
}

// OwedKey is the comparable dual-format obligation key.
type OwedKey struct {
	To     string
	From   string
	Seq    uint64
	Stamp  string
	Suffix string
}

// OwedKey converts a legacy message identity into an obligation key.
func (m MsgID) OwedKey() OwedKey {
	return OwedKey{To: m.To, From: m.From, Seq: m.Seq}
}

// OwedKey converts a timestamp message identity into an obligation key.
func (t TsID) OwedKey() OwedKey {
	return OwedKey{To: t.To, From: t.From, Stamp: t.Stamp, Suffix: t.Suffix}
}

// OwedKey satisfies OwedIdentity.
func (k OwedKey) OwedKey() OwedKey { return k }

// Equal reports whether two obligation identities have the same exact form and
// components.
func (k OwedKey) Equal(other OwedIdentity) bool {
	return k == other.OwedKey()
}

// RefString renders the reference matching this key's identity form.
func (k OwedKey) RefString() string {
	if k.Seq > 0 {
		return (MsgID{To: k.To, From: k.From, Seq: k.Seq}).RefString()
	}
	return (TsID{To: k.To, From: k.From, Stamp: k.Stamp, Suffix: k.Suffix}).RefString()
}

func validateOwedKey(key OwedKey) error {
	if err := ValidateAgentID(key.To); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if err := ValidateAgentID(key.From); err != nil {
		return fmt.Errorf("from: %w", err)
	}

	hasSeq := key.Seq > 0
	hasTsFields := key.Stamp != "" || key.Suffix != ""
	if hasSeq && hasTsFields {
		return fmt.Errorf("both seq and timestamp identities are set")
	}
	if hasSeq {
		return nil
	}
	if !hasTsFields {
		return fmt.Errorf("neither seq nor timestamp identity is set")
	}
	if _, ok := ParseStamp(key.Stamp); !ok || !tsSuffixRE.MatchString(key.Suffix) {
		return fmt.Errorf("invalid timestamp identity")
	}
	return nil
}

// OwedEntry is one outstanding obligation. From=asker (ledger owner),
// To=recipient owing the reply; exactly one identity form must be present.
type OwedEntry struct {
	To         string    `json:"to"`
	From       string    `json:"from"`
	Seq        uint64    `json:"seq,omitempty"`
	Stamp      string    `json:"stamp,omitempty"`
	Suffix     string    `json:"suffix,omitempty"`
	By         time.Time `json:"by"` // deadline; By < now => expired (dead-recipient signal).
	RecordedAt time.Time `json:"recorded_at"`
}

// Key returns the committed delivery identity this obligation is keyed on.
func (e OwedEntry) Key() OwedKey {
	return OwedKey{To: e.To, From: e.From, Seq: e.Seq, Stamp: e.Stamp, Suffix: e.Suffix}
}

// MatchesRef reports whether this entry is keyed by the supplied identity.
func (e OwedEntry) MatchesRef(key OwedIdentity) bool {
	return e.Key().Equal(key)
}

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
// entries are surfaced — defense-in-depth so a hand-edited/peer-corrupted state
// file cannot reach the gate with a path-escaping or unclearable value.
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
		if err := validateOwedKey(e.Key()); err != nil {
			return nil, fmt.Errorf("parse owed ledger %s: entry %d: %w", path, i, err)
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
// keyed by either committed identity form. Re-recording the same key is a no-op.
// Runs under withAgentLock(asker). `now` is injectable for deterministic
// recorded_at.
func RecordOwed(cfg *Config, asker string, identity OwedIdentity, by, now time.Time) error {
	if err := ValidateAgentID(asker); err != nil {
		return err
	}
	if identity == nil {
		return fmt.Errorf("RecordOwed: identity is nil")
	}
	key := identity.OwedKey()
	if err := validateOwedKey(key); err != nil {
		return fmt.Errorf("RecordOwed: %w", err)
	}
	if key.From != asker {
		return fmt.Errorf("RecordOwed: key.From %q must equal asker %q (the ledger owner)", key.From, asker)
	}
	return withAgentLock(cfg, asker, func() error {
		ledger, err := LoadOwedLedger(cfg, asker)
		if err != nil {
			return err
		}
		for _, e := range ledger.Owed {
			if e.MatchesRef(key) {
				return nil // idempotent: obligation already recorded.
			}
		}
		ledger.Owed = append(ledger.Owed, OwedEntry{
			To:         key.To,
			From:       key.From,
			Seq:        key.Seq,
			Stamp:      key.Stamp,
			Suffix:     key.Suffix,
			By:         by.UTC(),
			RecordedAt: now.UTC(),
		})
		return SaveOwedLedger(cfg, asker, ledger)
	})
}

// ClearOwed discharges the obligation keyed by either supported identity form.
// A non-matching key removes nothing. Clearing an absent key is idempotent.
func ClearOwed(cfg *Config, asker string, identity OwedIdentity) error {
	if err := ValidateAgentID(asker); err != nil {
		return err
	}
	if identity == nil {
		return fmt.Errorf("ClearOwed: identity is nil")
	}
	key := identity.OwedKey()
	if err := validateOwedKey(key); err != nil {
		return fmt.Errorf("ClearOwed: %w", err)
	}
	return withAgentLock(cfg, asker, func() error {
		ledger, err := LoadOwedLedger(cfg, asker)
		if err != nil {
			return err
		}
		kept := make([]OwedEntry, 0, len(ledger.Owed))
		removed := false
		for _, e := range ledger.Owed {
			if e.MatchesRef(key) {
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
