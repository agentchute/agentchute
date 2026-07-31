package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// guard.go — the per-session PreToolUse guard latch (v2.5 plan A7, C21-C23;
// decision §9 rev 2.3). The latch is the durable fact "this session is
// currently holding claimed-but-unacked mail" — set the moment a session
// claims or is shown any claimed message (fresh or crash-redelivered
// residue), cleared ONLY by that same session's own end-of-turn handler
// (turn_end.go). It is NEVER derived from `.claimed` directory emptiness:
// draining `.claimed` by any means other than turn-end must not unlock
// anything (the claim-then-abandon attack this exists to close).
//
// A latch whose stored session does not match the CURRENT session key is a
// DEAD latch (a crashed/relaunched session's leftover): every reader treats
// it as unset for their own purposes, and the next SetGuardLatch silently
// overwrites it. There is no cross-session cleanup step — a dead latch is
// simply inert until overwritten.

// MaxGuardLatchBytes caps the guard latch file on read (defense against a
// hand-corrupted/runaway file).
const MaxGuardLatchBytes = 4 << 10

// GuardLatch is the on-disk shape at <loop>/state/<id>/guard.latch.
type GuardLatch struct {
	V       int       `json:"v"`
	Session string    `json:"session"`
	SetAt   time.Time `json:"set_at"`
}

// SetGuardLatch durably records that `session` now holds id's guard latch.
// Idempotent: re-setting the SAME session is a no-op write (still succeeds,
// still under the lock, but does not disturb SetAt — a session that claims
// several messages across one turn does not need its latch to "re-arm").
func SetGuardLatch(cfg *Config, id, session string) error {
	if err := ValidateAgentID(id); err != nil {
		return err
	}
	if session == "" {
		return fmt.Errorf("SetGuardLatch: session must not be empty")
	}
	return withAgentLock(cfg, id, func() error {
		// Any read failure — absent OR corrupt/unparseable — is treated as
		// "no existing claim to respect": a hand-corrupted or truncated latch
		// file is not a valid hold by any session, so it must never refuse a
		// fresh set (codex review, PR #89 finding #4 — a corrupt latch must
		// never itself become the reason a lane wedges shut).
		existing, err := readGuardLatch(cfg, id)
		if err != nil {
			existing = nil
		}
		if existing != nil && existing.Session == session {
			return nil // already set for this session; SetAt stays as first-set.
		}
		latch := GuardLatch{V: 1, Session: session, SetAt: time.Now().UTC()}
		return writeGuardLatch(cfg, id, &latch)
	})
}

// ReadGuardLatch reads id's current guard latch. Absent surfaces
// os.ErrNotExist. Read under WithAgentLock(id) — cheap (one flock + one small
// file read) and keeps every latch access serialized through the same
// primitive, matching C21's "all under WithAgentLock(id)" instruction; a
// caller on the allow fast-path (guard.go) takes exactly this one lock and
// nothing else.
func ReadGuardLatch(cfg *Config, id string) (*GuardLatch, error) {
	if err := ValidateAgentID(id); err != nil {
		return nil, err
	}
	var latch *GuardLatch
	err := withAgentLock(cfg, id, func() error {
		l, err := readGuardLatch(cfg, id)
		if err != nil {
			return err
		}
		latch = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return latch, nil
}

// ClearGuardLatch clears id's guard latch ONLY if its stored session matches
// `session` exactly. A latch belonging to a different (foreign or dead)
// session, one that fails to read at all (absent OR corrupt/unparseable), or
// no latch at all, is left completely untouched — clearing is not a "reset
// to unset for anyone", it is "release MY hold". Idempotent: clearing an
// absent, corrupt, or already-foreign latch is a no-op success — a read
// failure here must never become a hard error, or turn-end's caller would
// wedge on every future call once the file is corrupt (codex review, PR #89
// finding #4).
func ClearGuardLatch(cfg *Config, id, session string) error {
	if err := ValidateAgentID(id); err != nil {
		return err
	}
	return withAgentLock(cfg, id, func() error {
		existing, err := readGuardLatch(cfg, id)
		if err != nil {
			return nil // absent OR corrupt: nothing (identifiably ours) to clear.
		}
		if existing.Session != session {
			return nil // foreign/dead latch: not ours to clear.
		}
		if rmErr := os.Remove(cfg.GuardLatchPath(id)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("clear guard latch %s: %w", id, rmErr)
		}
		return syncDir(cfg.AgentStateDir(id))
	})
}

// ClearStaleGuardLatch is the recovery path for a lane wedged by a mixed
// hook-trust state — e.g. a vendor whose PreToolUse guard hook is active
// while its Stop hook (which would normally run turn-end) is independently
// disabled or failing (codex review, PR #89 round 3 finding #1; claude-code
// review round 4). In that state the active guard denies a model's own
// direct `turn-end` invocation too (turn-end is deliberately deny-listed to
// prevent a same-turn self-unlock bypass), so neither the automatic nor the
// manual recovery path works. This is the third path: age, not session
// identity, is the authorization. A latch old enough that no legitimate
// single turn could still be holding it is presumed abandoned and force-
// cleared regardless of which session (if any) set it — deliberately NOT
// gated on a session match, since the whole point is recovering when we
// cannot prove which session (if any) is still alive.
//
// Returns cleared=true only if a latch existed, read back successfully, AND
// was at least olderThan old. A latch younger than olderThan is left
// UNTOUCHED and returns cleared=false, found=true, err=nil — refusing is the
// safe default (it might be an active turn), not an error. No latch at all,
// or one that fails to read (corrupt), returns cleared=false, found=false,
// err=nil: there is nothing this call needs to do in either case.
//
// This is intentionally NOT reachable through cmdGuard's --pre-tool-use deny
// list (guard.go's guardAgentchuteSubcmdRE does not list "guard" as a
// sensitive subcommand): age-gating, not the deny list, is what keeps this
// safe from being used to instantly clear a session's own FRESH latch mid-
// turn — the same latch a model would want to bypass is, by construction,
// too young to qualify.
//
// found reports whether a latch existed and read back successfully at all
// (decoupled from cleared/age, which would otherwise be ambiguous between
// "no latch" and "a latch so fresh its age rounds to zero").
func ClearStaleGuardLatch(cfg *Config, id string, olderThan time.Duration, now time.Time) (cleared, found bool, age time.Duration, err error) {
	if err := ValidateAgentID(id); err != nil {
		return false, false, 0, err
	}
	err = withAgentLock(cfg, id, func() error {
		existing, rerr := readGuardLatch(cfg, id)
		if rerr != nil {
			return nil // absent or corrupt: nothing to clear here.
		}
		found = true
		a := now.Sub(existing.SetAt)
		if a < 0 {
			a = 0 // future-dated (clock skew) reads as fresh, never stale.
		}
		age = a
		if age < olderThan {
			return nil // too young to presume abandoned; refuse (cleared stays false).
		}
		if rmErr := os.Remove(cfg.GuardLatchPath(id)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("clear stale guard latch %s: %w", id, rmErr)
		}
		if serr := syncDir(cfg.AgentStateDir(id)); serr != nil {
			return serr
		}
		cleared = true
		return nil
	})
	return cleared, found, age, err
}

// readGuardLatch is the lock-free body shared by the exported, lock-taking
// entry points above. Callers MUST already hold withAgentLock(id).
func readGuardLatch(cfg *Config, id string) (*GuardLatch, error) {
	data, err := ReadFileLimit(cfg.GuardLatchPath(id), MaxGuardLatchBytes)
	if err != nil {
		return nil, err
	}
	var latch GuardLatch
	if err := json.Unmarshal(data, &latch); err != nil {
		return nil, fmt.Errorf("parse guard latch %s: %w", id, err)
	}
	return &latch, nil
}

// writeGuardLatch is the lock-free body shared by SetGuardLatch. Callers
// MUST already hold withAgentLock(id).
func writeGuardLatch(cfg *Config, id string, latch *GuardLatch) error {
	data, err := json.MarshalIndent(latch, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal guard latch: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(cfg.GuardLatchPath(id), data)
}
