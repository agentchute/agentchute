package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/hubwire"
	"github.com/agentchute/agentchute/internal/loop"
)

func setupHubJoinTest(t *testing.T) (string, *loop.RemoteConfig) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	remote, err := loop.ParseRemoteURL("ssh://alex@hub.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	// These package-level seams require serial tests; do not call t.Parallel in this file.
	originalProbe := hubJoinProbe
	originalAuthorize := hubJoinAutoAuthorize
	originalJoinReapMux := hubJoinReapMux
	originalInstall := hubJoinInstallShims
	originalHostname := hubJoinHostname
	originalFingerprint := hubJoinFingerprint
	originalDiscoverFingerprint := hubJoinDiscoverFingerprint
	originalReapMux := hubMigrationReapMux
	hubJoinInstallShims = func() error { return nil }
	hubJoinHostname = func() (string, error) { return "Tiny.local", nil }
	hubJoinFingerprint = func(*loop.RemoteConfig) (string, error) { return "SHA256:host", nil }
	hubJoinDiscoverFingerprint = func(*loop.RemoteConfig) (string, error) { return "SHA256:host", nil }
	hubMigrationReapMux = func(*hubclient.HubConfig, string) {}
	hubJoinReapMux = func(*loop.RemoteConfig, string, string, string) error { return nil }
	t.Cleanup(func() {
		hubJoinProbe = originalProbe
		hubJoinAutoAuthorize = originalAuthorize
		hubJoinReapMux = originalJoinReapMux
		hubJoinInstallShims = originalInstall
		hubJoinHostname = originalHostname
		hubJoinFingerprint = originalFingerprint
		hubJoinDiscoverFingerprint = originalDiscoverFingerprint
		hubMigrationReapMux = originalReapMux
	})
	return root, remote
}

func successfulHubHello(agentID string) hubwire.HelloOK {
	return hubwire.HelloOK{
		ResponseBase: hubwire.ResponseBase{T: "hello-ok", Re: 1},
		V:            hubwire.Version, Agent: agentID, Pool: "/canonical/pool", Pool12: "0123456789ab",
		Writable: true, HubBin: "test", HubTime: time.Now().UTC(),
	}
}

func TestHubJoinURLFirstIdempotentAndHostnameInvariant(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	withCwd(t, root, func() {
		probeCalls := 0
		authorizeCalls := 0
		hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
			probeCalls++
			if probeCalls == 1 {
				return hubwire.HelloOK{}, nil, &hubclient.Error{Code: "E_UNAUTHORIZED", Msg: "unauthorized"}
			}
			return successfulHubHello("codex-tiny"), nil, nil
		}
		hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error {
			authorizeCalls++
			return nil
		}
		if err := cmdHubJoin([]string{remote.URL, "--name", "codex"}); err != nil {
			t.Fatal(err)
		}
		if authorizeCalls != 1 {
			t.Fatalf("authorize calls = %d, want 1", authorizeCalls)
		}
	})

	cfg, err := hubclient.ReadHubConfig(remote.HubID)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Names["codex"]; got != "codex-tiny" {
		t.Fatalf("names[codex] = %q, want codex-tiny", got)
	}
	if len(cfg.JoinedAs) != 1 || cfg.JoinedAs[0] != "codex-tiny" || cfg.Pool12 != "0123456789ab" {
		t.Fatalf("config = %#v", cfg)
	}
	active := filepath.Join(remote.HubDir, "keys", "codex-tiny_ed25519")
	target1, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	pub1, err := os.ReadFile(filepath.Join(filepath.Dir(active), target1+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, loop.PointerFileName)); err != nil {
		t.Fatal(err)
	}
	exclude, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exclude), loop.PointerFileName) {
		t.Fatalf("git exclude missing pointer: %s", exclude)
	}

	hubJoinHostname = func() (string, error) { return "Renamed.local", nil }
	hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello("codex-tiny"), nil, nil
	}
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	target2, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	pub2, err := os.ReadFile(filepath.Join(filepath.Dir(active), target2+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	if target2 != target1 || string(pub2) != string(pub1) {
		t.Fatalf("idempotent join changed key: %s -> %s", target1, target2)
	}
}

func TestHubJoinAuthFallbackWritesProvisionalPointer(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
		return hubwire.HelloOK{}, nil, &hubclient.Error{Code: "E_UNAUTHORIZED", Msg: "unauthorized"}
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error {
		return errors.New("operator ssh unavailable")
	}
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, AgentID: "work-tiny"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := hubclient.ReadHubConfig(remote.HubID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool12 != "" || !containsString(cfg.JoinedAs, "work-tiny") {
		t.Fatalf("provisional config = %#v", cfg)
	}
	data, err := os.ReadFile(filepath.Join(root, loop.PointerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != remote.URL {
		t.Fatalf("pointer = %q, want %q", data, remote.URL)
	}
}

func TestHubJoinRefusesUnknownNameBeforeKeygen(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	probeCalls := 0
	hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
		probeCalls++
		return hubwire.HelloOK{}, nil, nil
	}
	err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "work"})
	if err == nil || !strings.Contains(err.Error(), "does not name a wrapper") {
		t.Fatalf("error = %v", err)
	}
	if probeCalls != 0 {
		t.Fatalf("probe calls = %d, want 0", probeCalls)
	}
	if _, err := os.Stat(filepath.Join(remote.HubDir, "keys")); !os.IsNotExist(err) {
		t.Fatalf("keys directory exists after refusal: %v", err)
	}
}

func TestHubJoinKeyVersionNumericOrderingAndInvalidSuffix(t *testing.T) {
	_, remote := setupHubJoinTest(t)
	keysDir := filepath.Join(remote.HubDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex_ed25519.v2", "codex_ed25519.v10"} {
		if err := os.WriteFile(filepath.Join(keysDir, name), []byte("key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := scanHubKeyVersions(keysDir, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Number != 2 || versions[1].Number != 10 || nextHubKeyVersion(versions) != 11 {
		t.Fatalf("versions = %#v", versions)
	}
	invalid := filepath.Join(keysDir, "codex_ed25519.v01")
	if err := os.WriteFile(invalid, []byte("operator file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = scanHubKeyVersions(keysDir, "codex")
	if err == nil || !strings.Contains(err.Error(), "not a valid key version") {
		t.Fatalf("error = %v", err)
	}
	if got, err := os.ReadFile(invalid); err != nil || string(got) != "operator file" {
		t.Fatalf("invalid suffix file changed: %q, %v", got, err)
	}
}

func TestHubJoinAdoptsOrphanAndRecoversStagedRotation(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello("codex-tiny"), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(remote.HubDir, "keys", "codex-tiny_ed25519")
	first, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	if adopted, err := os.Readlink(active); err != nil || adopted != first {
		t.Fatalf("adopted target = %q, %v; want %q", adopted, err, first)
	}

	state, _, err := prepareHubKey(remote.HubDir, "codex-tiny")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := mintStagedHubKey(remote.HubDir, "codex-tiny", state)
	if err != nil {
		t.Fatal(err)
	}
	authorizeCalls := 0
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error {
		authorizeCalls++
		return nil
	}
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	if authorizeCalls != 0 {
		t.Fatalf("recovery re-drove remote replace %d time(s)", authorizeCalls)
	}
	if target, err := os.Readlink(active); err != nil || target != filepath.Base(staged.Private) {
		t.Fatalf("active target = %q, %v; want %q", target, err, filepath.Base(staged.Private))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(active), first)); !os.IsNotExist(err) {
		t.Fatalf("old key was not pruned after staged verification: %v", err)
	}
}

func TestHubJoinRotateUsesReplaceAndPromotes(t *testing.T) {
	root, remote := setupHubJoinTest(t)
	hubJoinProbe = func(*loop.RemoteConfig, string, string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello("codex-tiny"), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(remote.HubDir, "keys", "codex-tiny_ed25519")
	old, _ := os.Readlink(active)
	probeCalls := 0
	replaced := false
	reaped := ""
	hubJoinProbe = func(_ *loop.RemoteConfig, _ string, keyPath string) (hubwire.HelloOK, []string, error) {
		probeCalls++
		if strings.HasSuffix(keyPath, ".v2") && probeCalls == 1 {
			return hubwire.HelloOK{}, nil, &hubclient.Error{Code: "E_UNAUTHORIZED", Msg: "staged not active"}
		}
		return successfulHubHello("codex-tiny"), nil, nil
	}
	hubJoinAutoAuthorize = func(_ *loop.RemoteConfig, _ string, _ string, replace bool) error {
		replaced = replace
		return nil
	}
	hubJoinReapMux = func(_ *loop.RemoteConfig, agentID, keyPath, stateDir string) error {
		if agentID != "codex-tiny" || stateDir != remote.HubDir {
			t.Fatalf("reap args = agent %q state %q", agentID, stateDir)
		}
		reaped = keyPath
		return nil
	}
	if err := runHubJoin(root, remote, hubJoinOptions{URL: remote.URL, Name: "codex", RotateKey: true}); err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("rotation did not request --replace-key")
	}
	if !strings.HasSuffix(reaped, ".v2") {
		t.Fatalf("rotation reaped key path %q, want staged v2", reaped)
	}
	if target, err := os.Readlink(active); err != nil || target == old || !strings.HasSuffix(target, ".v2") {
		t.Fatalf("rotation target = %q, %v; old %q", target, err, old)
	}
}

func TestHubJoinShadowingBothOrders(t *testing.T) {
	cfg := &hubclient.HubConfig{JoinedAs: []string{"codex"}, Names: map[string]string{"codex": "codex-tiny"}}
	if err := validateHubJoinShadowing(cfg, "", "codex", hubJoinOptions{AgentID: "codex"}); err == nil || !strings.Contains(err.Error(), "already this machine's local name") {
		t.Fatalf("explicit shadow error = %v", err)
	}
	if err := validateHubJoinShadowing(&hubclient.HubConfig{JoinedAs: []string{"codex"}}, "codex", "codex-tiny", hubJoinOptions{Name: "codex"}); err == nil || !strings.Contains(err.Error(), "already joined as pool id") {
		t.Fatalf("name shadow error = %v", err)
	}
}

func TestHubJoinLockBusyIsBounded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	originalTimeout := hubLockTimeout
	hubLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hubLockTimeout = originalTimeout })
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withHubLock("0123456789ab", func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	err := withHubLock("0123456789ab", func() error { return nil })
	close(release)
	if heldErr := <-done; heldErr != nil {
		t.Fatal(heldErr)
	}
	if err == nil || !strings.Contains(err.Error(), "another agentchute hub join/rotate") {
		t.Fatalf("busy error = %v", err)
	}
}

// Issue #165 — the migration RECOVERY branch destroys the authorized key.
//
// These two rows are the partner of TestHubJoinSameHubMigrationPreservesStateAndKey
// below: that one drives the COPY path and asserts the active key survives;
// these drive the RECOVERY path and assert the same property. The recovery
// branch (hub_migrate.go) returns early when the new hub dir already exists —
// treating it as "I crashed after my own rename" — and so writes the pointer,
// reaps the mux and deletes the old dir WITHOUT copying anything. It keys on
// nothing but the directory's existence, so a dir created by anything else is
// indistinguishable from a half-finished migration.
//
// Both rows FAIL until #165 lands, deliberately: they assert the post-fix
// contract, and the defect is a silent data-loss path that should be pinned by
// a failing test rather than a comment.
//
// The load-bearing assertion is the SURVIVING KEY, not the error. An error-only
// row would pass against a fix that refuses for the wrong reason, or that
// refuses after deleting — so the key is checked first and the error second.

// Arm 1: a FAILED fresh join left the new dir behind with a key and no
// config.json. This is the severe sub-case and the likely one in practice:
// prepareHubKey mints before the join ever contacts the hub, so any failure at
// or after the hello leaves exactly this state — and retrying is the first thing
// an operator does. findHubMigrationCandidate then probes with the OLD key and
// returns the old id BECAUSE that probe succeeded: the code proves the key works
// and then deletes it.
func TestHubJoinRecoveryBranchWithUnrecognizedDirPreservesOldKey(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)

	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	// The failed-join residue: a minted key, no config.json.
	if _, _, err := prepareHubKey(newRemote.HubDir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(newRemote.HubDir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("precondition: new dir must have no config.json, got %v", err)
	}

	var joinErr error
	withCwd(t, root, func() {
		joinErr = cmdHubJoin([]string{newRemote.URL, "--name", "codex"})
	})
	assertOldHubStatePreserved(t, oldRemote, newRemote, oldPub, oldTarget, joinErr)
}

// Arm 2: a COMPLETED fresh join to the new URL. The new dir has a valid config
// carrying the new URL and the same pool, so findHubMigrationCandidate takes its
// other arm and still returns the old id — reaching the same destructive branch.
// Access survives here because the new key was authorized, but the old key is
// orphaned in the hub's authorized_keys and everything under the old hub dir is
// still deleted uncopied. Covering only arm 1 would leave this free to regress.
func TestHubJoinRecoveryBranchWithCompletedFreshJoinPreservesOldKey(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	oldPub, oldTarget := seedJoinedHub(t, root, oldRemote)

	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	// A complete, ordinary join to the new URL: config.json and all.
	if err := runHubJoin(root, newRemote, hubJoinOptions{URL: newRemote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(newRemote.HubDir, "config.json")); err != nil {
		t.Fatalf("precondition: new dir must have a config.json: %v", err)
	}

	var joinErr error
	withCwd(t, root, func() {
		joinErr = cmdHubJoin([]string{newRemote.URL, "--name", "codex"})
	})
	assertOldHubStatePreserved(t, oldRemote, newRemote, oldPub, oldTarget, joinErr)
}

// seedJoinedHub completes a normal join against oldRemote and returns the active
// key's public material and symlink target, which is the state the recovery
// branch must not destroy.
func seedJoinedHub(t *testing.T, root string, oldRemote *loop.RemoteConfig) ([]byte, string) {
	t.Helper()
	hubJoinProbe = func(_ *loop.RemoteConfig, agentID, _ string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello(agentID), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, oldRemote, hubJoinOptions{URL: oldRemote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(oldRemote.HubDir, "keys", "codex-tiny_ed25519")
	target, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(filepath.Dir(active), target+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	return pub, target
}

// assertOldHubStatePreserved checks the property first and the symptom second.
func assertOldHubStatePreserved(t *testing.T, oldRemote, newRemote *loop.RemoteConfig, oldPub []byte, oldTarget string, joinErr error) {
	t.Helper()
	_, statErr := os.Stat(oldRemote.HubDir)
	oldGone := os.IsNotExist(statErr)

	// Report the actual consequence when it happens, rather than "expected an
	// error, got nil" — a regression here is data loss and should read as such.
	if oldGone {
		newActive := filepath.Join(newRemote.HubDir, "keys", "codex-tiny_ed25519")
		newTarget, _ := os.Readlink(newActive)
		newPub, _ := os.ReadFile(filepath.Join(filepath.Dir(newActive), newTarget+".pub"))
		if string(oldPub) != string(newPub) {
			t.Fatalf("DATA LOSS: the old hub dir was deleted without being copied, so the authorized key %q no longer exists anywhere (the new dir holds an unrelated key %q)", oldTarget, newTarget)
		}
		t.Fatalf("the old hub dir was deleted by a migration that copied nothing (key material happened to match)")
	}
	if statErr != nil {
		t.Fatalf("stat old hub dir: %v", statErr)
	}
	active := filepath.Join(oldRemote.HubDir, "keys", "codex-tiny_ed25519")
	target, err := os.Readlink(active)
	if err != nil {
		t.Fatalf("old active key symlink: %v", err)
	}
	pub, err := os.ReadFile(filepath.Join(filepath.Dir(active), target+".pub"))
	if err != nil {
		t.Fatalf("read old active pubkey: %v", err)
	}
	if target != oldTarget || string(pub) != string(oldPub) {
		t.Fatalf("old active key changed: target %q -> %q", oldTarget, target)
	}

	// Only now the symptom. Naming the directory keeps an unrelated failure from
	// satisfying the row.
	if joinErr == nil {
		t.Fatal("alias join succeeded against an unrecognized new hub dir; it must refuse rather than migrate into it")
	}
	if !strings.Contains(joinErr.Error(), newRemote.HubDir) {
		t.Fatalf("refusal does not name the unrecognized directory %s: %v", newRemote.HubDir, joinErr)
	}
}

func TestHubJoinSameHubMigrationPreservesStateAndKey(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	hubJoinProbe = func(_ *loop.RemoteConfig, agentID, _ string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello(agentID), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, oldRemote, hubJoinOptions{URL: oldRemote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(oldRemote.HubDir, "keys", "codex-tiny_ed25519")
	oldTarget, err := os.Readlink(active)
	if err != nil {
		t.Fatal(err)
	}
	oldPub, err := os.ReadFile(filepath.Join(filepath.Dir(active), oldTarget+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	latch := filepath.Join(oldRemote.ShadowLoopDir, "state", "codex-tiny", "guard.latch")
	if err := os.MkdirAll(filepath.Dir(latch), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(latch, []byte("preserve-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(oldRemote.HubDir, "mux"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRemote.HubDir, "mux", "socket-residue"), []byte("skip"), 0o600); err != nil {
		t.Fatal(err)
	}

	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		if err := cmdHubJoin([]string{newRemote.URL, "--name", "codex"}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(oldRemote.HubDir); !os.IsNotExist(err) {
		t.Fatalf("old hub dir still exists: %v", err)
	}
	newActive := filepath.Join(newRemote.HubDir, "keys", "codex-tiny_ed25519")
	newTarget, err := os.Readlink(newActive)
	if err != nil {
		t.Fatal(err)
	}
	newPub, err := os.ReadFile(filepath.Join(filepath.Dir(newActive), newTarget+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	if newTarget != oldTarget || string(newPub) != string(oldPub) {
		t.Fatalf("migration changed active key: %q -> %q", oldTarget, newTarget)
	}
	gotLatch, err := os.ReadFile(filepath.Join(newRemote.ShadowLoopDir, "state", "codex-tiny", "guard.latch"))
	if err != nil || string(gotLatch) != "preserve-me" {
		t.Fatalf("migrated latch = %q, %v", gotLatch, err)
	}
	if _, err := os.Stat(filepath.Join(newRemote.HubDir, "mux")); !os.IsNotExist(err) {
		t.Fatalf("mux directory was copied: %v", err)
	}
	cfg, err := hubclient.ReadHubConfig(newRemote.HubID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != newRemote.URL || cfg.Names["codex"] != "codex-tiny" {
		t.Fatalf("migrated config = %#v", cfg)
	}
}

func TestHubJoinMigrationRefusesAttributedLiveLane(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	hubJoinProbe = func(_ *loop.RemoteConfig, agentID, _ string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello(agentID), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, oldRemote, hubJoinOptions{URL: oldRemote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	loopCfg := &loop.Config{ControlRepo: root, LoopDir: oldRemote.ShadowLoopDir}
	if err := loop.SaveRunnerState(loopCfg, loop.RunnerState{
		AgentID: "codex-tiny", Host: localHostname(), RunnerPID: os.Getpid(), ChildPID: os.Getpid(),
		StartedAt: time.Now().UTC(), Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	originalCmdline := setupProcessCommandLine
	setupProcessCommandLine = func(int) string { return "agentchute serve --control-repo " + oldRemote.URL + " -- codex" }
	t.Cleanup(func() { setupProcessCommandLine = originalCmdline })
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-live-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		err = cmdHubJoin([]string{newRemote.URL, "--name", "codex"})
	})
	if err == nil || !strings.Contains(err.Error(), "is still running against the old URL") {
		t.Fatalf("migration error = %v", err)
	}
	if _, statErr := os.Stat(oldRemote.HubDir); statErr != nil {
		t.Fatalf("old hub dir changed after refusal: %v", statErr)
	}
	if _, statErr := os.Stat(newRemote.HubDir); !os.IsNotExist(statErr) {
		t.Fatalf("new hub dir exists after refusal: %v", statErr)
	}
}

func TestHubJoinMigrationRefusesUnreadableRunnerState(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	hubJoinProbe = func(_ *loop.RemoteConfig, agentID, _ string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello(agentID), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, oldRemote, hubJoinOptions{URL: oldRemote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	loopCfg := &loop.Config{ControlRepo: root, LoopDir: oldRemote.ShadowLoopDir}
	runnerPath := loopCfg.RunnerStatePath("codex-tiny")
	if err := os.MkdirAll(filepath.Dir(runnerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runnerPath, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-corrupt-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		err = cmdHubJoin([]string{newRemote.URL, "--name", "codex"})
	})
	if err == nil || !strings.Contains(err.Error(), "repair the JSON or remove the corrupt file") {
		t.Fatalf("migration error = %v", err)
	}
	if _, statErr := os.Stat(oldRemote.HubDir); statErr != nil {
		t.Fatalf("old hub dir changed after refusal: %v", statErr)
	}
	if _, statErr := os.Stat(newRemote.HubDir); !os.IsNotExist(statErr) {
		t.Fatalf("new hub dir exists after refusal: %v", statErr)
	}
}

func TestHubJoinMigrationRefusesMismatchedRunnerState(t *testing.T) {
	root, oldRemote := setupHubJoinTest(t)
	hubJoinProbe = func(_ *loop.RemoteConfig, agentID, _ string) (hubwire.HelloOK, []string, error) {
		return successfulHubHello(agentID), nil, nil
	}
	hubJoinAutoAuthorize = func(*loop.RemoteConfig, string, string, bool) error { return nil }
	if err := runHubJoin(root, oldRemote, hubJoinOptions{URL: oldRemote.URL, Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	loopCfg := &loop.Config{ControlRepo: root, LoopDir: oldRemote.ShadowLoopDir}
	runnerPath := loopCfg.RunnerStatePath("codex-tiny")
	if err := os.MkdirAll(filepath.Dir(runnerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runnerPath, []byte(`{"agent_id":"other-lane"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	newRemote, err := loop.ParseRemoteURL("ssh://alex@hub-mismatch-alias.example/home/alex/code/agentchute")
	if err != nil {
		t.Fatal(err)
	}
	withCwd(t, root, func() {
		err = cmdHubJoin([]string{newRemote.URL, "--name", "codex"})
	})
	want := fmt.Sprintf("hub join: confirm lane %q is stopped, then move runner state %s to agent_id %q's state directory (or remove it if stale) and re-run. This agent_id mismatch prevents the join from proving the lane is stopped", "codex-tiny", runnerPath, "other-lane")
	if err == nil || err.Error() != want {
		t.Fatalf("migration error = %q, want %q", err, want)
	}
	if _, statErr := os.Stat(oldRemote.HubDir); statErr != nil {
		t.Fatalf("old hub dir changed after refusal: %v", statErr)
	}
	if _, statErr := os.Stat(newRemote.HubDir); !os.IsNotExist(statErr) {
		t.Fatalf("new hub dir exists after refusal: %v", statErr)
	}
}

func TestRemoteMigrationCommandAttribution(t *testing.T) {
	oldURL := "ssh://alex@hub.example/pool"
	shadow := "/tmp/hub/.agentchute/loop"
	for _, cmdline := range []string{
		"/usr/local/bin/agentchute serve codex",
		"agentchute run --loop-dir " + shadow + " -- codex",
		"agentchute serve --control-repo=" + oldURL + " -- codex --control-repo ssh://wrong/wrapper",
	} {
		if !remoteMigrationCommandMatches(cmdline, oldURL, shadow) {
			t.Errorf("expected attribution: %q", cmdline)
		}
	}
	for _, cmdline := range []string{
		"sleep 100",
		"agentchute serve --control-repo ssh://alex@other.example/pool -- codex",
		"agentchute serve --loop-dir /tmp/other -- codex",
	} {
		if remoteMigrationCommandMatches(cmdline, oldURL, shadow) {
			t.Errorf("unexpected attribution: %q", cmdline)
		}
	}
}
