package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// helper: builds a minimal valid control repo under root.
func makeControlRepo(t *testing.T, root string) {
	t.Helper()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	mustMkdir(t, filepath.Join(root, ".rehumanlabs", "loop"))
}

func TestPreparePoolPlanFreshTarget(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(plan.Targets))
	}
	tp := plan.Targets[0]
	if tp.PointerAction.Action != "write pointer" {
		t.Errorf("PointerAction = %q, want 'write pointer'", tp.PointerAction.Action)
	}
	if tp.PointerAction.Apply == nil {
		t.Error("PointerAction.Apply must be non-nil for fresh target")
	}
	// 4 wrappers (CLAUDE/CODEX/GEMINI/AGENTS)
	if len(tp.WrapperActions) != 4 {
		t.Fatalf("WrapperActions count = %d, want 4", len(tp.WrapperActions))
	}
	for _, w := range tp.WrapperActions {
		if w.Action != "create v4" {
			t.Errorf("wrapper %s action = %q, want 'create v4'", filepath.Base(w.Target), w.Action)
		}
	}
	if tp.GitignoreAction != nil {
		t.Error("GitignoreAction should be nil when --update-gitignore is not set")
	}
}

// Codex's POOL-1 review regression: an existing pointer that uses a DIFFERENT
// spelling (e.g., absolute path) but resolves to the SAME control repo must
// NOT be treated as a conflicting redirect. Should be a skip (or, under
// --replace-pointer, a deliberate spelling normalization).
func TestPreparePoolPlanEquivalentSpellingIsSkip(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	// Pre-existing pointer using ABSOLUTE path; desired pointer would be RELATIVE.
	mustWrite(t, filepath.Join(target, loop.PointerFileName), []byte(root+"\n"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatalf("equivalent-spelling pointer should NOT error: %v", err)
	}
	tp := plan.Targets[0]
	if tp.PointerAction.Action != "skip pointer" {
		t.Errorf("PointerAction = %q, want 'skip pointer' (equivalent target)", tp.PointerAction.Action)
	}
}

// Same case but with --replace-pointer should rewrite to the preferred
// (relative) spelling as a deliberate normalization.
func TestPreparePoolPlanEquivalentSpellingWithReplaceNormalizes(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	mustWrite(t, filepath.Join(target, loop.PointerFileName), []byte(root+"\n"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	tp := plan.Targets[0]
	if tp.PointerAction.Action != "replace pointer" {
		t.Errorf("PointerAction = %q, want 'replace pointer' (normalize)", tp.PointerAction.Action)
	}
	if !strings.Contains(tp.PointerAction.Detail, "normalize") {
		t.Errorf("detail should mention normalization, got %q", tp.PointerAction.Detail)
	}
}

func TestPreparePoolPlanExistingMatchingPointer(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	// Pre-existing pointer pointing at the same control repo.
	rel, _ := filepath.Rel(target, root)
	mustWrite(t, filepath.Join(target, loop.PointerFileName), []byte(rel+"\n"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	tp := plan.Targets[0]
	if tp.PointerAction.Action != "skip pointer" {
		t.Errorf("PointerAction = %q, want 'skip pointer'", tp.PointerAction.Action)
	}
	if tp.PointerAction.Apply != nil {
		t.Error("skip should not have an Apply func")
	}
}

func TestPreparePoolPlanExistingConflictingPointerWithoutReplaceErrors(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	// Pre-existing pointer pointing somewhere else.
	mustWrite(t, filepath.Join(target, loop.PointerFileName), []byte("../some-other-path\n"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	_, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err == nil {
		t.Fatal("expected error on conflicting pointer without --replace-pointer")
	}
	if !strings.Contains(err.Error(), "--replace-pointer") {
		t.Errorf("expected error to mention --replace-pointer, got %v", err)
	}
}

func TestPreparePoolPlanExistingConflictingPointerWithReplaceAllowed(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	mustWrite(t, filepath.Join(target, loop.PointerFileName), []byte("../some-other-path\n"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	tp := plan.Targets[0]
	if tp.PointerAction.Action != "replace pointer" {
		t.Errorf("PointerAction = %q, want 'replace pointer'", tp.PointerAction.Action)
	}
	if !strings.Contains(tp.PointerAction.Detail, "../some-other-path") {
		t.Errorf("detail should show old path, got %q", tp.PointerAction.Detail)
	}
}

func TestPreparePoolPlanTargetIsControlRepoErrors(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	_, err := computePreparePoolPlan(cfg, []string{root}, false, false)
	if err == nil {
		t.Fatal("expected error when target is the control repo itself")
	}
	if !strings.Contains(err.Error(), "control repo itself") {
		t.Errorf("expected 'control repo itself' in error, got %v", err)
	}
}

func TestPreparePoolPlanTargetIsAnotherControlRepoErrors(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)

	// Target also has a vendor loop dir — already a control repo.
	target := t.TempDir()
	mustMkdir(t, filepath.Join(target, ".myorg", "loop"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	_, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err == nil {
		t.Fatal("expected error when target already has its own vendor loop dir")
	}
	if !strings.Contains(err.Error(), "loop directory") {
		t.Errorf("expected error mentioning loop directory, got %v", err)
	}
}

func TestPreparePoolPlanMultiTarget(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	t1 := t.TempDir()
	t2 := t.TempDir()

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{t1, t2}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 2 {
		t.Fatalf("Targets = %d, want 2", len(plan.Targets))
	}
}

func TestPreparePoolPlanWithUpdateGitignore(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()
	// Make target a git repo so the gitignore planner doesn't skip.
	mustMkdir(t, filepath.Join(target, ".git"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	tp := plan.Targets[0]
	if tp.GitignoreAction == nil {
		t.Fatal("GitignoreAction should be set when --update-gitignore is on and target is a git repo")
	}
}

func TestPreparePoolPlanNonGitTargetSurfacedAsNote(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir() // no .git

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Targets[0].NotGit {
		t.Error("NotGit should be true when target has no .git")
	}

	// printPlan output should mention the non-git note.
	var buf bytes.Buffer
	printPreparePoolPlan(&buf, plan)
	if !strings.Contains(buf.String(), "not a git repo") {
		t.Errorf("plan output should warn about non-git target:\n%s", buf.String())
	}
}

func TestPreparePoolPlanRejectsMalformedExistingPointer(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	// Pre-existing pointer with multi-path (malformed per ParsePointerFile).
	mustWrite(t, filepath.Join(target, loop.PointerFileName), []byte("../one\n../two\n"))

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	// Even with --replace-pointer, malformed existing should be a hard error
	// — operator must fix it manually.
	_, err := computePreparePoolPlan(cfg, []string{target}, true, false)
	if err == nil {
		t.Fatal("expected hard error on malformed existing pointer")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("expected 'malformed' in error, got %v", err)
	}
}

// Codex's POOL-2 ask: symlinked targets must be rejected before any write.
// prepare-pool's other filesystem ops avoid following symlinks; this one
// shouldn't either, or writes land at the symlink's destination rather than
// the operator's intended target.
func TestPreparePoolPlanRejectsSymlinkTarget(t *testing.T) {
	parent := t.TempDir()
	realTarget := filepath.Join(parent, "real")
	mustMkdir(t, realTarget)
	symlinkPath := filepath.Join(parent, "link-to-real")
	if err := os.Symlink(realTarget, symlinkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	root := t.TempDir()
	makeControlRepo(t, root)
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	_, err := computePreparePoolPlan(cfg, []string{symlinkPath}, false, false)
	if err == nil {
		t.Fatal("expected error on symlinked target")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected 'symlink' in error, got %v", err)
	}
}

// End-to-end: dry-run plan → apply → resulting filesystem state matches
// the plan. Pointer file exists, wrapper files prepended, and a fresh
// Discover() from inside the target picks up the pointer.
func TestPreparePoolApplyEndToEnd(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}

	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := applyPreparePoolPlan(plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 1 pointer + 4 wrappers = 5 actions.
	if applied != 5 {
		t.Errorf("applied = %d, want 5", applied)
	}

	pointerPath := filepath.Join(target, loop.PointerFileName)
	if _, err := os.Stat(pointerPath); err != nil {
		t.Fatalf("pointer file not written: %v", err)
	}
	for _, wt := range wrapperTargets {
		wrapperPath := filepath.Join(target, wt.Filename)
		if _, err := os.Stat(wrapperPath); err != nil {
			t.Errorf("wrapper %s not written: %v", wt.Filename, err)
		}
	}
	agentsPath := filepath.Join(target, "AGENTS.md")
	if _, err := os.Stat(agentsPath); err != nil {
		t.Errorf("AGENTS.md not written: %v", err)
	}

	// Fresh Discover() from inside the target should find the pointer and
	// resolve back to the control repo.
	discovered, err := loop.Discover(loop.DiscoverOpts{Cwd: target})
	if err != nil {
		t.Fatalf("Discover via pointer: %v", err)
	}
	gotReal, _ := filepath.EvalSymlinks(discovered.ControlRepo)
	wantReal, _ := filepath.EvalSymlinks(root)
	if gotReal != wantReal {
		t.Errorf("Discover got control_repo %q, want %q", discovered.ControlRepo, root)
	}
	if !strings.HasPrefix(discovered.ControlRepoOrigin, "pointer:") {
		t.Errorf("ControlRepoOrigin = %q, want pointer:...", discovered.ControlRepoOrigin)
	}
}

// Idempotency: running prepare-pool twice on the same target should be a
// no-op the second time (pointer skip + wrapper skip).
func TestPreparePoolApplyIdempotent(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan1, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyPreparePoolPlan(plan1); err != nil {
		t.Fatal(err)
	}

	plan2, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatalf("second plan should not error: %v", err)
	}
	tp := plan2.Targets[0]
	if tp.PointerAction.Action != "skip pointer" {
		t.Errorf("second-run PointerAction = %q, want 'skip pointer'", tp.PointerAction.Action)
	}
	if tp.PointerAction.Apply != nil {
		t.Error("idempotent skip should not have an Apply")
	}
	for _, w := range tp.WrapperActions {
		if !strings.HasPrefix(w.Action, "skip") && w.Apply != nil {
			t.Errorf("wrapper %s action = %q with non-nil Apply on second run",
				filepath.Base(w.Target), w.Action)
		}
	}
}

// Preflight catches a non-writable target before any real write happens.
// Skip on systems where chmod 0o500 doesn't restrict writes (root, some CI).
func TestPreparePoolPreflightCatchesNonWritableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o500 writability check")
	}
	root := t.TempDir()
	makeControlRepo(t, root)

	target := t.TempDir()
	t1 := filepath.Join(target, "writable")
	t2 := filepath.Join(target, "readonly")
	mustMkdir(t, t1)
	mustMkdir(t, t2)
	if err := os.Chmod(t2, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(t2, 0o700) })

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{t1, t2}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyPreparePoolPlan(plan); err == nil {
		t.Fatal("expected preflight to catch readonly target")
	}
	// Nothing should have been written at the writable target either —
	// preflight must abort BEFORE any actual write.
	if _, err := os.Stat(filepath.Join(t1, loop.PointerFileName)); err == nil {
		t.Error("preflight should have aborted before writing the pointer at the writable target")
	}
}

// Atomic write: the pointer file should never exist in a partial state. We
// can't simulate a crash mid-write easily, but we can verify the temp file
// is cleaned up after a successful write (no stragglers left in the dir).
func TestPreparePoolAtomicWriteCleansTempFiles(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyPreparePoolPlan(plan); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp_") {
			t.Errorf("temp file leak after atomic write: %s", e.Name())
		}
	}
}

// bestPointerPath: cross-tree paths (different filesystem roots) use absolute.
// Without this, the rel path is "../../Users/alex/..." which is unreadable
// and breaks when the project is cloned with a different filesystem layout.
func TestBestPointerPathCrossTreeUsesAbsolute(t *testing.T) {
	target := "/tmp/cross-tree-target"
	control := "/Users/somebody/code/control"
	got := bestPointerPath(target, control)
	if got != control {
		t.Errorf("cross-tree got %q, want absolute %q", got, control)
	}
}

// bestPointerPath: sibling repos under a common project parent use relative.
func TestBestPointerPathSiblingUsesRelative(t *testing.T) {
	target := "/Users/alex/code/repo-A"
	control := "/Users/alex/code/coordination"
	got := bestPointerPath(target, control)
	if got != "../coordination" {
		t.Errorf("sibling got %q, want '../coordination'", got)
	}
}

// bestPointerPath: nephew layout (two team subdirs under same project)
// still uses relative.
func TestBestPointerPathNephewUsesRelative(t *testing.T) {
	target := "/Users/alex/code/project/frontend/repo-A"
	control := "/Users/alex/code/project/coordination"
	got := bestPointerPath(target, control)
	expected := filepath.Join("..", "..", "coordination")
	if got != expected {
		t.Errorf("nephew got %q, want %q", got, expected)
	}
}

// Preflight visibility (codex's POOL-2 follow-up): a multi-target plan where
// MULTIPLE targets are unwritable should report ALL of them, not fail on the
// first and force fix-and-retry per failure.
func TestPreparePoolPreflightCollectsAllFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0o500 writability check")
	}
	root := t.TempDir()
	makeControlRepo(t, root)

	parent := t.TempDir()
	writable := filepath.Join(parent, "writable")
	ro1 := filepath.Join(parent, "ro1")
	ro2 := filepath.Join(parent, "ro2")
	for _, d := range []string{writable, ro1, ro2} {
		mustMkdir(t, d)
	}
	if err := os.Chmod(ro1, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro2, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(ro1, 0o700)
		_ = os.Chmod(ro2, 0o700)
	})

	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{writable, ro1, ro2}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, applyErr := applyPreparePoolPlan(plan)
	if applyErr == nil {
		t.Fatal("expected preflight to fail on multiple unwritable targets")
	}
	msg := applyErr.Error()
	// Both unwritable targets should appear in the consolidated error message.
	if !strings.Contains(msg, ro1) {
		t.Errorf("preflight error should mention %s; got: %s", ro1, msg)
	}
	if !strings.Contains(msg, ro2) {
		t.Errorf("preflight error should mention %s; got: %s", ro2, msg)
	}
	if !strings.Contains(msg, "2 target(s)") {
		t.Errorf("preflight error should report count (2); got: %s", msg)
	}
}

func TestPreparePoolDryRunOutputContainsExpectedSections(t *testing.T) {
	root := t.TempDir()
	makeControlRepo(t, root)
	target := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".rehumanlabs", "loop"),
		Vendor:      "rehumanlabs",
	}
	plan, err := computePreparePoolPlan(cfg, []string{target}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	printPreparePoolPlan(&buf, plan)
	out := buf.String()

	for _, want := range []string{
		"agentchute prepare-pool plan",
		"control_repo:",
		"=== " + target + " ===",
		"pointer:",
		"wrapper:",
		"CLAUDE.md",
		"AGENTS.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\n--full output--\n%s", want, out)
		}
	}
}
