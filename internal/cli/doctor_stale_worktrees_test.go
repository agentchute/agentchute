package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// doctor_stale_worktrees_test.go — item 4 of the post-1.5.x friction
// program: a doctor WARN check for registered git worktrees that are done
// with their work (branch already merged into origin/main) but never
// removed. Offline and cheap by construction: local git plumbing only, no
// fetch, no network. Uses a real temp git repo + real `git worktree`
// invocations, since the check parses actual `git worktree list
// --porcelain` output.

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newStaleWorktreeFixture creates a real git repo with one commit on
// "main", and fakes refs/remotes/origin/main pointing at it — a local ref
// is all gitRefMergedIntoOriginMain needs, no real remote required.
func newStaleWorktreeFixture(t *testing.T) (*loop.Config, string) {
	t.Helper()
	root := t.TempDir()
	mustGit(t, root, "init", "-q", "-b", "main")
	mustGit(t, root, "config", "user.email", "test@test.invalid")
	mustGit(t, root, "config", "user.name", "test")
	mustWrite(t, filepath.Join(root, "README.md"), []byte("x"))
	mustGit(t, root, "add", "README.md")
	mustGit(t, root, "commit", "-q", "-m", "init")
	mustGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	cfg := &loop.Config{ControlRepo: root, LoopDir: filepath.Join(root, ".agentchute", "loop")}
	return cfg, root
}

func TestCheckStaleWorktreesOKWhenNoLinkedWorktrees(t *testing.T) {
	cfg, _ := newStaleWorktreeFixture(t)
	got := checkStaleWorktrees(cfg)
	if got.Severity != severityOK {
		t.Fatalf("severity = %q, want OK; msg=%q", got.Severity, got.Message)
	}
}

func TestCheckStaleWorktreesWarnsOnMergedBranch(t *testing.T) {
	cfg, root := newStaleWorktreeFixture(t)
	wt := filepath.Join(t.TempDir(), "merged-wt")
	mustGit(t, root, "worktree", "add", "-b", "merged-branch", wt, "main")
	got := checkStaleWorktrees(cfg)
	if got.Severity != severityWarn {
		t.Fatalf("severity = %q, want WARN for a worktree whose branch is already merged; msg=%q", got.Severity, got.Message)
	}
	if !strings.Contains(got.Message, wt) {
		t.Errorf("message missing the stale worktree path %q: %q", wt, got.Message)
	}
}

func TestCheckStaleWorktreesSkipsUnmergedBranch(t *testing.T) {
	cfg, root := newStaleWorktreeFixture(t)
	wt := filepath.Join(t.TempDir(), "unmerged-wt")
	mustGit(t, root, "worktree", "add", "-b", "unmerged-branch", wt, "main")
	mustWrite(t, filepath.Join(wt, "new.txt"), []byte("x"))
	mustGit(t, wt, "add", "new.txt")
	mustGit(t, wt, "commit", "-q", "-m", "extra work not on main")
	got := checkStaleWorktrees(cfg)
	if got.Severity != severityOK {
		t.Fatalf("severity = %q, want OK (unmerged work must never be flagged); msg=%q", got.Severity, got.Message)
	}
}

func TestCheckStaleWorktreesSkipsLockedWorktree(t *testing.T) {
	cfg, root := newStaleWorktreeFixture(t)
	wt := filepath.Join(t.TempDir(), "locked-wt")
	mustGit(t, root, "worktree", "add", "-b", "locked-branch", wt, "main")
	mustGit(t, root, "worktree", "lock", wt)
	got := checkStaleWorktrees(cfg)
	if got.Severity != severityOK {
		t.Fatalf("severity = %q, want OK (a locked worktree is a live lane's session and must never be flagged even if merged); msg=%q", got.Severity, got.Message)
	}
}

func TestCheckStaleWorktreesWarnsOnDetachedMergedHead(t *testing.T) {
	cfg, root := newStaleWorktreeFixture(t)
	sha := mustGit(t, root, "rev-parse", "HEAD")
	wt := filepath.Join(t.TempDir(), "review-99")
	mustGit(t, root, "worktree", "add", "--detach", wt, sha)
	got := checkStaleWorktrees(cfg)
	if got.Severity != severityWarn {
		t.Fatalf("severity = %q, want WARN for a merged detached-HEAD review checkout; msg=%q", got.Severity, got.Message)
	}
	if !strings.Contains(got.Message, "detached") {
		t.Errorf("message should note the detached form: %q", got.Message)
	}
}

func TestCheckStaleWorktreesOKWhenNotAGitRepo(t *testing.T) {
	cfg := &loop.Config{ControlRepo: t.TempDir()}
	got := checkStaleWorktrees(cfg)
	if got.Severity != severityOK {
		t.Fatalf("severity = %q, want OK (never block a diagnostic on a scan failure); msg=%q", got.Severity, got.Message)
	}
}
