package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestRegisterAutoDetectsTmuxPane(t *testing.T) {
	root := t.TempDir()
	origCwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origCwd); err != nil {
			t.Errorf("failed to restore cwd: %v", err)
		}
	}()

	// Setup a dummy repo
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

	// Set TMUX_PANE
	t.Setenv("TMUX_PANE", "%99")

	// Run register without --wake-target
	args := []string{"--as", "test-agent", "--vendor", "test-vendor"}
	if err := cmdRegister(args); err != nil {
		t.Fatalf("cmdRegister failed: %v", err)
	}

	// Check registration file
	regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test-agent.md")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatalf("failed to read registration: %v", err)
	}
	if reg.WakeTarget != "%99" {
		t.Errorf("expected WakeTarget %%99, got %q", reg.WakeTarget)
	}
}

// Re-running register without --wake-target and without TMUX_PANE set
// must preserve the existing registration's wake_target rather than
// silently clearing it. Explicit --wake-target "" remains the way to clear.
func TestRegisterReRunPreservesExistingWakeTarget(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		t.Setenv("TMUX_PANE", "%42")
		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("initial register: %v", err)
		}

		// Re-run outside tmux (no TMUX_PANE, no --wake-target). Must preserve %42.
		os.Unsetenv("TMUX_PANE")
		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("re-register: %v", err)
		}

		regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test-agent.md")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeTarget != "%42" {
			t.Errorf("expected preserved WakeTarget %%42, got %q", reg.WakeTarget)
		}
	})
}

// Explicit --wake-target "" on re-run still clears the binding.
func TestRegisterReRunExplicitEmptyClearsWakeTarget(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		t.Setenv("TMUX_PANE", "%42")
		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor"}); err != nil {
			t.Fatalf("initial register: %v", err)
		}

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test-vendor", "--wake-target", ""}); err != nil {
			t.Fatalf("re-register with empty target: %v", err)
		}

		regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test-agent.md")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeTarget != "" {
			t.Errorf("expected cleared WakeTarget, got %q", reg.WakeTarget)
		}
	})
}

// Re-running register on an agent that was previously marked exhausted
// or offline (e.g., by its own wrapper after token exhaustion) must reset
// Status to active and clear RestartAt. Otherwise the agent stays hidden
// from watchdog pokes until the registration is hand-edited, defeating the
// purpose of re-enrolling.
func TestRegisterClearsStaleStatusAndRestartAt(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test", "--wake-method", "tmux", "--wake-target", "%0"}); err != nil {
			t.Fatal(err)
		}

		regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test-agent.md")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour).UTC()
		reg.Status = loop.StatusExhausted
		reg.RestartAt = &future
		if err := loop.WriteRegistration(regPath, reg); err != nil {
			t.Fatal(err)
		}

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test", "--wake-method", "tmux", "--wake-target", "%0"}); err != nil {
			t.Fatal(err)
		}

		reg, err = loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.Status != loop.StatusActive {
			t.Errorf("Status = %q, want active", reg.Status)
		}
		if reg.RestartAt != nil {
			t.Errorf("RestartAt = %v, want nil", reg.RestartAt)
		}
	})
}

// --bio sets the registration body. Without --bio on a re-register, the
// existing body is preserved (idempotence). With --bio, the body is
// overwritten with the new text.
func TestRegisterBioFlagSetsAndOverwritesBody(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		if err := cmdRegister([]string{"--as", "test", "--vendor", "test", "--wake-target", "", "--bio", "first bio"}); err != nil {
			t.Fatal(err)
		}

		regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test.md")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reg.Body, "first bio") {
			t.Errorf("body did not contain bio: %q", reg.Body)
		}

		// Re-register without --bio: existing body preserved.
		if err := cmdRegister([]string{"--as", "test", "--vendor", "test", "--wake-target", ""}); err != nil {
			t.Fatal(err)
		}
		reg, err = loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reg.Body, "first bio") {
			t.Errorf("re-register without --bio dropped body: %q", reg.Body)
		}

		// Re-register with --bio: body replaced.
		if err := cmdRegister([]string{"--as", "test", "--vendor", "test", "--wake-target", "", "--bio", "second bio"}); err != nil {
			t.Fatal(err)
		}
		reg, err = loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(reg.Body, "first bio") {
			t.Errorf("--bio did not replace previous body: %q", reg.Body)
		}
		if !strings.Contains(reg.Body, "second bio") {
			t.Errorf("--bio did not set new body: %q", reg.Body)
		}
	})
}

func TestRegisterExplicitEmptyTmuxPaneOverridesEnv(t *testing.T) {
	root := t.TempDir()
	origCwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origCwd); err != nil {
			t.Errorf("failed to restore cwd: %v", err)
		}
	}()

	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

	t.Setenv("TMUX_PANE", "%99")

	args := []string{"--as", "test-agent", "--vendor", "test-vendor", "--wake-target", ""}
	if err := cmdRegister(args); err != nil {
		t.Fatalf("cmdRegister failed: %v", err)
	}

	regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test-agent.md")
	reg, err := loop.ReadRegistration(regPath)
	if err != nil {
		t.Fatalf("failed to read registration: %v", err)
	}
	if reg.WakeTarget != "" {
		t.Errorf("expected empty WakeTarget, got %q", reg.WakeTarget)
	}
}

// TMUX_PANE in env MUST NOT be auto-bound as wake_target when the user
// explicitly chose a non-tmux wake_method. Otherwise we silently bind a
// tmux pane id to a wezterm/kitty/etc. adapter that can't reach it.
func TestRegisterExplicitNonTmuxMethodIgnoresTmuxPane(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		t.Setenv("TMUX_PANE", "%99")
		// User explicitly chose wezterm but didn't pass --wake-target.
		// The bare wake_method without wake_target must fail validation
		// (AGENTCHUTE.md §5) — NOT silently bind %99.
		err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test", "--wake-method", "wezterm"})
		if err == nil {
			t.Fatal("expected wake_target required error for explicit non-tmux method without target")
		}
		if !strings.Contains(err.Error(), "wake_target is required") {
			t.Fatalf("expected wake_target required error, got %v", err)
		}
	})
}

// TMUX_PANE in env MUST NOT be auto-bound as wake_target when the user
// explicitly chose empty wake_method (non-pokable).
func TestRegisterExplicitEmptyMethodIgnoresTmuxPane(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		t.Setenv("TMUX_PANE", "%99")
		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test", "--wake-method", ""}); err != nil {
			t.Fatalf("expected non-pokable registration to succeed, got %v", err)
		}

		regPath := filepath.Join(root, ".examplecorp", "loop", "agents", "test-agent.md")
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeMethod != "" {
			t.Errorf("expected empty WakeMethod, got %q", reg.WakeMethod)
		}
		if reg.WakeTarget != "" {
			t.Errorf("expected empty WakeTarget (not auto-bound from TMUX_PANE), got %q", reg.WakeTarget)
		}
	})
}
