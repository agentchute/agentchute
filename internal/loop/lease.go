package loop

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lease.go — the serve lease + fencing token that makes "one live process owns
// an id at a time" an ENFORCED, FENCED invariant (protocol-v2 TEAM-DECISION
// §6b), not just a convention. The per-(sender,recipient) seq design is only
// sound if a single writer owns an id; the lease produces that guarantee.
//
// GATE 2: PURELY ADDITIVE. Not wired into launch/heartbeat/seq-write yet (that
// is Gate 6). The fence verifier IS already callable by MintSendStamp via a
// non-empty serveToken so the two halves can be tested together.
//
// THE FENCE (the load-bearing addition): a stale holder that resumes AFTER its
// lease was reclaimed fails the serve_token equality check on its next
// heartbeat AND its next seq write — so a zombie/paused process cannot create a
// dup-writer even though launch was guarded. Launch-time guarding alone does not
// close that hole.

// Lease sizing. Package vars (test-tunable like agentLockTimeout); production
// keeps lease-timeout >> heartbeat-interval + max-skew (protocol-v2 §7), e.g.
// 10s / 1s / 2s. Severe clock skew degrades to premature/delayed reclaim but
// the fence still prevents a dup-WRITE.
var (
	leaseTimeout      = 10 * time.Second
	heartbeatInterval = 1 * time.Second //nolint:unused // documents the sizing relation; serve uses it in Gate 6.
)

// MaxClaimBytes caps the serve.claim file size on read (defense against a
// hand-corrupted/runaway claim).
const MaxClaimBytes = 64 << 10

// ErrLeaseHeld is returned when AcquireServeLease fails closed: a FRESH valid
// claim owns the id, or a stale-reclaim lost the locked CAS to a concurrent
// reclaimer/fresh-acquirer. The caller (serve launch) must NOT start a second
// writer.
var ErrLeaseHeld = errors.New("agentchute: serve lease already held")

// ErrFenced is returned when a token check fails: the holder was reclaimed (or
// the claim is gone), so this process no longer owns the id. RenewLease,
// ReleaseLease, and MintSendStamp all surface it — a fenced holder must stop.
var ErrFenced = errors.New("agentchute: serve lease fenced (token mismatch)")

// ServeClaim is the on-disk lease at <loop>/state/<id>/serve.claim. Acquired via
// link-no-clobber; renewed/reclaimed via atomic rename.
type ServeClaim struct {
	ID         string    `json:"id"`
	Host       string    `json:"host"`
	PID        int       `json:"pid"`
	BootRef    string    `json:"boot_ref,omitempty"`
	ServeToken string    `json:"serve_token"` // the FENCE epoch (128-bit crypto/rand hex).
	StartedAt  time.Time `json:"started_at"`
	LastSeen   time.Time `json:"last_seen"`
}

// ServeLease is the handle returned by AcquireServeLease. It carries the fence
// (Token) every heartbeat and every seq write must verify.
type ServeLease struct {
	cfg   *Config
	ID    string
	Token string
}

// claimPath returns the serve.claim path for id. Owner-private (under
// state/<id>/), so cross-host acquire/verify assumes a same-uid pool on the
// shared mount (protocol-v2 §7 deployment constraint).
func claimPath(cfg *Config, id string) string {
	return filepath.Join(cfg.AgentStateDir(id), "serve.claim")
}

// mintServeToken returns a 128-bit crypto/rand hex epoch. Equality-checked, so
// collision-resistant uniqueness suffices — a resumed holder's old token never
// equals the live one. Package var so tests can force a deterministic token.
var mintServeToken = func() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// readBootRef is injectable so reclaim behavior is deterministic in tests.
// Production implementations are platform-specific and return "" on failure.
var readBootRef = platformBootRef

// afterReclaimWriteHook, when non-nil, is invoked AFTER a stale-reclaim writes
// its claim, still INSIDE withAgentLock. Test-only: lets a test observe the
// mid-reclaim window — e.g. confirm a concurrent acquirer is BLOCKED on the lock
// while the reclaim holds it. nil in production.
var afterReclaimWriteHook func()

// afterInvalidateSnapshotHook runs after InvalidateAllServeLeases snapshots a
// claim and before it takes that agent's lock. Test-only: lets a test replace
// the claim in the exact enumeration-to-lock window. nil in production.
var afterInvalidateSnapshotHook func(string)

func readClaim(path string) (*ServeClaim, error) {
	data, err := ReadFileLimit(path, MaxClaimBytes)
	if err != nil {
		return nil, err
	}
	var c ServeClaim
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse serve claim %s: %w", path, err)
	}
	if err := ValidateAgentID(c.ID); err != nil {
		return nil, fmt.Errorf("serve claim %s: invalid id: %w", path, err)
	}
	return &c, nil
}

// claimIsStale reports whether a claim is past the lease timeout relative to
// now. A future-dated last_seen (negative age, clock skew) reads as FRESH —
// failing closed is the safe direction.
func claimIsStale(c *ServeClaim, now time.Time) bool {
	age := now.UTC().Sub(c.LastSeen.UTC())
	return age >= leaseTimeout
}

func marshalClaim(c *ServeClaim) ([]byte, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal serve claim: %w", err)
	}
	return append(data, '\n'), nil
}

// AcquireServeLease claims id's serve lease, FAILING CLOSED on a fresh valid
// claim (protocol-v2 §6b acceptance 1). On a stale claim it reclaims only via
// the R1 liveness rule: same-host requires pid-proof failure (a frozen-but-alive
// process keeps its id); cross-host uses freshness/timeout only (pid is not
// provable across hosts).
//
// EVERYTHING — the fresh-acquire link attempt AND the stale-reclaim CAS —
// runs under ONE withAgentLock(id) critical section (the same lock
// RenewLease/ReleaseLease take, and the same lock any other caller, e.g. a
// destructive command, can use to get true mutual exclusion against a claim
// appearing) (review fix, plan A4 blocker: a previous unlocked fresh-acquire
// "fast path" let a brand-new claim appear inside a caller's own
// withAgentLock(id) critical section, since that caller's lock never
// contended with this one). link-no-clobber is already atomic on its own —
// the lock was never required for "exactly one fresh-acquirer wins", only for
// closing this external-exclusion gap — so unifying the two paths costs one
// extra (uncontended, cheap) lock acquisition on the common case and changes
// no observable behavior otherwise.
func AcquireServeLease(cfg *Config, id string) (*ServeLease, error) {
	if err := ValidateAgentID(id); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	token, err := mintServeToken()
	if err != nil {
		return nil, err
	}

	stateDir := cfg.AgentStateDir(id)
	if err := ensurePrivateDir(stateDir); err != nil {
		return nil, err
	}
	path := claimPath(cfg, id)
	var tempPath string
	defer func() {
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()

	// withAgentLock is NON-reentrant (filelock_unix.go): nothing in this closure
	// may call a function that itself takes withAgentLock(cfg,id).
	var lease *ServeLease
	lockErr := withAgentLock(cfg, id, func() error {
		// The boot reference is sampled inside the same critical section as the
		// stale-claim decision. A reboot boundary can therefore never be observed
		// on one side of the decision but recorded on the other.
		bootRef := readBootRef()
		now := time.Now().UTC()
		claim := &ServeClaim{
			ID:         id,
			Host:       host,
			PID:        os.Getpid(),
			BootRef:    bootRef,
			ServeToken: token,
			StartedAt:  now,
			LastSeen:   now,
		}
		data, err := marshalClaim(claim)
		if err != nil {
			return err
		}
		tempFile, err := os.CreateTemp(stateDir, tempFilePrefix+"*")
		if err != nil {
			return err
		}
		tempPath = tempFile.Name()
		if err := writeAndSyncOpenFile(tempFile, data); err != nil {
			return err
		}

		// Fresh-acquire attempt, now INSIDE the lock: link-no-clobber a unique
		// temp inode into place. No claim exists yet in the common case, so
		// this succeeds immediately.
		linkErr := linkNoClobber(tempPath, path)
		if linkErr == nil {
			lease = &ServeLease{cfg: cfg, ID: id, Token: token}
			return nil
		}
		if !errors.Is(linkErr, os.ErrExist) {
			return fmt.Errorf("acquire serve lease %s: %w", path, linkErr)
		}

		// EEXIST: a claim already exists. Resolve fresh-vs-stale — still under
		// the SAME lock acquisition as the attempt above, so the
		// read→staleness-decision→rename is one mutually-exclusive CAS. Two
		// concurrent reclaimers can no longer both pass the staleness check and
		// both rename.
		nowInLock := time.Now().UTC()
		existing, rerr := readClaim(path)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				// The holder released between our EEXIST and this re-read.
				// Try a fresh link of our temp claim.
				linkErr2 := linkNoClobber(tempPath, path)
				if linkErr2 == nil {
					lease = &ServeLease{cfg: cfg, ID: id, Token: token}
					return nil
				}
				if errors.Is(linkErr2, os.ErrExist) {
					return ErrLeaseHeld // impossible to reach concurrently now (same lock), kept for the released-then-relinked-by-us-only case; defensive.
				}
				return fmt.Errorf("acquire serve lease %s: %w", path, linkErr2)
			}
			// (b) Unreadable/corrupt: cannot prove stale — fail closed.
			return fmt.Errorf("acquire serve lease %s: unreadable existing claim: %w", path, rerr)
		}
		// (c) Not stale: a live serve or a fresh reclaimer owns id.
		if !claimIsStale(existing, nowInLock) {
			return ErrLeaseHeld
		}
		// (d) Stale + same-host + pid alive: a frozen-but-alive process keeps its
		// id (don't steal a live lane).
		//
		// A differing, non-empty per-boot reference proves the recorded process
		// belonged to a prior boot even when its pid has since been recycled. An
		// absent or matching reference preserves the pid-only fail-closed rule.
		if existing.Host == host && pidAlive(existing.PID) &&
			(existing.BootRef == "" || bootRef == "" || existing.BootRef == bootRef) {
			return ErrLeaseHeld
		}
		// (e) Stale + reclaimable: rename over the stale claim. Under the lock no
		// other reclaimer races us, and a fresh-acquirer cannot link because the
		// path still exists. This is the authoritative CAS (no read-back needed).
		if werr := atomicWriteFile(path, data); werr != nil {
			return fmt.Errorf("reclaim serve lease %s: %w", path, werr)
		}
		if afterReclaimWriteHook != nil {
			afterReclaimWriteHook()
		}
		lease = &ServeLease{cfg: cfg, ID: id, Token: token}
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return lease, nil
}

// VerifyFence is the lock-free, read-only token check called by EVERY heartbeat
// and EVERY seq write. It returns nil iff the live claim's serve_token equals
// token. An absent claim (released/never-acquired) or a mismatch returns
// ErrFenced — a holder that cannot prove ownership must stop. A corrupt claim
// returns a wrapped parse error (can't prove ownership; fail closed).
//
// LOCK-FREE: it takes NO lock (just readClaim of the claim file), so it is safe
// to call from INSIDE withAgentLock(id) without violating non-reentrancy — which
// is exactly what MintSendStamp's in-lock fence re-check relies on to close its
// reclaim TOCTOU.
func VerifyFence(cfg *Config, id, token string) error {
	if token == "" {
		return fmt.Errorf("VerifyFence: empty token")
	}
	c, err := readClaim(claimPath(cfg, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrFenced
		}
		return err
	}
	if c.ServeToken != token {
		return ErrFenced
	}
	return nil
}

// ReadServeClaim reads id's serve.claim (the serve-lease holder fact) without
// taking the lease or any lock. An absent claim surfaces os.ErrNotExist; a
// corrupt/invalid claim surfaces a parse error. Exported (read-only) for
// cross-pool liveness checks such as `setup --wipe-state`, which must refuse to
// wipe a bus that a FRESH claim on ANOTHER HOST still owns. The returned
// ServeClaim's Host/LastSeen are the load-bearing fields for that check; pair
// with ClaimIsStale to fold in the lease-timeout freshness rule.
func ReadServeClaim(cfg *Config, id string) (*ServeClaim, error) {
	if err := ValidateAgentID(id); err != nil {
		return nil, err
	}
	return readClaim(claimPath(cfg, id))
}

// ClaimIsStale reports whether c is past the serve-lease timeout relative to
// now (a future-dated last_seen reads as FRESH — failing closed is the safe
// direction). Exported alongside ReadServeClaim so external liveness checks
// apply the same freshness rule AcquireServeLease uses internally.
func ClaimIsStale(c *ServeClaim, now time.Time) bool {
	if c == nil {
		return true
	}
	return claimIsStale(c, now)
}

// RenewLease is the heartbeat: under withAgentLock(id) it verifies our token
// still owns the claim (ErrFenced if reclaimed) and bumps last_seen.
func RenewLease(l *ServeLease) error {
	if l == nil {
		return fmt.Errorf("RenewLease: nil lease")
	}
	return withAgentLock(l.cfg, l.ID, func() error {
		path := claimPath(l.cfg, l.ID)
		c, err := readClaim(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrFenced
			}
			return err
		}
		if c.ServeToken != l.Token {
			return ErrFenced
		}
		c.LastSeen = time.Now().UTC()
		data, err := marshalClaim(c)
		if err != nil {
			return err
		}
		return atomicWriteFile(path, data)
	})
}

// ReleaseLease removes the claim on clean shutdown, but ONLY if we still own it
// (VerifyFence). If we were already reclaimed it returns ErrFenced and does NOT
// delete the new owner's claim.
func ReleaseLease(l *ServeLease) error {
	if l == nil {
		return fmt.Errorf("ReleaseLease: nil lease")
	}
	return withAgentLock(l.cfg, l.ID, func() error {
		path := claimPath(l.cfg, l.ID)
		c, err := readClaim(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // already gone — nothing to release.
			}
			return err
		}
		if c.ServeToken != l.Token {
			return ErrFenced
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("release serve lease %s: %w", path, err)
		}
		return syncDir(filepath.Dir(path))
	})
}

// InvalidateAllServeLeases fences every currently-running supervisor in this
// pool by removing its serve.claim under that agent's lock. The next
// RenewLease or VerifyFence by an old holder returns ErrFenced. Registration
// rows are deliberately preserved; setup/update use lease invalidation, not
// row deletion, as the wire-break forcing function.
func InvalidateAllServeLeases(cfg *Config) (int, error) {
	if cfg == nil {
		return 0, fmt.Errorf("InvalidateAllServeLeases: nil config")
	}
	stateDir := filepath.Join(cfg.LoopDir, "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("list serve leases in %s: %w", stateDir, err)
	}

	type claimSnapshot struct {
		id         string
		path       string
		token      string
		tokenKnown bool
		info       os.FileInfo
	}

	var (
		snapshots []claimSnapshot
		failures  []error
	)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		path := claimPath(cfg, id)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			failures = append(failures, fmt.Errorf("%s: inspect serve claim: %w", id, err))
			continue
		}
		if err := ValidateAgentID(id); err != nil {
			failures = append(failures, fmt.Errorf("%q: invalid serve-lease directory: %w", id, err))
			continue
		}
		if !info.Mode().IsRegular() {
			failures = append(failures, fmt.Errorf("%s: serve claim is not a regular file", id))
			continue
		}

		snapshot := claimSnapshot{id: id, path: path, info: info}
		if claim, err := readClaim(path); err == nil {
			snapshot.token = claim.ServeToken
			snapshot.tokenKnown = true
		}
		snapshots = append(snapshots, snapshot)
		if afterInvalidateSnapshotHook != nil {
			afterInvalidateSnapshotHook(id)
		}
	}

	invalidated := 0
	for _, snapshot := range snapshots {
		removed := false
		err := withAgentLock(cfg, snapshot.id, func() error {
			currentInfo, err := os.Lstat(snapshot.path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("inspect current serve claim: %w", err)
			}
			if !currentInfo.Mode().IsRegular() {
				return fmt.Errorf("current serve claim is not a regular file")
			}

			if snapshot.tokenKnown {
				current, err := readClaim(snapshot.path)
				switch {
				case err == nil && current.ServeToken != snapshot.token:
					return nil
				case err != nil && !os.SameFile(snapshot.info, currentInfo):
					return fmt.Errorf("current serve claim changed and is unreadable: %w", err)
				}
			} else if !os.SameFile(snapshot.info, currentInfo) {
				return nil
			}

			if err := os.Remove(snapshot.path); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("remove serve claim: %w", err)
			}
			removed = true
			return syncDir(filepath.Dir(snapshot.path))
		})
		if removed {
			invalidated++
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: invalidate serve lease: %w", snapshot.id, err))
		}
	}
	return invalidated, errors.Join(failures...)
}
