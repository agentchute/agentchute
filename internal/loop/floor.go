package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// floor.go — the monotonic per-sender send floor (v2.5 plan B7, C7/C8). It
// replaces the per-(from,to) seq allocator tower (seq.go's deleted
// state/<from>/seq/<to>.json files) with ONE durable file per sender: the
// new timestamp+random-suffix identity (tsid.go) needs no per-recipient
// counter — it only needs "never mint a stamp at or before the last one this
// sender issued", so two mints in the same microsecond (or a clock
// regression) can't produce an out-of-order filename within that sender's
// own inbox-sort ordering. Uniqueness across senders needs nothing at all:
// two different senders' messages never share a filename (each lands as
// from-<their-own-id>_... in the recipient's inbox), so two senders may
// legally mint the identical stamp.

// MaxSendFloorBytes caps the per-sender floor file. Tiny in practice; this is
// defense against a runaway/hand-corrupted file, same posture as the deleted
// MaxSeqStateBytes.
const MaxSendFloorBytes = 4 << 20

// sendFloorState is the durable per-sender floor at
// <loop>/state/<from>/send.floor (C7).
type sendFloorState struct {
	V          int    `json:"v"`
	LastIssued string `json:"last_issued,omitempty"` // C2 stamp form; empty = never minted.
}

// sendFloorPath returns from's floor path. Sender-owned, under from's private
// state dir, so it is serialized by the SAME withAgentLock(from) that guards
// from's registration + ledgers — no new lock primitive (C7).
func sendFloorPath(cfg *Config, from string) string {
	return filepath.Join(cfg.AgentStateDir(from), "send.floor")
}

// loadSendFloor reads from's floor. A missing file is not an error (a fresh
// sender has never minted) — it returns a zero-value floor. Caller MUST
// already hold withAgentLock(from).
func loadSendFloor(cfg *Config, from string) (*sendFloorState, error) {
	path := sendFloorPath(cfg, from)
	data, err := ReadFileLimit(path, MaxSendFloorBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &sendFloorState{V: 1}, nil
		}
		return nil, fmt.Errorf("read send floor %s: %w", path, err)
	}
	if len(data) == 0 {
		return &sendFloorState{V: 1}, nil
	}
	var st sendFloorState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse send floor %s: %w", path, err)
	}
	return &st, nil
}

// saveSendFloor durably commits the floor (tmp -> fsync -> rename -> fsync-dir
// via atomicWriteFile). This is the WRITE-AHEAD commit: it happens BEFORE the
// message links, so a crash between persist and link can only ever waste a
// stamp (the next mint is still > the lost one, C7) — never reissue it.
// Caller MUST already hold withAgentLock(from).
func saveSendFloor(cfg *Config, from string, st *sendFloorState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal send floor: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(sendFloorPath(cfg, from), data)
}

// MintSendStamp returns the next monotonic C2-form timestamp for a message
// FROM the given sender, write-ahead persisting it as the new floor BEFORE
// returning (v2.5 plan B7, replaces AllocateSeq).
//
// Mint rule (C7): stamp = now; if stamp <= floor, stamp = floor + 1
// microsecond. This is the ONLY ordering guarantee the floor provides — it
// bounds THIS sender's own stamps to be strictly increasing across restarts
// and clock regressions. Because the fixed-width C2 form sorts
// lexicographically == chronologically, the comparison is a plain string
// compare, no parsing needed on the fast path; parsing the stored floor back
// into a time.Time only happens on the (rare) clamp path, to compute +1us.
//
// serveToken: the active serve lease fence (lease.go). When non-empty,
// MintSendStamp VerifyFence's it INSIDE withAgentLock(from), immediately
// after loading the floor and before persisting it — this is the ONLY fence
// check (fail-closed). The deleted AllocateSeq's OLD outer, pre-lock check is
// deliberately NOT carried over (sonnet plan-review, B7 §4 risk bullet):
// mints are rare, and the in-lock check is the TOCTOU-closing one — a
// reclaimer (AcquireServeLease) also takes withAgentLock(from)==
// withAgentLock(id) while it writes a fresh serve.claim, so it can never run
// CONCURRENTLY with this critical section; it either fully precedes or fully
// follows it under the same lock. A reclaim that already landed before this
// call is therefore always caught here — there is no unprotected outer
// window left to separately guard against. When serveToken is EMPTY, the
// mint is intentionally UNFENCED (callers not running under a serve lease,
// e.g. AnnounceEnrollment from `boot`/`register`).
//
// Lock discipline (C8): this function takes and releases exactly ONE lock
// (from's own) on every return path. The caller MUST let it fully release
// before taking any recipient lock for delivery — self-send (from==to) would
// deadlock the non-reentrant flock instantly if the two were ever held
// together.
func MintSendStamp(cfg *Config, from string, now time.Time, serveToken string) (string, error) {
	if err := ValidateAgentID(from); err != nil {
		return "", fmt.Errorf("from: %w", err)
	}
	var stamp string
	err := withAgentLock(cfg, from, func() error {
		st, err := loadSendFloor(cfg, from)
		if err != nil {
			return err
		}

		// FENCE CHECK — the sole check (see doc comment above for why no
		// outer check is carried over). Runs INSIDE the lock, after loading
		// and BEFORE persisting, so a fenced (reclaimed) holder writes
		// NOTHING. VerifyFence is lock-free, so this does not re-enter
		// withAgentLock.
		if serveToken != "" {
			if err := VerifyFence(cfg, from, serveToken); err != nil {
				return err
			}
		}

		stamp = FormatStamp(now)
		if st.LastIssued != "" && stamp <= st.LastIssued {
			floor, ok := ParseStamp(st.LastIssued)
			if !ok {
				return fmt.Errorf("send floor %s: stored last_issued %q does not parse", sendFloorPath(cfg, from), st.LastIssued)
			}
			stamp = FormatStamp(floor.Add(time.Microsecond))
		}
		st.LastIssued = stamp
		return saveSendFloor(cfg, from, st)
	})
	if err != nil {
		return "", err
	}
	return stamp, nil
}

// SendTsMessageWithCommit is the off-bus convenience composition of
// MintSendStamp + DeliverUnderRecipientLock (v2.5 plan B7, replaces the
// deleted SendSeqMessageWithCommit): mint under from's OWN lock, release,
// THEN take to's lock for delivery — the two locks are NEVER held together
// (C8; self-send would deadlock the non-reentrant flock instantly if they
// were). Both cmdSend and AnnounceEnrollment use this so the lock-ordering
// invariant lives in exactly one place.
//
// serveToken behaves exactly as documented on MintSendStamp and
// DeliverUnderRecipientLock (empty = intentionally unfenced).
func SendTsMessageWithCommit(cfg *Config, from, to string, content []byte, serveToken string) (TsID, bool, error) {
	stamp, err := MintSendStamp(cfg, from, time.Now().UTC(), serveToken)
	if err != nil {
		return TsID{}, false, err
	}
	suffix, err := rand128hex()
	if err != nil {
		return TsID{}, false, err
	}
	id := TsID{To: to, From: from, Stamp: stamp, Suffix: suffix}
	committedID, committed, err := DeliverUnderRecipientLock(cfg, id, content, serveToken)
	return committedID, committed, err
}
