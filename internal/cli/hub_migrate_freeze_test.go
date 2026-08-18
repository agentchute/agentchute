package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

// ROW A — the invariant: what gets DELETED is what got VERIFIED.
//
// codex reproduced the old defect through the mux-reap seam. That seam is no
// longer in the window — the reap now runs first, by necessity — so re-running
// its reproduction would go green for the wrong reason. After the reorder the
// window is verify -> RemoveAll and nothing else, so verification IS the seam,
// and that is where this row writes.
//
// It writes to the PRE-RENAME path, which is what a real lane resolves, not the
// frozen path a test could compute. The assertion is that the bytes SURVIVE, not
// that the write fails: ensurePrivateDir is os.MkdirAll, so a racing write
// recreates the old path happily. Asserting a refusal would pin behaviour this
// code does not have.
//
// This does NOT make a concurrent lane safe — the recreated tree is ORPHANED, not
// deleted, and the real repair is the writer barrier in issue #179. It makes the
// stated invariant true and downgrades the worst case from gone to recoverable.
func TestHubMigrationDoesNotDeleteWhatItDidNotVerify(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	_, _ = seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}

	late := filepath.Join(oldRemote.HubDir, "late-lane-write")
	original := hubMigrationVerify
	t.Cleanup(func() { hubMigrationVerify = original })
	hubMigrationVerify = func(frozen, newDir string) error {
		if err := original(frozen, newDir); err != nil {
			return err
		}
		// A lane that resolved the OLD pointer writes here, in the only window
		// that is left.
		if err := os.MkdirAll(filepath.Dir(late), 0o700); err != nil {
			return err
		}
		return os.WriteFile(late, []byte("state a lane wrote mid-migration"), 0o600)
	}

	withCwd(t, root, func() {
		if err := cmdHubJoin([]string{newRemote.URL, "--name", "codex"}); err != nil {
			t.Fatalf("migration failed: %v", err)
		}
	})

	got, err := os.ReadFile(late)
	if err != nil {
		t.Fatalf("state written in the verify->delete window was DELETED having never been verified: %v", err)
	}
	if string(got) != "state a lane wrote mid-migration" {
		t.Fatalf("late write = %q", got)
	}
}

// ROW B — the reap must stay ahead of the verify.
//
// Row A cannot catch a refactor that slides the reap back into the window,
// because Row A's own seam IS the window. This one records the order directly.
//
// It is a correctness constraint, not a preference: muxIsolationKey resolves the
// key path with EvalSymlinks UNDER the old directory and falls back to
// filepath.Clean silently, so a reap after the rename computes a different
// isolation key and reaps the wrong socket without saying anything.
func TestHubMigrationReapsBeforeItFreezes(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	_, _ = seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}

	var order []string
	originalReap := hubMigrationReapMux
	originalVerify := hubMigrationVerify
	t.Cleanup(func() {
		hubMigrationReapMux = originalReap
		hubMigrationVerify = originalVerify
	})
	hubMigrationReapMux = func(cfg *hubclient.HubConfig, oldDir string) {
		// Record whether the key path is still RESOLVABLE, not merely the order.
		// Asserting "reap before verify" is not the constraint and does not
		// exclude the defect: sliding the reap to just before the verify keeps
		// that order true while the tree is already renamed. What matters is that
		// muxIsolationKey can still EvalSymlinks a key under oldDir.
		_, statErr := os.Stat(filepath.Join(oldDir, "keys"))
		order = append(order, fmt.Sprintf("reap:%s:keysResolvable=%v", filepath.Base(oldDir), statErr == nil))
	}
	hubMigrationVerify = func(frozen, newDir string) error {
		order = append(order, "verify:"+filepath.Base(frozen))
		return originalVerify(frozen, newDir)
	}

	withCwd(t, root, func() {
		if err := cmdHubJoin([]string{newRemote.URL, "--name", "codex"}); err != nil {
			t.Fatalf("migration failed: %v", err)
		}
	})

	if len(order) != 2 || !strings.HasPrefix(order[0], "reap:") || !strings.HasPrefix(order[1], "verify:") {
		t.Fatalf("order = %v, want the reap first and the verify second", order)
	}
	// THE load-bearing assertion. Reaping after the rename leaves
	// muxIsolationKey unable to resolve the key, so it falls back to
	// filepath.Clean and reaps a DIFFERENT socket without reporting anything.
	if !strings.HasSuffix(order[0], ":keysResolvable=true") {
		t.Fatalf("the reap ran after the freeze (%s): muxIsolationKey cannot EvalSymlinks the key under a renamed tree, so it silently reaps the wrong socket", order[0])
	}
	if order[0] != "reap:"+filepath.Base(oldRemote.HubDir)+":keysResolvable=true" {
		t.Fatalf("reap saw %q, want the old hub dir before the freeze", order[0])
	}
	if !strings.HasSuffix(order[1], hubMigrationFrozenSuffix) {
		t.Fatalf("verify saw %q, want the FROZEN tree — verifying the live path is the defect", order[1])
	}
}

// ROW C — the post-freeze crash state, and the negative control that makes it
// mean something.
//
// After the freeze the old directory is gone, so there is no migration candidate
// and nothing else can see the leftover. The join-start sweep is the only thing
// that recovers it. The second half is the load-bearing one: without it this row
// passes against a sweep that deletes because the directory EXISTS, which is
// issue #165 exactly, one directory over.
func TestHubJoinSweepsAFrozenTreeOnlyAfterVerifyingIt(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	frozen := newRemote.HubDir + hubMigrationFrozenSuffix

	// The crash state: newDir complete, oldDir gone, the old tree frozen.
	crashAfterHubMigrationRename(t, root, oldRemote, newRemote)
	if err := os.Rename(oldRemote.HubDir, frozen); err != nil {
		t.Fatal(err)
	}

	// Negative control FIRST, on the same fixture: make newDir not fully
	// represent the frozen tree, and the sweep must refuse and keep the bytes.
	held := filepath.Join(newRemote.HubDir, "keys", oldTarget)
	stash, err := os.ReadFile(held)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(held); err != nil {
		t.Fatal(err)
	}
	var refusal error
	withCwd(t, root, func() { refusal = cmdHubJoin([]string{newRemote.URL, "--name", "codex"}) })
	if refusal == nil {
		t.Fatal("the sweep deleted a frozen tree the new one does not represent — that is #165, one directory over")
	}
	if _, err := os.Stat(frozen); err != nil {
		t.Fatalf("refusal did not leave the frozen tree in place: %v", err)
	}
	assertOldHubKeyIntact(t, oldRemote, oldPub, oldTarget, frozen)

	// Now the positive half: restore what was missing and the sweep completes.
	if err := os.WriteFile(held, stash, 0o600); err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := cmdHubJoin([]string{newRemote.URL, "--name", "codex"}); err != nil {
			t.Fatalf("join did not recover the post-freeze crash state: %v", err)
		}
	})
	if _, err := os.Stat(frozen); !os.IsNotExist(err) {
		t.Fatalf("frozen tree survived a verified sweep: %v", err)
	}
	// The marker too. finishHubMigration clears it as its last step, so a crash
	// before that left it behind forever — harmless, but the one state that never
	// converged.
	if _, err := os.Stat(filepath.Join(newRemote.HubDir, hubMigrationMarker)); !os.IsNotExist(err) {
		t.Fatalf("the sweep left %s behind, so a sweep-recovered crash never converges: %v", hubMigrationMarker, err)
	}
}

// There is no "the frozen tree already exists, carry on" branch, and there must
// not be one: recovery lives in the sweep, in one place. A branch here would skip
// the rename, verify and delete somebody else's frozen tree, and report success
// having never migrated oldDir at all.
//
// This calls finishHubMigration DIRECTLY, because through cmdHubJoin the branch is
// unreachable — sweepFrozenHubMigration clears the frozen tree first, under the
// same lock — so a row driving the join cannot tell the two versions apart. My
// first attempt did exactly that and the mutation passed.
//
// The fixture is an EMPTY frozen directory, deliberately: it verifies trivially,
// so the restored branch cannot fail for the wrong reason. A non-empty fixture
// makes the branch fail on VERIFICATION instead — right answer, wrong reason —
// which is exactly how the first version of this row passed its own mutation.
//
// Note what the correct code does here, because it is stronger than it looks:
// os.Rename Lstats the target and returns a synthetic EEXIST for ANY directory,
// empty or not, without calling rename(2) (os/file_unix.go). So the freeze
// refuses to adopt a leftover tree in every case, and this row exercises the
// refusal rather than an overwrite.
func TestHubMigrationNeverSucceedsWithoutMigratingTheOldDirectory(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	_, _ = seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	crashAfterHubMigrationRename(t, root, oldRemote, newRemote)

	frozen := newRemote.HubDir + hubMigrationFrozenSuffix
	if err := os.MkdirAll(frozen, 0o700); err != nil {
		t.Fatal(err)
	}
	oldCfg, err := hubclient.ReadHubConfig(oldRemote.HubID)
	if err != nil {
		t.Fatal(err)
	}

	var finishErr error
	withCwd(t, root, func() {
		finishErr = finishHubMigration(root, newRemote, oldRemote.HubDir, oldCfg)
	})

	_, statErr := os.Stat(oldRemote.HubDir)
	migrated := os.IsNotExist(statErr)
	if finishErr == nil && !migrated {
		t.Fatal("finishHubMigration reported SUCCESS while the old hub directory is still there — it adopted a frozen tree it did not create and never migrated anything")
	}
	if finishErr != nil && migrated {
		t.Fatalf("reported failure after migrating: %v", finishErr)
	}
}

// ROW D — the frozen name can never be mistaken for a hub to migrate FROM.
func TestFrozenHubDirIsNotAMigrationCandidate(t *testing.T) {
	if hubDirNameRE.MatchString("0123456789ab" + hubMigrationFrozenSuffix) {
		t.Fatalf("the candidate scan matches %q; the frozen tree could be adopted as a hub", "0123456789ab"+hubMigrationFrozenSuffix)
	}
	if !hubDirNameRE.MatchString("0123456789ab") {
		t.Fatal("the candidate scan no longer matches a bare hub id; the row above proves nothing")
	}
}

// A lane holding the OLD hub id must block the SWEEP, not only the migration.
//
// The existing "shared lock on the old id refuses the migration" row exercises
// scope 2, which already held both ids. It stays green with the sweep completely
// unlocked, so it cannot pin this. This row drives the sweep path specifically:
// a frozen tree present and NO migration candidate, so scope 1 is the only thing
// that runs.
func TestSweepRefusesWhileALaneHoldsTheOldHubID(t *testing.T) {
	shortenHubLockTimeout(t)
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	frozen := newRemote.HubDir + hubMigrationFrozenSuffix

	// The post-freeze crash state: newDir complete and carrying the marker, the
	// old tree frozen, the old directory gone — so nothing is a candidate and
	// only the sweep has work to do.
	crashAfterHubMigrationRename(t, root, oldRemote, newRemote)
	if err := os.Rename(oldRemote.HubDir, frozen); err != nil {
		t.Fatal(err)
	}

	release, err := acquireHubLocks([]string{oldRemote.HubID}, hubLockShared)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var joinErr error
	withCwd(t, root, func() { joinErr = cmdHubJoin([]string{newRemote.URL, "--name", "codex"}) })
	if joinErr == nil {
		t.Fatal("the sweep deleted a frozen tree while a lane held the hub it came from")
	}
	if _, err := os.Stat(frozen); err != nil {
		t.Fatalf("the refusal did not leave the frozen tree in place: %v", err)
	}
	assertOldHubKeyIntact(t, oldRemote, oldPub, oldTarget, frozen)
}

// A frozen tree whose marker cannot name its origin is REFUSED, never swept.
//
// The sweep has to know which hub id to exclude before deleting anything.
// Without the marker it cannot, and deleting a tree without knowing whom to
// exclude is the whole class of defect this file exists to fix — so the tree
// stays and the operator is told where the bytes are.
func TestSweepRefusesAFrozenTreeItCannotAttribute(t *testing.T) {
	shortenHubLockTimeout(t)
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	frozen := newRemote.HubDir + hubMigrationFrozenSuffix

	crashAfterHubMigrationRename(t, root, oldRemote, newRemote)
	if err := os.Rename(oldRemote.HubDir, frozen); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(newRemote.HubDir, hubMigrationMarker)); err != nil {
		t.Fatal(err)
	}

	var joinErr error
	withCwd(t, root, func() { joinErr = cmdHubJoin([]string{newRemote.URL, "--name", "codex"}) })
	if joinErr == nil {
		t.Fatal("a frozen tree with no marker was swept; nothing knew which lanes to exclude")
	}
	if !strings.Contains(joinErr.Error(), "does not say which hub it came from") {
		t.Fatalf("refusal does not explain what is missing: %v", joinErr)
	}
	if _, err := os.Stat(frozen); err != nil {
		t.Fatalf("the refusal deleted the tree anyway: %v", err)
	}
	assertOldHubKeyIntact(t, oldRemote, oldPub, oldTarget, frozen)
}

// The pre-lock origin read is re-verified UNDER the lock.
//
// It chose which locks to take, so until it is confirmed under them it is a hint.
// The reachable case is two joins to the same new URL where the first crashes
// after its freeze: the second sees no frozen tree before the lock, waits on the
// new id, and would then sweep a tree whose origin it never learned and whose hub
// it does not hold.
//
// The seam makes the two reads disagree, which is the only thing that
// distinguishes a guard from no guard here.
func TestHubJoinRefusesWhenAFrozenTreeAppearsWhileLocking(t *testing.T) {
	shortenHubLockTimeout(t)
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	frozen := newRemote.HubDir + hubMigrationFrozenSuffix
	crashAfterHubMigrationRename(t, root, oldRemote, newRemote)
	if err := os.Rename(oldRemote.HubDir, frozen); err != nil {
		t.Fatal(err)
	}

	original := hubFrozenOrigin
	t.Cleanup(func() { hubFrozenOrigin = original })
	calls := 0
	hubFrozenOrigin = func(remote *loop.RemoteConfig) (string, error) {
		calls++
		if calls == 1 {
			// The pre-lock read: nothing there yet.
			return "", nil
		}
		return original(remote)
	}

	var joinErr error
	withCwd(t, root, func() { joinErr = cmdHubJoin([]string{newRemote.URL, "--name", "codex"}) })
	if joinErr == nil {
		t.Fatal("the join swept a frozen tree that appeared after it had already chosen which locks to hold")
	}
	if !strings.Contains(joinErr.Error(), "while locks were acquired") {
		t.Fatalf("refusal does not name the race: %v", joinErr)
	}
	if _, err := os.Stat(frozen); err != nil {
		t.Fatalf("the refusal deleted the tree anyway: %v", err)
	}
	assertOldHubKeyIntact(t, oldRemote, oldPub, oldTarget, frozen)
	if calls < 2 {
		t.Fatalf("the origin was read %d time(s); the under-lock re-read never happened", calls)
	}
}
