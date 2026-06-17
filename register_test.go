package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Concurrent SessionStart commands (boot + self-check fire from the same hook)
// share one tmux pane and one contextual base. Both resolve the base before
// either write is visible; the exclusive-write loser used to fall into the
// os.IsExist loop and suffix itself to "<base>-2", producing duplicate live
// registrations for one wrapper in one pane. The collision handler must instead
// re-read the now-visible same-pane same-vendor registration and adopt it.
func TestPerformRegisterConcurrentSamePaneReusesBase(t *testing.T) {
	withFakeTmuxTargets(t, "%1")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("TMUX_PANE", "%1")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		base := "claude-code-" + getFolderSlug(root)

		// Race many startup commands at once. With the bug, at least one loses
		// the exclusive create and suffixes to "<base>-2".
		const racers = 12
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, racers)
		now := time.Now().UTC()
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				opts := registerOpts{
					AgentID:            base,
					Vendor:             "anthropic",
					ContextualIdentity: true,
					ContextualBaseID:   base,
					PruneStalePeerTmux: true,
				}
				if _, err := performRegister(cfg, opts, now); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("performRegister racer failed: %v", err)
		}
		if t.Failed() {
			t.FailNow()
		}

		entries, err := os.ReadDir(cfg.AgentsDir())
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				ids = append(ids, strings.TrimSuffix(e.Name(), ".md"))
			}
		}
		if len(ids) != 1 || ids[0] != base {
			t.Fatalf("concurrent same-pane register produced duplicate identities %v, want exactly [%s]", ids, base)
		}
	})
}

func TestRegisterAutoDetectsTmuxPane(t *testing.T) {
	withFakeTmuxTargets(t, "%99")
	root := t.TempDir()
	withCwd(t, root, func() {
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
	})
}

// Re-running register without --wake-target and without TMUX_PANE set
// preserves the existing wake target. Active wrapper hooks use self-check
// for aggressive wake reconciliation; register keeps the historical
// explicit-enrollment merge behavior.
func TestRegisterReRunPreservesExistingWakeTarget(t *testing.T) {
	withFakeTmuxTargets(t, "%42")
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
		if reg.WakeMethod != "tmux" || reg.WakeTarget != "%42" {
			t.Errorf("expected preserved tmux wake, got method=%q target=%q", reg.WakeMethod, reg.WakeTarget)
		}
	})
}

// Explicit --wake-target "" on re-run still clears the binding.
func TestRegisterReRunExplicitEmptyClearsWakeTarget(t *testing.T) {
	withFakeTmuxTargets(t, "%42")
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

func TestRegisterPrunesStaleSameHostPeerTmuxRegistration(t *testing.T) {
	withFakeTmuxTargets(t, "%1")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		host, _ := os.Hostname()
		stale := &loop.Registration{
			AgentID:     "grok",
			Vendor:      "xai",
			ControlRepo: root,
			Host:        host,
			WakeMethod:  "tmux",
			WakeTarget:  "%9",
			LastSeen:    time.Now().UTC().Truncate(time.Second),
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("grok"), stale); err != nil {
			t.Fatal(err)
		}
		remote := *stale
		remote.AgentID = "remote"
		remote.Host = "other-host"
		remote.WakeTarget = "%8"
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("remote"), &remote); err != nil {
			t.Fatal(err)
		}

		t.Setenv("TMUX_PANE", "%1")
		out, err := captureStdout(t, func() error {
			return cmdRegister([]string{"--as", "test-agent", "--vendor", "test"})
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if !strings.Contains(out, "pruned_tmux:") {
			t.Fatalf("register output did not report stale tmux pruning:\n%s", out)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath("grok")); !os.IsNotExist(err) {
			t.Fatalf("same-host stale tmux registration should be removed, stat err=%v", err)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath("remote")); err != nil {
			t.Fatalf("cross-host stale tmux registration should remain: %v", err)
		}
	})
}

func TestRegisterPrunesSamePaneTmuxRegistration(t *testing.T) {
	withFakeTmuxTargets(t, "%1")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		host, _ := os.Hostname()
		old := &loop.Registration{
			AgentID:     "old-agent",
			Vendor:      "anthropic",
			ControlRepo: root,
			Host:        host,
			WakeMethod:  "tmux",
			WakeTarget:  "%1",
			LastSeen:    time.Now().UTC().Truncate(time.Second),
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath(old.AgentID), old); err != nil {
			t.Fatal(err)
		}

		t.Setenv("TMUX_PANE", "%1")
		out, err := captureStdout(t, func() error {
			return cmdRegister([]string{"--as", "new-agent", "--vendor", "openai"})
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if !strings.Contains(out, "pruned_tmux:") {
			t.Fatalf("register output did not report same-pane pruning:\n%s", out)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath(old.AgentID)); !os.IsNotExist(err) {
			t.Fatalf("same-pane tmux registration should be removed, stat err=%v", err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("new-agent"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeMethod != "tmux" || reg.WakeTarget != "%1" {
			t.Fatalf("new registration wake = %s:%s, want tmux:%%1", reg.WakeMethod, reg.WakeTarget)
		}
	})
}

func TestPerformRegisterConcurrentSamePaneKeepsSingleRegistration(t *testing.T) {
	withFakeTmuxTargets(t, "%7")
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		t.Setenv("TMUX_PANE", "%7")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}

		racers := []registerOpts{
			{AgentID: "claude-code-proj", Vendor: "anthropic", PruneStalePeerTmux: true},
			{AgentID: "codex-proj", Vendor: "openai", PruneStalePeerTmux: true},
			{AgentID: "gemini-cli-proj", Vendor: "google", PruneStalePeerTmux: true},
			{AgentID: "grok-proj", Vendor: "xai", PruneStalePeerTmux: true},
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, len(racers))
		now := time.Now().UTC()
		for _, opts := range racers {
			opts := opts
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := performRegister(cfg, opts, now); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("performRegister racer failed: %v", err)
		}
		if t.Failed() {
			t.FailNow()
		}

		entries, err := os.ReadDir(cfg.AgentsDir())
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				ids = append(ids, strings.TrimSuffix(e.Name(), ".md"))
			}
		}
		if len(ids) != 1 {
			t.Fatalf("concurrent same-pane register produced identities %v, want exactly one", ids)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(ids[0]))
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeMethod != "tmux" || reg.WakeTarget != "%7" {
			t.Fatalf("surviving registration wake = %s:%s, want tmux:%%7", reg.WakeMethod, reg.WakeTarget)
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
	withFakeTmuxTargets(t, "%99")
	root := t.TempDir()
	withCwd(t, root, func() {
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
	})
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
