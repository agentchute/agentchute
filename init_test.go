package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fresh init in an empty directory should plan every action as new/create.
func TestInitFreshEmpty(t *testing.T) {
	root := t.TempDir()
	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}

	expectAction(t, plan, "AGENTCHUTE.md", "write")
	expectAction(t, plan, "CLAUDE.md", "create v8")
	expectAction(t, plan, "CODEX.md", "create v8")
	expectAction(t, plan, "GEMINI.md", "create v8")
	expectAction(t, plan, "AGENTS.md", "create v8")
	expectAction(t, plan, ".gitignore", "skip") // not in git
	expectAction(t, plan, ".agentchute/loop/agents", "mkdir 0700")
	expectAction(t, plan, ".agentchute/loop/inbox", "mkdir 0700")
	expectAction(t, plan, ".agentchute/loop/archive", "mkdir 0700")
	expectAction(t, plan, ".agentchute/loop/malformed", "mkdir 0700")
}

// Applying a fresh plan should leave a tree we can re-plan as "nothing to do".
func TestInitFreshAppliedThenReplanIsNoOp(t *testing.T) {
	root := t.TempDir()
	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	applyAll(t, plan)

	// Sanity: files exist with expected content.
	for _, f := range []string{"AGENTCHUTE.md", "CLAUDE.md", "CODEX.md", "GEMINI.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("expected %s to exist: %v", f, err)
		}
	}
	for _, d := range []string{"agents", "inbox", "archive", "malformed"} {
		info, err := os.Stat(filepath.Join(root, ".agentchute", "loop", d))
		if err != nil {
			t.Fatalf("expected loop/%s: %v", d, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("loop/%s mode = %o, want 0700", d, info.Mode().Perm())
		}
	}

	// Replan: every action should be a "skip"-flavored no-op (no Apply fn).
	plan2, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	if planHasMutations(plan2) {
		var muts []string
		for _, a := range plan2.Actions {
			if a.Apply != nil {
				muts = append(muts, a.Target+"="+a.Action)
			}
		}
		t.Fatalf("expected no-op replan, got mutations: %v", muts)
	}
}

// Existing CLAUDE.md without a marker → prepend the block.
func TestInitPrependsBlockWhenNoMarker(t *testing.T) {
	root := t.TempDir()
	originalContent := "# CLAUDE.md\n\nMy existing notes.\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(originalContent))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "CLAUDE.md", "prepend v8")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "agentchute-enrollment v8 begin") {
		t.Errorf("CLAUDE.md missing marker after prepend:\n%s", got)
	}
	if !strings.HasSuffix(string(got), originalContent) {
		t.Errorf("CLAUDE.md original content lost after prepend")
	}
}

// Existing CLAUDE.md with a v1 marker and matching content → skip.
func TestInitSkipsWhenMarkerCurrentAndMatches(t *testing.T) {
	root := t.TempDir()
	block := renderWrapperBlock("claude-code", "anthropic")
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(block+"\nMy notes.\n"))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "CLAUDE.md", "skip")
}

// Existing CLAUDE.md with an OLDER agentchute-enrollment marker → replace marked
// region with the current version. (Prior to v4, this test used an older
// marker at the current version to test the drift path; with v4 current, the v1
// marker exercises the older-version path.)
func TestInitReplacesDriftedV1Content(t *testing.T) {
	root := t.TempDir()
	drifted := "<!-- agentchute-enrollment v1 begin -->\nstale content that does not match canonical\n<!-- agentchute-enrollment v1 end -->\n\nMy notes.\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(drifted))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "CLAUDE.md", "replace v1→v8")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "stale content") {
		t.Errorf("CLAUDE.md still contains drifted content after replace:\n%s", got)
	}
	if !strings.Contains(string(got), "My notes.") {
		t.Errorf("CLAUDE.md lost preserved user content after replace:\n%s", got)
	}
}

// Existing file with a future version marker → leave alone with warning.
func TestInitLeavesNewerVersionAlone(t *testing.T) {
	root := t.TempDir()
	future := "<!-- agentchute-enrollment v9 begin -->\nfuture\n<!-- agentchute-enrollment v9 end -->\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(future))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Target == "CLAUDE.md" {
			if a.Action != "skip (warn)" {
				t.Errorf("expected skip (warn), got %q", a.Action)
			}
			if a.Apply != nil {
				t.Errorf("future-version action should be a no-op")
			}
			return
		}
	}
	t.Fatal("CLAUDE.md action not found in plan")
}

// Multiple agentchute-enrollment markers → hard fail at plan time.
func TestInitFailsOnMultipleMarkers(t *testing.T) {
	root := t.TempDir()
	twice := "<!-- agentchute-enrollment v1 begin -->\nA\n<!-- agentchute-enrollment v1 end -->\n<!-- agentchute-enrollment v1 begin -->\nB\n<!-- agentchute-enrollment v1 end -->\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(twice))

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected multiple-marker error, got nil")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("error message should mention 'multiple': %v", err)
	}
}

// Malformed marker (begin without matching end) → hard fail.
func TestInitFailsOnMalformedMarker(t *testing.T) {
	root := t.TempDir()
	broken := "<!-- agentchute-enrollment v1 begin -->\norphan begin, no end\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(broken))

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected malformed-marker error, got nil")
	}
}

// AGENTCHUTE.md exists with non-agentchute content → hard fail (mismatched spec
// breaks the §7.2 reference in the enrollment block).
func TestInitFailsOnUnrecognizableAgentchuteMd(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Something Else\n\nNot the agentchute spec.\n"))

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected non-agentchute-spec error, got nil")
	}
	if !strings.Contains(err.Error(), "does not look like an agentchute spec") {
		t.Errorf("error should mention recognizability: %v", err)
	}
}

// Existing dir at 0755 → chmod 0700 action.
func TestInitChmodsExistingLoopDir(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".agentchute", "loop", "agents")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, ".agentchute/loop/agents", "chmod 0700")

	applyAll(t, plan)
	info, err := os.Stat(stale)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("loop dir mode after apply = %o, want 0700", info.Mode().Perm())
	}
}

// In a git worktree, missing .gitignore is created with the stanza.
func TestInitCreatesGitignoreInGitWorktree(t *testing.T) {
	root := t.TempDir()
	plan, err := computeInitPlan(root, "agentchute", true)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, ".gitignore", "create v2")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), gitignoreBeginV1) {
		t.Errorf(".gitignore missing gitignore begin marker:\n%s", got)
	}
	if !strings.Contains(string(got), gitignoreEndV1) {
		t.Errorf(".gitignore missing gitignore end marker:\n%s", got)
	}
	if !strings.Contains(string(got), ".agentchute/loop/agents/*.md") {
		t.Errorf(".gitignore missing namespace stanza:\n%s", got)
	}
}

// --namespace override changes both the loop dir and the gitignore stanza.
func TestInitNamespaceOverride(t *testing.T) {
	root := t.TempDir()
	plan, err := computeInitPlan(root, "acmecorp", true)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, ".acmecorp/loop/agents", "mkdir 0700")
	expectAction(t, plan, ".acmecorp/loop/inbox", "mkdir 0700")
	expectAction(t, plan, ".acmecorp/loop/archive", "mkdir 0700")
	expectAction(t, plan, ".acmecorp/loop/malformed", "mkdir 0700")
	if !strings.Contains(plan.GitignoreStanza, ".acmecorp/loop/") {
		t.Errorf("gitignore stanza did not pick up override:\n%s", plan.GitignoreStanza)
	}
}

func TestInitFailsBeforeCreatingSecondLoopNamespace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".examplecorp", "loop"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected conflicting loop namespace error, got nil")
	}
	for _, want := range []string{".examplecorp/loop", ".agentchute/loop", "multiple loop dirs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestInitFailsClearlyWhenTwoLoopNamespacesAlreadyExist(t *testing.T) {
	root := t.TempDir()
	for _, namespace := range []string{".agentchute", ".examplecorp"} {
		if err := os.MkdirAll(filepath.Join(root, namespace, "loop"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected existing multiple loop namespace error, got nil")
	}
	for _, want := range []string{".agentchute/loop", ".examplecorp/loop", "AGENTCHUTE_LOOP_DIR", "--loop-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestInitNamespaceOverrideCanManageExistingLoopNamespace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".examplecorp", "loop"), 0o700); err != nil {
		t.Fatal(err)
	}

	plan, err := computeInitPlan(root, "examplecorp", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, ".examplecorp/loop/agents", "mkdir 0700")
	expectAction(t, plan, ".examplecorp/loop/inbox", "mkdir 0700")
	expectAction(t, plan, ".examplecorp/loop/archive", "mkdir 0700")
	expectAction(t, plan, ".examplecorp/loop/malformed", "mkdir 0700")
}

// Symlinked namespace dir → hard fail at plan time. Otherwise os.MkdirAll
// on a missing loop subdir would follow the symlink and create files outside
// the project.
func TestInitFailsOnSymlinkedNamespaceDir(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, ".agentchute")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention symlink: %v", err)
	}
}

// Existing loop dir that is itself a symlink → hard fail at plan time.
func TestInitFailsOnSymlinkedLoopSubdir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agentchute", "loop"), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, ".agentchute", "loop", "inbox")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected symlinked-subdir rejection, got nil")
	}
}

// .gitignore with multiple agentchute-gitignore markers → fail.
func TestInitGitignoreFailsOnMultipleMarkers(t *testing.T) {
	root := t.TempDir()
	doubled := "# agentchute-gitignore v1 begin\nfoo\n# agentchute-gitignore v1 end\n# agentchute-gitignore v1 begin\nbar\n# agentchute-gitignore v1 end\n"
	mustWrite(t, filepath.Join(root, ".gitignore"), []byte(doubled))

	_, err := computeInitPlan(root, "agentchute", true)
	if err == nil {
		t.Fatal("expected multiple-marker rejection, got nil")
	}
	if !strings.Contains(err.Error(), "multiple") {
		t.Errorf("error should mention 'multiple': %v", err)
	}
}

// .gitignore with malformed marker (begin without end) → fail.
func TestInitGitignoreFailsOnMalformedMarker(t *testing.T) {
	root := t.TempDir()
	broken := "# agentchute-gitignore v1 begin\norphan\n"
	mustWrite(t, filepath.Join(root, ".gitignore"), []byte(broken))

	_, err := computeInitPlan(root, "agentchute", true)
	if err == nil {
		t.Fatal("expected malformed-marker rejection, got nil")
	}
}

// .gitignore with future-version marker → skip with warn (don't downgrade).
// Gitignore version is currently 1; v2 here is "newer than current."
func TestInitGitignoreSkipsNewerVersion(t *testing.T) {
	root := t.TempDir()
	future := "# agentchute-gitignore v3 begin\nstuff\n# agentchute-gitignore v3 end\n"
	mustWrite(t, filepath.Join(root, ".gitignore"), []byte(future))

	plan, err := computeInitPlan(root, "agentchute", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Target == ".gitignore" {
			if a.Action != "skip (warn)" {
				t.Errorf(".gitignore action = %q, want skip (warn)", a.Action)
			}
			if a.Apply != nil {
				t.Errorf("newer-version .gitignore should be no-op")
			}
			return
		}
	}
	t.Fatal(".gitignore not in plan")
}

// promptConfirm returns true for "y" / "yes" (any case), false otherwise.
func TestPromptConfirm(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"\n", false},
		{"n\n", false},
		{"maybe\n", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := promptConfirm(strings.NewReader(c.input), &out, "? ")
		if err != nil {
			t.Errorf("promptConfirm(%q) errored: %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("promptConfirm(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

func expectAction(t *testing.T, plan initPlan, target, action string) {
	t.Helper()
	for _, a := range plan.Actions {
		if a.Target == target {
			if a.Action != action {
				t.Errorf("target %s: action = %q, want %q (detail: %s)", target, a.Action, action, a.Detail)
			}
			return
		}
	}
	t.Errorf("target %s not found in plan; actions: %v", target, planTargets(plan))
}

func planTargets(plan initPlan) []string {
	out := make([]string, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		out = append(out, a.Target+":"+a.Action)
	}
	return out
}

func applyAll(t *testing.T, plan initPlan) {
	t.Helper()
	for _, a := range plan.Actions {
		if a.Apply == nil {
			continue
		}
		if err := a.Apply(); err != nil {
			t.Fatalf("apply %s: %v", a.Target, err)
		}
	}
}

// Suppress unused-import warning if go test gets clever about it; io is used
// indirectly via promptConfirm via bytes.Buffer.
var _ = io.Discard
