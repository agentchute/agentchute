package loop

import (
	"errors"
	"os"
	"testing"
)

// TestGuardLatchRoundTrip is the basic set/read/clear contract at the
// primitive level (v2.5 plan A7/C21).
func TestGuardLatchRoundTrip(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
		t.Fatalf("SetGuardLatch: %v", err)
	}
	latch, err := ReadGuardLatch(cfg, "bob")
	if err != nil {
		t.Fatalf("ReadGuardLatch: %v", err)
	}
	if latch.Session != "tok-1" || latch.V != 1 {
		t.Fatalf("latch = %+v, want session tok-1 v1", latch)
	}
	if err := ClearGuardLatch(cfg, "bob", "tok-1"); err != nil {
		t.Fatalf("ClearGuardLatch: %v", err)
	}
	if _, err := ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
		t.Fatalf("latch should be gone after clear; err=%v", err)
	}
}

// TestSetGuardLatchOverwritesCorruptFile is half of codex review PR #89
// finding #4: a hand-corrupted/truncated latch file is not a valid claim by
// any session, so SetGuardLatch must overwrite it rather than refuse (the
// other half — ClearGuardLatch fails open on the same corrupt file — is
// covered by internal/cli's TestTurnEndMalformedLatchDoesNotWedge, which
// exercises the full turn-end path).
func TestSetGuardLatchOverwritesCorruptFile(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := ensurePrivateDir(cfg.AgentStateDir("bob")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(cfg.GuardLatchPath("bob"), []byte("{not valid json")); err != nil {
		t.Fatal(err)
	}

	if err := SetGuardLatch(cfg, "bob", "tok-fresh"); err != nil {
		t.Fatalf("SetGuardLatch over a corrupt file must succeed, not refuse: %v", err)
	}
	latch, err := ReadGuardLatch(cfg, "bob")
	if err != nil {
		t.Fatalf("ReadGuardLatch after overwrite: %v", err)
	}
	if latch.Session != "tok-fresh" {
		t.Fatalf("latch.Session = %q, want tok-fresh", latch.Session)
	}
}

// TestClearGuardLatchFailsOpenOnCorruptFile is the other half of finding #4
// at the primitive level directly (see the comment above).
func TestClearGuardLatchFailsOpenOnCorruptFile(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := ensurePrivateDir(cfg.AgentStateDir("bob")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(cfg.GuardLatchPath("bob"), []byte("{not valid json")); err != nil {
		t.Fatal(err)
	}

	if err := ClearGuardLatch(cfg, "bob", "tok-1"); err != nil {
		t.Fatalf("ClearGuardLatch over a corrupt file must fail open (no-op success), not error: %v", err)
	}
}

func TestClearGuardLatchRequiresMatchingSession(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := SetGuardLatch(cfg, "bob", "tok-A"); err != nil {
		t.Fatal(err)
	}
	if err := ClearGuardLatch(cfg, "bob", "tok-B"); err != nil {
		t.Fatal(err)
	}
	latch, err := ReadGuardLatch(cfg, "bob")
	if err != nil {
		t.Fatalf("latch should survive a mismatched clear: %v", err)
	}
	if latch.Session != "tok-A" {
		t.Fatalf("latch.Session = %q, want tok-A", latch.Session)
	}
}

func TestReadGuardLatchAbsentSurfacesErrNotExist(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	_, err := ReadGuardLatch(cfg, "bob")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

// TestPeekGuardLatchDoesNotCreateStateDir is codex review PR #89 round 5:
// unlike ReadGuardLatch (which takes WithAgentLock and so creates
// state/<id>/ + its lock file as a side effect), PeekGuardLatch must never
// manufacture state — it exists specifically for read-only callers like
// doctor.
func TestPeekGuardLatchDoesNotCreateStateDir(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	stateDir := cfg.AgentStateDir("bob")
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: state dir already exists: %v", err)
	}
	if _, err := PeekGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("PeekGuardLatch created state dir: %v", err)
	}
}

// TestPeekGuardLatchMatchesReadGuardLatch confirms the lock-free peek path
// returns the same data as the lock-taking read for a real latch.
func TestPeekGuardLatchMatchesReadGuardLatch(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := SetGuardLatch(cfg, "bob", "tok-1"); err != nil {
		t.Fatal(err)
	}
	peeked, err := PeekGuardLatch(cfg, "bob")
	if err != nil {
		t.Fatalf("PeekGuardLatch: %v", err)
	}
	read, err := ReadGuardLatch(cfg, "bob")
	if err != nil {
		t.Fatalf("ReadGuardLatch: %v", err)
	}
	if peeked.Session != read.Session || !peeked.SetAt.Equal(read.SetAt) {
		t.Fatalf("peeked=%+v, read=%+v, want equal", peeked, read)
	}
}
