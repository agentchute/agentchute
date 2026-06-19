package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// withFakeHerdr installs a fake `herdr` as herdrProbeBinary. `agent rename`
// always succeeds; `agent get <boundName>` resolves to boundPane (for collision
// tests) and every other target reports agent_not_found.
func withFakeHerdr(t *testing.T, boundName, boundPane string) {
	t.Helper()
	old := herdrProbeBinary
	path := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\n" +
		"sub=\"$2\"\n" +
		"target=\"$3\"\n" +
		"case \"$sub\" in\n" +
		"  rename) exit 0 ;;\n" +
		"  get)\n" +
		"    if [ -n \"" + boundName + "\" ] && [ \"$target\" = \"" + boundName + "\" ]; then\n" +
		"      printf '{\"result\":{\"agent\":{\"pane_id\":\"" + boundPane + "\"}}}\\n'\n" +
		"      exit 0\n" +
		"    fi\n" +
		"    printf '{\"error\":{\"code\":\"agent_not_found\"}}\\n'\n" +
		"    exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrProbeBinary = path
	t.Cleanup(func() { herdrProbeBinary = old })
}

func setupHerdrEnv(t *testing.T, pane string) {
	t.Helper()
	t.Setenv("HERDR_PANE_ID", pane)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr-test.sock")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("AGENTCHUTE_RUNNER", "")
	t.Setenv("AGENTCHUTE_RUNNER_PID", "")
	// The dogfood session exports its own AGENTCHUTE_AGENT_ID; clear it so
	// identity resolution exercises detection/adoption, not the ambient lane.
	t.Setenv("AGENTCHUTE_AGENT_ID", "")
}

func mustExampleRepo(t *testing.T, root string) {
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
}

func readExampleReg(t *testing.T, root, agentID string) *loop.Registration {
	t.Helper()
	reg, err := loop.ReadRegistration(filepath.Join(root, ".examplecorp", "loop", "agents", agentID+".md"))
	if err != nil {
		t.Fatalf("read registration %s: %v", agentID, err)
	}
	return reg
}

func TestResolveWakeExplicitHerdrOutsidePaneWarns(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", "")

	method, target, preserved, warnings := resolveWakeForRegistration(registerOpts{
		AgentID:            "test-agent",
		WakeMethod:         "herdr",
		WakeMethodProvided: true,
	}, nil)
	if method != "" || target != "" {
		t.Fatalf("explicit herdr outside pane = method %q target %q, want non-pokable", method, target)
	}
	if preserved {
		t.Fatalf("explicit herdr clear path = preservedFromExisting true, want false (deliberate clear)")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "HERDR_PANE_ID unset") {
		t.Fatalf("warnings = %#v, want HERDR_PANE_ID warning", warnings)
	}
}

func TestRenameCurrentHerdrAgentIncludesCommandOutput(t *testing.T) {
	old := herdrProbeBinary
	path := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\n" +
		"printf 'rename denied\\n' >&2\n" +
		"exit 42\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrProbeBinary = path
	t.Cleanup(func() { herdrProbeBinary = old })
	t.Setenv("HERDR_PANE_ID", "w3:p7")

	err := renameCurrentHerdrAgent("test-agent")
	if err == nil {
		t.Fatal("renameCurrentHerdrAgent succeeded, want error")
	}
	if !strings.Contains(err.Error(), "rename denied") {
		t.Fatalf("error = %q, want command output", err)
	}
}

// A bare launch inside a herdr pane auto-registers wake_method=herdr with the
// agent id as the stable target.
func TestRegisterAutoDetectsHerdrPane(t *testing.T) {
	withFakeHerdr(t, "", "")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("cmdRegister failed: %v", err)
		}
		reg := readExampleReg(t, root, "test-agent")
		if reg.WakeMethod != "herdr" {
			t.Errorf("WakeMethod = %q, want herdr", reg.WakeMethod)
		}
		if reg.WakeTarget != "test-agent" {
			t.Errorf("WakeTarget = %q, want the agent id (stable name)", reg.WakeTarget)
		}
	})
}

// herdr takes precedence over tmux when both terminal envs are present.
func TestRegisterHerdrWinsOverTmux(t *testing.T) {
	withFakeHerdr(t, "", "")
	withFakeTmuxTargets(t, "%99")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")
		t.Setenv("TMUX_PANE", "%99")

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("cmdRegister failed: %v", err)
		}
		reg := readExampleReg(t, root, "test-agent")
		if reg.WakeMethod != "herdr" {
			t.Errorf("WakeMethod = %q, want herdr (herdr precedes tmux)", reg.WakeMethod)
		}
	})
}

// Under `agentchute run` (AGENTCHUTE_RUNNER_PID set) the runner socket wake must
// be preserved even inside a herdr pane — boot/self-check must not switch to
// herdr just because HERDR_ENV is also set.
func TestRegisterUnderRunnerKeepsRunnerWakeInHerdr(t *testing.T) {
	withFakeHerdr(t, "", "")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")
		t.Setenv("AGENTCHUTE_RUNNER", "1")
		t.Setenv("AGENTCHUTE_RUNNER_PID", "4242")

		// Pre-existing runner registration (as run.go would have written).
		runnerTarget := loop.RunnerWakeTarget("/tmp/runner-test.sock")
		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor", "--wake-method", loop.RunnerWakeMethod, "--wake-target", runnerTarget}); err != nil {
			t.Fatalf("seed runner registration failed: %v", err)
		}

		// A subsequent non-explicit register (the child's boot) must NOT flip to herdr.
		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("cmdRegister failed: %v", err)
		}
		reg := readExampleReg(t, root, "test-agent")
		if reg.WakeMethod != loop.RunnerWakeMethod {
			t.Errorf("WakeMethod = %q, want runner wake preserved under the runner", reg.WakeMethod)
		}
		if reg.WakeTarget != runnerTarget {
			t.Errorf("WakeTarget = %q, want %q", reg.WakeTarget, runnerTarget)
		}
	})
}

// A bare resolve (no --as / AGENTCHUTE_AGENT_ID) inside a herdr pane adopts the
// agent id of the registration whose stable herdr name maps to this pane —
// instead of falling through to the contextual default and splitting the inbox.
func TestResolveAgentIDAdoptsHerdrPane(t *testing.T) {
	withFakeHerdr(t, "claude-code-foo", "w3:p7")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")

		if err := cmdRegister([]string{"--as", "claude-code-foo", "--vendor", "anthropic"}); err != nil {
			t.Fatalf("seed herdr registration failed: %v", err)
		}
		if reg := readExampleReg(t, root, "claude-code-foo"); reg.WakeMethod != "herdr" {
			t.Fatalf("seed reg should be herdr, got %q", reg.WakeMethod)
		}

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		id, err := resolveAgentID("", "anthropic", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if id != "claude-code-foo" {
			t.Errorf("resolveAgentID = %q, want adopted claude-code-foo", id)
		}
	})
}

// If both herdr and tmux env are present, herdr identity adoption wins to match
// wake auto-detection precedence for bare herdr launches.
func TestResolveAgentIDHerdrWinsOverTmuxWhenBothEnvsPresent(t *testing.T) {
	withFakeHerdr(t, "codex-herdr", "w3:p7")
	withFakeTmuxTargets(t, "%7")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")
		t.Setenv("TMUX_PANE", "%7")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		host, _ := os.Hostname()
		for _, reg := range []*loop.Registration{
			{
				AgentID:     "codex-tmux",
				Vendor:      "openai",
				ControlRepo: root,
				Host:        host,
				WakeMethod:  "tmux",
				WakeTarget:  "%7",
				LastSeen:    time.Now().UTC(),
				Status:      loop.StatusActive,
			},
			{
				AgentID:     "codex-herdr",
				Vendor:      "openai",
				ControlRepo: root,
				Host:        host,
				WakeMethod:  "herdr",
				WakeTarget:  "codex-herdr",
				LastSeen:    time.Now().UTC(),
				Status:      loop.StatusActive,
			},
		} {
			if err := loop.WriteRegistration(cfg.AgentRegistrationPath(reg.AgentID), reg); err != nil {
				t.Fatal(err)
			}
			mustMkdir(t, cfg.AgentInboxDir(reg.AgentID))
		}

		id, err := resolveAgentID("", "openai", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if id != "codex-herdr" {
			t.Errorf("resolveAgentID = %q, want herdr registration", id)
		}
	})
}

func TestAgentIDForCurrentHerdrPaneRequiresHerdrEnvMarker(t *testing.T) {
	withFakeHerdr(t, "codex-herdr", "w3:p7")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		t.Setenv("HERDR_PANE_ID", "w3:p7")
		t.Setenv("HERDR_ENV", "")
		t.Setenv("HERDR_SOCKET_PATH", "")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		host, _ := os.Hostname()
		reg := &loop.Registration{
			AgentID:     "codex-herdr",
			Vendor:      "openai",
			ControlRepo: root,
			Host:        host,
			WakeMethod:  "herdr",
			WakeTarget:  "codex-herdr",
			LastSeen:    time.Now().UTC(),
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath(reg.AgentID), reg); err != nil {
			t.Fatal(err)
		}

		if id, ok := agentIDForCurrentHerdrPane(cfg, "openai"); ok || id != "" {
			t.Fatalf("agentIDForCurrentHerdrPane = %q, %v; want no adoption without herdr env marker", id, ok)
		}
	})
}

// herdr env present but the `herdr` binary unavailable must NOT register a herdr
// wake (it could neither rename nor poke) — the agent enrolls non-pokable.
func TestRegisterHerdrEnvMissingBinaryIsNonPokable(t *testing.T) {
	old := herdrProbeBinary
	herdrProbeBinary = filepath.Join(t.TempDir(), "definitely-not-herdr")
	t.Cleanup(func() { herdrProbeBinary = old })
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("cmdRegister failed: %v", err)
		}
		if reg := readExampleReg(t, root, "test-agent"); reg.WakeMethod == "herdr" {
			t.Errorf("herdr env but missing binary should NOT register herdr wake; got method=%q", reg.WakeMethod)
		}
	})
}

// An explicit identity whose herdr name is already bound to a different pane is
// not hijacked: the herdr wake is skipped rather than registered ambiguously.
func TestRegisterExplicitHerdrNameCollisionSkipsHerdrWake(t *testing.T) {
	withFakeHerdr(t, "taken-agent", "w9:pOther")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustExampleRepo(t, root)
		setupHerdrEnv(t, "w3:p7")

		if err := cmdRegister([]string{"--as", "taken-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("cmdRegister failed: %v", err)
		}
		reg := readExampleReg(t, root, "taken-agent")
		if reg.WakeMethod == "herdr" {
			t.Errorf("explicit identity colliding with another pane should NOT register herdr wake; got method=%q target=%q", reg.WakeMethod, reg.WakeTarget)
		}
	})
}
