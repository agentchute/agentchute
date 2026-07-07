package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// stubPresenceListers neutralizes every presence enumerator so a single test
// only exercises the source it explicitly overrides (and never shells out to a
// real herdr/tmux/ps on the dev machine). Restored on cleanup.
func stubPresenceListers(t *testing.T) {
	t.Helper()
	oh, or, op := listHerdrAgents, listRunnerSockets, listProcesses
	listHerdrAgents = func() []herdrPresenceEntry { return nil }
	listRunnerSockets = func(_ *loop.Config) []runnerPresenceEntry { return nil }
	listProcesses = func() []processPresenceEntry { return nil }
	t.Cleanup(func() {
		listHerdrAgents, listRunnerSockets, listProcesses = oh, or, op
	})
}

// presencePoolCfg scaffolds a real control repo (so cwd→pool mapping via
// loop.Discover resolves) and returns a Config rooted at it.
func presencePoolCfg(t *testing.T) (*loop.Config, string) {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	root := t.TempDir()
	mustExampleRepo(t, root)
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	mustMkdir(t, cfg.AgentsDir())
	return cfg, root
}

func TestScanUnenrolledWrappers_HerdrAndProcessSources(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	// Enroll a herdr agent named "claude-code". Pull-only: a herdr presence is
	// matched to a registration by NAME==agent id (no wake target).
	enrolled := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code"), enrolled); err != nil {
		t.Fatal(err)
	}
	// Record an active session PID so a process with that PID counts enrolled.
	if err := loop.SaveActiveSession(cfg, loop.ActiveSession{
		AgentID:  "claude-code",
		Host:     "test-host",
		PID:      4242,
		LastSeen: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	stubPresenceListers(t)
	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{
			{Name: "claude-code", Cwd: root}, // enrolled (herdr target) -> excluded
			{Name: "ghost-herdr", Cwd: root}, // unenrolled -> included
		}
	}
	listProcesses = func() []processPresenceEntry {
		return []processPresenceEntry{
			{PID: 4242, Command: "claude", Cwd: root}, // PID in active session -> excluded
			{PID: 9999, Command: "codex", Cwd: root},  // raw process -> included
			{PID: 1234, Command: "bash", Cwd: root},   // not a wrapper -> excluded
		}
	}

	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatalf("scanUnenrolledWrappers: %v", err)
	}
	var kinds []string
	hints := map[string]bool{}
	for _, p := range got {
		kinds = append(kinds, p.Kind)
		hints[p.Hint] = true
	}
	sort.Strings(kinds)
	if len(got) != 2 {
		t.Fatalf("want 2 unenrolled (ghost-herdr + raw codex), got %d: %+v", len(got), got)
	}
	if !hints["ghost-herdr"] {
		t.Fatalf("expected ghost-herdr in results: %+v", got)
	}
	// The raw codex process (pid 9999) must surface; the enrolled pid 4242 and
	// the non-wrapper bash must not.
	foundRawCodex := false
	for _, p := range got {
		if p.Kind == "process" {
			foundRawCodex = true
			if p.Cwd != root {
				t.Fatalf("process entry cwd = %q, want %q", p.Cwd, root)
			}
		}
	}
	if !foundRawCodex {
		t.Fatalf("expected the raw codex process to be reported: %+v", got)
	}
}

// Entries whose cwd does NOT map to this pool's control repo must be ignored.
func TestScanUnenrolledWrappers_IgnoresOutOfPoolCwd(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	_ = root

	outOfPool := t.TempDir() // no AGENTCHUTE.md -> Discover fails -> not in pool

	stubPresenceListers(t)
	listProcesses = func() []processPresenceEntry {
		return []processPresenceEntry{{PID: 99, Command: "codex", Cwd: outOfPool}}
	}

	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatalf("scanUnenrolledWrappers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("out-of-pool cwd should not be reported, got %+v", got)
	}
}

// The scan MUST be strictly read-only: the agents/ directory is byte-identical
// before and after a scan that surfaces an unenrolled wrapper.
func TestScanUnenrolledWrappers_PerformsNoWrites(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	// Seed an existing enrolled registration so the dir is non-empty.
	enrolled := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), enrolled); err != nil {
		t.Fatal(err)
	}

	stubPresenceListers(t)
	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{{Name: "ghost-herdr", Cwd: root}}
	}

	before := snapshotDir(t, cfg.AgentsDir())

	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatalf("scanUnenrolledWrappers: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("expected the scan to surface the ghost agent (otherwise the no-write assertion is vacuous)")
	}

	after := snapshotDir(t, cfg.AgentsDir())
	if len(before) != len(after) {
		t.Fatalf("agents/ entry count changed: %d -> %d", len(before), len(after))
	}
	for name, sum := range before {
		if after[name] != sum {
			t.Fatalf("agents/%s changed during scan (write detected)", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Fatalf("agents/%s created during scan (write detected)", name)
		}
	}
}

// snapshotDir returns name->content map for files directly in dir.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// Task 3: a wrapper process whose ancestry includes an enrolled runner (the
// runner launched it: agentchute serve -> wrapper -> ... -> vendor binary) must NOT
// be reported as unenrolled, while a truly raw wrapper still is.
func TestScanUnenrolledWrappers_SuppressesRunnerChildren(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	// An enrolled agent with a live local runner (runner.json binds RunnerPID).
	enrolled := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), enrolled); err != nil {
		t.Fatal(err)
	}
	if err := loop.SaveRunnerState(cfg, loop.RunnerState{
		AgentID:   "codex",
		Host:      localHostname(),
		RunnerPID: 5000,
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}

	stubPresenceListers(t)
	listProcesses = func() []processPresenceEntry {
		return []processPresenceEntry{
			{PID: 8848, Command: "codex", Cwd: root}, // child of the runner -> suppressed
			{PID: 7777, Command: "codex", Cwd: root}, // truly raw -> reported
		}
	}

	oldParent, oldCmd := processParentPID, setupProcessCommandLine
	oldAlive, oldUID := setupProcessAlive, processOwnerUID
	processParentPID = func(pid int) int {
		switch pid {
		case 8848:
			return 8000
		case 8000:
			return 5000 // the runner ancestor
		case 7777:
			return 1 // raw: ancestry hits init with no runner
		default:
			return 1
		}
	}
	// The ancestor 5000 is revalidated DIRECTLY: a live, same-user `agentchute serve`
	// for THIS pool. A recorded runner.json pid is never trusted on its own.
	setupProcessCommandLine = func(pid int) string {
		if pid == 5000 {
			return "/usr/local/bin/agentchute serve --vendor openai --control-repo " + cfg.ControlRepo + " --loop-dir " + cfg.LoopDir + " --shim-name ac -- /usr/bin/codex"
		}
		return ""
	}
	setupProcessAlive = func(pid int) bool { return pid == 5000 }
	processOwnerUID = func(int) (int, bool) { return os.Getuid(), true }
	t.Cleanup(func() {
		processParentPID = oldParent
		setupProcessCommandLine = oldCmd
		setupProcessAlive = oldAlive
		processOwnerUID = oldUID
	})

	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatalf("scanUnenrolledWrappers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly the raw process reported, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "process" || !strings.Contains(got[0].Hint, "7777") {
		t.Fatalf("expected only the raw pid-7777 process, got %+v", got[0])
	}
}

// Security (codex Gate-3 review): a STALE/reused runner.json pid must NOT suppress
// a raw wrapper. The ancestor pid is recorded but is no longer an agentchute serve,
// so it must not count — the raw child is reported.
func TestScanUnenrolledWrappers_StalePIDDoesNotSuppress(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	enrolled := &loop.Registration{AgentID: "codex", Vendor: "openai", ControlRepo: root, LastSeen: time.Now().UTC(), Status: loop.StatusActive}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), enrolled); err != nil {
		t.Fatal(err)
	}
	if err := loop.SaveRunnerState(cfg, loop.RunnerState{AgentID: "codex", Host: localHostname(), RunnerPID: 5000, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	stubPresenceListers(t)
	listProcesses = func() []processPresenceEntry {
		return []processPresenceEntry{{PID: 8848, Command: "codex", Cwd: root}}
	}
	oldParent, oldCmd := processParentPID, setupProcessCommandLine
	oldAlive, oldUID := setupProcessAlive, processOwnerUID
	processParentPID = func(pid int) int {
		if pid == 8848 {
			return 5000
		}
		return 1
	}
	// 5000 is the recorded RunnerPID but has been reused as an unrelated process.
	setupProcessCommandLine = func(int) string { return "/usr/bin/node /some/other/app.js" }
	setupProcessAlive = func(int) bool { return true }
	processOwnerUID = func(int) (int, bool) { return os.Getuid(), true }
	t.Cleanup(func() {
		processParentPID = oldParent
		setupProcessCommandLine = oldCmd
		setupProcessAlive = oldAlive
		processOwnerUID = oldUID
	})
	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Hint, "8848") {
		t.Fatalf("a stale runner.json pid must NOT suppress; want pid-8848 reported, got %+v", got)
	}
}

// Security (codex Gate-3 review): an ancestor whose cmdline IS an agentchute serve
// for this pool but owned by ANOTHER user must NOT suppress (no cross-user trust).
func TestScanUnenrolledWrappers_CrossUserAncestorDoesNotSuppress(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)
	listProcesses = func() []processPresenceEntry {
		return []processPresenceEntry{{PID: 8850, Command: "codex", Cwd: root}}
	}
	oldParent, oldCmd := processParentPID, setupProcessCommandLine
	oldAlive, oldUID := setupProcessAlive, processOwnerUID
	processParentPID = func(pid int) int {
		if pid == 8850 {
			return 9100
		}
		return 1
	}
	setupProcessCommandLine = func(pid int) string {
		if pid == 9100 {
			return "/usr/local/bin/agentchute serve --control-repo " + cfg.ControlRepo + " --loop-dir " + cfg.LoopDir
		}
		return ""
	}
	setupProcessAlive = func(int) bool { return true }
	processOwnerUID = func(int) (int, bool) { return os.Getuid() + 1, true } // different user
	t.Cleanup(func() {
		processParentPID = oldParent
		setupProcessCommandLine = oldCmd
		setupProcessAlive = oldAlive
		processOwnerUID = oldUID
	})
	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Hint, "8850") {
		t.Fatalf("a cross-user runner ancestor must NOT suppress; want pid-8850 reported, got %+v", got)
	}
}

// Task 3 fallback: an ancestor that is an `agentchute serve` for this pool (matched
// by cmdline) also suppresses the child, even without a runner.json pid binding.
func TestScanUnenrolledWrappers_SuppressesViaRunnerCmdlineAncestor(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	stubPresenceListers(t)
	listProcesses = func() []processPresenceEntry {
		return []processPresenceEntry{{PID: 8850, Command: "codex", Cwd: root}}
	}
	oldParent, oldCmd := processParentPID, setupProcessCommandLine
	oldAlive, oldUID := setupProcessAlive, processOwnerUID
	processParentPID = func(pid int) int {
		if pid == 8850 {
			return 9100
		}
		return 1
	}
	setupProcessCommandLine = func(pid int) string {
		if pid == 9100 {
			return "/usr/local/bin/agentchute serve --control-repo " + cfg.ControlRepo + " --loop-dir " + cfg.LoopDir
		}
		return ""
	}
	setupProcessAlive = func(int) bool { return true }
	processOwnerUID = func(int) (int, bool) { return os.Getuid(), true }
	t.Cleanup(func() {
		processParentPID = oldParent
		setupProcessCommandLine = oldCmd
		setupProcessAlive = oldAlive
		processOwnerUID = oldUID
	})

	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatalf("scanUnenrolledWrappers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a child of an `agentchute serve` ancestor must be suppressed, got %+v", got)
	}
}

func TestDoctor_UnenrolledPresenceWarnsNeverBlocks(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	stubPresenceListers(t)
	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{{Name: "ghost-herdr", Cwd: root}}
	}

	got := checkUnenrolledPresence(cfg)
	if got.Name != "unenrolled_presence" {
		t.Fatalf("check name = %q, want unenrolled_presence", got.Name)
	}
	if got.Severity != severityWarn {
		t.Fatalf("severity = %q, want WARN (presence is advisory, never a blocker)", got.Severity)
	}
}

func TestDoctor_UnenrolledPresenceOKWhenClean(t *testing.T) {
	cfg, _ := presencePoolCfg(t)
	stubPresenceListers(t)

	got := checkUnenrolledPresence(cfg)
	if got.Severity != severityOK {
		t.Fatalf("severity = %q, want OK (no unenrolled wrappers)", got.Severity)
	}
}
