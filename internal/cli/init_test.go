package cli

import (
	"bytes"
	"fmt"
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

	expectAction(t, plan, "AGENTCHUTE.md", "create v1")
	expectAction(t, plan, "CLAUDE.md", "create v30")
	expectAction(t, plan, "CODEX.md", "create v30")
	expectAction(t, plan, "GEMINI.md", "create v30")
	expectAction(t, plan, "GROK.md", "create v30")
	expectAction(t, plan, "AGENTS.md", "create v30")
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
	for _, f := range []string{"AGENTCHUTE.md", "CLAUDE.md", "CODEX.md", "GEMINI.md", "GROK.md", "AGENTS.md"} {
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
	expectAction(t, plan, "CLAUDE.md", "prepend v30")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "agentchute-enrollment v30 begin") {
		t.Errorf("CLAUDE.md missing marker after prepend:\n%s", got)
	}
	if !strings.HasSuffix(string(got), originalContent) {
		t.Errorf("CLAUDE.md original content lost after prepend")
	}
}

// Existing CLAUDE.md with a v1 marker and matching content → skip.
// renderWrapperBlock must fully substitute every template token; a leaked
// {{...}} means a token-name drift between the template and renderWrapperBlock
// (e.g. template uses {{AGENT_ID}} while the code still replaces {{AS}}), which
// silently ships literal placeholders into the concrete enrollment files.
func TestRenderWrapperBlockLeavesNoUnexpandedTokens(t *testing.T) {
	for _, w := range []struct{ id, vendor string }{
		{"claude-code", "anthropic"},
		{"codex", "openai"},
		{"gemini-cli", "google"},
		{"grok", "xai"},
	} {
		block := renderWrapperBlock(w.id, w.vendor)
		if strings.Contains(block, "{{") {
			t.Errorf("%s: rendered block contains unexpanded token(s):\n%s", w.id, block)
		}
		if !strings.Contains(block, w.id) {
			t.Errorf("%s: rendered block missing the agent id", w.id)
		}
	}
}

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
// marker at the current version to test the drift path; with the current marker, the v1
// marker exercises the older-version path.)
func TestInitReplacesDriftedV1Content(t *testing.T) {
	root := t.TempDir()
	drifted := "<!-- agentchute-enrollment v1 begin -->\nstale content that does not match canonical\n<!-- agentchute-enrollment v1 end -->\n\nMy notes.\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(drifted))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "CLAUDE.md", "replace v1→v30")
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

func TestInitUpgradesV11EnrollmentBlockToV13(t *testing.T) {
	root := t.TempDir()
	stale := "<!-- agentchute-enrollment v11 begin -->\nstale env guidance\n<!-- agentchute-enrollment v11 end -->\n\nMy notes.\n"
	mustWrite(t, filepath.Join(root, "CODEX.md"), []byte(stale))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "CODEX.md", "replace v11→v30")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, "CODEX.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "stale env guidance") {
		t.Errorf("CODEX.md still contains stale enrollment content after upgrade:\n%s", text)
	}
	if !strings.Contains(text, "AGENTCHUTE_AGENT_ID") {
		t.Errorf("CODEX.md missing env identity guidance after upgrade:\n%s", text)
	}
	if !strings.Contains(text, "My notes.") {
		t.Errorf("CODEX.md lost preserved user content after upgrade:\n%s", text)
	}
}

// Existing file with a future version marker → leave alone with warning.
func TestInitLeavesNewerVersionAlone(t *testing.T) {
	root := t.TempDir()
	future := "<!-- agentchute-enrollment v31 begin -->\nfuture\n<!-- agentchute-enrollment v31 end -->\n"
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

// TestInitUpgradesV29EnrollmentToV30 is the enrollment-bump lock (introduced
// by the guard-latch-livelock fix as brief test case 12, and re-pinned at
// every bump since): an older repo file re-renders its marked region to the
// current template — reported as a clean version upgrade ("replace
// v29→v30"), a distinct action string from planEnrollmentFile's same-version
// "replace vN drift" branch, so the two never get confused in plan output.
// TestInitLeavesNewerVersionAlone (this file, now pinned to a v31 fixture)
// already proves the complementary direction: an older binary encountering a
// marker newer than its own enrollmentVersion skips with a warning rather than
// rewriting — the same generic branch a literal v29 binary against a v30 file
// would take, which cannot be built as a second binary within this same test
// run.
func TestInitUpgradesV29EnrollmentToV30(t *testing.T) {
	root := t.TempDir()
	old := "<!-- agentchute-enrollment v29 begin -->\nstale v29 content\n<!-- agentchute-enrollment v29 end -->\n\nMy notes.\n"
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), []byte(old))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Target != "CLAUDE.md" {
			continue
		}
		if a.Action != "replace v29→v30" {
			t.Errorf("action = %q, want %q", a.Action, "replace v29→v30")
		}
		if strings.Contains(a.Detail, "drift") {
			t.Errorf("a clean older-version upgrade must not be reported as same-version drift: %+v", a)
		}
	}
	applyAll(t, plan)
	got, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "agentchute-enrollment v30 begin") {
		t.Errorf("CLAUDE.md was not upgraded to v30:\n%s", got)
	}
	if strings.Contains(string(got), "stale v29 content") {
		t.Errorf("CLAUDE.md still contains stale v29 content after upgrade:\n%s", got)
	}
	if !strings.Contains(string(got), "My notes.") {
		t.Errorf("CLAUDE.md lost preserved user content after upgrade:\n%s", got)
	}
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
// breaks the §5 reference in the enrollment block).
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

// codex review on PR #131 [P1]: a symlinked AGENTCHUTE.md must never be read
// or written through — the version-compare-and-replace path would otherwise
// follow it and mutate whatever it points at, outside the discovered control
// repo. Errors closed, and the external target is untouched.
func TestInitRefusesSymlinkedAgentchuteMd(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "external-spec.md")
	targetContent := []byte("# AGENTCHUTE.md\n\nan external file this repo must never touch\n")
	mustWrite(t, target, targetContent)

	link := filepath.Join(root, "AGENTCHUTE.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected a symlink refusal, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention the symlink refusal: %v", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(targetContent) {
		t.Errorf("external symlink target was modified; got %q, want unchanged %q", got, targetContent)
	}
}

// sibling-path-gate-parity follow-up: the same symlink guard proven above for
// AGENTCHUTE.md must also cover planEnrollmentFile's targets (CLAUDE.md/
// CODEX.md/GEMINI.md/GROK.md/AGENTS.md) and planGitignore's .gitignore — all
// three share the identical read-then-maybe-write-through-a-symlink shape.
func TestInitRefusesSymlinkedEnrollmentFile(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "external.md")
	targetContent := []byte("an external file this repo must never touch\n")
	mustWrite(t, target, targetContent)

	link := filepath.Join(root, "CLAUDE.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := computeInitPlan(root, "agentchute", false)
	if err == nil {
		t.Fatal("expected a symlink refusal, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention the symlink refusal: %v", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(targetContent) {
		t.Errorf("external symlink target was modified; got %q, want unchanged %q", got, targetContent)
	}
}

func TestInitRefusesSymlinkedGitignore(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".git"))
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "external.gitignore")
	targetContent := []byte("an external file this repo must never touch\n")
	mustWrite(t, target, targetContent)

	link := filepath.Join(root, ".gitignore")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := computeInitPlan(root, "agentchute", true)
	if err == nil {
		t.Fatal("expected a symlink refusal, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention the symlink refusal: %v", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(targetContent) {
		t.Errorf("external symlink target was modified; got %q, want unchanged %q", got, targetContent)
	}
}

// 2026-08-11 hook-refresh-reliability follow-up, finding 3: planSpecFile now
// gets the same version-compare-and-replace treatment planEnrollmentFile
// already has, instead of skipping unconditionally once recognizable.

// A recognizable AGENTCHUTE.md with no agentchute-spec marker at all predates
// this versioning scheme entirely — treated as older than any version, so it
// is replaced with the current embedded content (matching planEnrollmentFile's
// own "no marker — write current" branch for CLAUDE.md/AGENTS.md/etc).
func TestInitReplacesLegacySpecWithNoMarker(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# AGENTCHUTE.md\n\nan old spec from before versioning existed\n"))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "AGENTCHUTE.md", "replace legacy→v1")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, "AGENTCHUTE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != embeddedSpecContent {
		t.Errorf("legacy AGENTCHUTE.md was not replaced with the embedded spec")
	}
}

// A marker at the current version whose content byte-matches the embedded
// spec exactly → no-op, no Apply function (planHasMutations-safe).
func TestInitSkipsWhenSpecMarkerCurrentAndMatches(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte(embeddedSpecContent))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Target != "AGENTCHUTE.md" {
			continue
		}
		if a.Action != "skip" {
			t.Errorf("action = %q, want skip", a.Action)
		}
		if a.Apply != nil {
			t.Errorf("an already-current spec must be a no-op")
		}
	}
}

// A marker at the current version whose content has DRIFTED from the
// embedded spec (e.g. hand-edited) → replace per the same ruling
// planEnrollmentFile already follows for its own marked block.
func TestInitReplacesDriftedCurrentVersionSpec(t *testing.T) {
	root := t.TempDir()
	drifted := "# AGENTCHUTE.md\n<!-- agentchute-spec v1 -->\n\nhand-edited drift, not the canonical body\n"
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte(drifted))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	expectAction(t, plan, "AGENTCHUTE.md", "replace v1 drift")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, "AGENTCHUTE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != embeddedSpecContent {
		t.Errorf("drifted AGENTCHUTE.md was not replaced with the embedded spec")
	}
}

// A marker newer than this binary's embedded specVersion → leave alone with
// a warning, never overwrite a deliberately future-dated spec.
func TestInitLeavesNewerSpecVersionAlone(t *testing.T) {
	root := t.TempDir()
	future := fmt.Sprintf("# AGENTCHUTE.md\n<!-- agentchute-spec v%d -->\n\nfrom a newer binary\n", specVersion+1)
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte(future))

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range plan.Actions {
		if a.Target == "AGENTCHUTE.md" {
			if a.Action != "skip (warn)" {
				t.Errorf("expected skip (warn), got %q", a.Action)
			}
			if a.Apply != nil {
				t.Errorf("future-version action should be a no-op")
			}
			return
		}
	}
	t.Fatal("AGENTCHUTE.md action not found in plan")
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
	expectAction(t, plan, ".gitignore", "create v3")
	applyAll(t, plan)

	got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), gitignoreBegin) {
		t.Errorf(".gitignore missing gitignore begin marker:\n%s", got)
	}
	if !strings.Contains(string(got), gitignoreEnd) {
		t.Errorf(".gitignore missing gitignore end marker:\n%s", got)
	}
	if !strings.Contains(string(got), ".agentchute/loop/agents/*.md") {
		t.Errorf(".gitignore missing namespace stanza:\n%s", got)
	}
}

// Vendor namespacing was removed (simple-again): init always scaffolds the fixed
// .agentchute/loop, there is no --namespace override, and the multi-namespace
// ambiguity guard / legacy migration are gone. Discovery resolves the one fixed
// loop dir; coexisting foreign dotdirs are simply ignored rather than refused.
func TestInitScaffoldsFixedAgentchuteLoopIgnoringForeignDotdir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".agentchute", "loop"), 0o700); err != nil {
		t.Fatal(err)
	}

	plan, err := computeInitPlan(root, "agentchute", false)
	if err != nil {
		t.Fatalf("init must not refuse on a coexisting foreign dotdir: %v", err)
	}
	for _, sub := range []string{"agents", "inbox", "archive", "malformed"} {
		expectAction(t, plan, ".agentchute/loop/"+sub, "mkdir 0700")
	}
	if !strings.Contains(plan.GitignoreStanza, ".agentchute/loop/") {
		t.Errorf("gitignore stanza missing fixed namespace:\n%s", plan.GitignoreStanza)
	}
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
// Gitignore version is currently 3; v4 here is "newer than current."
func TestInitGitignoreSkipsNewerVersion(t *testing.T) {
	root := t.TempDir()
	future := "# agentchute-gitignore v4 begin\nstuff\n# agentchute-gitignore v4 end\n"
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
