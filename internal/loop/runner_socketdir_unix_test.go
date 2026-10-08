//go:build !windows

package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review 2026-10-08 S9: a socket directory pre-created world-writable by ANOTHER
// user is refused, not adopted and chmodded. Tests cannot chown, so the expected
// owner is moved instead: the directory is ours, and the check is told to
// expect someone else.
func TestEnsureOwnedSocketDirRefusesADirectoryOwnedByAnotherUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ac-501")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	orig := socketDirOwnerUID
	socketDirOwnerUID = func() int { return os.Getuid() + 4242 }
	t.Cleanup(func() { socketDirOwnerUID = orig })

	err := EnsureOwnedSocketDir(dir)
	if err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Fatalf("EnsureOwnedSocketDir on another user's 0777 dir = %v, want an ownership refusal", err)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("mode = %o; a refused directory must not be chmodded (that would adopt it)", info.Mode().Perm())
	}
}
