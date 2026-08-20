package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #166, the sixth transition in A2's staged-rotation recovery set.
//
// ssh-keygen writes the private file and then the public one, and mintHubKey
// fsyncs the keys dir only after both — so an interruption in that window is a
// real on-disk state: the version's private half present, its `.pub` absent.
// prepareHubKey adopted the orphan (correct: the hub may already have authorized
// it), the join then died at readHubPublicKey, and a plain re-run returned
// cleanly with the identical broken state. The obvious operator action looped
// forever, and nothing in the error named `--rotate-key` as the escape.
//
// The repair was already in the same file: checkHubKeyPassphraseFree runs
// `ssh-keygen -y`, whose stdout IS the public key derived from the private one,
// and threw that stdout away. Regenerating is also the only SAFE repair —
// minting a replacement would strand a credential the hub may already have
// authorized and present a different one, the failure family #165 is about.

func TestHubKeyRecoveryRegeneratesAMissingPublicHalf(t *testing.T) {
	dir, keysDir := newHubKeyDir(t)
	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(keysDir, "codex-tiny_ed25519")
	original := readKeyFile(t, active+".v1.pub")

	// The crash state: private half survives, public half never landed, and the
	// active symlink was not written either.
	if err := os.Remove(active + ".v1.pub"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}

	state, minted, err := prepareHubKey(dir, "codex-tiny")
	if err != nil {
		t.Fatalf("recovery failed instead of converging: %v", err)
	}
	if minted {
		t.Fatal("recovery MINTED over a key whose private half was intact; the hub may already have authorized it")
	}
	if state.Active.Number != 1 {
		t.Fatalf("adopted version = v%d, want v1", state.Active.Number)
	}
	// Regenerated, and regenerated CORRECTLY — a .pub that exists but describes
	// a different key would authorize the wrong credential, which is worse than
	// the missing file.
	if got := readKeyFile(t, active+".v1.pub"); got != original {
		t.Fatalf("regenerated public half does not match the private one:\n want=%s\n  got=%s", original, got)
	}
	// The convergence claim, stated as the thing the join actually does: this is
	// the call that used to fail and send the operator back into the loop.
	pub, err := readHubPublicKey(state.Active)
	if err != nil {
		t.Fatalf("the join still cannot read the public key after recovery: %v", err)
	}
	// readKeyFile returns the base64 body; readHubPublicKey returns the whole
	// line. Compare on the body, which is the part that decides which key the
	// hub authorizes.
	if !strings.Contains(pub, original) {
		t.Fatalf("readHubPublicKey returned %q, which does not carry the original key body %q", pub, original)
	}
	if versions := countKeyVersions(t, keysDir, "codex-tiny"); versions != 1 {
		t.Fatalf("recovery left %d versions, want 1", versions)
	}
}

// The same missing file reached by the other path: the active symlink is intact,
// so prepareHubKey never enters the adopt branch. Without this row a repair
// placed only in the orphan branch looks complete and leaves the commoner state
// — crash after the symlink, .pub still missing — looping exactly as before.
func TestHubKeyRegeneratesAMissingPublicHalfWithTheSymlinkIntact(t *testing.T) {
	dir, keysDir := newHubKeyDir(t)
	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(keysDir, "codex-tiny_ed25519")
	original := readKeyFile(t, active+".v1.pub")
	if err := os.Remove(active + ".v1.pub"); err != nil {
		t.Fatal(err)
	}

	state, _, err := prepareHubKey(dir, "codex-tiny")
	if err != nil {
		t.Fatalf("recovery failed instead of converging: %v", err)
	}
	if got := readKeyFile(t, active+".v1.pub"); got != original {
		t.Fatalf("public half not regenerated on the symlink-intact path:\n want=%s\n  got=%s", original, got)
	}
	if _, err := readHubPublicKey(state.Active); err != nil {
		t.Fatalf("readHubPublicKey still fails: %v", err)
	}
}

// A .pub that is already there must be left exactly alone. Rewriting it on every
// join would be a write nobody asked for on the credential path, and it would
// hide a real mismatch by overwriting the evidence.
func TestHubKeyLeavesAnExistingPublicHalfUntouched(t *testing.T) {
	dir, keysDir := newHubKeyDir(t)
	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(keysDir, "codex-tiny_ed25519.v1.pub")
	// A comment field ssh-keygen -y would NOT reproduce: if the file is rewritten
	// this is what disappears.
	raw, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	marked := strings.TrimSpace(string(raw)) + " operator-added-comment\n"
	if err := os.WriteFile(pubPath, []byte(marked), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := prepareHubKey(dir, "codex-tiny"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(after); got != marked {
		t.Fatalf("an existing public half was rewritten:\n want=%s\n  got=%s", marked, got)
	}
}

// grok's #167 sweep, folded in: mintHubKey returned a hubKeyVersion naming a
// .pub it never checked for. Same class as the rest of this file — a command
// exits zero and the caller reads that as "both files are there".
func TestMintHubKeyRefusesToClaimAPublicHalfItCannotSee(t *testing.T) {
	_, keysDir := newHubKeyDir(t)
	original := runHubSSHKeygen
	t.Cleanup(func() { runHubSSHKeygen = original })
	// A keygen that writes the private half and exits 0 without the public one:
	// the interrupted-mint state, made deterministic.
	runHubSSHKeygen = func(args ...string) ([]byte, error) {
		path := ""
		for i, arg := range args {
			if arg == "-f" && i+1 < len(args) {
				path = args[i+1]
			}
		}
		if path == "" {
			t.Fatalf("keygen invoked without -f: %v", args)
		}
		if err := os.WriteFile(path, []byte("PRIVATE"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil
	}

	_, err := mintHubKey(keysDir, "codex-tiny", 1)
	if err == nil {
		t.Fatal("mintHubKey reported success for a key whose public half was never written")
	}
	if !strings.Contains(err.Error(), ".pub") {
		t.Fatalf("the error does not name what is missing: %v", err)
	}
}

func newHubKeyDir(t *testing.T) (dir, keysDir string) {
	t.Helper()
	dir = t.TempDir()
	keysDir = filepath.Join(dir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, keysDir
}
