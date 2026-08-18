package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// codex's row, and the primary one: a one-shot must not write into the hub tree
// while a migration holds it.
//
// The defect is on the ERROR path. writeSendSpool puts an unsent body in
// HubDir/spool when the config is remote, and preserveRemoteSendBody runs after
// hubclient has already returned — so a lock scoped to the dial would leave this
// open while looking like a fix. The lock spans the command.
func TestRemoteSendRefusesAndSpoolsNothingWhileTheHubIsMigrating(t *testing.T) {
	shortenHubLockTimeout(t)
	root, remote := setupHubJoinTest(t)
	seedJoinedHub(t, root, remote)

	release, err := acquireHubLocks([]string{remote.HubID}, hubLockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var sendErr error
	withCwd(t, root, func() {
		sendErr = cmdSend([]string{"--from", "codex-tiny", "--to", "grok", "--body", "must not be spooled"})
	})
	if sendErr == nil {
		t.Fatal("send ran while the hub was held exclusively for a migration")
	}
	if !strings.Contains(sendErr.Error(), "being migrated right now") {
		t.Fatalf("refusal does not name the migration: %v", sendErr)
	}
	if entries := spoolEntries(t, remote.HubDir); len(entries) != 0 {
		t.Fatalf("a body was written into the tree being migrated: %v", entries)
	}
}

// The negative control, without which the row above passes against a `send` that
// refuses unconditionally.
//
// The same forced failure with NO lock held must still spool the body AND still
// print the preserved-body path. That recovery UX is the thing being protected —
// N-05 on real machines showed it is what stands between an operator and a
// duplicate — so a "fix" that quietly breaks it has to fail differently.
func TestRemoteSendStillSpoolsAndReportsWhenNothingHoldsTheHub(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	seedJoinedHub(t, root, remote)

	var out string
	var sendErr error
	withCwd(t, root, func() {
		var capErr error
		out, capErr = captureStdout(t, func() error {
			sendErr = cmdSend([]string{"--from", "codex-tiny", "--to", "grok", "--body", "must be preserved"})
			return nil
		})
		if capErr != nil {
			t.Fatal(capErr)
		}
	})
	if sendErr == nil {
		t.Fatal("the fixture hub is unreachable; the send was expected to fail")
	}
	if strings.Contains(sendErr.Error(), "being migrated right now") {
		t.Fatalf("nothing held the hub, yet the send refused on the lock: %v", sendErr)
	}
	entries := spoolEntries(t, remote.HubDir)
	if len(entries) != 1 {
		t.Fatalf("the body was not preserved: %v", entries)
	}
	combined := out + sendErr.Error()
	if !strings.Contains(combined, entries[0]) {
		t.Fatalf("the preserved-body path is not reported anywhere the operator sees:\nstdout:\n%s\nerror:\n%v", out, sendErr)
	}
}

// hub join must NOT take the shared lock through the wrapper.
//
// It takes the same ids EXCLUSIVELY itself, so a shared acquisition from the same
// process would make its own exclusive acquire spin the full timeout and fail with
// a busy error naming its own lock — the self-relock trap, one layer up, and this
// seam is exactly what would introduce it. Structural today (hub join resolves its
// root with resolveInitRoot, never discoverConfig), and structural is one refactor
// from not being structural.
func TestHubJoinTakesNoSharedLockThroughTheDiscoverSeam(t *testing.T) {
	shortenHubLockTimeout(t)
	root, remote := setupHubJoinTest(t)
	seedJoinedHub(t, root, remote)

	var joinErr error
	withCwd(t, root, func() {
		joinErr = cmdHubJoin([]string{remote.URL, "--name", "codex"})
	})
	if joinErr != nil && strings.Contains(joinErr.Error(), "this hub is busy") {
		t.Fatalf("hub join blocked on a lock it took itself, one layer up: %v", joinErr)
	}
	if joinErr != nil {
		t.Fatalf("hub join failed: %v", joinErr)
	}
}

func spoolEntries(t *testing.T, hubDir string) []string {
	t.Helper()
	dir := filepath.Join(hubDir, "spool")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths
}

// The seam guard: nothing in internal/cli may call loop.Discover directly.
//
// This is the row that replaces twenty judgment calls. The rule "decide per
// command whether this one touches the hub tree" is twenty chances to be wrong
// once; "discovering a remote config takes the lock" is one place to be right,
// and this keeps it that way. A new command that reaches past the wrapper gets a
// remote config with no lock held, which is the whole defect back again — and it
// would not fail any other test, because the file it writes is only destroyed by
// a migration that happens to be running.
func TestNoDirectLoopDiscoverOutsideTheSeam(t *testing.T) {
	// The package directory is derived from THIS FILE, not from the working
	// directory. `.` is not reliably the package dir here — measured: it is the
	// module root — and a guard that scans the wrong directory finds nothing and
	// passes forever. The first version of this row did exactly that.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this file, so the scan would have no directory to read")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Prove the scan is looking at something: this very file must be in it.
	found := false
	for _, e := range entries {
		if e.Name() == filepath.Base(thisFile) {
			found = true
		}
	}
	if !found {
		t.Fatalf("scanned %s and did not find this test file; the guard is reading the wrong directory", dir)
	}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// The wrapper itself is the one legitimate caller. Test files are
		// exempt: they construct configs directly and never take the lock, which
		// is what makes them able to drive the contended cases at all.
		if name == "hub_lock_discover.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "loop.Discover(") {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) != 0 {
		t.Fatalf("these call loop.Discover directly and so get a remote config with no hub lock held: %v\nuse discoverConfig — see the contract in AGENTCHUTE.md 13.9a", offenders)
	}
}
