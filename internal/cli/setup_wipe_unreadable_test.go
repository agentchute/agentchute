package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// grok's #167 sweep, category 1: an absent or unreadable thing reported as a
// satisfied condition. Both rows here are the same shape — a directory the
// scanner could not read contributed nothing, and contributing nothing is how
// this code spells "all clear".
//
// The wipe is the worst place for that shape. Its whole job is to refuse when it
// cannot prove the pool is idle, and it advertises exactly that discipline in
// its own comments for unreadable CLAIMS. The directory holding them was the one
// case that failed open.

func TestWipeRefusesWhenTheServeClaimDirectoryIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory regardless of mode")
	}
	_, cfg := newWipeTestRepo(t)
	stateDir := filepath.Join(cfg.LoopDir, "state")
	// A live serve's claim is in there. The scanner is about to be unable to see
	// it — which must produce a refusal, not silence.
	mustWrite(t, filepath.Join(stateDir, "codex", "serve.claim"), []byte("{}"))
	if err := os.Chmod(stateDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })

	reasons := scanWipeServeClaims(cfg, "localhost", time.Now().UTC())
	if len(reasons) == 0 {
		t.Fatal("an unreadable serve-claim directory produced no refusal; the wipe would have proceeded with a live claim it could not see")
	}
	joined := strings.Join(reasons, "; ")
	if !strings.Contains(joined, "unreadable") {
		t.Fatalf("the refusal does not say what went wrong: %s", joined)
	}
	// It must name the directory, or an operator cannot act on it.
	if !strings.Contains(joined, "state") {
		t.Fatalf("the refusal does not name the directory: %s", joined)
	}
}

// The control: a pool with no state directory at all has genuinely no claims,
// and must NOT be refused. Without this the row above is satisfied by a scanner
// that refuses everything, which would block every first-time wipe.
func TestWipeDoesNotRefuseWhenThereIsSimplyNoStateDirectory(t *testing.T) {
	_, cfg := newWipeTestRepo(t)
	if err := os.RemoveAll(filepath.Join(cfg.LoopDir, "state")); err != nil {
		t.Fatal(err)
	}
	if reasons := scanWipeServeClaims(cfg, "localhost", time.Now().UTC()); len(reasons) != 0 {
		t.Fatalf("a pool with no state directory was refused: %v", reasons)
	}
}

// The post-wipe rescan asks "did anything reappear?". "I looked and found
// nothing" and "I could not look" are opposite answers, and the caller was being
// handed the reassuring one whenever a directory could not be read.
func TestPostWipeRescanReportsWhatItCouldNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory regardless of mode")
	}
	_, cfg := newWipeTestRepo(t)
	inbox := filepath.Join(cfg.LoopDir, "inbox")
	if err := os.Chmod(inbox, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inbox, 0o700) })

	leftovers, unverifiable := rescanWipeLeftovers(cfg.LoopDir)
	if len(unverifiable) == 0 {
		t.Fatal("an unreadable runtime directory was reported as clean")
	}
	if !strings.Contains(strings.Join(unverifiable, "; "), "inbox") {
		t.Fatalf("the unverifiable list does not name the directory: %v", unverifiable)
	}
	// And it must not be quietly folded in with real leftovers: they mean
	// different things and lead to different actions.
	for _, l := range leftovers {
		if strings.HasPrefix(l, "inbox") {
			t.Fatalf("an unreadable directory was reported as a leftover: %v", leftovers)
		}
	}
}

// A directory that is simply GONE after the wipe is also unverifiable, not
// clean. The wipe recreates these immediately before the rescan, so one missing
// here means something removed it mid-wipe — the exact condition the rescan
// exists to catch, and the one an excused not-exist would hide.
func TestPostWipeRescanTreatsAMissingDirectoryAsUnverifiable(t *testing.T) {
	_, cfg := newWipeTestRepo(t)
	if err := os.RemoveAll(filepath.Join(cfg.LoopDir, "live")); err != nil {
		t.Fatal(err)
	}
	_, unverifiable := rescanWipeLeftovers(cfg.LoopDir)
	if !strings.Contains(strings.Join(unverifiable, "; "), "live") {
		t.Fatalf("a directory removed mid-wipe was not reported: %v", unverifiable)
	}
}
