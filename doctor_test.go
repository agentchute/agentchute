package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// findCheck returns the named check from a report, or fails the test.
func findCheck(t *testing.T, r doctorReport, name string) doctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not in report; checks=%v", name, r.Checks)
	return doctorCheck{}
}

func newDoctorCfg(t *testing.T) *loop.Config {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	// Scaffold the same layout init produces.
	for _, d := range []string{cfg.AgentsDir(), filepath.Join(cfg.LoopDir, "inbox")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestDoctorMissingScaffoldBlocks(t *testing.T) {
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".agentchute", "loop"),
		Vendor:      "agentchute",
	}
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "loop_dir_scaffold")
	if got.Severity != severityBlocker {
		t.Errorf("loop_dir_scaffold severity = %q, want BLOCKER", got.Severity)
	}
	if r.Blockers == 0 {
		t.Errorf("Blockers = 0; want >0 (missing scaffold)")
	}
}

func TestDoctorBareAgentchuteCheckInHookBlocks(t *testing.T) {
	cfg := newDoctorCfg(t)
	// Drop a hook file that contains the silent-drain antipattern.
	claudeDir := filepath.Join(cfg.ControlRepo, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookContent := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"agentchute check --as claude-code"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(hookContent), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityBlocker {
		t.Errorf("hook_content_sanity severity = %q, want BLOCKER (bare `agentchute check` in hook)", got.Severity)
	}
	if !strings.Contains(got.Message, "claude-code") {
		t.Errorf("message should name claude-code wrapper: %q", got.Message)
	}
}

// Codex review on bff226c: the templated `check` form must also be
// caught. The silent-drain risk doesn't depend on the binary resolution
// path; check archives regardless of how it was invoked.
func TestDoctorTemplatedAgentchuteCheckInHookBlocks(t *testing.T) {
	cfg := newDoctorCfg(t)
	claudeDir := filepath.Join(cfg.ControlRepo, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookContent := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"${AGENTCHUTE_BIN:-agentchute} check --as claude-code"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(hookContent), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityBlocker {
		t.Errorf("hook_content_sanity severity = %q, want BLOCKER for templated `${AGENTCHUTE_BIN:-agentchute} check`", got.Severity)
	}
}

func TestDoctorEnvOnlyAgentchuteCheckInHookBlocks(t *testing.T) {
	cfg := newDoctorCfg(t)
	claudeDir := filepath.Join(cfg.ControlRepo, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookContent := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"$AGENTCHUTE_BIN check --as claude-code"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(hookContent), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityBlocker {
		t.Errorf("hook_content_sanity severity = %q, want BLOCKER for env-only `$AGENTCHUTE_BIN check`", got.Severity)
	}
}

// Codex review on bff226c: a mixed file with a templated `pending` and a
// bare `agentchute check` must be flagged for the bare-check, not given a
// pass because some other command uses the override convention.
func TestDoctorMixedFormsDoNotMaskCheckOffender(t *testing.T) {
	cfg := newDoctorCfg(t)
	claudeDir := filepath.Join(cfg.ControlRepo, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookContent := `{"hooks":{
		"SessionStart":[{"hooks":[{"type":"command","command":"${AGENTCHUTE_BIN:-agentchute} boot --as claude-code --vendor anthropic"}]}],
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"agentchute check --as claude-code"}]}]
	}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(hookContent), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityBlocker {
		t.Errorf("hook_content_sanity severity = %q, want BLOCKER (mixed bare-check + templated must still flag the bare-check)", got.Severity)
	}
}

// Codex review on bff226c: --json discovery failure must still exit
// errBlocked. Previously emitDoctorJSON returned nil before the
// errBlocked guard ran.
func TestCmdDoctorJSONDiscoveryFailureBlocks(t *testing.T) {
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdDoctor([]string{"--json"}) })
		if err == nil {
			t.Fatal("expected error on missing scaffold with --json")
		}
		if !errors.Is(err, errBlocked) {
			t.Errorf("err = %v, want errBlocked", err)
		}
	})
}

// The templated form ${AGENTCHUTE_BIN:-agentchute} passes sanity when
// AGENTCHUTE_BIN points at a valid binary (the documented override path).
// Updated post-codex review (bff226c): the templated form is no longer a
// free pass; it specifically requires AGENTCHUTE_BIN OR PATH to resolve.
func TestDoctorTemplatedBinaryReferenceIsOKWhenAGENTCHUTE_BINSet(t *testing.T) {
	cfg := newDoctorCfg(t)
	claudeDir := filepath.Join(cfg.ControlRepo, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookContent := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"${AGENTCHUTE_BIN:-agentchute} boot --as claude-code --vendor anthropic --context-only"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(hookContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a stub "binary" the env var can point at — only its existence
	// matters for the resolution check.
	stub := filepath.Join(cfg.ControlRepo, "stub-agentchute")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_BIN", stub)
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityOK {
		t.Errorf("hook_content_sanity severity = %q, want OK (templated form resolves via AGENTCHUTE_BIN)", got.Severity)
	}
}

// Conversely: templated form with NO AGENTCHUTE_BIN AND no PATH resolution
// is now a BLOCKER (codex review on bff226c — the previous lax pass was
// a real gap).
func TestDoctorTemplatedBinaryReferenceBlocksWhenNothingResolves(t *testing.T) {
	cfg := newDoctorCfg(t)
	claudeDir := filepath.Join(cfg.ControlRepo, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookContent := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"${AGENTCHUTE_BIN:-agentchute} boot --as claude-code --vendor anthropic"}]}]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(hookContent), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_BIN", "")
	// Note: test runner's PATH may or may not have `agentchute`. The
	// reliable check is to set AGENTCHUTE_BIN to "" — which makes the
	// templated form depend entirely on PATH — and assert at least that
	// the severity is NOT OK if PATH also lacks the binary. Skip if
	// PATH happens to have it (e.g., developer ran the test from a
	// shell with agentchute installed).
	if isAgentchuteOnPath() {
		t.Skip("agentchute is on PATH in this test environment; cannot test the BLOCKER path")
	}
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityBlocker {
		t.Errorf("hook_content_sanity severity = %q, want BLOCKER (no AGENTCHUTE_BIN, no PATH)", got.Severity)
	}
}

func TestDoctorPerAgentChecksRunWithAgentID(t *testing.T) {
	cfg := newDoctorCfg(t)
	// Write a valid registration so the per-agent checks have something
	// to act on.
	regPath := cfg.AgentRegistrationPath("claude-code")
	reg := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		Host:        "doctor-test",
		LastSeen:    time.Now().UTC().Truncate(time.Second),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentInboxDir("claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	// GATE 3: registration_freshness reads `.live`; give this healthy agent a
	// fresh presence fact so the check is OK rather than warning on absent .live.
	mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC())

	r := runDoctorChecks(cfg, "claude-code", doctorOptions{Now: time.Now().UTC()})

	for _, name := range []string{"self_registration", "registration_freshness", "inbox_state", "ledger_state"} {
		c := findCheck(t, r, name)
		if c.Severity == "" {
			t.Errorf("%s has empty severity", name)
		}
	}

	// Self registration must be OK.
	if got := findCheck(t, r, "self_registration"); got.Severity != severityOK {
		t.Errorf("self_registration severity = %q, want OK", got.Severity)
	}
	// Gate 6a (pull-only): wake_target_validity / runner_socket_staleness checks
	// were removed (sender-side wake reachability no longer exists).
}

// Gate 6a (pull-only): TestDoctorTmuxWakeValidityHonorsProbeSeam was removed
// along with the checkWakeTargetValidity tmux arm it exercised.

func TestDoctorAsBlocksWhenActingWrapperHookMissing(t *testing.T) {
	cfg := newDoctorCfg(t)
	for _, dir := range []string{".claude", ".gemini"} {
		if err := os.MkdirAll(filepath.Join(cfg.ControlRepo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.ControlRepo, ".claude", "settings.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ControlRepo, ".gemini", "settings.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	r := runDoctorChecks(cfg, "codex", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_file_presence")
	if got.Severity != severityBlocker {
		t.Fatalf("hook_file_presence severity = %q, want BLOCKER when codex hook is missing", got.Severity)
	}
	if !strings.Contains(got.Message, "hooks install --wrapper codex") {
		t.Fatalf("message missing codex install hint: %q", got.Message)
	}
}

func TestDoctorAsBlocksWhenActingWrapperHookDiverged(t *testing.T) {
	cfg := newDoctorCfg(t)
	if err := os.MkdirAll(filepath.Join(cfg.ControlRepo, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ControlRepo, ".codex", "hooks.json"), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	r := runDoctorChecks(cfg, "codex", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "hook_file_presence")
	if got.Severity != severityBlocker {
		t.Fatalf("hook_file_presence severity = %q, want BLOCKER when codex hook diverged", got.Severity)
	}
	if !strings.Contains(got.Message, "--force") {
		t.Fatalf("message missing force reinstall hint: %q", got.Message)
	}
}

// A tmux/herdr-only wake set — including the combined "tmux,herdr" — installs
// only hookless shims, so a hookable wrapper (codex) relies on its lifecycle
// hook and shadowing is not applicable. Regression for the pre-fix `wake ==
// tmux || wake == herdr` equality, which failed any multi-token set and emitted
// false shadowing WARNs.
func TestDoctorShadowingSkipsHookableWrapperForTmuxHerdrSet(t *testing.T) {
	cfg := newDoctorCfg(t)
	// Cover BOTH the base id and the contextual id real setups enroll with —
	// the contextual case is the one the exact-match bug missed.
	for _, agentID := range []string{"codex", "codex-agentchute", "claude-code-agentchute"} {
		for _, wake := range []string{"tmux", "herdr", "tmux,herdr"} {
			got := checkWrapperShadowing(cfg, agentID, doctorOptions{PoolState: &setupPoolState{Wake: wake}})
			if got.Severity != severitySkip {
				t.Fatalf("wrapper_shadowing severity = %q for agent %q wake %q, want SKIP; msg=%q", got.Severity, agentID, wake, got.Message)
			}
		}
		// When runner is in the set, shims ARE required on PATH, so never skip.
		got := checkWrapperShadowing(cfg, agentID, doctorOptions{PoolState: &setupPoolState{Wake: "runner,tmux"}})
		if got.Severity == severitySkip {
			t.Fatalf("wrapper_shadowing must not skip for agent %q when runner is in the set; msg=%q", agentID, got.Message)
		}
	}
}

// The v0.8.8 ac_dispatcher check: OK when the `ac` dispatcher resolves from the
// shim dir ahead of any other `ac` (e.g. the system /usr/sbin/ac), WARN when a
// non-shim-dir `ac` shadows it or the shim dir is absent from PATH.
func TestDoctorAcDispatcherResolution(t *testing.T) {
	cfg := newDoctorCfg(t)
	shimDir := filepath.Join(t.TempDir(), "shim")
	sysDir := filepath.Join(t.TempDir(), "sbin") // stand-in for /usr/sbin
	for _, d := range []string{shimDir, sysDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec := func(path string) {
		mustWrite(t, path, []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(filepath.Join(shimDir, "ac"))
	writeExec(filepath.Join(sysDir, "ac"))

	gs := &setupGlobalState{Wake: "runner", ShimDir: shimDir}
	mkOpts := func(pathEnv string) doctorOptions {
		return doctorOptions{GlobalState: gs, PoolState: &setupPoolState{Wake: "runner"}, PathEnv: pathEnv}
	}
	sep := string(os.PathListSeparator)

	// OK: shim dir precedes the system ac.
	got := checkWrapperShadowing(cfg, "codex", mkOpts(shimDir+sep+sysDir))
	if got.Name != "ac_dispatcher" || got.Severity != severityOK {
		t.Fatalf("precedes case: name=%q sev=%q msg=%q, want ac_dispatcher OK", got.Name, got.Severity, got.Message)
	}

	// WARN: the system ac precedes the shim dir (shadowing).
	got = checkWrapperShadowing(cfg, "codex", mkOpts(sysDir+sep+shimDir))
	if got.Severity != severityWarn || !strings.Contains(got.Message, "shadows the agentchute dispatcher") {
		t.Fatalf("shadowed case: sev=%q msg=%q, want WARN shadowing", got.Severity, got.Message)
	}

	// WARN: shim dir not on PATH.
	got = checkWrapperShadowing(cfg, "codex", mkOpts(sysDir))
	if got.Severity != severityWarn || !strings.Contains(got.Message, "not on PATH") {
		t.Fatalf("not-on-path case: sev=%q msg=%q, want WARN not-on-PATH", got.Severity, got.Message)
	}
}

// hookWrapperForAgent and shimNamesForAgent must resolve contextual ids
// (codex-agentchute) to their canonical wrapper, not only exact base ids.
func TestWrapperResolutionHandlesContextualIDs(t *testing.T) {
	cases := []struct {
		id          string
		wantWrapper string
		hookable    bool
		wantShim    string
	}{
		{"codex", "codex", true, "ac-codex"},
		{"codex-agentchute", "codex", true, "ac-codex"},
		{"claude-code-agentchute", "claude-code", true, "ac-claude"},
		{"gemini-cli-agentchute", "gemini-cli", true, "ac-gemini"},
		{"grok-agentchute", "", false, "ac-grok"}, // hookless, but still shimmed
	}
	for _, tc := range cases {
		w, ok := hookWrapperForAgent(tc.id)
		if ok != tc.hookable || (ok && w != tc.wantWrapper) {
			t.Errorf("hookWrapperForAgent(%q) = (%q,%v), want (%q,%v)", tc.id, w, ok, tc.wantWrapper, tc.hookable)
		}
		if names := shimNamesForAgent(tc.id); len(names) != 1 || names[0] != tc.wantShim {
			t.Errorf("shimNamesForAgent(%q) = %v, want [%s]", tc.id, names, tc.wantShim)
		}
	}
}

// WI-E3: the launch-bypass check WARNS (never BLOCKS) when a runner setup has a
// wrapper that enrolled raw (launched_by=manual / unset), and stays OK for a
// managed (runner) enrollment. It must never flip the runner default or block.
func TestDoctor_WarnsOnRawWrapperBypass(t *testing.T) {
	writeReg := func(t *testing.T, cfg *loop.Config, launchedBy string) {
		t.Helper()
		reg := &loop.Registration{
			AgentID:     "gemini-cli",
			Vendor:      "google",
			ControlRepo: cfg.ControlRepo,
			LastSeen:    time.Now().UTC(),
			Status:      loop.StatusActive,
			LaunchedBy:  launchedBy,
		}
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("gemini-cli"), reg); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cfg.AgentInboxDir("gemini-cli"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Runner is configured; an empty shim dir (not on PATH) keeps the shadowing
	// probe quiet so the provenance signal is isolated.
	t.Run("manual provenance warns, never blocks", func(t *testing.T) {
		cfg := newDoctorCfg(t)
		writeReg(t, cfg, loop.LaunchedByManual)
		r := runDoctorChecks(cfg, "gemini-cli", doctorOptions{
			Now:       time.Now().UTC(),
			PathEnv:   "/usr/bin",
			PoolState: &setupPoolState{Wake: "runner"},
		})
		got := findCheck(t, r, "launch_provenance")
		if got.Severity != severityWarn {
			t.Fatalf("launch_provenance severity = %q, want WARN; msg=%q", got.Severity, got.Message)
		}
		// "never blocks": the bypass check is WARN at most — it must NEVER be a
		// BLOCKER regardless of provenance (other unrelated checks may block in
		// this minimal fixture; this one is what we assert on).
		if got.Severity == severityBlocker {
			t.Fatalf("launch-bypass check must never be a BLOCKER; got %q", got.Severity)
		}
		if !strings.Contains(got.Message, "ac-gemini") {
			t.Fatalf("warn message should name the fix `ac-gemini`: %q", got.Message)
		}
	})

	t.Run("absent provenance warns", func(t *testing.T) {
		cfg := newDoctorCfg(t)
		writeReg(t, cfg, "")
		r := runDoctorChecks(cfg, "gemini-cli", doctorOptions{
			Now:       time.Now().UTC(),
			PathEnv:   "/usr/bin",
			PoolState: &setupPoolState{Wake: "runner"},
		})
		got := findCheck(t, r, "launch_provenance")
		if got.Severity != severityWarn {
			t.Fatalf("absent-provenance severity = %q, want WARN; msg=%q", got.Severity, got.Message)
		}
	})

	t.Run("runner provenance is OK", func(t *testing.T) {
		cfg := newDoctorCfg(t)
		writeReg(t, cfg, loop.LaunchedByRunner)
		r := runDoctorChecks(cfg, "gemini-cli", doctorOptions{
			Now:       time.Now().UTC(),
			PathEnv:   "/usr/bin",
			PoolState: &setupPoolState{Wake: "runner"},
		})
		got := findCheck(t, r, "launch_provenance")
		if got.Severity != severityOK {
			t.Fatalf("runner-provenance severity = %q, want OK; msg=%q", got.Severity, got.Message)
		}
	})

	t.Run("non-runner setup skips", func(t *testing.T) {
		cfg := newDoctorCfg(t)
		writeReg(t, cfg, loop.LaunchedByManual)
		r := runDoctorChecks(cfg, "gemini-cli", doctorOptions{
			Now:       time.Now().UTC(),
			PathEnv:   "/usr/bin",
			PoolState: &setupPoolState{Wake: "tmux"},
		})
		got := findCheck(t, r, "launch_provenance")
		if got.Severity != severitySkip {
			t.Fatalf("non-runner setup severity = %q, want SKIP; msg=%q", got.Severity, got.Message)
		}
	})
}

// GATE 3: registration_freshness diagnoses presence from the `.live` fact, NOT
// registration last_seen. A stale OR absent `.live` warns even when the
// registration's own last_seen is fresh; a fresh `.live` flips it back to OK.
func TestDoctorWarnsOnStaleRegistration(t *testing.T) {
	cfg := newDoctorCfg(t)
	regPath := cfg.AgentRegistrationPath("claude-code")
	reg := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		// Registration last_seen is FRESH on purpose — staleness must come from
		// `.live`, not here.
		LastSeen: time.Now().UTC().Truncate(time.Second),
		Status:   loop.StatusActive,
	}
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentInboxDir("claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Stale `.live` => WARN (despite the fresh registration last_seen).
	mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC().Add(-2*StaleRegThreshold))
	r := runDoctorChecks(cfg, "claude-code", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "registration_freshness")
	if got.Severity != severityWarn {
		t.Errorf("stale .live severity = %q, want WARN (not BLOCKER; doctor diagnoses, gate enforces)", got.Severity)
	}

	// Absent `.live` => WARN too.
	if err := os.Remove(filepath.Join(cfg.LoopDir, "live", "claude-code.live")); err != nil {
		t.Fatal(err)
	}
	r = runDoctorChecks(cfg, "claude-code", doctorOptions{Now: time.Now().UTC()})
	if got := findCheck(t, r, "registration_freshness"); got.Severity != severityWarn {
		t.Errorf("absent .live severity = %q, want WARN", got.Severity)
	}

	// Fresh `.live` => OK (freshness SOURCE is `.live`).
	mustWriteLiveAt(t, cfg, "claude-code", time.Now().UTC())
	r = runDoctorChecks(cfg, "claude-code", doctorOptions{Now: time.Now().UTC()})
	if got := findCheck(t, r, "registration_freshness"); got.Severity != severityOK {
		t.Errorf("fresh .live severity = %q, want OK; msg=%q", got.Severity, got.Message)
	}
}

func TestDoctorWarnsOnUnreadInboxNotBlocks(t *testing.T) {
	cfg := newDoctorCfg(t)
	mustWriteCanonicalHook(t, cfg.ControlRepo, "claude-code")
	stub := filepath.Join(cfg.ControlRepo, "stub-agentchute")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_BIN", stub)
	regPath := cfg.AgentRegistrationPath("claude-code")
	reg := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC().Truncate(time.Second),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	inbox := cfg.AgentInboxDir("claude-code")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.WriteInboxMessage(inbox, time.Now().UTC(), "codex",
		[]byte("---\nfrom: codex\nto: claude-code\ntask: x\n---\n\nb\n")); err != nil {
		t.Fatal(err)
	}
	mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")

	r := runDoctorChecks(cfg, "claude-code", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "inbox_state")
	if got.Severity != severityWarn {
		t.Errorf("inbox_state severity = %q, want WARN (doctor diagnoses; gate enforces blocking on unread)", got.Severity)
	}
	if r.Blockers != 0 {
		t.Errorf("Blockers = %d, want 0 (unread mail is informational in doctor)", r.Blockers)
	}
}

// v0.1.3 hotfix (codex review on d73d4dd): AGENTCHUTE_BIN must point at
// a regular, executable file. A directory at that path was previously
// accepted as "OK" because the check was just os.Stat — hooks would
// then fail to launch the binary because /tmp is not a binary.
func TestDoctorAGENTCHUTE_BINRejectsDirectory(t *testing.T) {
	cfg := newDoctorCfg(t)
	t.Setenv("AGENTCHUTE_BIN", cfg.ControlRepo) // a directory
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "binary_on_path")
	if got.Severity == severityOK {
		t.Errorf("binary_on_path severity = %q, want WARN/BLOCKER for directory at AGENTCHUTE_BIN", got.Severity)
	}
}

// Non-executable regular file at AGENTCHUTE_BIN must also be flagged —
// the hook would launch it via exec which requires the exec bit.
func TestDoctorAGENTCHUTE_BINRejectsNonExecutable(t *testing.T) {
	cfg := newDoctorCfg(t)
	notExec := filepath.Join(cfg.ControlRepo, "not-executable")
	if err := os.WriteFile(notExec, []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_BIN", notExec)
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "binary_on_path")
	if got.Severity == severityOK {
		t.Errorf("binary_on_path severity = %q, want WARN/BLOCKER for non-executable file at AGENTCHUTE_BIN", got.Severity)
	}
}

// Sanity: a real executable file at AGENTCHUTE_BIN does pass.
func TestDoctorAGENTCHUTE_BINAcceptsExecutableFile(t *testing.T) {
	cfg := newDoctorCfg(t)
	exec := filepath.Join(cfg.ControlRepo, "executable-stub")
	if err := os.WriteFile(exec, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCHUTE_BIN", exec)
	r := runDoctorChecks(cfg, "", doctorOptions{Now: time.Now().UTC()})
	got := findCheck(t, r, "binary_on_path")
	if got.Severity != severityOK {
		t.Errorf("binary_on_path severity = %q, want OK for valid executable", got.Severity)
	}
}

func TestCmdDoctorJSONShape(t *testing.T) {
	cfg := newDoctorCfg(t)
	// cmdDoctor calls loop.Discover, which needs AGENTCHUTE.md at the
	// control repo root. newDoctorCfg only sets up the loop dir; add the
	// spec file so discovery succeeds.
	if err := os.WriteFile(filepath.Join(cfg.ControlRepo, "AGENTCHUTE.md"), []byte("# Spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	withCwd(t, cfg.ControlRepo, func() {
		out, err := captureStdout(t, func() error { return cmdDoctor([]string{"--json"}) })
		if err != nil {
			t.Fatalf("cmdDoctor --json: %v", err)
		}
		var report doctorReport
		if jerr := json.Unmarshal([]byte(out), &report); jerr != nil {
			t.Fatalf("unmarshal doctor json: %v\n%s", jerr, out)
		}
		if len(report.Checks) == 0 {
			t.Error("checks empty in JSON output")
		}
	})
}

// Discovery failure → BLOCKER on `discover` + exit nonzero. Confirms
// doctor refuses to falsely declare a broken repo healthy.
func TestCmdDoctorDiscoveryFailureBlocks(t *testing.T) {
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	// No AGENTCHUTE.md, no .agentchute/loop — discovery will fail.
	withCwd(t, root, func() {
		_, err := captureStdout(t, func() error { return cmdDoctor(nil) })
		if err == nil {
			t.Fatal("expected error for missing scaffold")
		}
		if !errors.Is(err, errBlocked) {
			t.Errorf("err = %v, want errBlocked (discovery failure should be a doctor blocker)", err)
		}
	})
}
