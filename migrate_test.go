package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkLoop scaffolds .<ns>/loop under root. When live, it also writes a real
// registration so loopHasState reports true. A README is always written to
// prove scaffold artifacts are not counted as live state.
func mkLoop(t *testing.T, root, ns string, live bool) string {
	t.Helper()
	loopDir := filepath.Join(root, "."+ns, "loop")
	for _, sub := range []string{"agents", "inbox", "archive", "malformed"} {
		if err := os.MkdirAll(filepath.Join(loopDir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(loopDir, "agents", "README.md"), []byte("scaffold"), 0o600); err != nil {
		t.Fatal(err)
	}
	if live {
		if err := os.WriteFile(filepath.Join(loopDir, "agents", "claude-code.md"), []byte("registration"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(loopDir, "inbox", "claude-code"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(loopDir, "inbox", "claude-code", "msg-1.md"), []byte("mail"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return loopDir
}

func applyMigration(t *testing.T, root string) (*initAction, string, error) {
	t.Helper()
	action, rel, err := planLegacyMigration(root, "agentchute")
	if err != nil {
		return nil, rel, err
	}
	if action != nil && action.Apply != nil {
		if err := action.Apply(); err != nil {
			t.Fatalf("apply migration: %v", err)
		}
	}
	return action, rel, nil
}

func fileContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", path, b, want)
	}
}

// Idempotency: no legacy dir → no action, no error.
func TestMigrateNoLegacyIsNoop(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, "agentchute", true)
	action, _, err := planLegacyMigration(root, "agentchute")
	if err != nil {
		t.Fatal(err)
	}
	if action != nil {
		t.Errorf("expected no action when no legacy dir, got %+v", action)
	}
}

// Case A1: legacy only, canonical absent → atomic rename of the dotdir.
func TestMigrateRenamesWholeNamespace(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, legacyNamespace, true)

	action, _, err := applyMigration(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Action != "migrate legacy namespace" {
		t.Fatalf("expected rename action, got %+v", action)
	}
	fileContains(t, filepath.Join(root, ".agentchute", "loop", "agents", "claude-code.md"), "registration")
	fileContains(t, filepath.Join(root, ".agentchute", "loop", "inbox", "claude-code", "msg-1.md"), "mail")
	if pathExists(filepath.Join(root, "."+legacyNamespace)) {
		t.Errorf("legacy dotdir should be gone after rename")
	}
	assertSingleLoop(t, root)
}

// Case A2: canonical dotdir exists but has no loop → move the loop in.
func TestMigrateMovesLoopWhenCanonDirExistsWithoutLoop(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, legacyNamespace, true)
	if err := os.MkdirAll(filepath.Join(root, ".agentchute"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := applyMigration(t, root); err != nil {
		t.Fatal(err)
	}
	fileContains(t, filepath.Join(root, ".agentchute", "loop", "agents", "claude-code.md"), "registration")
	if dirExists(filepath.Join(root, "."+legacyNamespace, "loop")) {
		t.Errorf("legacy loop should be gone")
	}
	assertSingleLoop(t, root)
}

// Case B: canonical live, legacy scaffold-only → legacy moved aside, NOT rediscovered.
func TestMigrateRetiresLegacyScaffold(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, legacyNamespace, false) // scaffold only
	mkLoop(t, root, "agentchute", true)     // live canonical

	action, _, err := applyMigration(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Action != "retire legacy scaffold" {
		t.Fatalf("expected retire action, got %+v", action)
	}
	// canonical state untouched
	fileContains(t, filepath.Join(root, ".agentchute", "loop", "agents", "claude-code.md"), "registration")
	if dirExists(filepath.Join(root, "."+legacyNamespace, "loop")) {
		t.Errorf("legacy loop should be moved aside")
	}
	// The backup must NOT be rediscoverable as a vendor loop.
	assertSingleLoop(t, root)
}

// Case C: canonical scaffold-only, legacy live → canonical backed up, legacy moved in.
func TestMigratePromotesLegacyOverEmptyCanonical(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, legacyNamespace, true) // live legacy
	mkLoop(t, root, "agentchute", false)   // scaffold canonical

	action, _, err := applyMigration(t, root)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Action != "migrate legacy loop" {
		t.Fatalf("expected migrate action, got %+v", action)
	}
	fileContains(t, filepath.Join(root, ".agentchute", "loop", "agents", "claude-code.md"), "registration")
	if dirExists(filepath.Join(root, "."+legacyNamespace, "loop")) {
		t.Errorf("legacy loop should be gone")
	}
	assertSingleLoop(t, root)
}

// Case D: both live → refuse, no mutation.
func TestMigrateRefusesWhenBothLive(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, legacyNamespace, true)
	mkLoop(t, root, "agentchute", true)

	action, _, err := planLegacyMigration(root, "agentchute")
	if err == nil {
		t.Fatalf("expected refuse error, got action %+v", action)
	}
	if !strings.Contains(err.Error(), "live coordination state") {
		t.Errorf("error should explain the live-both conflict, got: %v", err)
	}
	// Nothing moved.
	if !dirExists(filepath.Join(root, "."+legacyNamespace, "loop")) || !dirExists(filepath.Join(root, ".agentchute", "loop")) {
		t.Errorf("refuse must not mutate either loop dir")
	}
}

// Symlinked legacy namespace is rejected.
func TestMigrateRejectsSymlinkedNamespace(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realDir, "loop", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "."+legacyNamespace)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, _, err := planLegacyMigration(root, "agentchute"); err == nil {
		t.Errorf("expected symlink rejection")
	}
}

// computeInitPlan must not trip the ambiguity guard for a safe migration case,
// and must include the migration action in the plan.
func TestComputeInitPlanMigratesSafeCoexistence(t *testing.T) {
	root := t.TempDir()
	mkLoop(t, root, legacyNamespace, false) // scaffold legacy
	mkLoop(t, root, "agentchute", true)     // live canonical

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatalf("computeInitPlan should not error for safe migration, got: %v", err)
	}
	found := false
	for _, a := range plan.Actions {
		if strings.HasPrefix(a.Action, "retire legacy") || strings.HasPrefix(a.Action, "migrate legacy") {
			found = true
		}
	}
	if !found {
		t.Errorf("plan missing migration action: %+v", plan.Actions)
	}
}

// assertSingleLoop verifies exactly one discoverable .<ns>/loop remains — the
// canonical one — proving backups/aside-moves are not auto-discovered.
func assertSingleLoop(t *testing.T, root string) {
	t.Helper()
	loops, err := findInitLoopDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 1 || loops[0] != ".agentchute/loop" {
		t.Errorf("expected exactly [.agentchute/loop], got %v", loops)
	}
}
