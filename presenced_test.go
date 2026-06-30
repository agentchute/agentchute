package main

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Pull-only (simple-again Gate 6c): the presence daemon no longer resolves or
// verifies a reachable wake endpoint (there is no wake state to write), so the
// stubPresenceWakeReachable seam and the REPAIR path it exercised are gone. A
// high-confidence candidate is now just an in-pool presence whose id maps to one
// known vendor; the daemon CREATEs a plain no-wake registration for it.

// A herdr presence whose id maps to a known vendor is auto-enrolled in one pass;
// a presence whose name yields no derivable vendor is ambiguous and reported
// read-only, never written.
func TestPresenced_RegistersOnlyDerivableVendorMatch(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	listHerdrAgents = func() []herdrPresenceEntry {
		return []herdrPresenceEntry{
			{Name: "claude-code-foo", Cwd: root}, // vendor derivable -> register
			{Name: "mystery-agent", Cwd: root},   // no derivable vendor -> report only
		}
	}

	reports, err := presencedPass(cfg, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}

	wrote := 0
	for _, r := range reports {
		if r.Wrote {
			wrote++
			if r.AgentID != "claude-code-foo" {
				t.Fatalf("unexpected write for %q; only the derivable-vendor claude-code-foo may be written", r.AgentID)
			}
		}
	}
	if wrote != 1 {
		t.Fatalf("want exactly 1 write, got %d: %+v", wrote, reports)
	}

	reg := readExampleReg(t, root, "claude-code-foo")
	if reg.Vendor != "anthropic" {
		t.Fatalf("registered vendor = %q, want anthropic", reg.Vendor)
	}
	if reg.LaunchedBy != loop.LaunchedByPresenced {
		t.Fatalf("launched_by = %q, want %q (daemon-discovered provenance)", reg.LaunchedBy, loop.LaunchedByPresenced)
	}

	// The ambiguous (no-vendor) candidate must NOT have been registered.
	if _, err := os.Stat(cfg.AgentRegistrationPath("mystery-agent")); !os.IsNotExist(err) {
		t.Fatalf("ambiguous candidate mystery-agent should not be registered (stat err=%v)", err)
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
// derived id under a DIFFERENT vendor/identity is never overwritten; a same-vendor
// existing registration is already enrolled and left untouched.
func TestPresenced_DoesNotClobberExistingIdentities(t *testing.T) {
	cfg, root := presencePoolCfg(t)

	existing := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "anthropic",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code-foo"), existing); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, cfg.AgentsDir())

	// A candidate that derives the SAME id but a DIFFERENT vendor must be refused.
	conflicting := presenceCandidate{
		AgentID: "claude-code-foo",
		Vendor:  "openai",
		Cwd:     root,
	}
	action, reason := decidePresenceAction(cfg, conflicting)
	if action != actionSkipConflict {
		t.Fatalf("different-vendor existing reg: action = %v (%s), want actionSkipConflict", action, reason)
	}

	// A same-vendor candidate against an existing reg is already enrolled -> skip.
	sameVendor := presenceCandidate{
		AgentID: "claude-code-foo",
		Vendor:  "anthropic",
		Cwd:     root,
	}
	if action, reason := decidePresenceAction(cfg, sameVendor); action != actionSkipHealthy {
		t.Fatalf("same-vendor existing reg: action = %v (%s), want actionSkipHealthy", action, reason)
	}

	after := snapshotDir(t, cfg.AgentsDir())
	if before["claude-code-foo.md"] != after["claude-code-foo.md"] {
		t.Fatalf("existing registration was modified by the daemon decision path")
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
		AgentID: "claude-code-foo",
		Vendor:  "anthropic",
		Cwd:     root,
	}

	// A concurrent create of a DIFFERENT-identity registration that becomes
	// visible only while the agent lock is held.
	conflict := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "openai",
		ControlRepo: root,
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
	if got.Vendor != "openai" {
		t.Fatalf("conflicting reg was clobbered: vendor=%q (want openai)", got.Vendor)
	}
}

// The create scan excludes already-registered ids, so a registered presence under
// a DIFFERENT vendor than its id-derived vendor is never surfaced or written.
func TestPresenced_ExcludesRegisteredDifferentVendor(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	// A reg whose stored vendor (openai) disagrees with the id-derived vendor
	// (google) — a different identity squatting the derived id.
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

	reports, err := presencedPass(cfg, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("presencedPass: %v", err)
	}
	for _, r := range reports {
		if r.Wrote {
			t.Fatalf("an already-registered id must never be written: %+v", reports)
		}
	}
	after := snapshotDir(t, cfg.AgentsDir())["gemini-cli-zzz.md"]
	if before != after {
		t.Fatalf("registered reg gemini-cli-zzz was modified")
	}
}

// A same-vendor existing registration is already enrolled; the daemon skips it
// (pull-only has no wake to repair).
func TestPresenced_SkipsExistingSameVendor(t *testing.T) {
	cfg, root := presencePoolCfg(t)
	stubPresenceListers(t)

	existing := &loop.Registration{
		AgentID:     "claude-code-foo",
		Vendor:      "anthropic",
		ControlRepo: root,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code-foo"), existing); err != nil {
		t.Fatal(err)
	}

	cand := presenceCandidate{
		AgentID: "claude-code-foo",
		Vendor:  "anthropic",
		Cwd:     root,
	}
	action, reason := decidePresenceAction(cfg, cand)
	if action != actionSkipHealthy {
		t.Fatalf("same-vendor existing reg: action = %v (%s), want actionSkipHealthy", action, reason)
	}
}
