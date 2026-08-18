package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// The real writer, reconstructed: a lane holding an O_APPEND descriptor into the
// tree a migration wants to move.
//
// This is codex's open-fd finding. runner.log lives INSIDE the migrated tree
// (ShadowLoopDir is hubDir/.agentchute/loop, AgentStateDir hangs off it), and a
// descriptor follows the INODE, not the name — so the freeze does not detach it.
// An append after the migration verifies lands in the frozen tree and is deleted
// with it. Freezing narrows the window; only excluding the PROCESS closes it.
//
// The lane is modelled the way serve actually behaves: it takes the SHARED hub
// lock on its own hub id BEFORE opening the descriptor. The guarantee under test
// is therefore the whole mechanism — lock, then fd — not the fd alone, because
// an fd alone is exactly what cannot be protected.
func TestMigrationRefusesWhileALaneHoldsAnOpenDescriptor(t *testing.T) {
	shortenHubLockTimeout(t)
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}

	// The lane: shared lock first, exactly as runWrapper does for a remote config.
	release, err := acquireHubLocks([]string{oldRemote.HubID}, hubLockShared)
	if err != nil {
		t.Fatalf("a lane could not take the shared lock: %v", err)
	}
	defer release()

	stateDir := filepath.Join(oldRemote.ShadowLoopDir, "state", "codex-tiny")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(stateDir, "runner.log")
	fd, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if _, err := fd.WriteString("before the migration\n"); err != nil {
		t.Fatal(err)
	}

	var joinErr error
	withCwd(t, root, func() { joinErr = cmdHubJoin([]string{newRemote.URL, "--name", "codex"}) })
	if joinErr == nil {
		t.Fatal("the migration ran while a lane held the hub — anything that lane appends after the verify is deleted with the frozen tree")
	}

	// The append AFTER the refusal must still land somewhere real, which is the
	// property codex's finding is about.
	if _, err := fd.WriteString("after the refusal\n"); err != nil {
		t.Fatalf("the lane's descriptor stopped working: %v", err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the lane's log was deleted out from under it: %v", err)
	}
	if !strings.Contains(string(got), "before the migration") || !strings.Contains(string(got), "after the refusal") {
		t.Fatalf("runner.log = %q, want both writes", got)
	}
	assertOldHubKeyIntact(t, oldRemote, oldPub, oldTarget)
}

// The exclusion has to work in BOTH directions, and each direction fails for a
// different reason if only one is tested.
//
// Direction 1 pins that the migration takes the OLD hub id exclusively. That is
// the specific thing nothing else catches: every at-risk writer is on the OLD id,
// and a version that locks only the new one passes every other row here.
//
// Direction 2 pins that a lane cannot START underneath a running migration. It is
// what a future refactor would break by taking the lock after opening the log,
// or by not taking it at all for a remote config.
func TestHubLockExcludesInBothDirections(t *testing.T) {
	shortenHubLockTimeout(t)
	_, remote := setupHubJoinTest(t)

	t.Run("a lane's shared lock blocks the migration's exclusive one", func(t *testing.T) {
		release, err := acquireHubLocks([]string{remote.HubID}, hubLockShared)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := acquireHubLocks([]string{remote.HubID}, hubLockExclusive); err == nil {
			t.Fatal("the migration took an exclusive lock while a lane held it shared")
		} else if !strings.Contains(err.Error(), "hub join: this hub is busy") {
			t.Fatalf("refusal does not explain the live lane: %v", err)
		}
	})

	t.Run("a migration's exclusive lock blocks a lane starting", func(t *testing.T) {
		release, err := acquireHubLocks([]string{remote.HubID}, hubLockExclusive)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := acquireHubLocks([]string{remote.HubID}, hubLockShared); err == nil {
			t.Fatal("a lane started while a migration held the hub exclusively")
		} else if !strings.Contains(err.Error(), "being migrated right now") {
			t.Fatalf("refusal does not explain the migration: %v", err)
		}
	})

	t.Run("two lanes share", func(t *testing.T) {
		first, err := acquireHubLocks([]string{remote.HubID}, hubLockShared)
		if err != nil {
			t.Fatal(err)
		}
		defer first()
		second, err := acquireHubLocks([]string{remote.HubID}, hubLockShared)
		if err != nil {
			t.Fatalf("two lanes could not hold the same hub shared, so the mode is exclusive in disguise: %v", err)
		}
		second()
	})
}

// shortenHubLockTimeout keeps the refusal path fast. These rows deliberately
// provoke a contended acquisition, and the production 5s retry would spend it
// waiting for something that is held for the whole test. The package var is
// restored; this file's rows are serial like the rest of the package's seam
// users.
func shortenHubLockTimeout(t *testing.T) {
	t.Helper()
	original := hubLockTimeout
	hubLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hubLockTimeout = original })
}

// runWrapper must take the shared lock ITSELF, for a remote config.
//
// Without this row nothing pins the fix at all: the exclusion rows above
// construct the lane by calling acquireHubLocks directly, so they never reach
// runWrapper's remote branch. Deleting the lock from serve left every one of them
// green — checked, not assumed.
//
// The discriminator is which error comes back. Holding the hub EXCLUSIVELY (as a
// migration does) must make runWrapper refuse with the migration message before
// it touches anything; without the lock it proceeds into the wrapper launch and
// fails, or does not fail, for some entirely different reason.
func TestServeTakesTheSharedHubLockBeforeItRuns(t *testing.T) {
	shortenHubLockTimeout(t)
	_, remote := setupHubJoinTest(t)

	release, err := acquireHubLocks([]string{remote.HubID}, hubLockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	cfg := &loop.Config{ControlRepo: t.TempDir(), LoopDir: remote.ShadowLoopDir, Remote: remote}
	opts := runnerOptions{
		AgentID:     "codex-tiny",
		Vendor:      "anthropic",
		WrapperArgs: []string{filepath.Join(t.TempDir(), "no-such-wrapper")},
	}

	// Bounded: a version that does not take the lock may block rather than
	// return, and a hung suite is a worse failure than a red one.
	done := make(chan error, 1)
	go func() { done <- runWrapper(cfg, opts, t.TempDir()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve started while a migration held the hub exclusively")
		}
		if !strings.Contains(err.Error(), "being migrated right now") {
			t.Fatalf("serve did not refuse on the hub lock; it got as far as %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runWrapper neither took the lock nor returned; it proceeded past the point the lock protects")
	}
}

// A LOCAL config must take no hub lock at all.
//
// Observed as "no lock FILE appears", not as "the lane did not block". Blocking
// only happens when the lock is contended, so a row that holds one specific id
// and waits to be blocked passes against a lane that locks any OTHER id —
// checked, and my first version of this row did exactly that. Every acquisition
// creates its lock file under <hub>/.locks, so counting them catches all of them.
func TestLocalServeTakesNoHubLock(t *testing.T) {
	shortenHubLockTimeout(t)
	_, remote := setupHubJoinTest(t)
	locksDir := filepath.Join(filepath.Dir(remote.HubDir), ".locks")

	before := lockFileNames(t, locksDir)

	root := t.TempDir()
	cfg := &loop.Config{ControlRepo: root, LoopDir: filepath.Join(root, ".agentchute", "loop")}
	if cfg.Remote != nil {
		t.Fatal("fixture is not local")
	}
	opts := runnerOptions{
		AgentID:     "codex",
		Vendor:      "anthropic",
		WrapperArgs: []string{filepath.Join(t.TempDir(), "no-such-wrapper")},
	}

	done := make(chan error, 1)
	go func() { done <- runWrapper(cfg, opts, root) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a local lane hung")
	}

	after := lockFileNames(t, locksDir)
	if len(after) != len(before) {
		t.Fatalf("a LOCAL lane created hub lock files %v (before: %v); a local pool must carry no hub dependency at all", after, before)
	}
}

func lockFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
