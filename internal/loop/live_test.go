package loop

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newLiveTestConfig(t *testing.T) *Config {
	t.Helper()
	root := t.TempDir()
	return &Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
}

func TestWriteLiveReadsAlive(t *testing.T) {
	cfg := newLiveTestConfig(t)
	if err := WriteLive(cfg, "alice", false); err != nil {
		t.Fatalf("WriteLive: %v", err)
	}
	if !IsLive(cfg, "alice", liveWindow, time.Now()) {
		t.Fatal("fresh .live must read alive")
	}
}

func TestForcedOldLiveReadsNotAlive(t *testing.T) {
	cfg := newLiveTestConfig(t)
	old := time.Now().Add(-time.Hour)
	if err := writeLiveAt(cfg, "alice", false, old); err != nil {
		t.Fatalf("writeLiveAt: %v", err)
	}
	if IsLive(cfg, "alice", liveWindow, time.Now()) {
		t.Fatal("forced-old .live must read not-alive")
	}
	// Same file is alive when evaluated relative to its own write time.
	if !IsLive(cfg, "alice", liveWindow, old.Add(time.Second)) {
		t.Fatal("within-window evaluation must read alive")
	}
}

func TestAbsentLiveReadsNotAliveNoError(t *testing.T) {
	cfg := newLiveTestConfig(t)
	// Never written: IsLive must be false, NOT an error/panic (R1 dead-mailbox).
	if IsLive(cfg, "never-ran", liveWindow, time.Now()) {
		t.Fatal("absent .live must read not-alive")
	}
	if _, err := ReadLive(cfg, "never-ran"); err == nil {
		t.Fatal("ReadLive on absent file should surface an error (IsLive folds it to not-alive)")
	}
}

func TestLiveBusyIsAdvisoryOnly(t *testing.T) {
	cfg := newLiveTestConfig(t)
	if err := WriteLive(cfg, "alice", true); err != nil {
		t.Fatal(err)
	}
	// busy=true must NOT affect aliveness (avoids the false-dead direction).
	if !IsLive(cfg, "alice", liveWindow, time.Now()) {
		t.Fatal("busy agent must still read alive")
	}
	live, err := ReadLive(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !live.Busy {
		t.Fatal("busy flag should round-trip true")
	}
}

func TestLiveRoundTripFields(t *testing.T) {
	cfg := newLiveTestConfig(t)
	if err := WriteLive(cfg, "alice", true); err != nil {
		t.Fatal(err)
	}
	live, err := ReadLive(cfg, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if live.ID != "alice" {
		t.Fatalf("ID = %q, want alice", live.ID)
	}
	if live.PID == 0 {
		t.Fatal("PID should be populated")
	}
	if live.LastSeen.IsZero() {
		t.Fatal("LastSeen should be populated")
	}
}

// TestLiveConcurrentWritesNeverTorn: many concurrent WriteLive calls (atomic
// tmp+rename) always leave a parseable, non-torn file.
func TestLiveConcurrentWritesNeverTorn(t *testing.T) {
	cfg := newLiveTestConfig(t)
	// Pre-create so the first reader doesn't race the dir creation.
	if err := WriteLive(cfg, "alice", false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = WriteLive(cfg, "alice", i%2 == 0)
		}(i)
	}
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if live, err := ReadLive(cfg, "alice"); err == nil {
				if live.ID != "alice" {
					t.Errorf("torn read: ID=%q", live.ID)
				}
			}
		}()
	}
	wg.Wait()
	// Final state is valid.
	if _, err := ReadLive(cfg, "alice"); err != nil {
		t.Fatalf("final ReadLive: %v", err)
	}
}

func TestIsLiveInvalidID(t *testing.T) {
	cfg := newLiveTestConfig(t)
	// An invalid id reads not-alive (ReadLive validation fails -> false).
	if IsLive(cfg, "BAD ID", liveWindow, time.Now()) {
		t.Fatal("invalid id must read not-alive")
	}
}
