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
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
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
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
	got := findCheck(t, r, "hook_content_sanity")
	if got.Severity != severityBlocker {
		t.Errorf("hook_content_sanity severity = %q, want BLOCKER (mixed bare-check + templated must still flag the bare-check)", got.Severity)
	}
}

// Codex review on bff226c: --json discovery failure must still exit
// errBlocked. Previously emitDoctorJSON returned nil before the
// errBlocked guard ran.
func TestCmdDoctorJSONDiscoveryFailureBlocks(t *testing.T) {
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
		WakeMethod:  "",
		WakeTarget:  "",
		LastSeen:    time.Now().UTC().Truncate(time.Second),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentInboxDir("claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := runDoctorChecks(cfg, "claude-code", time.Now().UTC())

	for _, name := range []string{"self_registration", "registration_freshness", "inbox_state", "ledger_state", "wake_target_validity"} {
		c := findCheck(t, r, name)
		if c.Severity == "" {
			t.Errorf("%s has empty severity", name)
		}
	}

	// Self registration must be OK.
	if got := findCheck(t, r, "self_registration"); got.Severity != severityOK {
		t.Errorf("self_registration severity = %q, want OK", got.Severity)
	}

	// No wake target → WARN.
	if got := findCheck(t, r, "wake_target_validity"); got.Severity != severityWarn {
		t.Errorf("wake_target_validity severity = %q, want WARN (no method declared)", got.Severity)
	}
}

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

	r := runDoctorChecks(cfg, "codex", time.Now().UTC())
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

	r := runDoctorChecks(cfg, "codex", time.Now().UTC())
	got := findCheck(t, r, "hook_file_presence")
	if got.Severity != severityBlocker {
		t.Fatalf("hook_file_presence severity = %q, want BLOCKER when codex hook diverged", got.Severity)
	}
	if !strings.Contains(got.Message, "--force") {
		t.Fatalf("message missing force reinstall hint: %q", got.Message)
	}
}

func TestDoctorWarnsOnStaleRegistration(t *testing.T) {
	cfg := newDoctorCfg(t)
	regPath := cfg.AgentRegistrationPath("claude-code")
	reg := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC().Add(-2 * StaleRegThreshold).Truncate(time.Second),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(regPath, reg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentInboxDir("claude-code"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := runDoctorChecks(cfg, "claude-code", time.Now().UTC())
	got := findCheck(t, r, "registration_freshness")
	if got.Severity != severityWarn {
		t.Errorf("stale-reg severity = %q, want WARN (not BLOCKER; doctor diagnoses, gate enforces)", got.Severity)
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

	r := runDoctorChecks(cfg, "claude-code", time.Now().UTC())
	got := findCheck(t, r, "inbox_state")
	if got.Severity != severityWarn {
		t.Errorf("inbox_state severity = %q, want WARN (doctor diagnoses; gate enforces blocking on unread)", got.Severity)
	}
	if r.Blockers != 0 {
		t.Errorf("Blockers = %d, want 0 (unread mail is informational in doctor)", r.Blockers)
	}
}

func TestDoctorRecipientLivenessBlocksWithoutWakeOrPoller(t *testing.T) {
	cfg := newDoctorCfg(t)
	reg := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code"), reg); err != nil {
		t.Fatal(err)
	}
	got := checkRecipientLiveness(cfg, "claude-code", time.Now().UTC())
	if got.Severity != severityBlocker {
		t.Fatalf("recipient_liveness severity = %q, want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Message, "poller ensure") {
		t.Errorf("message should include remediation command: %q", got.Message)
	}
}

func TestDoctorRecipientLivenessAcceptsFreshPoller(t *testing.T) {
	cfg := newDoctorCfg(t)
	reg := &loop.Registration{
		AgentID:     "claude-code",
		Vendor:      "anthropic",
		ControlRepo: cfg.ControlRepo,
		LastSeen:    time.Now().UTC(),
		Status:      loop.StatusActive,
	}
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("claude-code"), reg); err != nil {
		t.Fatal(err)
	}
	mustWriteFreshPollerHeartbeat(t, cfg, "claude-code")
	got := checkRecipientLiveness(cfg, "claude-code", time.Now().UTC())
	if got.Severity != severityOK {
		t.Fatalf("recipient_liveness severity = %q, want OK (%s)", got.Severity, got.Message)
	}
	if !strings.Contains(got.Message, "fresh poller heartbeat") {
		t.Errorf("message should mention fresh heartbeat: %q", got.Message)
	}
}

// v0.1.3 hotfix (codex review on d73d4dd): AGENTCHUTE_BIN must point at
// a regular, executable file. A directory at that path was previously
// accepted as "OK" because the check was just os.Stat — hooks would
// then fail to launch the binary because /tmp is not a binary.
func TestDoctorAGENTCHUTE_BINRejectsDirectory(t *testing.T) {
	cfg := newDoctorCfg(t)
	t.Setenv("AGENTCHUTE_BIN", cfg.ControlRepo) // a directory
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	r := runDoctorChecks(cfg, "", time.Now().UTC())
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
	root := t.TempDir()
	// No AGENTCHUTE.md, no .examplecorp/loop — discovery will fail.
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
