package loop

import (
	"errors"
	"os"
	"testing"
	"time"
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

// mustWriteGuardLatchAt writes a latch directly with a specific SetAt,
// bypassing SetGuardLatch (which always stamps time.Now()) — a test fixture
// for exercising ClearStaleGuardLatch's age gate.
func mustWriteGuardLatchAt(t *testing.T, cfg *Config, id, session string, setAt time.Time) {
	t.Helper()
	if err := ensurePrivateDir(cfg.AgentStateDir(id)); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardLatch(cfg, id, &GuardLatch{V: 1, Session: session, SetAt: setAt}); err != nil {
		t.Fatal(err)
	}
}

func TestClearStaleGuardLatchNoLatch(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	cleared, found, _, err := ClearStaleGuardLatch(cfg, "bob", 30*time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("ClearStaleGuardLatch: %v", err)
	}
	if found || cleared {
		t.Fatalf("found=%v cleared=%v, want both false for no latch", found, cleared)
	}
}

func TestClearStaleGuardLatchRefusesYoungLatch(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	mustWriteGuardLatchAt(t, cfg, "bob", "tok-1", now.Add(-5*time.Minute))

	cleared, found, age, err := ClearStaleGuardLatch(cfg, "bob", 30*time.Minute, now)
	if err != nil {
		t.Fatalf("ClearStaleGuardLatch: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if cleared {
		t.Fatal("cleared = true, want false (latch is only 5m old, under the 30m threshold)")
	}
	if age < 4*time.Minute || age > 6*time.Minute {
		t.Errorf("age = %v, want ~5m", age)
	}
	// The latch must survive untouched.
	if _, err := ReadGuardLatch(cfg, "bob"); err != nil {
		t.Fatalf("latch should still be present after a refused clear: %v", err)
	}
}

// TestClearStaleGuardLatchClearsOldLatchRegardlessOfSession is the core
// property: age, not session identity, authorizes the clear. A latch set by
// a completely different (or nonexistent) session is cleared once old
// enough — this is what makes it usable as a recovery path when we cannot
// prove which session (if any) is still alive.
func TestClearStaleGuardLatchClearsOldLatchRegardlessOfSession(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	now := time.Now().UTC()
	mustWriteGuardLatchAt(t, cfg, "bob", "tok-someone-else", now.Add(-45*time.Minute))

	cleared, found, age, err := ClearStaleGuardLatch(cfg, "bob", 30*time.Minute, now)
	if err != nil {
		t.Fatalf("ClearStaleGuardLatch: %v", err)
	}
	if !found || !cleared {
		t.Fatalf("found=%v cleared=%v, want both true for a 45m-old latch with a 30m threshold", found, cleared)
	}
	if age < 44*time.Minute || age > 46*time.Minute {
		t.Errorf("age = %v, want ~45m", age)
	}
	if _, err := ReadGuardLatch(cfg, "bob"); !os.IsNotExist(err) {
		t.Fatalf("latch should be gone; err=%v", err)
	}
}

func TestClearStaleGuardLatchTreatsCorruptFileAsNotFound(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := ensurePrivateDir(cfg.AgentStateDir("bob")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(cfg.GuardLatchPath("bob"), []byte("{not valid json")); err != nil {
		t.Fatal(err)
	}
	cleared, found, _, err := ClearStaleGuardLatch(cfg, "bob", 30*time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatalf("ClearStaleGuardLatch must fail open on a corrupt file, not error: %v", err)
	}
	if found || cleared {
		t.Fatalf("found=%v cleared=%v, want both false for a corrupt file", found, cleared)
	}
}
