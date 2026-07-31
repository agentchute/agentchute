package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// seq.go — the LEGACY protocol-v2 identity tuple (dual-read only, v2.5 plan
// B7) plus the recipient-reachability + locked-delivery machinery shared by
// both grammars.
//
// The durable, monotonic, per-(sender,recipient) sequence ALLOCATOR that used
// to live here is DELETED (v2.5 plan B7): the new write path is
// timestamp+random-suffix identity (tsid.go) minted under a single per-sender
// monotonic floor (floor.go), not a per-(from,to) counter tower. What
// SURVIVES here is read-only: `MsgID` and its parsers, so OLD-format mail
// already on disk (and OLD-format `in_reply_to` refs already recorded in
// `.owed` ledgers) keeps parsing forever (B6's dual-read choke point,
// parseAnyInboxName in inbox.go, already reads both grammars).
//
// The committed identity is the full delivery key (to,from,seq) for the OLD
// grammar — NOT a bare seq and NOT a sender-asserted message_id. The
// recipient (`to`) is encoded by LOCATION (which inbox directory), so it does
// NOT appear in the canonical filename; only (from,seq) are spelled there.

// MsgID is the LEGACY committed protocol-v2 delivery key (v2.5 plan B7: reads
// only — no writer mints these anymore). It remains the shared, load-bearing
// identity used by dual-read parsing (inbox.go) and by owed.go (obligation
// key) for any OLD-format mail/reference still on disk.
//
//	To   — recipient agent_id (the inbox the message lands in; encoded by
//	       LOCATION, so NOT part of Filename()).
//	From — sender agent_id.
//	Seq  — per-(From,To) durable+monotonic sequence number, starting at 1.
type MsgID struct {
	To   string `json:"to"`
	From string `json:"from"`
	Seq  uint64 `json:"seq"`
}

// Filename returns the LEGACY inbox filename for this identity:
//
//	from-<from>_seq-<020d>.md
//
// Seq is zero-padded to 20 digits (the max decimal width of uint64) so a plain
// lexicographic sort of a sender's files == per-sender FIFO by seq, with NO
// clock (O1 exact). `To` is intentionally absent: it is the inbox directory.
func (m MsgID) Filename() string {
	return fmt.Sprintf("from-%s_seq-%020d%s", m.From, m.Seq, inboxFilenameSuffix)
}

// Equal reports whether two identities denote the same committed delivery key.
func (m MsgID) Equal(other MsgID) bool {
	return m.To == other.To && m.From == other.From && m.Seq == other.Seq
}

// RefString returns the canonical, copyable in_reply_to reference for this
// identity:
//
//	to-<to>_from-<from>_seq-<020d>
//
// Unlike Filename(), RefString spells ALL THREE components (including `to`)
// because a reply travels to a DIFFERENT inbox than the original — the location
// no longer encodes `to`, so it must ride in the ref. The asker records its
// `.owed` obligation keyed on this exact tuple; the recipient echoes the ref as
// the reply's in_reply_to; the asker's `check` parses it and discharges the
// obligation (ClearOwed). Seq is zero-padded to 20 digits to round-trip cleanly
// through ParseMsgIDRef.
func (m MsgID) RefString() string {
	return fmt.Sprintf("to-%s_from-%s_seq-%020d", m.To, m.From, m.Seq)
}

// msgIDRefRE parses the canonical in_reply_to reference emitted by RefString. It
// is deliberately strict (both slugs match the agent_id rules; seq is exactly 20
// digits) so a freeform threading hint never accidentally parses as a delivery
// key and clears the wrong obligation.
var msgIDRefRE = regexp.MustCompile(
	`^to-(` + agentIDPattern + `)_from-(` + agentIDPattern + `)_seq-(\d{20})$`,
)

// ParseMsgIDRef inverts RefString. It returns the (To,From,Seq) identity and
// ok=true only when s is a well-formed canonical ref with BOTH slugs passing
// ValidateAgentID; otherwise ok=false (a non-canonical in_reply_to value — e.g.
// a legacy RFC3339 message_id — is simply ignored by the owed flip).
func ParseMsgIDRef(s string) (MsgID, bool) {
	m := msgIDRefRE.FindStringSubmatch(s)
	if m == nil {
		return MsgID{}, false
	}
	if err := ValidateAgentID(m[1]); err != nil {
		return MsgID{}, false
	}
	if err := ValidateAgentID(m[2]); err != nil {
		return MsgID{}, false
	}
	seq, err := strconv.ParseUint(m[3], 10, 64)
	if err != nil {
		return MsgID{}, false
	}
	return MsgID{To: m[1], From: m[2], Seq: seq}, true
}

// seqFilenameRE parses the canonical filename. It is deliberately strict: seq
// MUST be exactly 20 digits (the zero-padded form Filename emits), so a
// malformed or otherwise-unrecognized name never silently parses as a seq
// message.
var seqFilenameRE = regexp.MustCompile(
	`^from-(` + agentIDPattern + `)_seq-(\d{20})\.md$`,
)

// ParseSeqFilename inverts MsgID.Filename for the LEGACY format only. It
// returns the (From,Seq) pair — the recipient (`To`) is NOT recoverable from
// the name (it is the enclosing inbox directory). The captured sender MUST
// pass ValidateAgentID; otherwise ok=false.
func ParseSeqFilename(name string) (from string, seq uint64, ok bool) {
	m := seqFilenameRE.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	if err := ValidateAgentID(m[1]); err != nil {
		return "", 0, false
	}
	n, err := strconv.ParseUint(m[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return m[1], n, true
}

// syncSeqInboxDir is the final post-link durability barrier. It is a package
// variable so tests can prove that a failure after the canonical link exists is
// reported as committed rather than as a safe-to-retry failure.
var syncSeqInboxDir = syncDir

// maxLinkCollisionRetries bounds link-EEXIST retries with a FRESH suffix (C4).
// 128-bit suffixes make a real collision unreachable; this only bounds
// pathological filesystem behavior. EEXIST is NEVER treated as success under
// this grammar (unlike the deleted seq allocator's alreadyLanded semantics,
// where EEXIST on the SAME counter meant "this exact message already
// landed") — a (to,from,stamp,suffix) collision would mean two DIFFERENT
// messages independently drew the identical 128-bit suffix, never "the same
// message resent".
const maxLinkCollisionRetries = 3

// writeTsMessage lands content at inboxDir/<id.Filename()> via a tmp+link
// discipline (v2.5 plan B7). Unlike the deleted writeSeqMessage, an EEXIST is
// NEVER treated as success (C4) — it is returned as-is (wrapping
// os.ErrExist) so the caller can retry with a fresh suffix.
func writeTsMessage(inboxDir string, id TsID, content []byte) (committed bool, err error) {
	if err := ValidateAgentID(id.From); err != nil {
		return false, fmt.Errorf("from: %w", err)
	}
	if !dirExists(inboxDir) {
		return false, os.ErrNotExist
	}

	tempFile, err := os.CreateTemp(inboxDir, tempFilePrefix+"*")
	if err != nil {
		return false, err
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := writeAndSyncOpenFile(tempFile, content); err != nil {
		return false, err
	}

	finalPath := filepath.Join(inboxDir, id.Filename())
	if err := linkNoClobber(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, err // caller retries with a fresh suffix (C4).
		}
		return false, fmt.Errorf("link to %s: %w", finalPath, err)
	}
	if err := syncSeqInboxDir(inboxDir); err != nil {
		return true, err // linked but the post-link durability barrier failed — still committed (A5 partial-success rule).
	}
	return true, nil
}

// ErrRecipientUnknown classifies a recipient with no registration row at all.
// It wraps os.ErrNotExist so errors.Is(err, os.ErrNotExist) callers keep
// working unchanged. NOTE: the legacy os.IsNotExist does NOT see through
// this wrap — it only unwraps *PathError/*LinkError/*SyscallError, not an
// arbitrary fmt.Errorf %w chain; callers checking this specific error must
// use errors.Is, not os.IsNotExist.
var ErrRecipientUnknown = fmt.Errorf("loop: recipient has no registration row: %w", os.ErrNotExist)

// ErrRecipientUnreadable classifies a recipient whose registration row EXISTS
// but could not be parsed (a malformed agents/<to>.md — hand-edited, torn
// write, or otherwise corrupt). Unlike ErrRecipientUnknown, the row is
// physically present, so this is neither "no row" nor a stale-vs-fresh
// question — a row we cannot parse proves NOTHING about reachability, and
// must never be treated as fresh (codex/claude-code review, PR #95 P1: an
// earlier version fell back to the file's mtime here, mirroring sweep.go's
// registrationAge — but that fallback exists for the OPPOSITE requirement.
// Sweep needs a corrupt row to age out rather than be immortal against
// CLEANUP; send needs the opposite direction entirely: an unparseable row
// must fail CLOSED against DELIVERY, exactly like VerifyFence/
// AcquireServeLease already fail closed on an unreadable serve claim. One
// helper's fallback cannot serve both directions, so send does not use it).
var ErrRecipientUnreadable = errors.New("loop: recipient's registration row exists but could not be parsed")

// RecipientReachability is the outcome of checking whether a recipient is
// currently reachable enough to accept a send (B3, C29): registered, and if
// so, how old its last heartbeat is relative to the pool's stale_after.
type RecipientReachability struct {
	LastSeen  time.Time     // parsed registration LastSeen.
	Age       time.Duration // now - LastSeen, NOT clamped (a future-dated LastSeen reads as extra-fresh here — the safe direction for a delivery decision, unlike sweep's cleanup decision, C12/B1).
	Threshold time.Duration // loop.StaleAfter(cfg) at the moment of the check.
	Fresh     bool          // Age <= Threshold.
}

// CheckRecipientReachability reads to's registration WITHOUT taking any lock
// — this is send's fast, lock-free preflight (B3 §4 risk: WithAgentLock's
// ensurePrivateDir side effect must never manufacture state/<to>/ for an
// arbitrary --to typo). Returns ErrRecipientUnknown when no row exists at all
// (fails CLOSED against os.ErrNotExist), ErrRecipientUnreadable when a row
// exists but fails to parse (fails CLOSED — no mtime fallback; see that
// error's doc comment for why), or the reachability snapshot with Fresh
// telling the caller whether to proceed.
func CheckRecipientReachability(cfg *Config, to string, now time.Time) (RecipientReachability, error) {
	if err := ValidateAgentID(to); err != nil {
		return RecipientReachability{}, err
	}
	path := cfg.AgentRegistrationPath(to)
	reg, err := ReadRegistration(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RecipientReachability{}, ErrRecipientUnknown
		}
		return RecipientReachability{}, ErrRecipientUnreadable
	}
	threshold := StaleAfter(cfg)
	age := now.UTC().Sub(reg.LastSeen.UTC())
	return RecipientReachability{LastSeen: reg.LastSeen, Age: age, Threshold: threshold, Fresh: age <= threshold}, nil
}

// ErrRecipientStale reports that DeliverUnderRecipientLock's re-check found
// to's row present but past stale_after. The caller (cmdSend) knows whether
// its OWN earlier lock-free preflight had already passed — if so, this is
// the fresh-but-racing case (C29c: the row went stale in the gap between
// preflight and this lock, e.g. a fleet-wake storm mid-restart); if the
// caller never preflighted at all (AnnounceEnrollment), this is a plain
// stale classification. Loop stays silent on which C29 wording applies —
// that choice, and the literal text, lives entirely in send.go (AGENTCHUTE.md
// §9: no text may coach registering the recipient, and that ban is easiest to
// audit from a single source).
type ErrRecipientStale struct {
	To        string
	LastSeen  time.Time
	Age       time.Duration
	Threshold time.Duration
}

func (e *ErrRecipientStale) Error() string {
	return fmt.Sprintf("recipient %q registration is stale (last_seen=%s age=%s > stale_after=%s)",
		e.To, e.LastSeen.UTC().Format(time.RFC3339), e.Age.Round(time.Second), e.Threshold)
}

// afterRecipientLockHook, when non-nil, fires inside DeliverUnderRecipientLock
// immediately after WithAgentLock(to) is acquired but BEFORE the freshness
// re-check reads to's registration. Test-only seam: lets a test simulate the
// EXACT race this lock exists to close — to's row going stale or vanishing in
// the gap between cmdSend's lock-free preflight and this lock (e.g. a sweep
// or a peer's send racing in). nil in production.
var afterRecipientLockHook func()

// DeliverUnderRecipientLock is send's single point of TRUTH for whether a
// delivery may proceed (B3): the lock-free preflight in cmdSend (or
// AnnounceEnrollment's per-peer loop, which has none) is a fast-fail
// optimization ONLY, never the enforcement itself — a sweep, a heartbeat, or
// another sender can all land in the window between any earlier check and
// here. Re-derives to's reachability under WithAgentLock(to) and, only if
// still fresh, re-verifies id.From's fence (when serveToken != "") and links
// content into to's inbox under id's timestamp identity (v2.5 plan B7).
//
// On link EEXIST, retries with a FRESH random suffix (id.To/From/Stamp held
// fixed) up to maxLinkCollisionRetries times — NEVER exists-as-success (C4).
// The returned identity is the one that actually landed (its Suffix may
// differ from the caller's initial id if a retry fired); callers must use
// THIS returned identity (Filename()/RefString()), not their original one.
//
// On any failure (ErrRecipientUnknown, ErrRecipientUnreadable,
// *ErrRecipientStale, ErrFenced, exhausted collision retries) nothing is
// linked.
//
// NO LOCK NESTING: id.From's OWN WithAgentLock(id.From) (taken by the
// caller's MintSendStamp) MUST have already released before this is called —
// never call this from inside that lock's closure. Self-send (id.From==to)
// would deadlock the non-reentrant flock instantly if the two were ever held
// together (C8).
func DeliverUnderRecipientLock(cfg *Config, to string, id TsID, content []byte, serveToken string) (committedID TsID, committed bool, err error) {
	if err := ValidateAgentID(to); err != nil {
		return TsID{}, false, fmt.Errorf("to: %w", err)
	}
	lockErr := withAgentLock(cfg, to, func() error {
		if afterRecipientLockHook != nil {
			afterRecipientLockHook()
		}
		rr, rerr := CheckRecipientReachability(cfg, to, time.Now().UTC())
		if rerr != nil {
			return rerr
		}
		if !rr.Fresh {
			return &ErrRecipientStale{To: to, LastSeen: rr.LastSeen, Age: rr.Age, Threshold: rr.Threshold}
		}
		if serveToken != "" {
			if err := VerifyFence(cfg, id.From, serveToken); err != nil {
				return err
			}
		}
		attempt := id
		for i := 0; i < maxLinkCollisionRetries; i++ {
			c, werr := writeTsMessage(cfg.AgentInboxDir(to), attempt, content)
			if c {
				committedID = attempt
				committed = true
				return werr // nil, or the post-link sync-barrier error (partial success).
			}
			if werr == nil || !errors.Is(werr, os.ErrExist) {
				return werr
			}
			suffix, suffixErr := rand128hex()
			if suffixErr != nil {
				return suffixErr
			}
			attempt.Suffix = suffix
		}
		return fmt.Errorf("link collision for %s after %d attempt(s): %w", to, maxLinkCollisionRetries, os.ErrExist)
	})
	return committedID, committed, lockErr
}
