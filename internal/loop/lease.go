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
// is Gate 6). The fence verifier IS already callable by AllocateSeq via a
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
// ReleaseLease, and AllocateSeq all surface it — a fenced holder must stop.
var ErrFenced = errors.New("agentchute: serve lease fenced (token mismatch)")

// ServeClaim is the on-disk lease at <loop>/state/<id>/serve.claim. Acquired via
// link-no-clobber; renewed/reclaimed via atomic rename.
type ServeClaim struct {
	ID         string    `json:"id"`
	Host       string    `json:"host"`
	PID        int       `json:"pid"`
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

// afterReclaimWriteHook, when non-nil, is invoked AFTER a stale-reclaim writes
// its claim, still INSIDE withAgentLock. Test-only: lets a test observe the
// mid-reclaim window — e.g. confirm a concurrent acquirer is BLOCKED on the lock
// while the reclaim holds it. nil in production.
var afterReclaimWriteHook func()

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
// provable across hosts). Reclaim runs under withAgentLock (the same lock
// RenewLease/ReleaseLease take): the read→staleness-decision→rename is one
// mutually-exclusive CAS, so two concurrent reclaimers cannot both win. A loser
// observes the now-fresh claim under the lock and fails closed (any residual
// cross-host / clock-skew double-reclaim window is contained by the fence on
// every subsequent write — protocol-v2 §7).
func AcquireServeLease(cfg *Config, id string) (*ServeLease, error) {
	if err := ValidateAgentID(id); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	token, err := mintServeToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	claim := &ServeClaim{
		ID:         id,
		Host:       host,
		PID:        os.Getpid(),
		ServeToken: token,
		StartedAt:  now,
		LastSeen:   now,
	}

	stateDir := cfg.AgentStateDir(id)
	if err := ensurePrivateDir(stateDir); err != nil {
		return nil, err
	}
	data, err := marshalClaim(claim)
	if err != nil {
		return nil, err
	}

	// Fresh-acquire FAST PATH (unlocked): link-no-clobber a unique temp inode
	// into place. link is atomic, so at most one concurrent fresh-acquirer wins
	// here; the rest get EEXIST and fall through to the locked reclaim CAS below.
	// The temp inode is kept (defer-removed) so the EEXIST/release-race branch can
	// re-link it under the lock without rebuilding it.
	tempFile, err := os.CreateTemp(stateDir, tempFilePrefix+"*")
	if err != nil {
		return nil, err
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := writeAndSyncOpenFile(tempFile, data); err != nil {
		return nil, err
	}
	path := claimPath(cfg, id)
	linkErr := linkNoClobber(tempPath, path)
	if linkErr == nil {
		return &ServeLease{cfg: cfg, ID: id, Token: token}, nil
	}
	if !errors.Is(linkErr, os.ErrExist) {
		return nil, fmt.Errorf("acquire serve lease %s: %w", path, linkErr)
	}

	// EEXIST: a claim already exists. Resolve fresh-vs-stale UNDER withAgentLock
	// (the same lock RenewLease/ReleaseLease take) so the read→staleness-decision
	// →write is one mutually-exclusive CAS. Two unlocked reclaimers could
	// otherwise both pass the staleness check and both rename — the prior read-
	// back was NOT authoritative. We RE-READ inside the lock because the claim may
	// have changed since the unlocked link attempt above (a holder may have
	// released, or a concurrent reclaimer/fresh-acquirer may have won it).
	//
	// withAgentLock is NON-reentrant (filelock_unix.go): nothing in this closure
	// may call a function that itself takes withAgentLock(cfg,id).
	var lease *ServeLease
	lockErr := withAgentLock(cfg, id, func() error {
		// The entry-time `now` is stale after blocking on the lock; re-sample.
		nowInLock := time.Now().UTC()
		existing, rerr := readClaim(path)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				// (a) The holder released between our EEXIST and acquiring the lock.
				// Try a fresh link of our temp claim. Do NOT rename: a rename would
				// clobber a concurrent FRESH-acquirer that may have linked first
				// (the fast path is unlocked, so it races us here).
				linkErr2 := linkNoClobber(tempPath, path)
				if linkErr2 == nil {
					lease = &ServeLease{cfg: cfg, ID: id, Token: token}
					return nil
				}
				if errors.Is(linkErr2, os.ErrExist) {
					return ErrLeaseHeld // a concurrent fresh-acquirer beat us.
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
		// LIMITATION (FIX 4 / §6b deferred to Gate 6): pidAlive is pid-ONLY
		// liveness and is vulnerable to OS PID REUSE — a recycled pid number can
		// read as "alive" and wrongly protect a dead lane (or, conversely, mask a
		// distinct live process). §6b's full "pid/socket proof" — corroborate the
		// pid with the process start-time or the runner socket — is deferred to
		// Gate 6, when serve owns the socket. No behavior change here.
		if existing.Host == host && pidAlive(existing.PID) {
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
// is exactly what AllocateSeq's in-lock fence re-check relies on to close its
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
