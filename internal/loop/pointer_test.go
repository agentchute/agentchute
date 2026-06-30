package loop

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePointerFileHappy(t *testing.T) {
	got, err := ParsePointerFile("../coordination\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "../coordination" {
		t.Fatalf("got %q, want %q", got, "../coordination")
	}
}

func TestParsePointerFileIgnoresCommentsAndBlanks(t *testing.T) {
	got, err := ParsePointerFile(`
# coordination repo for the frontend / backend split
# maintained by the platform team

../coordination

`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "../coordination" {
		t.Fatalf("got %q, want ../coordination", got)
	}
}

func TestParsePointerFileRejectsMultiplePaths(t *testing.T) {
	_, err := ParsePointerFile("../one\n../two\n")
	if err == nil {
		t.Fatal("expected error on multiple path lines")
	}
	if !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("expected 'more than one' error, got %v", err)
	}
}

func TestParsePointerFileRejectsEmpty(t *testing.T) {
	for _, content := range []string{"", "\n\n", "# only comments\n# nothing else\n"} {
		_, err := ParsePointerFile(content)
		if err == nil {
			t.Fatalf("expected error on empty/comment-only content %q", content)
		}
	}
}

func TestParsePointerFileHandlesWindowsLineEndings(t *testing.T) {
	got, err := ParsePointerFile("# crlf\r\n../target\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "../target" {
		t.Fatalf("got %q", got)
	}
}

func TestDiscoverPointerNoneIsCleanNil(t *testing.T) {
	dir := t.TempDir()
	got, err := DiscoverPointer(dir)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil pointer, got %+v", got)
	}
}

func TestDiscoverPointerNearestWins(t *testing.T) {
	root := t.TempDir()
	// Two pointers in the ancestor chain. Both point at the same valid target;
	// we only assert nearest-wins on which file was selected.
	target := t.TempDir()
	mustWrite(t, filepath.Join(target, "AGENTCHUTE.md"), []byte("# spec\n"))

	near := filepath.Join(root, "sub")
	mustMkdir(t, near)
	mustWrite(t, filepath.Join(near, PointerFileName), []byte(target+"\n"))
	mustWrite(t, filepath.Join(root, PointerFileName), []byte(target+"\n"))

	got, err := DiscoverPointer(near)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a pointer, got nil")
	}
	if got.PointerFilePath != filepath.Join(near, PointerFileName) {
		t.Fatalf("PointerFilePath = %q, want nearest %q", got.PointerFilePath, filepath.Join(near, PointerFileName))
	}
	if len(got.Shadowed) != 1 || got.Shadowed[0] != filepath.Join(root, PointerFileName) {
		t.Fatalf("Shadowed = %v, want [%q]", got.Shadowed, filepath.Join(root, PointerFileName))
	}
}

func TestResolvePointerTargetRelative(t *testing.T) {
	// Build a sibling layout: parent/{ptr-dir, target}
	parent := t.TempDir()
	ptrDir := filepath.Join(parent, "ptr-dir")
	target := filepath.Join(parent, "target")
	mustMkdir(t, ptrDir)
	mustMkdir(t, target)

	got, err := ResolvePointerTarget(ptrDir, "../target")
	if err != nil {
		t.Fatal(err)
	}
	// On macOS /tmp is a symlink to /private/tmp; both paths must resolve to
	// the same absolute. Compare using EvalSymlinks for robustness.
	gotReal, _ := filepath.EvalSymlinks(got)
	wantReal, _ := filepath.EvalSymlinks(target)
	if gotReal != wantReal {
		t.Fatalf("ResolvePointerTarget = %q (real %q), want %q (real %q)", got, gotReal, target, wantReal)
	}
}

func TestResolvePointerTargetAbsoluteAccepted(t *testing.T) {
	target := t.TempDir()
	got, err := ResolvePointerTarget("/some/other/dir", target)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("ResolvePointerTarget returned empty path")
	}
}

func TestResolvePointerTargetMissingDir(t *testing.T) {
	parent := t.TempDir()
	_, err := ResolvePointerTarget(parent, "../does-not-exist")
	if err == nil {
		t.Fatal("expected error on missing target")
	}
}

func TestResolvePointerTargetEmptyRejected(t *testing.T) {
	_, err := ResolvePointerTarget(t.TempDir(), "   ")
	if err == nil {
		t.Fatal("expected error on empty target")
	}
}

// Sibling-repo case — the primary use case enabled by §4 / §4.2: a pointer at
// repo-A/.agentchute-control-repo points at ../control. The resolved target
// MUST be allowed even though it escapes repo-A's directory (codex's round-2
// revision; the hard-boundary check from earlier rounds was wrong).
func TestDiscoverPointerSiblingRepoEscapeIsAllowed(t *testing.T) {
	parent := t.TempDir()
	control := filepath.Join(parent, "control")
	mustMkdir(t, control)
	mustWrite(t, filepath.Join(control, "AGENTCHUTE.md"), []byte("# spec\n"))

	repoA := filepath.Join(parent, "repo-A")
	mustMkdir(t, repoA)
	mustWrite(t, filepath.Join(repoA, PointerFileName), []byte("../control\n"))

	got, err := DiscoverPointer(repoA)
	if err != nil {
		t.Fatalf("sibling-repo pointer should resolve, got err: %v", err)
	}
	if got == nil {
		t.Fatal("expected pointer, got nil")
	}
	gotReal, _ := filepath.EvalSymlinks(got.ResolvedTarget)
	wantReal, _ := filepath.EvalSymlinks(control)
	if gotReal != wantReal {
		t.Fatalf("ResolvedTarget = %q (real %q), want %q (real %q)", got.ResolvedTarget, gotReal, control, wantReal)
	}
}

// Discover() integrates the pointer file as a discovery cascade step.
func TestDiscoverIntegratesPointerFile(t *testing.T) {
	parent := t.TempDir()
	// Set up a valid control repo at parent/control with a vendor loop.
	control := filepath.Join(parent, "control")
	mustMkdir(t, filepath.Join(control, ".agentchute", "loop"))
	mustWrite(t, filepath.Join(control, "AGENTCHUTE.md"), []byte("# spec\n"))

	// repo-A has the pointer but no local loop or AGENTCHUTE.md.
	repoA := filepath.Join(parent, "repo-A")
	mustMkdir(t, repoA)
	mustWrite(t, filepath.Join(repoA, PointerFileName), []byte("../control\n"))

	cfg, err := Discover(DiscoverOpts{Cwd: repoA})
	if err != nil {
		t.Fatal(err)
	}

	gotReal, _ := filepath.EvalSymlinks(cfg.ControlRepo)
	wantReal, _ := filepath.EvalSymlinks(control)
	if gotReal != wantReal {
		t.Fatalf("ControlRepo = %q (real %q), want %q (real %q)", cfg.ControlRepo, gotReal, control, wantReal)
	}
	if !strings.HasPrefix(cfg.ControlRepoOrigin, "pointer:") {
		t.Fatalf("ControlRepoOrigin = %q, want pointer:...", cfg.ControlRepoOrigin)
	}
	if cfg.Vendor != "agentchute" {
		t.Fatalf("Vendor = %q, want agentchute", cfg.Vendor)
	}
}

// Flag explicitly beats pointer (most-explicit-first cascade).
func TestDiscoverFlagBeatsPointer(t *testing.T) {
	parent := t.TempDir()
	pointerTarget := filepath.Join(parent, "pointer-target")
	mustMkdir(t, filepath.Join(pointerTarget, ".agentchute", "loop"))
	mustWrite(t, filepath.Join(pointerTarget, "AGENTCHUTE.md"), []byte("# spec\n"))

	flagTarget := filepath.Join(parent, "flag-target")
	mustMkdir(t, filepath.Join(flagTarget, ".agentchute", "loop"))
	mustWrite(t, filepath.Join(flagTarget, "AGENTCHUTE.md"), []byte("# spec\n"))

	repoA := filepath.Join(parent, "repo-A")
	mustMkdir(t, repoA)
	mustWrite(t, filepath.Join(repoA, PointerFileName), []byte(pointerTarget+"\n"))

	cfg, err := Discover(DiscoverOpts{Cwd: repoA, ControlRepoFlag: flagTarget})
	if err != nil {
		t.Fatal(err)
	}
	gotReal, _ := filepath.EvalSymlinks(cfg.ControlRepo)
	wantReal, _ := filepath.EvalSymlinks(flagTarget)
	if gotReal != wantReal {
		t.Fatalf("ControlRepo = %q, want flag target %q", cfg.ControlRepo, flagTarget)
	}
	if cfg.ControlRepoOrigin != "flag" {
		t.Fatalf("ControlRepoOrigin = %q, want flag", cfg.ControlRepoOrigin)
	}
}

// Malformed pointer file = hard error (operator put it there on purpose).
func TestDiscoverPointerMalformedIsHardError(t *testing.T) {
	parent := t.TempDir()
	mustWrite(t, filepath.Join(parent, PointerFileName), []byte("../one\n../two\n"))
	if _, err := Discover(DiscoverOpts{Cwd: parent}); err == nil {
		t.Fatal("expected hard error on malformed pointer file")
	}
}

// Pointer target that doesn't have AGENTCHUTE.md = hard error.
func TestDiscoverPointerTargetMissingSpecFile(t *testing.T) {
	parent := t.TempDir()
	bad := filepath.Join(parent, "no-spec")
	mustMkdir(t, bad)
	mustWrite(t, filepath.Join(parent, PointerFileName), []byte(bad+"\n"))

	_, err := Discover(DiscoverOpts{Cwd: parent})
	if err == nil {
		t.Fatal("expected hard error when pointer target lacks AGENTCHUTE.md")
	}
	if !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("expected 'does not contain' in error, got %v", err)
	}
}

// mustWrite + mustMkdir live in config_test.go; reusing them.
