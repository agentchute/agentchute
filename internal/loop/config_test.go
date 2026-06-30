package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverFindsSingleLoopDirFromCwd(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
	mustMkdir(t, filepath.Join(root, "sub", "dir"))

	cfg, err := Discover(DiscoverOpts{Cwd: filepath.Join(root, "sub", "dir")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlRepo != root {
		t.Fatalf("ControlRepo = %q, want %q", cfg.ControlRepo, root)
	}
	wantLoop := filepath.Join(root, ".examplecorp", "loop")
	if cfg.LoopDir != wantLoop {
		t.Fatalf("LoopDir = %q, want %q", cfg.LoopDir, wantLoop)
	}
	if cfg.Vendor != "examplecorp" {
		t.Fatalf("Vendor = %q, want examplecorp", cfg.Vendor)
	}
}

func TestDiscoverRequiresLoopDirWhenMultipleExist(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	mustMkdir(t, filepath.Join(root, ".one", "loop"))
	mustMkdir(t, filepath.Join(root, ".two", "loop"))

	_, err := Discover(DiscoverOpts{Cwd: root})
	if err == nil {
		t.Fatal("expected multiple-loop-dir error")
	}

	cfg, err := Discover(DiscoverOpts{Cwd: root, LoopDirFlag: ".two/loop"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vendor != "two" {
		t.Fatalf("Vendor = %q, want two", cfg.Vendor)
	}
}

// Flag wins over env per the AGENTCHUTE.md §4 cascade (most-explicit-first).
func TestDiscoverFlagLoopDirBeatsEnv(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	mustMkdir(t, filepath.Join(root, ".flag", "loop"))
	mustMkdir(t, filepath.Join(root, ".env", "loop"))

	cfg, err := Discover(DiscoverOpts{
		Cwd:         root,
		LoopDirFlag: ".flag/loop",
		EnvLoopDir:  ".env/loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vendor != "flag" {
		t.Fatalf("Vendor = %q, want flag (--loop-dir wins over env)", cfg.Vendor)
	}
	if cfg.LoopDirOrigin != "flag" {
		t.Fatalf("LoopDirOrigin = %q, want flag", cfg.LoopDirOrigin)
	}
}

func TestDiscoverFallsBackToEnvControlRepo(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

	cfg, err := Discover(DiscoverOpts{Cwd: outside, EnvControlRepo: root})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlRepo != root {
		t.Fatalf("ControlRepo = %q, want %q", cfg.ControlRepo, root)
	}
}

func TestDiscoverFallsBackToEnvWhenCwdRepoHasNoLoopDir(t *testing.T) {
	control := t.TempDir()
	cwdRepo := t.TempDir()
	mustWrite(t, filepath.Join(control, "AGENTCHUTE.md"), []byte("# spec\n"))
	mustMkdir(t, filepath.Join(control, ".examplecorp", "loop"))
	mustWrite(t, filepath.Join(cwdRepo, "AGENTCHUTE.md"), []byte("# other spec\n"))

	cfg, err := Discover(DiscoverOpts{Cwd: cwdRepo, EnvControlRepo: control})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlRepo != control {
		t.Fatalf("ControlRepo = %q, want %q", cfg.ControlRepo, control)
	}
}

func TestDirExistsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	mustMkdir(t, target)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if dirExists(link) {
		t.Fatal("dirExists accepted symlink to directory")
	}
}

func TestDiscoverRejectsSymlinkLoopDirFlag(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	target := filepath.Join(root, ".real", "loop")
	link := filepath.Join(root, ".link", "loop")
	mustMkdir(t, target)
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := Discover(DiscoverOpts{Cwd: root, LoopDirFlag: ".link/loop"}); err == nil {
		t.Fatal("expected symlink loop-dir flag rejection")
	}
}

func TestDiscoverRejectsSymlinkEnvLoopDir(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# spec\n"))
	target := filepath.Join(root, ".real", "loop")
	link := filepath.Join(root, ".link", "loop")
	mustMkdir(t, target)
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := Discover(DiscoverOpts{Cwd: root, EnvLoopDir: ".link/loop"}); err == nil {
		t.Fatal("expected symlink env loop-dir rejection")
	}
}

func TestEnsurePrivateDirTightensExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "live")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
}

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	mustMkdir(t, target)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := ensurePrivateDir(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerSocketPathShortStaysInState(t *testing.T) {
	cfg := &Config{
		ControlRepo: "/r",
		LoopDir:     "/r/.x/loop",
		Vendor:      "x",
	}
	got := cfg.RunnerSocketPath("codex")
	want := filepath.Join(cfg.AgentStateDir("codex"), "runner.sock")
	if got != want {
		t.Fatalf("short in-state RunnerSocketPath = %q, want %q", got, want)
	}
}

func TestRunnerSocketPathLongFallsToPerUserTempDir(t *testing.T) {
	// A loop dir long enough that the in-state socket path crosses the 100-char
	// trigger, forcing the temp fallback.
	longRoot := filepath.Join("/", strings.Repeat("deep-directory-segment/", 6), "control-repo")
	cfg := &Config{
		ControlRepo: longRoot,
		LoopDir:     filepath.Join(longRoot, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
	got := cfg.RunnerSocketPath("codex-agentchute")
	tempDir := filepath.Join(os.TempDir(), "agentchute-run-"+currentUID())
	if filepath.Dir(got) != tempDir {
		t.Fatalf("long-path RunnerSocketPath dir = %q, want per-user temp dir %q", filepath.Dir(got), tempDir)
	}
	if !strings.HasSuffix(got, "-codex-agentchute.sock") {
		t.Fatalf("temp socket path %q should end with -<agentID>.sock", got)
	}
	// Per-user: the directory name must carry the current uid, so a second
	// local user gets a different directory.
	if !strings.Contains(filepath.Base(tempDir), currentUID()) {
		t.Fatalf("temp dir %q is not per-user (missing uid)", tempDir)
	}
}

// Pull-only (Gate 6c): TestRunnerWakeTargetOwnedBy was removed. The runner
// receive socket and the wake_target recipient-binding check
// (RunnerWakeTargetOwnedBy / RunnerWakeTarget) it exercised no longer exist —
// registrations publish no wake target.
