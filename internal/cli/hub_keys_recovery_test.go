package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A2 (first slice) / DESIGN §10.3 "staged rotation crash recovery (D4b/F3/H3)".
//
// The row specifies injecting failure at every transition of the staged
// rotation. These are the two grok ranked as the ones that catch production —
// the orphaned first key, and a half-finished promote — because both leave the
// key directory in a state where the WRONG recovery silently changes which
// credential the lane presents.
//
// Approach, as agreed: construct the post-crash STATE and assert convergence,
// rather than injecting a crash. There is no failure seam in the key path, and
// adding one would be a production change for a test. The trade is explicit:
// this proves *state ⇒ converges*, not *crash at X ⇒ that state*. The second
// half stays unproven, and a state-construction test derived from the
// implementation can encode the same misunderstanding the implementation has —
// which is the reason these want a reviewer who reads the code, not just the row.
//
// NOT sshd rows. They are filesystem-local key-state recovery and belong beside
// the code they exercise; parking them under integration/sshd would report sshd
// coverage that does not exist.

// Transition: minted the first key, crashed BEFORE creating the active symlink.
// Recovery must ADOPT the orphan, never mint over it — minting would strand a
// key the hub may already have authorized and present a different one.
func TestHubKeyRecoveryAdoptsOrphanedFirstKey(t *testing.T) {
	dir := t.TempDir()
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The state a crash between keygen and symlink leaves: version files, no
	// active symlink.
	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(keysDir, "codex-tiny_ed25519")
	orphanPub := readKeyFile(t, active+".v1.pub")
	if err := os.Remove(active); err != nil {
		t.Fatalf("stage the orphan: %v", err)
	}

	state, minted, err := prepareHubKey(dir, "codex-tiny")
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if minted {
		t.Fatal("recovery MINTED a new key over an adoptable orphan; the orphan may already be authorized on the hub")
	}
	if got := activeKeyBody(t, active); got != orphanPub {
		t.Fatalf("active key material changed during recovery:\n orphan=%s\n active=%s", orphanPub, got)
	}
	if state.Active.Number != 1 {
		t.Fatalf("adopted version = v%d, want v1", state.Active.Number)
	}
	// And no second version was created alongside it.
	if versions := countKeyVersions(t, keysDir, "codex-tiny"); versions != 1 {
		t.Fatalf("recovery left %d versions, want 1", versions)
	}
}

// Transition: rotation crashed MID-PROMOTE — the temporary symlink exists, the
// rename never happened. Recovery must leave the ACTIVE key unchanged: a
// half-finished promote must not silently switch which credential the lane
// presents, because the new one may not be authorized yet.
func TestHubKeyRecoveryFromHalfFinishedPromoteKeepsActiveKey(t *testing.T) {
	dir := t.TempDir()
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(keysDir, "codex-tiny_ed25519")
	v1Pub := readKeyFile(t, active+".v1.pub")

	// Mint v2 the way a rotation does, then stop just before the rename.
	v2, err := mintHubKey(keysDir, "codex-tiny", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(v2.Private), active+".tmp"); err != nil {
		t.Fatalf("stage the half-promote: %v", err)
	}

	state, minted, err := prepareHubKey(dir, "codex-tiny")
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if minted {
		t.Fatal("recovery minted a key while two versions were already present")
	}
	if got := activeKeyBody(t, active); got != v1Pub {
		t.Fatalf("a half-finished promote silently switched the active key:\n was=%s\n now=%s", v1Pub, got)
	}
	if state.Active.Number != 1 {
		t.Fatalf("active = v%d after an incomplete promote, want v1", state.Active.Number)
	}
	// The stray temp symlink must not survive to be adopted later.
	if _, err := os.Lstat(active + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp promote symlink survived recovery (stat err = %v)", err)
	}

	// "Did not switch the active key" is only HALF the property. The other half
	// is that the rotation is still RESUMABLE — recovery must not silently
	// abandon it. Nothing above can tell the difference: a change that made
	// recovery ignore versions above active (which reads like hardening) would
	// keep this row green while v2 became unreachable. v2 would then survive on
	// disk forever, since pruneOlderHubKeys only removes versions BELOW active,
	// and a later --rotate-key would mint v3 — stranding a key the hub may
	// already have authorized. That is the same failure this row exists to
	// catch, one step later.
	if state.Staged == nil {
		t.Fatal("recovery abandoned the rotation: Staged = nil, want the staged v2")
	}
	if state.Staged.Number != 2 {
		t.Fatalf("staged v%d, want v2", state.Staged.Number)
	}
	if state.Staged.Private != v2.Private {
		t.Fatalf("staged names %s, want the minted %s", state.Staged.Private, v2.Private)
	}

	// And prove it by finishing the job: drive the RECOVERED state through the
	// same promote the interrupted rotation was in the middle of. This is what
	// makes the row prove its stated `state => converges` claim rather than only
	// `state => nothing was destroyed`. finishHubJoinKey is the production
	// caller, but it probes the key over SSH; promoteHubKey is the step that
	// actually resumes, and it is reached with exactly this argument.
	if err := promoteHubKey(dir, "codex-tiny", *state.Staged); err != nil {
		t.Fatalf("the rotation did not resume from the recovered state: %v", err)
	}
	if got := activeKeyBody(t, active); got != readKeyFile(t, v2.Public) {
		t.Fatalf("resumed promote did not land on v2:\n active=%s", got)
	}
}

// The NEGATIVE arm of the orphan row above, and the one that decides whether
// "adopt what you find" is safe: an orphan that FAILS its passphrase-free probe
// must be retired to `.invalid.<stamp>` and replaced, never adopted.
//
// Adopting an unusable key would produce a lane that authenticates with nothing
// and reports no error until the first connect; silently deleting it would
// destroy material an operator may need to inspect. The retire-and-remint path
// is what makes the adopt path in the row above safe to have at all.
func TestHubKeyRecoveryRetiresAnUnusableOrphanAndRemints(t *testing.T) {
	dir := t.TempDir()
	keysDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(keysDir, "codex-tiny_ed25519")
	orphanPub := readKeyFile(t, active+".v1.pub")
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	// Make the orphan fail its probe. Corrupting the private key is the same
	// observable a passphrase-protected one produces — ssh-keygen -y -P "" fails
	// — without needing to mint an encrypted key in a test.
	if err := os.WriteFile(active+".v1", []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, minted, err := prepareHubKey(dir, "codex-tiny")
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if !minted {
		t.Fatal("recovery adopted an orphan that cannot be used; the lane would authenticate with nothing")
	}
	if got := activeKeyBody(t, active); got == orphanPub {
		t.Fatal("recovery kept the unusable key material")
	}
	// Deliberately NOT asserting the reminted version number. The retired file
	// leaves the `.vN` namespace entirely (it becomes `.invalid.<stamp>`), so
	// reusing v1 collides with nothing and no contract says otherwise. Asserting
	// it would have pinned an invention of mine rather than a documented rule —
	// which is exactly the hazard of a test built from the implementation.

	// Retired, not deleted: the operator can still inspect it.
	entries, err := os.ReadDir(keysDir)
	if err != nil {
		t.Fatal(err)
	}
	retired := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".invalid.") {
			retired++
		}
	}
	if retired == 0 {
		t.Fatalf("unusable key was destroyed rather than retired; keys dir = %v", names(entries))
	}
	// The retired file carries the unusable MATERIAL. Asserting the .v1 NAME is
	// gone would be wrong: the remint legitimately reuses it, so the name says
	// nothing about which bytes are where.
	var retiredBody string
	for _, e := range entries {
		// The retired PUBLIC file is `<name>.pub.invalid.<stamp>`, so it does not
		// end in .pub — match on the substring anywhere in the name.
		if strings.Contains(e.Name(), ".invalid.") && !strings.Contains(e.Name(), ".pub") {
			data, rerr := os.ReadFile(filepath.Join(keysDir, e.Name()))
			if rerr != nil {
				t.Fatal(rerr)
			}
			retiredBody = string(data)
		}
	}
	if retiredBody != "not a key\n" {
		t.Fatalf("the retired file does not hold the unusable key material: %q", retiredBody)
	}

	// Both halves, same stamp. The retired PUBLIC half matters more here than the
	// private one: the private is corrupt, so nothing can regenerate the public
	// from it, and mintHubKey immediately overwrites <agent>_ed25519.v1.pub with
	// the fresh key's — the remint reuses v1. The retired .pub is therefore the
	// ONLY surviving record of which key this was, and it is what an operator
	// needs to find and revoke the matching authorized_keys line on the hub. Lose
	// it and the stranded credential becomes unidentifiable.
	//
	// Asserting the paired stamp as well, because "for every artifact the code
	// moves, assert every file that artifact is made of" is exactly the class of
	// miss this row has already produced twice.
	var retiredPub, privStamp, pubStamp string
	for _, e := range entries {
		name := e.Name()
		idx := strings.Index(name, ".invalid.")
		if idx < 0 {
			continue
		}
		if strings.Contains(name, ".pub") {
			retiredPub = readKeyFile(t, filepath.Join(keysDir, name))
			pubStamp = name[idx+len(".invalid."):]
		} else {
			privStamp = name[idx+len(".invalid."):]
		}
	}
	if retiredPub != orphanPub {
		t.Fatalf("the retired public half was not preserved: %q, want %q", retiredPub, orphanPub)
	}
	if privStamp == "" || privStamp != pubStamp {
		t.Fatalf("the two halves were not retired under the same stamp: private=%q public=%q", privStamp, pubStamp)
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// activeKeyBody resolves the active symlink and reads ITS public file — the
// documented way to get the current pubkey. There is deliberately no
// <agent>_ed25519.pub symlink, so reading beside the link finds nothing.
func activeKeyBody(t *testing.T, active string) string {
	t.Helper()
	target, err := os.Readlink(active)
	if err != nil {
		t.Fatalf("read active symlink %s: %v", active, err)
	}
	return readKeyFile(t, filepath.Join(filepath.Dir(active), target+".pub"))
}

func readKeyFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		t.Fatalf("malformed key file %s: %q", path, data)
	}
	return fields[1]
}

func countKeyVersions(t *testing.T, keysDir, agentID string) int {
	t.Helper()
	versions, err := scanHubKeyVersions(keysDir, agentID)
	if err != nil {
		t.Fatalf("scan versions: %v", err)
	}
	return len(versions)
}
