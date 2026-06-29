package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// stubPresenceListers neutralizes every presence enumerator so a single test
// only exercises the source it explicitly overrides (and never shells out to a
// real herdr/tmux/ps on the dev machine). Restored on cleanup.
func stubPresenceListers(t *testing.T) {
	t.Helper()
	oh, ot, or, op := listHerdrAgents, listTmuxPanes, listRunnerSockets, listProcesses
	listHerdrAgents = func() []herdrPresenceEntry { return nil }
	listTmuxPanes = func() []tmuxPresenceEntry { return nil }
	listRunnerSockets = func(_ *loop.Config) []runnerPresenceEntry { return nil }
	listProcesses = func() []processPresenceEntry { return nil }
	t.Cleanup(func() {
		listHerdrAgents, listTmuxPanes, listRunnerSockets, listProcesses = oh, ot, or, op
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
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
	mustMkdir(t, cfg.AgentsDir())
	return cfg, root
}

func TestScanUnenrolledWrappers_FindsUnregisteredPaneInPool(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	// Enroll codex on tmux pane %1.
	enrolled := &loop.Registration{
		AgentID:     "codex",
		Vendor:      "openai",
		ControlRepo: root,
		WakeMethod:  "tmux",
		WakeTarget:  "%1",
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), enrolled); err != nil {
		t.Fatal(err)
	}

	stubPresenceListers(t)
	listTmuxPanes = func() []tmuxPresenceEntry {
		return []tmuxPresenceEntry{
			{PaneID: "%1", Cwd: root}, // enrolled -> excluded
			{PaneID: "%2", Cwd: root}, // unenrolled -> included
		}
	}

	got, err := scanUnenrolledWrappers(cfg)
	if err != nil {
		t.Fatalf("scanUnenrolledWrappers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 unenrolled wrapper, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "tmux" || got[0].Hint != "%2" {
		t.Fatalf("unexpected entry: %+v", got[0])
	}
	if got[0].Suggestion == "" {
		t.Fatalf("entry missing Suggestion: %+v", got[0])
	}
}

func TestScanUnenrolledWrappers_HerdrAndProcessSources(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	// Enroll a herdr agent named "claude-code".
	enrolled := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: root,
		WakeMethod:  "herdr",
		WakeTarget:  "claude-code",
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
	listTmuxPanes = func() []tmuxPresenceEntry {
		return []tmuxPresenceEntry{{PaneID: "%7", Cwd: outOfPool}}
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
		WakeMethod:  "tmux",
		WakeTarget:  "%1",
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
