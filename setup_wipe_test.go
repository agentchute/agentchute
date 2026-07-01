package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// newWipeTestRepo builds a control repo with a canonical .agentchute/loop and
// returns the root + discovered config. Runtime dirs are created empty; callers
// populate what they need.
func newWipeTestRepo(t *testing.T) (string, *loop.Config) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	loopDir := filepath.Join(root, ".agentchute", "loop")
	for _, sub := range []string{"agents", "inbox", "archive", "malformed", "live", "state"} {
		mustMkdir(t, filepath.Join(loopDir, sub))
	}
	cfg, err := loop.Discover(loop.DiscoverOpts{ControlRepoFlag: root, Cwd: root})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return root, cfg
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("expected %s to be gone", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("lstat %s: %v", path, err)
	}
}

// ---------- path guards ----------

func TestValidateWipePathsRejectsEmpty(t *testing.T) {
	if err := validateWipePaths("", "/x/.agentchute/loop"); err == nil {
		t.Fatal("empty control repo must be rejected")
	}
	if err := validateWipePaths("/x", ""); err == nil {
		t.Fatal("empty loop dir must be rejected")
	}
}

func TestValidateWipePathsRejectsRootAndDegenerate(t *testing.T) {
	root, _ := newWipeTestRepo(t)
	for _, ld := range []string{"/", ".", root} {
		if err := validateWipePaths(root, ld); err == nil {
			t.Fatalf("loop dir %q must be rejected", ld)
		}
	}
}

func TestValidateWipePathsRejectsNonCanonicalLoopOutsideRepo(t *testing.T) {
	root, _ := newWipeTestRepo(t)
	// A loop dir outside the control repo (a sibling tmp dir) is non-canonical.
	other := t.TempDir()
	outside := filepath.Join(other, ".agentchute", "loop")
	mustMkdir(t, outside)
	if err := validateWipePaths(root, outside); err == nil {
		t.Fatal("loop dir outside the control repo must be rejected")
	}
	// A loop dir inside the repo but not at .agentchute/loop is non-canonical.
	nonCanon := filepath.Join(root, ".other", "loop")
	mustMkdir(t, nonCanon)
	if err := validateWipePaths(root, nonCanon); err == nil {
		t.Fatal("non-canonical in-repo loop dir must be rejected")
	}
}

func TestValidateWipePathsRejectsSymlinkedLoop(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	// Replace the loop dir with a symlink to a real dir elsewhere.
	if err := os.RemoveAll(cfg.LoopDir); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, cfg.LoopDir); err != nil {
		t.Fatal(err)
	}
	if err := validateWipePaths(root, cfg.LoopDir); err == nil {
		t.Fatal("symlinked loop dir must be rejected")
	}
}

func TestValidateWipePathsRejectsSymlinkedTopLevelRuntimeDir(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	inbox := filepath.Join(cfg.LoopDir, "inbox")
	if err := os.RemoveAll(inbox); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), inbox); err != nil {
		t.Fatal(err)
	}
	if err := validateWipePaths(root, cfg.LoopDir); err == nil {
		t.Fatal("symlinked top-level runtime dir must be rejected")
	}
}

func TestValidateWipePathsAcceptsCanonical(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	if err := validateWipePaths(root, cfg.LoopDir); err != nil {
		t.Fatalf("canonical loop must validate: %v", err)
	}
}

// ---------- plan + execution ----------

// populateRuntime fills the loop with runtime state + scaffold and returns the
// scaffold paths that must survive a wipe.
func populateRuntime(t *testing.T, cfg *loop.Config) (survive, gone []string) {
	t.Helper()
	ld := cfg.LoopDir
	// scaffold (must survive)
	survive = []string{
		filepath.Join(ld, "README.md"),
		filepath.Join(ld, "agents", "README.md"),
		filepath.Join(ld, "agents", "claude-code.example.md"),
		filepath.Join(ld, "state", "setup.json"),
	}
	for _, p := range survive {
		mustWrite(t, p, []byte("scaffold"))
	}
	// runtime (must be gone)
	gone = []string{
		filepath.Join(ld, "agents", "claude-code.md"),
		filepath.Join(ld, "inbox", "claude-code", "msg.md"),
		filepath.Join(ld, "inbox", "claude-code", ".claimed", "c.md"),
		filepath.Join(ld, "archive", "old.md"),
		filepath.Join(ld, "malformed", "bad.md"),
		filepath.Join(ld, "live", "claude-code.live"),
		filepath.Join(ld, "state", "claude-code", "poller.json"),
		filepath.Join(ld, "watchdog.log"),
		filepath.Join(ld, "poller.log"),
		filepath.Join(ld, "scratch-abc", "x"),
	}
	for _, p := range gone {
		mustWrite(t, p, []byte("runtime"))
	}
	return survive, gone
}

func TestComputeAndExecuteWipePreservesScaffold(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	survive, gone := populateRuntime(t, cfg)

	plan, err := computeWipePlan(cfg, root)
	if err != nil {
		t.Fatalf("computeWipePlan: %v", err)
	}
	if err := executeWipePlan(plan); err != nil {
		t.Fatalf("executeWipePlan: %v", err)
	}
	if err := recreateWipeDirs(cfg.LoopDir); err != nil {
		t.Fatalf("recreateWipeDirs: %v", err)
	}

	for _, p := range survive {
		mustExist(t, p)
	}
	for _, p := range gone {
		mustNotExist(t, p)
	}
	// scratch-abc directory itself must be gone (not just its child).
	mustNotExist(t, filepath.Join(cfg.LoopDir, "scratch-abc"))
	// runtime dirs recreated.
	for _, sub := range []string{"inbox", "archive", "malformed", "live", "agents", "state"} {
		mustExist(t, filepath.Join(cfg.LoopDir, sub))
	}
	if leftovers := rescanWipeLeftovers(cfg.LoopDir); len(leftovers) != 0 {
		t.Fatalf("post-wipe leftovers: %v", leftovers)
	}
}

func TestWipeStatePreservesSetupJSON(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	setupJSON := filepath.Join(cfg.LoopDir, "state", "setup.json")
	mustWrite(t, setupJSON, []byte(`{"version":1}`))
	// other per-agent state must be removed.
	other := filepath.Join(cfg.LoopDir, "state", "claude-code", "pending-replies.json")
	mustWrite(t, other, []byte("x"))

	plan, err := computeWipePlan(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := executeWipePlan(plan); err != nil {
		t.Fatal(err)
	}
	mustExist(t, setupJSON)
	mustNotExist(t, filepath.Join(cfg.LoopDir, "state", "claude-code"))
}

// ---------- legacy namespace allowlist ----------

func writeLegacyLoop(t *testing.T, root, dotdir string, sentinel bool) string {
	t.Helper()
	loopDir := filepath.Join(root, dotdir, "loop")
	mustMkdir(t, filepath.Join(loopDir, "agents"))
	mustMkdir(t, filepath.Join(loopDir, "inbox"))
	if sentinel {
		mustWrite(t, filepath.Join(loopDir, "README.md"), []byte("# agentchute loop"))
	}
	return loopDir
}

func TestScanLegacyNamespacesAllowlist(t *testing.T) {
	root, _ := newWipeTestRepo(t)
	allowed := writeLegacyLoop(t, root, ".rehumanlabs", true) // allowlisted + sentinel -> delete
	unknown := writeLegacyLoop(t, root, ".somevendor", true)  // not allowlisted + sentinel -> manual
	noSentinel := writeLegacyLoop(t, root, ".bare", false)    // looks like a loop dir but no agentchute marker -> skipped

	legacy, manual, err := scanLegacyNamespaces(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0] != allowed {
		t.Fatalf("expected legacy=[%s], got %v", allowed, legacy)
	}
	if len(manual) != 1 || !strings.Contains(manual[0], ".somevendor") {
		t.Fatalf("expected one manual report for .somevendor, got %v", manual)
	}
	// the no-sentinel dir must appear in neither set.
	for _, m := range manual {
		if strings.Contains(m, ".bare") {
			t.Fatalf("no-sentinel dir must not be reported: %v", manual)
		}
	}
	_ = unknown
	_ = noSentinel
}

func TestExecuteWipeRemovesAllowlistedLegacyLoop(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	legacyLoop := writeLegacyLoop(t, root, ".rehumanlabs", true)
	plan, err := computeWipePlan(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := executeWipePlan(plan); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, legacyLoop)
	// the canonical loop survives (its contents wiped, dir intact handled elsewhere).
	mustExist(t, cfg.LoopDir)
}

// ---------- live-bus refusal ----------

func TestScanWipeLiveSignalsRefusesLiveRunner(t *testing.T) {
	_, cfg := newWipeTestRepo(t)
	const id = "claude-code"

	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:   id,
		Host:      localHostname(),
		RunnerPID: 4242,
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}

	oldAlive := setupProcessAlive
	oldCmd := setupProcessCommandLine
	setupProcessAlive = func(pid int) bool { return pid == 4242 }
	// Realistic runner cmdline: NO --as (runners use the contextual id), so the
	// pool proof is the exact --control-repo/--loop-dir value match.
	setupProcessCommandLine = func(pid int) string {
		return "/usr/local/bin/agentchute run --vendor openai --control-repo " + cfg.ControlRepo + " --loop-dir " + cfg.LoopDir + " --shim-name ac -- /usr/bin/codex"
	}
	t.Cleanup(func() { setupProcessAlive = oldAlive; setupProcessCommandLine = oldCmd })

	reasons := scanWipeLiveSignals(cfg, []string{id})
	if len(reasons) == 0 {
		t.Fatal("expected a refusal reason for a live runner")
	}
	joined := strings.Join(reasons, "\n")
	if !strings.Contains(joined, "live runner") {
		t.Fatalf("expected a live-runner reason, got: %s", joined)
	}
}

func TestScanWipeLiveSignalsRefusesForeignHostPresence(t *testing.T) {
	_, cfg := newWipeTestRepo(t)
	const id = "codex"
	mustWriteLiveAt(t, cfg, id, time.Now()) // mustWriteLiveAt stamps Host="test-host"
	reasons := scanWipeLiveSignals(cfg, nil)
	if len(reasons) == 0 || !strings.Contains(strings.Join(reasons, "\n"), "another host") {
		t.Fatalf("expected a foreign-host presence refusal, got %v", reasons)
	}
}

func TestSetupRunWipeStateRefusesLiveRunner(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	populateRuntime(t, cfg)
	const id = "claude-code"
	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:   id,
		Host:      localHostname(),
		RunnerPID: 4243,
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}
	oldAlive := setupProcessAlive
	oldCmd := setupProcessCommandLine
	oldSig := setupSignalProcess
	oldWait := wipeProcessWait
	setupProcessAlive = func(pid int) bool { return pid == 4243 }
	setupProcessCommandLine = func(pid int) string {
		return "/usr/local/bin/agentchute run " + id + " --as " + id + " " + cfg.LoopDir
	}
	setupSignalProcess = func(pid int, sig os.Signal) error { return nil } // no-op: never signal a real pid
	wipeProcessWait = time.Millisecond
	t.Cleanup(func() {
		setupProcessAlive = oldAlive
		setupProcessCommandLine = oldCmd
		setupSignalProcess = oldSig
		wipeProcessWait = oldWait
	})

	err := setupRunWipeState(root, cfg, []string{id}, setupOptions{Yes: true})
	if err == nil {
		t.Fatal("setupRunWipeState must refuse when a live runner is present")
	}
	// nothing should have been deleted (refusal happens before execute).
	mustExist(t, filepath.Join(cfg.LoopDir, "archive", "old.md"))
}

func TestSetupRunWipeStateHappyPath(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	survive, gone := populateRuntime(t, cfg)
	oldWait := wipeProcessWait
	wipeProcessWait = time.Millisecond
	t.Cleanup(func() { wipeProcessWait = oldWait })

	if err := setupRunWipeState(root, cfg, []string{"claude-code"}, setupOptions{Yes: true}); err != nil {
		t.Fatalf("setupRunWipeState happy path: %v", err)
	}
	for _, p := range survive {
		mustExist(t, p)
	}
	for _, p := range gone {
		mustNotExist(t, p)
	}
}

// ---------- Task 1: runner matcher fix (no --as on runners) ----------

func TestSetupCommandMatchesRunnerPool(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
	}

	// A runner is launched WITHOUT --as (contextual id), but DOES carry the pool
	// path. It must match — this is the false-negative the fix repairs.
	runnerNoAs := "/usr/local/bin/agentchute run --control-repo " + root + " --loop-dir " + cfg.LoopDir
	if !setupCommandMatchesRunnerPool(runnerNoAs, cfg) {
		t.Fatalf("runner cmdline without --as but with the pool path must match: %q", runnerNoAs)
	}

	// A poller carries --as <id>; the poller matcher still requires it.
	poller := "/usr/local/bin/agentchute poller run --as codex-x --control-repo " + root
	if !setupCommandMatches(poller, "codex-x", "poller run", cfg) {
		t.Fatalf("poller cmdline with --as must match: %q", poller)
	}

	// A foreign pool / non-agentchute process must NOT match (still ambiguous ->
	// fail closed).
	foreignPool := "/usr/local/bin/agentchute run --control-repo /some/other/repo"
	if setupCommandMatchesRunnerPool(foreignPool, cfg) {
		t.Fatalf("a runner for a DIFFERENT pool must not match: %q", foreignPool)
	}
	notAgentchute := "node /opt/app/server.js " + root
	if setupCommandMatchesRunnerPool(notAgentchute, cfg) {
		t.Fatalf("a non-agentchute process must not match: %q", notAgentchute)
	}
}

// A live runner whose cmdline has NO --as (the real-world case) must now be
// reported as a live runner — not as an "ambiguous" fail-closed refusal.
func TestScanWipeLiveSignalsRunnerWithoutAsIsLiveNotAmbiguous(t *testing.T) {
	_, cfg := newWipeTestRepo(t)
	const id = "claude-code"
	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:   id,
		Host:      localHostname(),
		RunnerPID: 4711,
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}
	oldAlive := setupProcessAlive
	oldCmd := setupProcessCommandLine
	setupProcessAlive = func(pid int) bool { return pid == 4711 }
	setupProcessCommandLine = func(pid int) string {
		// NO --as: runners launch with the contextual id.
		return "/usr/local/bin/agentchute run --control-repo " + cfg.ControlRepo + " --loop-dir " + cfg.LoopDir
	}
	t.Cleanup(func() { setupProcessAlive = oldAlive; setupProcessCommandLine = oldCmd })

	reasons := scanWipeLiveSignals(cfg, []string{id})
	joined := strings.Join(reasons, "\n")
	if !strings.Contains(joined, "live runner") {
		t.Fatalf("expected a live-runner reason for a no-as runner, got: %s", joined)
	}
	if strings.Contains(joined, "ambiguous") {
		t.Fatalf("a runner bound by runner.json must NOT be reported ambiguous: %s", joined)
	}
}

// ---------- cmdSetup flag wiring ----------

func TestSetupWipeStateWithoutResetRejected(t *testing.T) {
	err := cmdSetup([]string{"--wipe-state", "--wake", "runner", "--wrappers", "none"})
	if err == nil || !strings.Contains(err.Error(), "requires --reset") {
		t.Fatalf("expected --wipe-state-without-reset rejection, got %v", err)
	}
}

func TestSetupWipeStateDryRunMutatesNothing(t *testing.T) {
	root, cfg := newWipeTestRepo(t)
	survive, gone := populateRuntime(t, cfg)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")

	withCwd(t, root, func() {
		if err := cmdSetup([]string{
			"--reset", "--wipe-state", "--dry-run",
			"--control-repo", root,
			"--wake", "runner",
			"--wrappers", "none",
			"--no-profile",
		}); err != nil {
			t.Fatalf("dry-run cmdSetup: %v", err)
		}
	})

	// dry-run must not delete anything.
	for _, p := range append(append([]string{}, survive...), gone...) {
		mustExist(t, p)
	}
}

// Security (codex Gate-3 review): exact --control-repo/--loop-dir value matching —
// a foreign sibling-prefix pool must NOT match (substring matching would, and it
// gates a SIGTERM for runners that carry no agent id).
func TestSetupCommandMatchesRunnerPool_SiblingPrefixRejected(t *testing.T) {
	cfg := &loop.Config{ControlRepo: "/tmp/repo", LoopDir: "/tmp/repo/.agentchute/loop"}
	foreign := "/usr/local/bin/agentchute run --control-repo /tmp/repo2 --loop-dir /tmp/repo2/.agentchute/loop"
	if setupCommandMatchesRunnerPool(foreign, cfg) {
		t.Fatal("sibling-prefix /tmp/repo2 must NOT match pool /tmp/repo")
	}
	ours := "/usr/local/bin/agentchute run --vendor openai --control-repo /tmp/repo --loop-dir /tmp/repo/.agentchute/loop --shim-name ac -- /usr/bin/codex"
	if !setupCommandMatchesRunnerPool(ours, cfg) {
		t.Fatal("this pool's runner (no --as) must match by exact path value")
	}
	if setupCommandMatchesRunnerPool("/usr/bin/node /tmp/repo/app.js", cfg) {
		t.Fatal("a non-agentchute process must not match")
	}
}

// Security (codex Gate-3 re-review): --control-repo/--loop-dir appearing in the
// WRAPPER argv (after the `--` separator) must NOT attribute a foreign runner to
// this pool. setupCommandFlagValue stops at `--`.
func TestSetupCommandMatchesRunnerPool_IgnoresWrapperArgsAfterDashDash(t *testing.T) {
	cfg := &loop.Config{ControlRepo: "/tmp/pool", LoopDir: "/tmp/pool/.agentchute/loop"}
	// Foreign runner for /tmp/other; its launched wrapper happens to take a
	// --control-repo /tmp/pool of its own — that is AFTER `--` and must be ignored.
	foreign := "/usr/local/bin/agentchute run --vendor openai --control-repo /tmp/other --loop-dir /tmp/other/.agentchute/loop -- /usr/bin/codex --control-repo /tmp/pool --loop-dir /tmp/pool/.agentchute/loop"
	if setupCommandMatchesRunnerPool(foreign, cfg) {
		t.Fatal("--control-repo/--loop-dir in wrapper argv (after --) must NOT attribute to this pool")
	}
}
