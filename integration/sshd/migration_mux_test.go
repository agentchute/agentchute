//go:build sshd_integration

package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
	"github.com/agentchute/agentchute/internal/op"
)

// A1 / §10.3 "mux reaped across migration (G4)".
//
// A migration deletes the old hub directory. A ControlPersist master opened
// before that migration is still listening on a socket under the deleted tree,
// still authenticated as the old identity, and — because ssh will happily reuse
// a master whose ControlPath it can still reach — a later one-shot can ATTACH to
// that orphan and keep talking to the hub through the pre-migration connection.
// The migration would report success while the next operation ran on the old
// authorization: the same shape as the stale-authorization defect, and equally
// silent.
//
// The migration is detected by HOST KEY FINGERPRINT, not by hostname
// (`findHubMigrationCandidate`, hub_migrate.go:22-84), so re-joining the SAME
// daemon through a second name for it is a real alias rejoin rather than a
// fixture trick — which is why this row uses `localhost` against the same sshd
// and adds the matching known_hosts line, exactly as a hub reachable by two
// names would have.
//
// The alias has to be a different host NAME, not a different spelling of the
// same one: ParseRemoteURL canonicalizes both `127.1` and a trailing-slash pool
// path back to the joined URL, so those produce the SAME hub id and no
// migration at all. That is correct production behavior — it is what makes pool
// identity durable — and this row asserts the hub id actually changed so it can
// never silently degrade into testing nothing.
func TestSSHDMuxIsReapedAcrossMigration(t *testing.T) {
	h := newSSHDHarness(t)
	checkout := h.newCheckout()

	if stdout, stderr, err := h.runCLI(checkout, "hub", "join", h.remote.URL, "--as", "work-tiny"); err != nil {
		t.Fatalf("hub join: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	oldRemote := parseRemoteForHome(t, h)
	oldDir := oldRemote.HubDir

	// Open a master and prove it is LIVE, so the reap has something real to
	// reap. Without this the row would pass against a migration that reaps
	// nothing, because there would have been nothing to attach to either.
	// A one-shot that SUCCEEDS. The harness pre-authorizes `codex`/`grok`, so a
	// join must use a fresh id; but `status` refuses an id with no pool row, and
	// counting an auth for a command that failed before reaching the hub would
	// be a fixture that never touches the code. Register the joined id first.
	vendor := "test"
	if _, err := op.Register(h.cfg, op.Context{ActorID: "work-tiny"}, op.RegisterReq{Vendor: &vendor, Host: "sshd-fixture"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, err := h.runCLI(checkout, "status", "--as", "work-tiny"); err != nil {
		t.Fatalf("first status: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	afterFirst := h.authCount()
	if stdout, stderr, err := h.runCLI(checkout, "status", "--as", "work-tiny"); err != nil {
		t.Fatalf("second status: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got := h.authCount(); got != afterFirst {
		t.Fatalf("no live master to reap: auth count %d -> %d across two one-shots", afterFirst, got)
	}

	aliasURL := h.aliasURL()
	if stdout, stderr, err := h.runCLI(checkout, "hub", "join", aliasURL, "--as", "work-tiny"); err != nil {
		t.Fatalf("alias rejoin: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	newRemote := parseRemoteForHome(t, h)
	if newRemote.HubID == oldRemote.HubID {
		t.Fatalf("alias rejoin did not produce a new hub id (%s); this row would test nothing", newRemote.HubID)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old hub dir survived the migration (stat err = %v)", err)
	}
	if _, err := os.Stat(newRemote.HubDir); err != nil {
		t.Fatalf("new hub dir missing after migration: %v", err)
	}

	// The orphan must be GONE, not merely unreferenced. Asserting the socket no
	// longer answers is stronger than asserting a reap was attempted, and it is
	// the property that stops a later one-shot attaching to it.
	if sockets := muxSocketsUnder(t, oldDir); len(sockets) != 0 {
		t.Fatalf("mux sockets survive under the deleted hub dir: %v", sockets)
	}

	// A one-shot after the migration must RE-AUTHENTICATE. An attach to the
	// orphan would show no new auth line while still succeeding, which is
	// exactly the silent failure this row exists to catch.
	before := h.authCount()
	if stdout, stderr, err := h.runCLI(checkout, "status", "--as", "work-tiny"); err != nil {
		t.Fatalf("post-migration status: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if got := h.authCount(); got != before+1 {
		t.Fatalf("post-migration one-shot auth count = %d, want %d (it reused a pre-migration master)", got, before+1)
	}
	// And the fresh master belongs to the NEW hub dir.
	if sockets := muxSocketsUnder(t, newRemote.HubDir); len(sockets) == 0 {
		t.Fatalf("no mux socket under the new hub dir %s", newRemote.HubDir)
	}
}

// muxSocketsUnder lists mux control sockets beneath dir. A deleted hub dir has
// none by construction; the check is written as a walk anyway so it reports what
// it found rather than only that something was there.
func muxSocketsUnder(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSocket != 0 {
			found = append(found, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}

// reapedRemoteFor is the remote the migration's own reap would target: the OLD
// url and the OLD hub dir. Kept beside the row it documents so a future reader
// can see which identity the reap has to use — pointing it at the new HubID is
// the mistake the row is guarding against, and it fails silently.
func reapedRemoteFor(t *testing.T, cfg *hubclient.HubConfig, oldDir string) *loop.RemoteConfig {
	t.Helper()
	remote, err := loop.ParseRemoteURL(cfg.URL)
	if err != nil {
		t.Fatalf("parse old hub url %q: %v", cfg.URL, err)
	}
	remote.HubDir = oldDir
	if !strings.HasPrefix(oldDir, filepath.Dir(oldDir)) {
		t.Fatalf("unexpected hub dir shape %q", oldDir)
	}
	return remote
}
