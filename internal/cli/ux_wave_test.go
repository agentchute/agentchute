package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #177 — joining a hub must not invent the operator's login profiles.
//
// On a fresh macOS account, `hub join` created ~/.zshrc and ~/.profile, neither
// of which existed: agentchute writing a user's shell startup files as a side
// effect of an unrelated command, with no warning, no opt-out, and a timestamped
// backup left behind each time.
func TestSetupDoesNotCreateAProfileThatDidNotExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	shimDir := filepath.Join(home, ".agentchute", "bin")

	stdout := captureStdout2(t, func() {
		if err := setupEnsureShimPath(setupOptions{ShimDir: shimDir}); err != nil {
			t.Fatal(err)
		}
	})

	for _, name := range []string{".zshrc", ".profile"} {
		if _, err := os.Stat(filepath.Join(home, name)); err == nil {
			t.Fatalf("agentchute created ~/%s, which did not exist", name)
		}
	}
	// Silence would be worse than the old behaviour: PATH is genuinely not set
	// up, and the operator has to be told what to add.
	if !strings.Contains(stdout, "none was created") {
		t.Fatalf("no warning that nothing was updated:\n%s", stdout)
	}
	if !strings.Contains(stdout, setupPathExpr(shimDir)) {
		t.Fatalf("the operator is not told what to add:\n%s", stdout)
	}
	// And how to get the old behaviour back deliberately.
	if !strings.Contains(stdout, "--profile") || !strings.Contains(stdout, "--no-profile") {
		t.Fatalf("neither escape hatch is named:\n%s", stdout)
	}
}

// The control, and the reason this is a narrowing rather than a removal: a
// profile that EXISTS is still updated, exactly as before.
func TestSetupStillUpdatesAProfileThatExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	zshrc := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(zshrc, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(home, ".agentchute", "bin")

	if err := setupEnsureShimPath(setupOptions{ShimDir: shimDir}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(zshrc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), setupPathExpr(shimDir)) {
		t.Fatalf("an existing profile was not updated:\n%s", data)
	}
	if !strings.Contains(string(data), "# mine") {
		t.Fatalf("the operator's own content was lost:\n%s", data)
	}
	// The absent sibling (~/.profile) still must not appear.
	if _, err := os.Stat(filepath.Join(home, ".profile")); err == nil {
		t.Fatal("agentchute created ~/.profile alongside the one that existed")
	}
}

// Naming a file is the consent that was missing, so --profile still creates it.
func TestSetupCreatesAProfileTheOperatorNamed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("PATH", "/usr/bin:/bin")
	named := filepath.Join(home, "my-shell-rc")
	shimDir := filepath.Join(home, ".agentchute", "bin")

	if err := setupEnsureShimPath(setupOptions{ShimDir: shimDir, Profile: named}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(named)
	if err != nil {
		t.Fatalf("a profile the operator named was not created: %v", err)
	}
	if !strings.Contains(string(data), setupPathExpr(shimDir)) {
		t.Fatalf("named profile has no PATH block:\n%s", data)
	}
}

// #178 — the verdict has to reach the exit code, or a script cannot see it.
func TestIncompleteHubJoinExitsTwo(t *testing.T) {
	if got := exitCodeForError(errHubJoinIncomplete); got != 2 {
		t.Fatalf("exit code = %d, want 2 — a script checking $? cannot tell half-joined from joined", got)
	}
	// The controls: an ordinary failure is still 1, and success is still 0.
	if got := exitCodeForError(errNotJoinedForTest()); got != 1 {
		t.Fatalf("an ordinary error exits %d, want 1", got)
	}
}

func errNotJoinedForTest() error { return os.ErrNotExist }

func captureStdout2(t *testing.T, fn func()) string {
	t.Helper()
	out, err := captureStdout(t, func() error {
		fn()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
