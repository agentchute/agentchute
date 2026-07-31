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

// TestGuardRecoveredMarkRoundTrip is the basic set/read contract for the
// mixed hook-trust recovery mark (mirrors TestGuardLatchRoundTrip).
func TestGuardRecoveredMarkRoundTrip(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := SetGuardRecoveredMark(cfg, "bob", "tok-1"); err != nil {
		t.Fatalf("SetGuardRecoveredMark: %v", err)
	}
	mark, err := ReadGuardRecoveredMark(cfg, "bob")
	if err != nil {
		t.Fatalf("ReadGuardRecoveredMark: %v", err)
	}
	if mark.Session != "tok-1" || mark.V != 1 {
		t.Fatalf("mark = %+v, want session tok-1 v1", mark)
	}
}

func TestReadGuardRecoveredMarkAbsentSurfacesErrNotExist(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	_, err := ReadGuardRecoveredMark(cfg, "bob")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

// TestSetGuardRecoveredMarkOverwritesCorruptFile mirrors
// TestSetGuardLatchOverwritesCorruptFile: a hand-corrupted mark file must
// never refuse a fresh set.
func TestSetGuardRecoveredMarkOverwritesCorruptFile(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := ensurePrivateDir(cfg.AgentStateDir("bob")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(cfg.GuardRecoveredMarkPath("bob"), []byte("{not valid json")); err != nil {
		t.Fatal(err)
	}
	if err := SetGuardRecoveredMark(cfg, "bob", "tok-fresh"); err != nil {
		t.Fatalf("SetGuardRecoveredMark over a corrupt file must succeed: %v", err)
	}
	mark, err := ReadGuardRecoveredMark(cfg, "bob")
	if err != nil {
		t.Fatalf("ReadGuardRecoveredMark after overwrite: %v", err)
	}
	if mark.Session != "tok-fresh" {
		t.Fatalf("mark.Session = %q, want tok-fresh", mark.Session)
	}
}

// TestGuardRecoveredMarkIdempotentForSameSession confirms re-setting the
// SAME session's mark does not error (a model running --recover twice in
// one session must not fail).
func TestGuardRecoveredMarkIdempotentForSameSession(t *testing.T) {
	cfg := newLeaseTestConfig(t)
	if err := SetGuardRecoveredMark(cfg, "bob", "tok-1"); err != nil {
		t.Fatal(err)
	}
	first, err := ReadGuardRecoveredMark(cfg, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetGuardRecoveredMark(cfg, "bob", "tok-1"); err != nil {
		t.Fatalf("re-setting the same session must succeed: %v", err)
	}
	second, err := ReadGuardRecoveredMark(cfg, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !first.SetAt.Equal(second.SetAt) {
		t.Errorf("SetAt changed on a same-session re-set: %v -> %v", first.SetAt, second.SetAt)
	}
}
