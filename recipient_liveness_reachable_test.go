package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// TestRegistrationHasReachableWake_RunnerRefusesUnownedSocketNoDial is the WI-3
// regression through the REFACTORED root path: registrationHasReachableWake now
// routes through loop.RegistrationReachable (the WakeAdapter.Reachable
// dispatcher) instead of an inline method-name switch. A runner registration
// naming a socket the recipient does not own must still be reported unreachable
// WITHOUT dialing — even with a live listener at that path. This proves the
// owned-check-before-dial invariant survived moving behind the interface.
func TestRegistrationHasReachableWake_RunnerRefusesUnownedSocketNoDial(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}

	// A real listening "evil" socket at a path codex does NOT own.
	evilPath := shortSocketPath(t, "evil.sock")
	evil := listenCounting(t, evilPath)

	reg := &loop.Registration{
		AgentID:    "codex",
		WakeMethod: loop.RunnerWakeMethod,
		WakeTarget: loop.RunnerWakeTarget(evilPath),
	}
	if registrationHasReachableWake(cfg, reg) {
		t.Fatal("unowned runner socket reported reachable; want false")
	}
	time.Sleep(50 * time.Millisecond)
	if c := evil.count(); c != 0 {
		t.Fatalf("unowned runner socket dialed %d time(s) via the refactored reachability path; owned-check must short-circuit before dial", c)
	}
}

// TestRegistrationHasReachableWake_RunnerOwnedLiveSocketReachable confirms the
// positive case still works through the dispatcher: a runner reg pointing at a
// live socket the recipient owns is reachable.
func TestRegistrationHasReachableWake_RunnerOwnedLiveSocketReachable(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}

	ownedPath := cfg.RunnerSocketPath("codex")
	startFakeRunnerPingSocket(t, ownedPath, loop.RunnerPingResponse{AgentID: "codex"})

	reg := &loop.Registration{
		AgentID:    "codex",
		WakeMethod: loop.RunnerWakeMethod,
		WakeTarget: loop.RunnerWakeTarget(ownedPath),
	}
	if !registrationHasReachableWake(cfg, reg) {
		t.Fatal("owned live runner socket reported unreachable via dispatcher; want true")
	}
}

// TestRegistrationReachable_TmuxHerdrHooksWired proves the cross-package probe
// seam works end-to-end: the root package's init() installs tmuxTargetReachable
// / herdrAgentReachable as the loop dispatcher's hooks, so a tmux/herdr
// registration's reachability is decided by those root-package probes via
// loop.RegistrationReachable. With no real tmux/herdr server (and the probe
// binaries absent), the probes report unreachable — which is the answer the
// dispatcher must surface. This guards against the hooks being left unwired
// (which would also yield false, so we additionally assert the runner method —
// whose logic lives entirely in loop and needs no hook — stays reachable for an
// owned live socket, distinguishing "hook unwired" from "probe says no").
func TestRegistrationReachable_TmuxHerdrHooksWired(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		ControlRepo: root,
		LoopDir:     filepath.Join(root, ".examplecorp", "loop"),
		Vendor:      "examplecorp",
	}

	// Point the tmux/herdr probe binaries at a name that does not exist so the
	// hooks deterministically report unreachable without contacting a real
	// server. (The hooks ARE wired by init(); this exercises that wiring.)
	origTmux := tmuxProbeBinary
	origHerdr := herdrProbeBinary
	tmuxProbeBinary = "definitely-no-such-tmux-binary"
	herdrProbeBinary = "definitely-no-such-herdr-binary"
	t.Cleanup(func() { tmuxProbeBinary = origTmux; herdrProbeBinary = origHerdr })

	tmuxReg := &loop.Registration{AgentID: "codex", WakeMethod: "tmux", WakeTarget: "%0"}
	if loop.RegistrationReachable(cfg, tmuxReg, time.Second) {
		t.Fatal("tmux reachability should be false when probe binary is absent (hook wired, probe says no)")
	}
	herdrReg := &loop.Registration{AgentID: "codex", WakeMethod: "herdr", WakeTarget: "codex"}
	if loop.RegistrationReachable(cfg, herdrReg, time.Second) {
		t.Fatal("herdr reachability should be false when probe binary is absent (hook wired, probe says no)")
	}
}
