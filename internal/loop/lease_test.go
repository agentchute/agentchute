package loop

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newLeaseTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
}

// writeClaim writes a ServeClaim directly to id's claim path (test fixture for
// pre-existing fresh/stale claims). Bypasses AcquireServeLease on purpose.
func writeClaim(t *testing.T, cfg *Config, c *ServeClaim) {
	t.Helper()
	if err := ensurePrivateDir(cfg.AgentStateDir(c.ID)); err != nil {
		t.Fatalf("ensure state dir: %v", err)
	}
	data, err := marshalClaim(c)
	if err != nil {
		t.Fatalf("marshalClaim: %v", err)
	}
	if err := atomicWriteFile(claimPath(cfg, c.ID), data); err != nil {
		t.Fatalf("write claim: %v", err)
	}
}

// overwriteClaimToken rewrites id's existing claim with a new serve_token,
// keeping it FRESH (last_seen=now). Simulates a concurrent reclaimer winning the
// id, so the prior holder is fenced. Shared with seq_test.go.
func overwriteClaimToken(t *testing.T, cfg *Config, id, token string) {
	t.Helper()
	host, _ := os.Hostname()
	writeClaim(t, cfg, &ServeClaim{
		ID:         id,
		Host:       host,
		PID:        os.Getpid(),
		ServeToken: token,
		StartedAt:  time.Now().UTC(),
		LastSeen:   time.Now().UTC(),
	})
}

// withPidAlive temporarily overrides the pidAlive probe and restores it.
func withPidAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	prev := pidAlive
	pidAlive = fn
	t.Cleanup(func() { pidAlive = prev })
}

func TestAcquireServeLeaseFreshClaimFailsClosed(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if lease.Token == "" {
		t.Fatal("lease token empty")
	}
	// A second acquire against a FRESH valid claim must fail closed (§6b acc 1).
	if _, err := AcquireServeLease(cfg, "alice"); err != ErrLeaseHeld {
		t.Fatalf("second acquire err = %v, want ErrLeaseHeld", err)
	}
}

func TestAcquireServeLeaseStaleSameHostPidAliveFailsClosed(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	host, _ := os.Hostname()
	// Stale claim (last_seen well past leaseTimeout), same host.
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       host,
		PID:        4242,
		ServeToken: "old",
		StartedAt:  time.Now().Add(-time.Hour).UTC(),
		LastSeen:   time.Now().Add(-time.Hour).UTC(),
	})
	// pid still alive => frozen-but-alive => DON'T steal the lane.
	withPidAlive(t, func(int) bool { return true })
	if _, err := AcquireServeLease(cfg, "alice"); err != ErrLeaseHeld {
		t.Fatalf("stale+same-host+pid-alive err = %v, want ErrLeaseHeld", err)
	}
}

func TestAcquireServeLeaseStaleSameHostPidDeadReclaims(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	host, _ := os.Hostname()
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       host,
		PID:        4242,
		ServeToken: "old",
		StartedAt:  time.Now().Add(-time.Hour).UTC(),
		LastSeen:   time.Now().Add(-time.Hour).UTC(),
	})
	// pid dead => same-host liveness FAILS => reclaim succeeds.
	withPidAlive(t, func(int) bool { return false })
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("reclaim should succeed: %v", err)
	}
	if lease.Token == "old" {
		t.Fatal("reclaim must mint a fresh token")
	}
}

func TestAcquireServeLeaseStaleCrossHostReclaims(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	// Cross-host stale claim: pid is unprovable, so freshness/timeout alone
	// governs => reclaim. pidAlive=true must NOT block a cross-host reclaim.
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       "some-other-host",
		PID:        4242,
		ServeToken: "old",
		StartedAt:  time.Now().Add(-time.Hour).UTC(),
		LastSeen:   time.Now().Add(-time.Hour).UTC(),
	})
	withPidAlive(t, func(int) bool { return true })
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("cross-host stale reclaim should succeed: %v", err)
	}
	if lease.Token == "old" {
		t.Fatal("reclaim must mint a fresh token")
	}
}

func TestAcquireServeLeaseFreshCrossHostFailsClosed(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	// A FRESH cross-host claim still owns the id (freshness => fail closed).
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       "some-other-host",
		PID:        4242,
		ServeToken: "live",
		StartedAt:  time.Now().UTC(),
		LastSeen:   time.Now().UTC(),
	})
	if _, err := AcquireServeLease(cfg, "alice"); err != ErrLeaseHeld {
		t.Fatalf("fresh cross-host err = %v, want ErrLeaseHeld", err)
	}
}

func TestRenewLeaseHeartbeatAndFence(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// Heartbeat with our token bumps last_seen.
	before, err := readClaim(claimPath(cfg, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := RenewLease(lease); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	after, err := readClaim(claimPath(cfg, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeen.After(before.LastSeen) {
		t.Fatalf("heartbeat did not advance last_seen (%v -> %v)", before.LastSeen, after.LastSeen)
	}

	// Reclaimed: a heartbeat with the STALE token is fenced (§6b acc 3).
	overwriteClaimToken(t, cfg, "alice", "NEW-OWNER-TOKEN")
	if err := RenewLease(lease); err != ErrFenced {
		t.Fatalf("RenewLease after reclaim err = %v, want ErrFenced", err)
	}
}

func TestVerifyFence(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFence(cfg, "alice", lease.Token); err != nil {
		t.Fatalf("VerifyFence with live token: %v", err)
	}
	if err := VerifyFence(cfg, "alice", "wrong"); err != ErrFenced {
		t.Fatalf("VerifyFence mismatch err = %v, want ErrFenced", err)
	}
	// Absent claim => can't prove ownership => fenced.
	if err := VerifyFence(cfg, "nobody", "any"); err != ErrFenced {
		t.Fatalf("VerifyFence absent claim err = %v, want ErrFenced", err)
	}
	// Empty token is rejected (callers must pass a real fence).
	if err := VerifyFence(cfg, "alice", ""); err == nil {
		t.Fatal("VerifyFence with empty token must error")
	}
}

func TestDistinctIDsNeverCollide(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	a, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, err := AcquireServeLease(cfg, "bob")
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if a.Token == b.Token {
		t.Fatal("distinct ids must mint distinct tokens")
	}
	// Both claims exist independently and verify.
	if err := VerifyFence(cfg, "alice", a.Token); err != nil {
		t.Fatalf("alice fence: %v", err)
	}
	if err := VerifyFence(cfg, "bob", b.Token); err != nil {
		t.Fatalf("bob fence: %v", err)
	}
}

// TestReclaimUnderLockSerializesConcurrentAcquire: the stale-reclaim runs under
// withAgentLock, so a concurrent acquirer arriving mid-reclaim BLOCKS on the
// lock and, once it is released, observes the now-FRESH reclaimed claim and
// fails closed (ErrLeaseHeld). This replaces the old read-back-CAS loser test:
// the read-back CAS is gone; mutual exclusion via the lock is the guarantee.
//
// afterReclaimWriteHook fires while we still hold the lock; it launches a real
// concurrent AcquireServeLease and asserts it cannot complete until we release.
func TestReclaimUnderLockSerializesConcurrentAcquire(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	host, _ := os.Hostname()
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       host,
		PID:        4242,
		ServeToken: "old",
		StartedAt:  time.Now().Add(-time.Hour).UTC(),
		LastSeen:   time.Now().Add(-time.Hour).UTC(),
	})
	withPidAlive(t, func(int) bool { return false })

	var (
		concurrentErr  error
		concurrentDone = make(chan struct{})
	)
	prev := afterReclaimWriteHook
	afterReclaimWriteHook = func() {
		// We hold the agent lock here. A concurrent acquirer MUST block on it.
		go func() {
			_, concurrentErr = AcquireServeLease(cfg, "alice")
			close(concurrentDone)
		}()
		// Give the goroutine time to reach the lock wait; it must NOT complete
		// while the reclaim holds the lock.
		time.Sleep(50 * time.Millisecond)
		select {
		case <-concurrentDone:
			t.Error("concurrent acquire completed while reclaim held the lock")
		default:
		}
	}
	t.Cleanup(func() { afterReclaimWriteHook = prev })

	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatalf("reclaim under lock should succeed: %v", err)
	}
	if lease.Token == "old" {
		t.Fatal("reclaim must mint a fresh token")
	}
	<-concurrentDone // happens-before: safe to read concurrentErr after this.
	if concurrentErr != ErrLeaseHeld {
		t.Fatalf("blocked concurrent acquire err = %v, want ErrLeaseHeld", concurrentErr)
	}
	// The reclaimer's token owns the claim on disk.
	if err := VerifyFence(cfg, "alice", lease.Token); err != nil {
		t.Fatalf("winner fence: %v", err)
	}
}

// TestAcquireServeLeaseConcurrentStaleReclaimSingleWinner: seed a STALE claim
// (pid dead so the same-host arm permits reclaim), launch N concurrent
// acquirers. Under the locked reclaim CAS EXACTLY ONE wins; its token equals the
// on-disk serve_token; every other acquirer fails closed with ErrLeaseHeld
// (never a dup-writer, never a non-fence error). Run with -race.
func TestAcquireServeLeaseConcurrentStaleReclaimSingleWinner(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	host, _ := os.Hostname()
	writeClaim(t, cfg, &ServeClaim{
		ID:         "alice",
		Host:       host,
		PID:        4242,
		ServeToken: "old",
		StartedAt:  time.Now().Add(-time.Hour).UTC(),
		LastSeen:   time.Now().Add(-time.Hour).UTC(),
	})
	withPidAlive(t, func(int) bool { return false })

	const n = 20
	var (
		mu      sync.Mutex
		winners []*ServeLease
		held    int
		othErrs []error
		wg      sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := AcquireServeLease(cfg, "alice")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, lease)
			case err == ErrLeaseHeld:
				held++
			default:
				othErrs = append(othErrs, err)
			}
		}()
	}
	wg.Wait()
	if len(othErrs) > 0 {
		t.Fatalf("unexpected non-fence errors: %v", othErrs)
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d, want exactly 1 (held=%d)", len(winners), held)
	}
	if held != n-1 {
		t.Fatalf("fail-closed losers = %d, want %d", held, n-1)
	}
	// The single winner's token must equal the on-disk serve_token (authoritative
	// CAS: the winner's claim is the one that landed).
	onDisk, err := readClaim(claimPath(cfg, "alice"))
	if err != nil {
		t.Fatalf("read-back on-disk claim: %v", err)
	}
	if winners[0].Token != onDisk.ServeToken {
		t.Fatalf("winner token %q != on-disk serve_token %q", winners[0].Token, onDisk.ServeToken)
	}
}

// TestAcquireServeLeaseConcurrentSingleWinner: many simultaneous acquirers on a
// clean id => exactly one wins, the rest fail closed with ErrLeaseHeld (never a
// dup-writer, never a non-fence error).
func TestAcquireServeLeaseConcurrentSingleWinner(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	const n = 20
	var (
		mu      sync.Mutex
		wins    int
		held    int
		wg      sync.WaitGroup
		othErrs []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := AcquireServeLease(cfg, "alice")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case err == ErrLeaseHeld:
				held++
			default:
				othErrs = append(othErrs, err)
			}
		}()
	}
	wg.Wait()
	if len(othErrs) > 0 {
		t.Fatalf("unexpected non-fence errors: %v", othErrs)
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1 (held=%d)", wins, held)
	}
	if held != n-1 {
		t.Fatalf("fail-closed losers = %d, want %d", held, n-1)
	}
}

func TestReleaseLease(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseLease(lease); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if _, err := os.Stat(claimPath(cfg, "alice")); !os.IsNotExist(err) {
		t.Fatalf("claim should be removed after release; stat err = %v", err)
	}
	// After release the id is free to acquire again.
	if _, err := AcquireServeLease(cfg, "alice"); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
}

func TestReleaseLeaseFencedDoesNotDeleteNewOwner(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	lease, err := AcquireServeLease(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	// We were reclaimed by a new owner.
	overwriteClaimToken(t, cfg, "alice", "NEW-OWNER")
	if err := ReleaseLease(lease); err != ErrFenced {
		t.Fatalf("ReleaseLease after reclaim err = %v, want ErrFenced", err)
	}
	// The new owner's claim must still be present.
	c, err := readClaim(claimPath(cfg, "alice"))
	if err != nil {
		t.Fatalf("new owner claim missing: %v", err)
	}
	if c.ServeToken != "NEW-OWNER" {
		t.Fatalf("claim token = %q, want NEW-OWNER", c.ServeToken)
	}
}
