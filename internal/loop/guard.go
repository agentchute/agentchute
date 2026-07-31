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

// GuardRecoveredMark is the on-disk shape at <loop>/state/<id>/guard.recovered
// (v2.5 plan A7 addendum, mixed hook-trust recovery — codex review round 3
// finding #1, tightened by grok's A1 attack on an earlier draft, ruled by
// claude-code review round 4). It records that THIS session ran
// `guard --recover`: its mailbox capability (ack/check/turn-end) was
// restored, but scope-expanding tools stay denied for the rest of the
// session — cleared ONLY by a session boundary (a new, different serve
// token from a fresh launch), never by anything a model can invoke. See
// cmdGuard's --recover doc comment (cli/guard.go) for the full design
// rationale, including why an age- or latch-based clear alone is NOT
// sufficient (grok's A1: recover → mailbox unlocked → turn-end → clears
// whatever gated the mark → scope tools unlocked too, if the mark were
// clearable by anything short of an actual relaunch).
type GuardRecoveredMark struct {
	V       int       `json:"v"`
	Session string    `json:"session"`
	SetAt   time.Time `json:"set_at"`
}

// SetGuardRecoveredMark durably records that `session` has recovered id's
// guard. Idempotent for the same session (re-running --recover does not
// disturb SetAt).
func SetGuardRecoveredMark(cfg *Config, id, session string) error {
	if err := ValidateAgentID(id); err != nil {
		return err
	}
	if session == "" {
		return fmt.Errorf("SetGuardRecoveredMark: session must not be empty")
	}
	return withAgentLock(cfg, id, func() error {
		existing, err := readGuardRecoveredMark(cfg, id)
		if err != nil {
			existing = nil // absent or corrupt: nothing to preserve; always safe to (re)write.
		}
		if existing != nil && existing.Session == session {
			return nil
		}
		mark := GuardRecoveredMark{V: 1, Session: session, SetAt: time.Now().UTC()}
		return writeGuardRecoveredMark(cfg, id, &mark)
	})
}

// ReadGuardRecoveredMark reads id's recovered mark. Absent surfaces
// os.ErrNotExist. Callers deciding policy on a read error MUST choose their
// own fail direction deliberately: unlike the latch (which always fails
// open), the recovered mark fails open for MAILBOX purposes but fails
// CLOSED (treat as still recovered/restricted) for SCOPE-expanding
// purposes — a corrupted mark file must never become a way to regain
// scope-expanding capability. See evaluateGuardDecision (cli/guard.go).
func ReadGuardRecoveredMark(cfg *Config, id string) (*GuardRecoveredMark, error) {
	if err := ValidateAgentID(id); err != nil {
		return nil, err
	}
	var mark *GuardRecoveredMark
	err := withAgentLock(cfg, id, func() error {
		m, err := readGuardRecoveredMark(cfg, id)
		if err != nil {
			return err
		}
		mark = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mark, nil
}

func readGuardRecoveredMark(cfg *Config, id string) (*GuardRecoveredMark, error) {
	data, err := ReadFileLimit(cfg.GuardRecoveredMarkPath(id), MaxGuardLatchBytes)
	if err != nil {
		return nil, err
	}
	var mark GuardRecoveredMark
	if err := json.Unmarshal(data, &mark); err != nil {
		return nil, fmt.Errorf("parse guard recovered mark %s: %w", id, err)
	}
	return &mark, nil
}

func writeGuardRecoveredMark(cfg *Config, id string, mark *GuardRecoveredMark) error {
	data, err := json.MarshalIndent(mark, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal guard recovered mark: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(cfg.GuardRecoveredMarkPath(id), data)
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
