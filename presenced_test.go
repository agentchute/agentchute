package main

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// stubPresenceWakeReachable forces the daemon's wake-reachability seams to a
// deterministic result so a unit test never dials a real herdr/runner socket.
func stubPresenceWakeReachable(t *testing.T, herdr func(string) bool, runner func(string, time.Duration) bool) {
	t.Helper()
	oh, or := presenceHerdrReachable, presenceRunnerReachable
	if herdr != nil {
		presenceHerdrReachable = herdr
	}
	if runner != nil {
		presenceRunnerReachable = runner
	}
	t.Cleanup(func() { presenceHerdrReachable, presenceRunnerReachable = oh, or })
}

// A high-confidence herdr candidate (derivable vendor + reachable wake) is
// auto-enrolled in one pass; an ambiguous candidate (no resolvable wake) is
// reported read-only and never written.
func TestPresenced_RegistersOnlyHighConfidenceMatch(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{
			{Name: "claude-code-foo", Cwd: root}, // vendor derivable + reachable -> register
			{Name: "gemini-cli-bar", Cwd: root},  // vendor derivable but UNreachable -> report only
		}
	}
	stubPresenceWakeReachable(t, func(name string) bool { return name == "claude-code-foo" }, nil)

	reports, err := presencedPass(cfg, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}

	wrote := 0
	for _, r := range reports {
		if r.Wrote {
			wrote++
			if r.AgentID != "claude-code-foo" {
				t.Fatalf("unexpected write for %q; only the high-confidence claude-code-foo may be written", r.AgentID)
			}
		}
	}
	if wrote != 1 {
		t.Fatalf("want exactly 1 write, got %d: %+v", wrote, reports)
	}

	reg := readExampleReg(t, root, "claude-code-foo")
	if reg.WakeMethod != "herdr" || reg.WakeTarget != "claude-code-foo" {
		t.Fatalf("registered wake = %q %q, want herdr claude-code-foo", reg.WakeMethod, reg.WakeTarget)
	}
	if reg.Vendor != "anthropic" {
		t.Fatalf("registered vendor = %q, want anthropic", reg.Vendor)
	}
	if reg.LaunchedBy != loop.LaunchedByPresenced {
		t.Fatalf("launched_by = %q, want %q (daemon-discovered provenance)", reg.LaunchedBy, loop.LaunchedByPresenced)
	}

	// The ambiguous (unreachable) candidate must NOT have been registered.
	if _, err := os.Stat(cfg.AgentRegistrationPath("gemini-cli-bar")); !os.IsNotExist(err) {
		t.Fatalf("ambiguous candidate gemini-cli-bar should not be registered (stat err=%v)", err)
	}
}

// Two distinct sources mapping to the SAME derived id is ambiguous: the daemon
// cannot attribute the identity to one process, so it writes nothing.
func TestPresenced_AmbiguousDuplicateDerivedIdNotWritten(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{{Name: "claude-code-foo", Cwd: root}}
	}
	listRunnerSockets = func(_ *loop.Config) []runnerPresenceEntry {
		return []runnerPresenceEntry{{AgentID: "claude-code-foo", Cwd: root}}
	}
	stubPresenceWakeReachable(t,
		func(string) bool { return true },
		func(string, time.Duration) bool { return true },
	)

	reports, err := presencedPass(cfg, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}
	for _, r := range reports {
		if r.Wrote {
			t.Fatalf("nothing should be written when two sources map to one derived id: %+v", reports)
		}
	}
	if _, err := os.Stat(cfg.AgentRegistrationPath("claude-code-foo")); !os.IsNotExist(err) {
		t.Fatalf("ambiguous duplicate-id candidate must not be registered (stat err=%v)", err)
	}
}

// --dry-run reports a high-confidence match but writes nothing: agents/ is
// byte-identical before and after.
func TestPresenced_DryRunNeverWrites(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{{Name: "claude-code-foo", Cwd: root}}
	}
	stubPresenceWakeReachable(t, func(string) bool { return true }, nil)

	before := snapshotDir(t, cfg.AgentsDir())

	reports, err := presencedPass(cfg, true /* dryRun */, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}

	reported := false
	for _, r := range reports {
		if r.AgentID == "claude-code-foo" {
			reported = true
			if r.Wrote {
				t.Fatalf("dry-run must not write: %+v", r)
			}
		}
	}
	if !reported {
		t.Fatalf("dry-run should still report the high-confidence candidate: %+v", reports)
	}

	after := snapshotDir(t, cfg.AgentsDir())
	if len(before) != len(after) {
		t.Fatalf("agents/ entry count changed under dry-run: %d -> %d", len(before), len(after))
	}
	for name, sum := range before {
		if after[name] != sum {
			t.Fatalf("agents/%s changed under dry-run", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Fatalf("agents/%s created under dry-run", name)
		}
	}
}

// The presence daemon is OPT-IN and OFF by default: it must never be wired into
// setup or any lifecycle hook (the only way it runs is a human invoking
// `agentchute presenced`).
func TestPresenced_DefaultOffNotAutoStarted(t *testing.T) {
	// No auto-start source file may reference the daemon.
	for _, src := range []string{"setup.go", "hooks.go", "boot.go", "run.go", "self_check.go", "poller.go", "shims.go"} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if strings.Contains(string(data), "presenced") {
			t.Fatalf("%s references presenced; the host presence daemon must be opt-in (never auto-started)", src)
		}
	}

	// No embedded hook template may invoke it.
	err := fs.WalkDir(hooksFS, "examples/hooks", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, e := hooksFS.ReadFile(path)
		if e != nil {
			return e
		}
		if strings.Contains(string(b), "presenced") {
			t.Fatalf("hook template %s references presenced; the daemon must never be auto-started by a hook", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk hook templates: %v", err)
	}
}

// The identity-mis-attribution guardrail: an existing registration for the
// derived id under a DIFFERENT vendor/identity is never overwritten.
func TestPresenced_DoesNotClobberHealthyDifferentIdentity(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	healthy := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "anthropic",
		ControlRepo: root,
		WakeMethod:  "tmux",
		WakeTarget:  "%5",
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code-foo"), healthy); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, cfg.AgentsDir())

	// A candidate that derives the SAME id but a DIFFERENT vendor must be refused.
	conflicting := presenceCandidate{
		AgentID:    "claude-code-foo",
		Vendor:     "openai",
		WakeMethod: loop.RunnerWakeMethod,
		WakeTarget: "unix:/tmp/x.sock",
		Cwd:        root,
	}
	action, reason := decidePresenceAction(cfg, conflicting)
	if action != actionSkipConflict {
		t.Fatalf("different-vendor existing reg: action = %v (%s), want actionSkipConflict", action, reason)
	}

	// A same-vendor candidate against a HEALTHY (already-pokable) reg is left
	// untouched too — repair is reserved for a stale reg with no wake.
	sameVendor := presenceCandidate{
		AgentID:    "claude-code-foo",
		Vendor:     "anthropic",
		WakeMethod: "herdr",
		WakeTarget: "claude-code-foo",
		Cwd:        root,
	}
	if action, reason := decidePresenceAction(cfg, sameVendor); action != actionSkipHealthy {
		t.Fatalf("same-vendor healthy reg: action = %v (%s), want actionSkipHealthy", action, reason)
	}

	after := snapshotDir(t, cfg.AgentsDir())
	if before["claude-code-foo.md"] != after["claude-code-foo.md"] {
		t.Fatalf("healthy registration was modified by the daemon decision path")
	}
}

// BLOCKER 2 (codex WI-E4 gate): the decide→write sequence MUST be atomic under a
// single per-agent lock so the guard is re-evaluated against the AUTHORITATIVE
// on-disk registration INSIDE the write lock — no decide→write TOCTOU window. We
// prove it by publishing a DIFFERENT-vendor registration while we hold the agent
// lock (i.e. AFTER any earlier lock-free decision would have run, but BEFORE the
// commit can acquire the lock and re-read). If the guard is re-checked inside the
// lock, the commit observes the different-vendor reg and SKIPS — no clobber.
func TestPresenced_DecideWriteAtomicUnderLock(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	cand := presenceCandidate{
		AgentID:    "claude-code-foo",
		Vendor:     "anthropic",
		WakeMethod: "herdr",
		WakeTarget: "claude-code-foo",
		Cwd:        root,
	}

	// A concurrent create of a DIFFERENT-identity registration that becomes
	// visible only while the agent lock is held.
	conflict := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "openai",
		ControlRepo: root,
		WakeMethod:  "tmux",
		WakeTarget:  "%9",
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}

	// Orchestration that deterministically distinguishes "decision under the lock"
	// from "decision lock-free before the write":
	//
	//  1. goroutine G takes the agent lock and parks (no reg on disk yet).
	//  2. goroutine C calls commitPresenceAction. A CORRECT impl blocks on the lock
	//     before reading anything; a BUGGY (decide-outside-lock) impl reads "no reg"
	//     here and resolves to CREATE.
	//  3. G publishes the DIFFERENT-vendor reg and releases the lock.
	//  4. C proceeds. A correct impl now reads the openai reg under the lock and
	//     SKIPS; a buggy impl writes its earlier CREATE decision and CLOBBERS.
	started := make(chan struct{})
	proceed := make(chan struct{})
	released := make(chan struct{})
	go func() {
		err := loop.WithAgentLock(cfg, cand.AgentID, func() error {
			close(started)
			<-proceed
			return loop.WriteRegistration(cfg.AgentRegistrationPath(cand.AgentID), conflict)
		})
		if err != nil {
			t.Errorf("seed conflict under lock: %v", err)
		}
		close(released)
	}()
	<-started

	type result struct {
		action presenceActionKind
		wrote  bool
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		action, _, wrote, err := commitPresenceAction(cfg, cand, time.Now().UTC())
		resCh <- result{action, wrote, err}
	}()

	// Give the commit goroutine time to reach its decision point. A correct impl
	// is now parked on the lock; a buggy lock-free decide has already (wrongly)
	// observed "no reg" and chosen CREATE.
	time.Sleep(100 * time.Millisecond)
	close(proceed) // G writes the openai reg, then releases the lock.
	<-released

	res := <-resCh
	if res.err != nil {
		t.Fatalf("commitPresenceAction: %v", res.err)
	}
	if res.wrote {
		t.Fatalf("commit wrote a registration; a different-vendor reg became authoritative under the lock and must NOT be clobbered")
	}
	if res.action != actionSkipConflict {
		t.Fatalf("action = %v, want actionSkipConflict (guard re-evaluated inside the write lock)", res.action)
	}

	got := readExampleReg(t, root, "claude-code-foo")
	if got.Vendor != "openai" || got.WakeMethod != "tmux" || got.WakeTarget != "%9" {
		t.Fatalf("conflicting reg was clobbered: vendor=%q wake=%q %q (want openai tmux %%9)", got.Vendor, got.WakeMethod, got.WakeTarget)
	}
}

// BLOCKER 1 (codex WI-E4 gate, path A): the repair path must be reachable from a
// FULL presencedPass, not just a direct decidePresenceAction call. A same-vendor
// registration with NO wake (non-pokable) for which a high-confidence reachable
// wake currently exists is repaired in one pass; a HEALTHY (already-pokable) reg
// in the same pass is left byte-identical.
func TestPresenced_PassLevelRepairsStaleSameVendorNoWake(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	// A same-vendor reg with NO wake -> repair candidate.
	stale := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "anthropic",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code-foo"), stale); err != nil {
		t.Fatal(err)
	}
	// A HEALTHY same-vendor reg that already declares a wake -> must be untouched.
	healthy := &loop.Registration{
		AgentID:     "claude-code-bar",
		Vendor:      "anthropic",
		ControlRepo: root,
		WakeMethod:  "herdr",
		WakeTarget:  "claude-code-bar",
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code-bar"), healthy); err != nil {
		t.Fatal(err)
	}
	healthyBefore := snapshotDir(t, cfg.AgentsDir())["claude-code-bar.md"]

	// Both ids have a live, reachable herdr presence in this pool. The create
	// scan excludes both (already registered); only the repair enumeration can
	// surface the no-wake one.
	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{
			{Name: "claude-code-foo", Cwd: root},
			{Name: "claude-code-bar", Cwd: root},
		}
	}
	stubPresenceWakeReachable(t, func(string) bool { return true }, nil)

	reports, err := presencedPass(cfg, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}

	wroteFoo := false
	for _, r := range reports {
		if r.Wrote {
			if r.AgentID != "claude-code-foo" {
				t.Fatalf("unexpected write for %q; only the no-wake claude-code-foo may be repaired", r.AgentID)
			}
			wroteFoo = true
		}
	}
	if !wroteFoo {
		t.Fatalf("expected pass-level repair of claude-code-foo: %+v", reports)
	}

	repaired := readExampleReg(t, root, "claude-code-foo")
	if repaired.WakeMethod != "herdr" || repaired.WakeTarget != "claude-code-foo" {
		t.Fatalf("repaired wake = %q %q, want herdr claude-code-foo", repaired.WakeMethod, repaired.WakeTarget)
	}
	if repaired.LaunchedBy != loop.LaunchedByPresenced {
		t.Fatalf("repaired launched_by = %q, want %q", repaired.LaunchedBy, loop.LaunchedByPresenced)
	}
	if repaired.Vendor != "anthropic" {
		t.Fatalf("repaired vendor = %q, want anthropic (same identity preserved)", repaired.Vendor)
	}

	healthyAfter := snapshotDir(t, cfg.AgentsDir())["claude-code-bar.md"]
	if healthyBefore != healthyAfter {
		t.Fatalf("healthy reg claude-code-bar was modified by the repair pass")
	}
}

// The pass-level repair enumeration must still honor the identity guard: a
// no-wake reg under a DIFFERENT vendor than the derived candidate is reported,
// never repaired/clobbered.
func TestPresenced_PassLevelRepairSkipsDifferentVendorNoWake(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	// A no-wake reg whose stored vendor (openai) disagrees with the id-derived
	// vendor (google) — a different identity squatting the derived id.
	mismatch := &loop.Registration{
		AgentID:     "gemini-cli-zzz",
		Vendor:      "openai",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("gemini-cli-zzz"), mismatch); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, cfg.AgentsDir())["gemini-cli-zzz.md"]

	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{{Name: "gemini-cli-zzz", Cwd: root}}
	}
	stubPresenceWakeReachable(t, func(string) bool { return true }, nil)

	reports, err := presencedPass(cfg, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}
	for _, r := range reports {
		if r.Wrote {
			t.Fatalf("different-vendor no-wake reg must never be written: %+v", reports)
		}
	}
	after := snapshotDir(t, cfg.AgentsDir())["gemini-cli-zzz.md"]
	if before != after {
		t.Fatalf("different-vendor reg gemini-cli-zzz was modified")
	}
}

// A stale registration with NO wake (non-pokable) under the SAME vendor is
// repairable: the daemon refreshes its wake target.
func TestPresenced_RepairsStaleSameVendorReg(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	// Seed a non-pokable reg under a DIFFERENT id so the herdr scan still
	// surfaces the candidate (the scan excludes already-registered ids).
	stale := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "anthropic",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
		// no wake_method/wake_target -> not pokable -> repair candidate
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code-foo"), stale); err != nil {
		t.Fatal(err)
	}

	cand := presenceCandidate{
		AgentID:    "claude-code-foo",
		Vendor:     "anthropic",
		WakeMethod: "herdr",
		WakeTarget: "claude-code-foo",
		Cwd:        root,
	}
	action, reason := decidePresenceAction(cfg, cand)
	if action != actionRepair {
		t.Fatalf("stale non-pokable same-vendor reg: action = %v (%s), want actionRepair", action, reason)
	}
}
