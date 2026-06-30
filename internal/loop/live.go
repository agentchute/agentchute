package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// live.go — the `.live` presence fact (protocol-v2 TEAM-DECISION §5, R1).
//
// Presence is a PUBLISHED FACT WITH FRESHNESS, not a cursor: a fresh `.live`
// means alive; a stale or absent one means not-alive. This is the R1
// dead-mailbox detection — "came back days later, one agent never returned"
// shows up as a stale `.live`. Crucially it avoids the FALSE-DEAD direction R1
// warns about (a busy native-loop agent mid-long-turn): `busy` is ADVISORY only
// and NEVER affects aliveness.
//
// GATE 2: PURELY ADDITIVE. serve will write `.live` each heartbeat tick and
// gate/doctor/roster will read it in Gate 3; nothing here is wired yet.
//
// PLACEMENT: <loop>/live/<id>.live — a PUBLIC presence dir parallel to agents/,
// so Gate 3 roster/gate/doctor readers can read peers' presence (unlike the
// owner-private state/<id>/ ledgers). (Public among a same-uid pool: the dir is
// 0700 like agents/; a multi-uid pool is out of scope — protocol-v2 §7.)

// liveWindow is the freshness window: a `.live` newer than this reads as alive.
// Package var (test-tunable); sized > NFS acregmax in production so attribute
// caching latency never reads a live agent as dead (protocol-v2 §7).
var liveWindow = 30 * time.Second

// MaxLiveBytes caps the `.live` file on read (defense against a corrupt/runaway file).
const MaxLiveBytes = 64 << 10

// Live is the on-disk presence fact at <loop>/live/<id>.live.
//
//	LastSeen — last heartbeat tick (the freshness signal).
//	Busy     — ADVISORY hint (agent mid-long-turn); never affects aliveness.
//	PID/Host — diagnostics for same-host correlation.
type Live struct {
	ID       string    `json:"id"`
	LastSeen time.Time `json:"last_seen"`
	Busy     bool      `json:"busy"`
	PID      int       `json:"pid"`
	Host     string    `json:"host"`
}

// livePath returns the public presence path for id.
func livePath(cfg *Config, id string) string {
	return filepath.Join(cfg.LoopDir, "live", id+".live")
}

// WriteLive publishes id's presence with last_seen=now, busy as given. Atomic
// tmp+rename (atomicWriteFile), so a reader never observes a torn file.
func WriteLive(cfg *Config, id string, busy bool) error {
	return writeLiveAt(cfg, id, busy, time.Now())
}

// writeLiveAt is WriteLive with an injectable clock (tests force an old
// last_seen to exercise the stale-reads-not-alive path).
func writeLiveAt(cfg *Config, id string, busy bool, now time.Time) error {
	if err := ValidateAgentID(id); err != nil {
		return err
	}
	host, _ := os.Hostname()
	live := Live{
		ID:       id,
		LastSeen: now.UTC(),
		Busy:     busy,
		PID:      os.Getpid(),
		Host:     host,
	}
	data, err := json.MarshalIndent(live, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal live %s: %w", id, err)
	}
	data = append(data, '\n')
	return atomicWriteFile(livePath(cfg, id), data)
}

// ReadLive parses id's `.live` fact. An absent file surfaces os.ErrNotExist
// (callers that want "absent => not alive" should use IsLive, which folds that
// in). Use this when you need the actual fields (e.g. busy/last_seen for a
// roster row).
func ReadLive(cfg *Config, id string) (*Live, error) {
	if err := ValidateAgentID(id); err != nil {
		return nil, err
	}
	data, err := ReadFileLimit(livePath(cfg, id), MaxLiveBytes)
	if err != nil {
		return nil, err
	}
	var live Live
	if err := json.Unmarshal(data, &live); err != nil {
		return nil, fmt.Errorf("parse live %s: %w", id, err)
	}
	return &live, nil
}

// IsLive reports whether id is alive: a `.live` exists and its last_seen is
// within window of now. STALE or ABSENT (or unreadable) reads as not-alive and
// is NEVER an error — an unregistered/long-gone agent simply reads not-alive
// (the R1 dead-mailbox detection). `busy` is advisory and does NOT affect this.
func IsLive(cfg *Config, id string, window time.Duration, now time.Time) bool {
	live, err := ReadLive(cfg, id)
	if err != nil {
		return false
	}
	age := now.UTC().Sub(live.LastSeen.UTC())
	if age < 0 {
		age = 0 // future-dated (clock skew) reads as very fresh.
	}
	return age < window
}

// LiveWindow returns the default freshness window callers should pass to IsLive
// when they have no reason to override it.
func LiveWindow() time.Duration { return liveWindow }
