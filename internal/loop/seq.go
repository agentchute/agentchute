package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// seq.go — the protocol-v2 identity tuple + the durable, monotonic,
// per-(sender,recipient) sequence allocator.
//
// The `(to,from,seq)` identity is the canonical inbox format: filenames are
// `from-<from>_seq-<020d>.md` (see ListInboxMessages in inbox.go); this file owns
// the per-(sender,recipient) seq allocator + writer.
//
// The committed identity is the full delivery key (to,from,seq) — NOT a bare
// seq and NOT a sender-asserted message_id (protocol-v2 TEAM-DECISION §2). The
// recipient (`to`) is encoded by LOCATION (which inbox directory), so it does
// NOT appear in the canonical filename; only (from,seq) are spelled there.
//
// WHY a durable+monotonic seq (conformance/seq_durability_test.go is the
// contract): with a write-ahead counter, a crash can only ever produce a GAP
// (an allocated seq whose message never linked), NEVER a reuse. Gaps are legal
// — seq is identity + sort key, not a no-gap contract; O1 tolerates them.
// Reusing a seq for DIFFERENT content (the §7 hazard) is structurally
// impossible FROM THIS ALLOCATOR because a genuinely-new message always
// consumes last_issued+1, never an occupied seq. link()-EEXIST on the canonical
// path means "this exact (to,from,seq) is STILL PRESENT in the inbox", so a
// crash-uncertain resend is a no-op ONLY while the message remains UNCONSUMED.
// Once consumed (archived) the file is gone, link no longer EEXISTs, so a later
// resend of the same (to,from,seq) RE-LANDS as a new delivery — it is NOT
// deduplicated by the protocol. There is no receiver-side dedup backstop; the
// covenant is handler idempotency. (Pre-consume, EEXIST folds the delivery-dedup
// half of C1 into the substrate.)

// MsgID is the committed protocol-v2 delivery key. It is the shared, load-bearing
// identity used by seq.go (writer/parser) and owed.go (obligation key).
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

// Filename returns the canonical NEW-format inbox filename for this identity:
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

// ParseSeqFilename inverts MsgID.Filename for the NEW format only. It returns
// the (From,Seq) pair — the recipient (`To`) is NOT recoverable from the name
// (it is the enclosing inbox directory). The captured sender MUST pass
// ValidateAgentID; otherwise ok=false.
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

// seqRecentWindow bounds how many recently-issued (key -> seq) bindings the
// allocator retains for crash-resend idempotency. It is a package var (not a
// const) ONLY so tests can shrink it to exercise the pruning path; production
// keeps the larger window. A crash-resend whose key was pruned out of the
// window (only on a very delayed resume) allocates a FRESH seq and lands a
// duplicate copy. There is no receiver-side dedup backstop for that duplicate;
// the covenant is handler idempotency. The window is sized for immediate
// post-crash resume.
var seqRecentWindow uint64 = 256

// MaxSeqStateBytes caps the per-(from,to) seq state file. Bounded by the recent
// window; 4 MiB is enormous headroom and refuses a runaway/hand-corrupted file.
const MaxSeqStateBytes = 4 << 20

// seqRecentEntry binds an idempotency key to the seq it was issued, so a
// crash-uncertain resend re-issues the SAME seq instead of consuming a new one.
//
// LOAD-BEARING INVARIANT: idempotencyKey MUST uniquely identify message CONTENT.
// Reusing a key for DIFFERENT content reissues the same seq; the canonical
// (to,from,seq) path then already exists, link-EEXISTs, and the NEW content is
// silently DROPPED (treated as an already-landed duplicate). Callers must derive
// the key from the content (e.g. a content hash) or a per-message unique token,
// never a coarse handle that two distinct messages could share.
type seqRecentEntry struct {
	Key string `json:"key"`
	Seq uint64 `json:"seq"`
}

// seqState is the durable per-(from,to) counter at <loop>/state/<from>/seq/<to>.json.
//
//	LastIssued — highest seq ever issued for (from,to). ONLY ever increases.
//	Recent     — bounded window of (idempotency-key -> seq) bindings.
type seqState struct {
	LastIssued uint64           `json:"last_issued"`
	Recent     []seqRecentEntry `json:"recent"`
}

// seqStatePath returns the durable counter path for (from,to). Sender-owned
// (from = sender/asker), under from's private state dir so it is serialized by
// the SAME withAgentLock(from) that guards from's registration + ledgers — no
// new lock primitive.
func seqStatePath(cfg *Config, from, to string) string {
	return filepath.Join(cfg.AgentStateDir(from), "seq", to+".json")
}

// loadSeqState reads the per-(from,to) counter. A missing file is not an error
// (fresh sender) — it returns a zero-value state. Caller MUST already hold
// withAgentLock(from).
func loadSeqState(cfg *Config, from, to string) (*seqState, error) {
	path := seqStatePath(cfg, from, to)
	data, err := ReadFileLimit(path, MaxSeqStateBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &seqState{}, nil
		}
		return nil, fmt.Errorf("read seq state %s: %w", path, err)
	}
	if len(data) == 0 {
		return &seqState{}, nil
	}
	var st seqState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse seq state %s: %w", path, err)
	}
	return &st, nil
}

// saveSeqState durably commits the counter (tmp -> fsync -> rename -> fsync-dir
// via atomicWriteFile). This DURABLE COMMIT happens BEFORE the message lands
// (write-ahead allocation) — the property that makes a crash produce gaps, never
// reuse. Caller MUST already hold withAgentLock(from).
func saveSeqState(cfg *Config, from, to string, st *seqState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal seq state: %w", err)
	}
	data = append(data, '\n')
	return atomicWriteFile(seqStatePath(cfg, from, to), data)
}

// afterOuterFenceHook, when non-nil, fires inside AllocateSeq AFTER the outer
// (pre-lock) VerifyFence passes but BEFORE withAgentLock(from) is taken. Test-only
// seam: lets a test inject a reclaim into the fence TOCTOU window so the in-lock
// re-verify is exercised. nil in production.
var afterOuterFenceHook func()

// AllocateSeq returns the durable+monotonic seq for the next message from
// `from` to `to`, write-ahead committed BEFORE the caller links the message.
//
// idempotencyKey: when non-empty, a re-call with the same key RE-ISSUES the
// same seq (crash-resend safety) instead of consuming a new one. When EMPTY
// there is NO re-issue path: a crash AFTER the durable counter save but BEFORE
// the message links is AT-MOST-ONCE — the allocated seq is lost as a gap and the
// message is never delivered. An AT-LEAST-ONCE guarantee therefore REQUIRES a
// stable, non-empty idempotencyKey so a resume re-issues the SAME seq and the
// resend lands (delivery-dedup'd by the canonical (to,from,seq) path).
//
// serveToken: the active serve lease fence (lease.go). When non-empty,
// AllocateSeq VerifyFence's it BEFORE persisting the counter, so a zombie/paused
// holder whose lease was reclaimed can NOT advance the counter (closes the
// dup-writer hole launch-time guarding alone leaves open). When EMPTY, the write
// is intentionally UNFENCED for callers that are not running under a serve lease.
//
// FENCING (fail-closed): when serveToken != "", VerifyFence is checked TWICE —
// once OUTSIDE withAgentLock (fast fail-closed) and AGAIN INSIDE the lock,
// immediately after loading and before any mutate/save. The in-lock re-check
// closes the TOCTOU a single outer check leaves open: a reclaimer
// (AcquireServeLease) holds withAgentLock(from)==withAgentLock(id) while it writes
// a fresh serve.claim, so it can land BETWEEN the outer check and our acquisition
// of the lock. Without the re-check the stale (reclaimed) holder would advance and
// persist the counter as the reclaimed id — NOT fail-closed. With it, a reclaimed
// holder returns ErrFenced and allocates/saves NOTHING. VerifyFence is lock-free
// (reads the claim file directly; see lease.go), so the in-lock call does NOT
// violate withAgentLock non-reentrancy.
func AllocateSeq(cfg *Config, from, to, idempotencyKey, serveToken string) (uint64, error) {
	if err := ValidateAgentID(from); err != nil {
		return 0, fmt.Errorf("from: %w", err)
	}
	if err := ValidateAgentID(to); err != nil {
		return 0, fmt.Errorf("to: %w", err)
	}

	if serveToken != "" {
		if err := VerifyFence(cfg, from, serveToken); err != nil {
			return 0, err
		}
	}

	// Test seam: fire AFTER the outer (pre-lock) fence passes but BEFORE we take
	// withAgentLock(from). Lets a test inject a reclaim into the fence TOCTOU
	// window so the in-lock re-verify below is exercised. nil in production.
	if afterOuterFenceHook != nil {
		afterOuterFenceHook()
	}

	var seq uint64
	err := withAgentLock(cfg, from, func() error {
		st, err := loadSeqState(cfg, from, to)
		if err != nil {
			return err
		}

		// FENCE RE-CHECK — closes the AllocateSeq TOCTOU. The outer VerifyFence
		// ran OUTSIDE this lock; a reclaim could have landed since. Re-verify HERE,
		// inside the lock, after loading and BEFORE allocating/saving (covers the
		// re-issue path too, so a fenced holder never even returns a re-issued seq).
		// VerifyFence is lock-free, so this does not re-enter withAgentLock.
		if serveToken != "" {
			if err := VerifyFence(cfg, from, serveToken); err != nil {
				return err
			}
		}

		// RE-ISSUE: an identical resend (same key) gets the seq it already
		// consumed. The state is already durable, so we do NOT re-save.
		if idempotencyKey != "" {
			for _, e := range st.Recent {
				if e.Key == idempotencyKey {
					seq = e.Seq
					return nil
				}
			}
		}

		// Fresh allocation: consume last_issued+1. Monotonic by construction.
		seq = st.LastIssued + 1
		st.LastIssued = seq
		if idempotencyKey != "" {
			st.Recent = append(st.Recent, seqRecentEntry{Key: idempotencyKey, Seq: seq})
			pruneSeqRecent(st)
		}
		return saveSeqState(cfg, from, to, st)
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// pruneSeqRecent drops key bindings older than the retention window
// (seq <= last_issued - window), keeping the recent[] slice bounded.
func pruneSeqRecent(st *seqState) {
	if seqRecentWindow == 0 {
		return
	}
	var minKeep uint64
	if st.LastIssued > seqRecentWindow {
		minKeep = st.LastIssued - seqRecentWindow
	}
	kept := st.Recent[:0]
	for _, e := range st.Recent {
		if e.Seq > minKeep {
			kept = append(kept, e)
		}
	}
	st.Recent = kept
}

// syncSeqInboxDir is the final post-link durability barrier. It is a package
// variable so tests can prove that a failure after the canonical link exists is
// reported as committed rather than as a safe-to-retry failure.
var syncSeqInboxDir = syncDir

// writeSeqMessageWithCommit lands content at inboxDir/<id.Filename()> via a
// tmp+link discipline and reports whether the canonical link exists.
func writeSeqMessageWithCommit(inboxDir string, id MsgID, content []byte) (alreadyLanded, committed bool, err error) {
	if err := ValidateAgentID(id.From); err != nil {
		return false, false, fmt.Errorf("from: %w", err)
	}
	if id.Seq == 0 {
		return false, false, fmt.Errorf("writeSeqMessage: seq must be >= 1")
	}
	if !dirExists(inboxDir) {
		return false, false, os.ErrNotExist
	}

	tempFile, err := os.CreateTemp(inboxDir, tempFilePrefix+"*")
	if err != nil {
		return false, false, err
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := writeAndSyncOpenFile(tempFile, content); err != nil {
		return false, false, err
	}

	finalPath := filepath.Join(inboxDir, id.Filename())
	if err := linkNoClobber(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			// This exact (to,from,seq) is still present in the inbox — safe no-op
			// success while unconsumed. Post-consume the path is free again, so a
			// resend re-lands; there is no receiver-side dedup backstop.
			return true, true, nil
		}
		return false, false, fmt.Errorf("link to %s: %w", finalPath, err)
	}
	if err := syncSeqInboxDir(inboxDir); err != nil {
		return false, true, err
	}
	return false, true, nil
}

func writeSeqMessage(inboxDir string, id MsgID, content []byte) (alreadyLanded bool, err error) {
	alreadyLanded, _, err = writeSeqMessageWithCommit(inboxDir, id, content)
	return alreadyLanded, err
}

// SendSeqMessage allocates the next durable seq for (from,to) and lands content
// in to's inbox under the canonical (to,from,seq) filename, returning the
// committed identity. It is the convenience composition of AllocateSeq +
// writeSeqMessage (the off-bus mirror of conformance SeqSender.Send); send/serve
// wiring is Gate 4/6.
//
// idempotencyKey and serveToken behave exactly as documented on AllocateSeq.
func SendSeqMessage(cfg *Config, from, to string, content []byte, idempotencyKey, serveToken string) (MsgID, error) {
	id, _, err := SendSeqMessageWithCommit(cfg, from, to, content, idempotencyKey, serveToken)
	if err != nil {
		return MsgID{}, err
	}
	return id, err
}

// SendSeqMessageWithCommit is SendSeqMessage with an explicit commit boundary.
// committed is true once the canonical inbox link exists, including when a
// later directory-sync barrier fails. Callers must not recommend resending a
// committed delivery.
//
// B3 (v2.5 plan): mint (AllocateSeq, under WithAgentLock(from)) and deliver
// (DeliverUnderRecipientLock, under WithAgentLock(to)) are two SEPARATE,
// SEQUENTIAL lock acquisitions — the first fully releases before the second
// begins. NO LOCK NESTING: self-send (from==to) would deadlock the
// non-reentrant flock instantly if these were ever held together; a
// dedicated sentinel test (TestSendSelfSendNoDeadlock) pins this.
func SendSeqMessageWithCommit(cfg *Config, from, to string, content []byte, idempotencyKey, serveToken string) (MsgID, bool, error) {
	seq, err := AllocateSeq(cfg, from, to, idempotencyKey, serveToken)
	if err != nil {
		return MsgID{}, false, err
	}
	id := MsgID{To: to, From: from, Seq: seq}
	committed, err := DeliverUnderRecipientLock(cfg, to, id, content, serveToken)
	return id, committed, err
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
// id/content into to's inbox. On any failure (ErrRecipientUnknown,
// ErrRecipientUnreadable, *ErrRecipientStale, ErrFenced) nothing is linked —
// the seq in id was already durably allocated by the caller's AllocateSeq and
// is consumed as a legal gap (O1 tolerates gaps; a resend needs a fresh
// preflight+mint).
//
// NO LOCK NESTING: id.From's OWN WithAgentLock(id.From) (taken by the
// caller's AllocateSeq) MUST have already released before this is called —
// never call this from inside that lock's closure. Self-send (id.From==to)
// would deadlock the non-reentrant flock instantly if the two were ever held
// together.
func DeliverUnderRecipientLock(cfg *Config, to string, id MsgID, content []byte, serveToken string) (committed bool, err error) {
	if err := ValidateAgentID(to); err != nil {
		return false, fmt.Errorf("to: %w", err)
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
		_, c, werr := writeSeqMessageWithCommit(cfg.AgentInboxDir(to), id, content)
		committed = c
		return werr
	})
	return committed, lockErr
}
