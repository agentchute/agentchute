package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// WI-E3: a SessionStart-class `boot` enroll (not under the runner) records
// launched_by=hook with the hook event.
func TestBootSetsHookProvenance(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("AGENTCHUTE_RUNNER", "")
		t.Setenv("TMUX_PANE", "")
		t.Setenv("HERDR_PANE_ID", "")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := cmdBoot([]string{"--as", "claude-code", "--vendor", "anthropic", "--quiet"}); err != nil {
			t.Fatal(err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("claude-code"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.LaunchedBy != loop.LaunchedByHook {
			t.Fatalf("LaunchedBy = %q, want %q", reg.LaunchedBy, loop.LaunchedByHook)
		}
		if reg.HookEvent != "boot" {
			t.Fatalf("HookEvent = %q, want boot", reg.HookEvent)
		}
	})
}

// WI-E3: a boot enroll firing INSIDE the runner (AGENTCHUTE_RUNNER=1) records
// launched_by=runner, not hook — the runner owns the lane and provenance must
// not be demoted.
func TestBootUnderRunnerKeepsRunnerProvenance(t *testing.T) {
	withFakeTmuxTargets(t, "%7")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("AGENTCHUTE_RUNNER", "1")
		t.Setenv("TMUX_PANE", "%7")
		t.Setenv("HERDR_PANE_ID", "")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		socketPath := cfg.RunnerSocketPath("claude-code")
		startFakeRunnerPingSocket(t, socketPath, loop.RunnerPingResponse{AgentID: "claude-code"})
		runnerTarget := loop.RunnerWakeTarget(socketPath)
		if err := cmdRegister([]string{"--as", "claude-code", "--vendor", "anthropic", "--wake-method", loop.RunnerWakeMethod, "--wake-target", runnerTarget}); err != nil {
			t.Fatalf("seed runner registration failed: %v", err)
		}
		if err := cmdBoot([]string{"--as", "claude-code", "--vendor", "anthropic", "--quiet"}); err != nil {
			t.Fatal(err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("claude-code"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.LaunchedBy != loop.LaunchedByRunner {
			t.Fatalf("LaunchedBy = %q, want %q (boot under runner must not demote)", reg.LaunchedBy, loop.LaunchedByRunner)
		}
		if reg.WakeMethod != loop.RunnerWakeMethod || reg.WakeTarget != runnerTarget {
			t.Fatalf("wake = %s/%s, want %s/%s (boot under runner must not demote to tmux)", reg.WakeMethod, reg.WakeTarget, loop.RunnerWakeMethod, runnerTarget)
		}
	})
}

// WI-E3: registerRunner records launched_by=runner and the fronting shim name.
func TestRunnerSetsRunnerProvenance(t *testing.T) {
	root := setupShortRunFixture(t)
	cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	opts := runnerOptions{AgentID: "claude-code", Vendor: "anthropic", ShimName: "ac-claude"}
	socketPath := cfg.RunnerSocketPath(opts.AgentID)
	if err := registerRunner(cfg, opts, socketPath, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(opts.AgentID))
	if err != nil {
		t.Fatal(err)
	}
	if reg.LaunchedBy != loop.LaunchedByRunner {
		t.Fatalf("LaunchedBy = %q, want %q", reg.LaunchedBy, loop.LaunchedByRunner)
	}
	if reg.ShimName != "ac-claude" {
		t.Fatalf("ShimName = %q, want ac-claude", reg.ShimName)
	}
	if reg.WakeMethod != loop.RunnerWakeMethod {
		t.Fatalf("WakeMethod = %q, want %q", reg.WakeMethod, loop.RunnerWakeMethod)
	}
}

// WI-E3: a raw/passthrough enroll is the manual path. A hand-run `agentchute
// register` records launched_by=manual; and the shim-fronted manual enroll
// (performRegister with a shim name) records manual + the shim name.
func TestShimPassthroughSetsManualProvenance(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("AGENTCHUTE_RUNNER", "")
		t.Setenv("TMUX_PANE", "")
		t.Setenv("HERDR_PANE_ID", "")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}

		// (a) hand-run register → manual.
		if err := cmdRegister([]string{"--as", "gemini-cli", "--vendor", "google"}); err != nil {
			t.Fatal(err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("gemini-cli"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.LaunchedBy != loop.LaunchedByManual {
			t.Fatalf("cmdRegister LaunchedBy = %q, want %q", reg.LaunchedBy, loop.LaunchedByManual)
		}

		// (b) shim-fronted manual enroll → manual + shim name.
		opts := registerOpts{
			AgentID:    "codex",
			Vendor:     "openai",
			LaunchedBy: loop.LaunchedByManual,
			ShimName:   "ac-codex",
		}
		if _, err := performRegister(cfg, opts, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		reg2, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
		if err != nil {
			t.Fatal(err)
		}
		if reg2.LaunchedBy != loop.LaunchedByManual {
			t.Fatalf("performRegister LaunchedBy = %q, want %q", reg2.LaunchedBy, loop.LaunchedByManual)
		}
		if reg2.ShimName != "ac-codex" {
			t.Fatalf("performRegister ShimName = %q, want ac-codex", reg2.ShimName)
		}
	})
}

// WI-E3: a re-register that supplies NO provenance preserves the previously
// recorded provenance (a last_seen-style refresh must not wipe how the lane
// enrolled).
func TestPerformRegisterPreservesProvenanceOnReRegister(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		// Seed with runner provenance.
		seed := registerOpts{AgentID: "codex", Vendor: "openai", LaunchedBy: loop.LaunchedByRunner, ShimName: "ac-codex"}
		if _, err := performRegister(cfg, seed, now); err != nil {
			t.Fatal(err)
		}
		// Re-register WITHOUT provenance (plain refresh).
		if _, err := performRegister(cfg, registerOpts{AgentID: "codex", Vendor: "openai"}, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.LaunchedBy != loop.LaunchedByRunner || reg.ShimName != "ac-codex" {
			t.Fatalf("re-register wiped provenance: LaunchedBy=%q ShimName=%q", reg.LaunchedBy, reg.ShimName)
		}
	})
}
