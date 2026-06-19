package main

import (
	"fmt"
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

// TestRegister_RMWUnderAgentLock drives a concurrent performRegister
// (existing-merge path) against many UpdateLastSeen calls and asserts the
// registration file is never torn (always parses) — the lost-update / file-tear
// surface Fix A closes by running performRegisterOnce's read of `existing` and
// the write inside one WithAgentLock.
func TestRegister_RMWUnderAgentLock(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		now := time.Now().UTC()

		// Seed an existing registration so performRegister takes the merge path.
		seed := registerOpts{AgentID: agentID, Vendor: "anthropic"}
		if _, err := performRegister(cfg, seed, now); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		errs := make(chan error, 128)
		// Many performRegister merges (read existing → write) concurrently...
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				opts := registerOpts{AgentID: agentID, Vendor: "anthropic"}
				if _, err := performRegister(cfg, opts, now.Add(time.Duration(i)*time.Second)); err != nil {
					errs <- err
				}
			}(i)
		}
		// ...racing UpdateLastSeen on the same registration.
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := loop.UpdateLastSeen(cfg, agentID, now.Add(time.Duration(i)*time.Minute)); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent register/update: %v", err)
		}

		// The file must always be readable (never half-written / torn).
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
		if err != nil {
			t.Fatalf("registration torn / unreadable after concurrency: %v", err)
		}
		if reg.AgentID != agentID {
			t.Fatalf("agent_id = %q, want %q", reg.AgentID, agentID)
		}
	})
}

// TestRegister_NoLostUpdateVsConcurrentUpdateLastSeen asserts that a
// performRegister merge cannot silently clobber a concurrently-recorded
// last_active. performRegister preserves existing.LastActive across the
// read-merge-write; without the lock its stale read could overwrite a
// last_active written by an interleaved UpdateLastActive (lost update). With
// Fix A the read and write are atomic under WithAgentLock, so the recorded
// last_active survives.
func TestRegister_NoLostUpdateVsConcurrentUpdateLastSeen(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		base := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

		// Seed.
		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, base); err != nil {
			t.Fatal(err)
		}

		lastActive := base.Add(48 * time.Hour)
		var wg sync.WaitGroup
		errs := make(chan error, 64)

		// Writer that records a definite last_active, racing many re-registers.
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := loop.UpdateLastActive(cfg, agentID, lastActive); err != nil {
				errs <- err
			}
		}()
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, base.Add(time.Duration(i)*time.Second)); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent mutation: %v", err)
		}

		// After UpdateLastActive committed, a re-register merge must preserve it,
		// not roll it back to nil. Run one final register to settle ordering, then
		// assert last_active is present.
		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID))
		if err != nil {
			t.Fatal(err)
		}
		if reg.LastActive == nil {
			t.Fatal("last_active was clobbered to nil by a stale register merge (lost update)")
		}
		if !reg.LastActive.Equal(lastActive) {
			t.Fatalf("last_active = %v, want %v (preserved across merge)", reg.LastActive, lastActive)
		}
	})
}

// TestRegister_InboxExistsBeforeRegistrationVisible: after register returns the
// inbox dir exists; Fix A2 additionally guarantees the inbox/state dirs are
// created BEFORE the registration file is published, so a peer can never observe
// a live registration with no inbox. We assert the post-condition (inbox exists)
// and, as an ordering probe, that the inbox dir's creation does not lag behind a
// readable registration.
func TestRegister_InboxExistsBeforeRegistrationVisible(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"

		res, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}

		// Post-condition: inbox exists once register returns.
		inbox := cfg.AgentInboxDir(agentID)
		info, err := os.Stat(inbox)
		if err != nil {
			t.Fatalf("inbox dir missing after register: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("inbox path %s is not a directory", inbox)
		}
		if res.InboxDir != inbox {
			t.Fatalf("result InboxDir = %q, want %q", res.InboxDir, inbox)
		}

		// Ordering invariant (Fix A2): whenever the registration file is visible,
		// the inbox dir is too. Since both exist now, the weaker observable check
		// is that a reader of the registration also finds the inbox. We assert the
		// registration parses AND the inbox exists — the code guarantees inbox is
		// created strictly before the registration write under the lock.
		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err != nil {
			t.Fatalf("registration unreadable: %v", err)
		}
		if _, err := os.Stat(inbox); err != nil {
			t.Fatalf("registration visible but inbox missing: %v", err)
		}
	})
}

func TestPerformRegisterConcurrentSameHerdrPaneReusesBase(t *testing.T) {
	root := t.TempDir()
	base := "claude-code-" + getFolderSlug(root)
	withFakeHerdr(t, base, "w3:p7")
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		setupHerdrEnv(t, "w3:p7")

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}

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
			t.Fatalf("concurrent same-herdr-pane register produced duplicate identities %v, want exactly [%s]", ids, base)
		}
		reg := readExampleReg(t, root, base)
		if reg.WakeMethod != "herdr" || reg.WakeTarget != base {
			t.Fatalf("registration wake = %s:%s, want herdr:%s", reg.WakeMethod, reg.WakeTarget, base)
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

// TestRegister_PreservedWakeUsesInLockFreshExisting closes the WI-1 follow-up
// defect: in a preserve-from-existing path, publishRegistrationOnce must take
// the wake fields from the AUTHORITATIVE in-lock re-read of `existing`, not from
// the stale pre-lock values resolveWakeForRegistration captured. We simulate a
// concurrent locked writer by mutating the on-disk registration to wake=NEW
// AFTER the pre-lock snapshot (passed in as OLD) but BEFORE publish takes the
// lock. The written registration must reflect NEW.
func TestRegister_PreservedWakeUsesInLockFreshExisting(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "test-agent"
		now := time.Now().UTC().Truncate(time.Second)
		regPath := cfg.AgentRegistrationPath(agentID)

		// On-disk registration the pre-lock read would have seen: wake=OLD (%10).
		old := &loop.Registration{
			AgentID:     agentID,
			Vendor:      "anthropic",
			ControlRepo: root,
			WakeMethod:  "tmux",
			WakeTarget:  "%10",
			LastSeen:    now,
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(regPath, old); err != nil {
			t.Fatal(err)
		}

		// Simulate a concurrent locked writer that updated the wake target to NEW
		// (%20) between the pre-lock snapshot and publish taking the lock.
		fresh := *old
		fresh.WakeTarget = "%20"
		if err := loop.WriteRegistration(regPath, &fresh); err != nil {
			t.Fatal(err)
		}

		// Pre-lock values are the STALE OLD (%10) snapshot; preservedFromExisting=true.
		opts := registerOpts{AgentID: agentID, Vendor: "anthropic"}
		if _, err := publishRegistrationOnce(cfg, opts, "", now, regPath, "tmux", "%10", nil, true); err != nil {
			t.Fatal(err)
		}

		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeTarget != "%20" {
			t.Fatalf("preserved wake target = %q, want %%20 (in-lock fresh existing, not stale pre-lock %%10)", reg.WakeTarget)
		}
		if reg.WakeMethod != "tmux" {
			t.Fatalf("preserved wake method = %q, want tmux", reg.WakeMethod)
		}
	})
}

// TestRegister_PreservedWakeExistingVanished: a preserve-from-existing path
// where the on-disk registration is deleted between the pre-lock snapshot and
// the locked write must NOT resurrect the stale pre-lock wake. The written reg
// has EMPTY wake.
func TestRegister_PreservedWakeExistingVanished(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "test-agent"
		now := time.Now().UTC().Truncate(time.Second)
		regPath := cfg.AgentRegistrationPath(agentID)

		// No on-disk registration (it vanished after the pre-lock read). Pre-lock
		// values are the stale OLD (%10) snapshot; preservedFromExisting=true.
		opts := registerOpts{AgentID: agentID, Vendor: "anthropic"}
		if _, err := publishRegistrationOnce(cfg, opts, "", now, regPath, "tmux", "%10", nil, true); err != nil {
			t.Fatal(err)
		}

		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeMethod != "" || reg.WakeTarget != "" {
			t.Fatalf("vanished-existing preserve resurrected stale wake = %q:%q, want empty", reg.WakeMethod, reg.WakeTarget)
		}
	})
}

// TestRegister_FreshResolvedWakeNotOverwritten: a fresh-resolved path
// (preservedFromExisting=false) must write the freshly-resolved wake regardless
// of what is on disk — the in-lock re-derive only applies to PRESERVED wake, so
// no regression to the live-pane / explicit-flag paths.
func TestRegister_FreshResolvedWakeNotOverwritten(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "test-agent"
		now := time.Now().UTC().Truncate(time.Second)
		regPath := cfg.AgentRegistrationPath(agentID)

		// On-disk registration with a DIFFERENT wake (%30) than the fresh one.
		old := &loop.Registration{
			AgentID:     agentID,
			Vendor:      "anthropic",
			ControlRepo: root,
			WakeMethod:  "tmux",
			WakeTarget:  "%30",
			LastSeen:    now,
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(regPath, old); err != nil {
			t.Fatal(err)
		}

		// Fresh-resolved wake (%40, e.g. from a live pane): preservedFromExisting=false.
		opts := registerOpts{AgentID: agentID, Vendor: "anthropic"}
		if _, err := publishRegistrationOnce(cfg, opts, "", now, regPath, "tmux", "%40", nil, false); err != nil {
			t.Fatal(err)
		}

		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeTarget != "%40" {
			t.Fatalf("fresh-resolved wake target = %q, want %%40 (not overwritten by on-disk %%30)", reg.WakeTarget)
		}
	})
}

// TestRegister_PaneLockKeyedOnAuthoritativeTarget asserts the tmux pane lock is
// keyed on the FINAL (in-lock authoritative) wake target, not the stale pre-lock
// snapshot. Defect #1: with the old pane->agent order, the lock wrapped publish
// keyed on the PRE-LOCK target (%10) while the in-lock preserve re-derive wrote —
// and pruned — the in-lock existing's target (%20). The fix flips to agent->pane
// so the pane lock is taken INSIDE the agent lock keyed on the written target.
//
// rev4 split: the same-pane FIND (write-time candidate snapshot) still runs
// UNDER the pane lock keyed on the FINAL target; the revalidated REMOVE now runs
// AFTER the critical section releases (per-peer lock, deadlock-safe). FINAL-
// target keying is still proven end-to-end: the pane lock fires for %20 only,
// the %20 same-pane peer is the one removed, and that removed peer surfaces in
// res.PeerWakeStale — i.e. the removed SET is driven by the FINAL target.
//
// Setup: drive publishRegistrationOnce with the preserve-from-existing path
// (deferToExisting=true) carrying a STALE pre-lock target %10, while the on-disk
// (in-lock authoritative) existing has %20. The pane lock, the same-pane FIND,
// and the resulting REMOVE must all target %20.
func TestRegister_PaneLockKeyedOnAuthoritativeTarget(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "test-agent"
		host, _ := os.Hostname()
		now := time.Now().UTC().Truncate(time.Second)
		regPath := cfg.AgentRegistrationPath(agentID)

		// In-lock authoritative existing: tmux %20 (NOT the stale pre-lock %10).
		existing := &loop.Registration{
			AgentID:     agentID,
			Vendor:      "anthropic",
			ControlRepo: root,
			Host:        host,
			WakeMethod:  "tmux",
			WakeTarget:  "%20",
			LastSeen:    now,
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(regPath, existing); err != nil {
			t.Fatal(err)
		}

		// A same-pane peer on %20 (the authoritative target) that MUST be pruned,
		// and a decoy peer on %10 (the stale pre-lock target) that must NOT be.
		peer20 := *existing
		peer20.AgentID = "peer-on-20"
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("peer-on-20"), &peer20); err != nil {
			t.Fatal(err)
		}
		peer10 := *existing
		peer10.AgentID = "peer-on-10"
		peer10.WakeTarget = "%10"
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("peer-on-10"), &peer10); err != nil {
			t.Fatal(err)
		}

		// Observe every pane-lock acquisition.
		var locked []string
		restore := setTmuxPaneLockObserver(func(target string) {
			locked = append(locked, target)
		})
		defer restore()

		// Pre-lock snapshot is the STALE %10; deferToExisting=true. The publish must
		// re-derive the wake from the in-lock authoritative %20, key the pane lock on
		// %20, and prune the %20 same-pane peer.
		opts := registerOpts{AgentID: agentID, Vendor: "anthropic", PruneStalePeerTmux: false}
		res, err := publishRegistrationOnce(cfg, opts, host, now, regPath, "tmux", "%10", nil, true)
		if err != nil {
			t.Fatal(err)
		}

		if res.ResolvedWakeTarget != "%20" {
			t.Fatalf("resolved wake target = %q, want %%20 (in-lock authoritative)", res.ResolvedWakeTarget)
		}
		if len(locked) != 1 || locked[0] != "%20" {
			t.Fatalf("pane lock targets = %v, want exactly [%%20] (keyed on authoritative target, not stale pre-lock %%10)", locked)
		}
		// Same-pane prune must have removed the %20 peer, not the %10 decoy.
		if _, err := os.Stat(cfg.AgentRegistrationPath("peer-on-20")); !os.IsNotExist(err) {
			t.Fatalf("same-pane peer on %%20 should be pruned (authoritative target), stat err=%v", err)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath("peer-on-10")); err != nil {
			t.Fatalf("peer on stale pre-lock %%10 must NOT be pruned: %v", err)
		}
		// rev4: the REMOVED SET (post-revalidation) must reflect the FINAL target's
		// same-pane peer — proves the find-in-lock candidate keyed on %20 is what
		// drove the revalidated remove, not the stale %10 snapshot.
		if len(res.PeerWakeStale) != 1 || res.PeerWakeStale[0].AgentID != "peer-on-20" {
			t.Fatalf("removed same-pane set = %+v, want exactly [peer-on-20] (FINAL target %%20)", res.PeerWakeStale)
		}
	})
}

// TestRegister_NonTmuxPreLockToTmuxInLock_HoldsPaneLock: when the pre-lock
// resolution was non-tmux (no pane lock would be taken under the old pane->agent
// order) but the in-lock authoritative existing IS tmux, the write + same-pane
// FIND must run UNDER a pane lock for the tmux target — not lock-free. Defect #1
// second face: the old order pruned a tmux pane with NO pane lock at all.
//
// rev4 split: the same-pane FIND (write-time candidate snapshot) runs under the
// pane lock for %33; the revalidated REMOVE runs after the critical section. The
// pane lock must still fire for %33, and the %33 peer must end up removed and in
// res.PeerWakeStale — binding the removed set to the FINAL target.
func TestRegister_NonTmuxPreLockToTmuxInLock_HoldsPaneLock(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "test-agent"
		host, _ := os.Hostname()
		now := time.Now().UTC().Truncate(time.Second)
		regPath := cfg.AgentRegistrationPath(agentID)

		// In-lock authoritative existing is tmux %33.
		existing := &loop.Registration{
			AgentID:     agentID,
			Vendor:      "anthropic",
			ControlRepo: root,
			Host:        host,
			WakeMethod:  "tmux",
			WakeTarget:  "%33",
			LastSeen:    now,
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(regPath, existing); err != nil {
			t.Fatal(err)
		}
		// Same-pane peer on %33 that must be pruned under the pane lock.
		peer := *existing
		peer.AgentID = "peer-on-33"
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("peer-on-33"), &peer); err != nil {
			t.Fatal(err)
		}

		var locked []string
		restore := setTmuxPaneLockObserver(func(target string) {
			locked = append(locked, target)
		})
		defer restore()

		// Pre-lock NON-tmux (empty method/target) but deferToExisting=true, so the
		// FINAL wake is derived from the in-lock tmux existing. The pane lock MUST
		// be acquired for %33 before the write + prune.
		opts := registerOpts{AgentID: agentID, Vendor: "anthropic", PruneStalePeerTmux: false}
		res, err := publishRegistrationOnce(cfg, opts, host, now, regPath, "", "", nil, true)
		if err != nil {
			t.Fatal(err)
		}

		if res.ResolvedWakeMethod != "tmux" || res.ResolvedWakeTarget != "%33" {
			t.Fatalf("resolved wake = %q:%q, want tmux:%%33", res.ResolvedWakeMethod, res.ResolvedWakeTarget)
		}
		if len(locked) != 1 || locked[0] != "%33" {
			t.Fatalf("pane lock targets = %v, want exactly [%%33] (tmux write/find must hold the pane lock)", locked)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath("peer-on-33")); !os.IsNotExist(err) {
			t.Fatalf("same-pane peer on %%33 should be pruned (found under the pane lock for the FINAL target), stat err=%v", err)
		}
		// rev4: the removed SET reflects the FINAL target's same-pane peer.
		if len(res.PeerWakeStale) != 1 || res.PeerWakeStale[0].AgentID != "peer-on-33" {
			t.Fatalf("removed same-pane set = %+v, want exactly [peer-on-33] (FINAL target %%33)", res.PeerWakeStale)
		}
	})
}

// TestRegister_NilToExistingRaceDoesNotClobberWake closes defect #2: a pre-lock
// resolution that saw NO existing (fresh, empty wake) must NOT clobber a wake
// written by a concurrent registrar that created the registration before the
// in-lock authoritative re-read. The fix: the no-live-wake/no-pre-lock-existing
// case defers the wake decision to the in-lock existing, so a nil->existing
// create is honored, not overwritten with empty.
func TestRegister_NilToExistingRaceDoesNotClobberWake(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "test-agent"
		host, _ := os.Hostname()
		now := time.Now().UTC().Truncate(time.Second)
		regPath := cfg.AgentRegistrationPath(agentID)

		// Concurrent registrar created the registration with a real wake AFTER the
		// pre-lock read saw nothing, but BEFORE the in-lock authoritative re-read.
		concurrent := &loop.Registration{
			AgentID:     agentID,
			Vendor:      "anthropic",
			ControlRepo: root,
			Host:        host,
			WakeMethod:  "tmux",
			WakeTarget:  "%55",
			LastSeen:    now,
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(regPath, concurrent); err != nil {
			t.Fatal(err)
		}

		// Defect #2 root: with NO pre-lock existing and no live wake context (no
		// pane, no explicit flags, no ClearStaleTmuxWake), the resolver must signal
		// DEFER-to-existing — NOT a deliberate empty clear. Otherwise the in-lock
		// publish writes empty over a concurrent nil->existing create.
		os.Unsetenv("TMUX_PANE")
		opts := registerOpts{AgentID: agentID, Vendor: "anthropic"}
		_, _, deferToExisting, _ := resolveWakeForRegistration(opts, nil)
		if !deferToExisting {
			t.Fatalf("resolveWakeForRegistration(nil existing, no live context) deferToExisting=false, want true (else empty wake clobbers a concurrent nil->existing create)")
		}

		// And the publish, given that defer signal, must honor the in-lock
		// existing's %55, NOT clobber it with empty.
		res, err := publishRegistrationOnce(cfg, opts, host, now, regPath, "", "", nil, deferToExisting)
		if err != nil {
			t.Fatal(err)
		}

		if res.ResolvedWakeMethod != "tmux" || res.ResolvedWakeTarget != "%55" {
			t.Fatalf("resolved wake = %q:%q, want tmux:%%55 (in-lock existing honored, not clobbered)", res.ResolvedWakeMethod, res.ResolvedWakeTarget)
		}
		reg, err := loop.ReadRegistration(regPath)
		if err != nil {
			t.Fatal(err)
		}
		if reg.WakeMethod != "tmux" || reg.WakeTarget != "%55" {
			t.Fatalf("written wake = %q:%q, want tmux:%%55 (nil->existing race clobbered the concurrent create)", reg.WakeMethod, reg.WakeTarget)
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

// WI-8: nextContextualAgentIDByFilesystem and callers must error (not collide) past cap.
// (added for TDD red-before)
func TestNextContextualAgentIDByFilesystem_ErrorsPastCap(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		base := "claude-code-" + getFolderSlug(root)

		agentsDir := cfg.AgentsDir()
		_ = os.MkdirAll(agentsDir, 0700)

		for i := 2; i <= 101; i++ {
			id := fmt.Sprintf("%s-%d", base, i)
			path := cfg.AgentRegistrationPath(id)
			_ = os.MkdirAll(filepath.Dir(path), 0700)
			_ = os.WriteFile(path, []byte("{}"), 0644)
		}

		_, err = nextContextualAgentIDByFilesystem(cfg, base, base)
		if err == nil {
			t.Fatal("next... past cap returned no error")
		}
		if !strings.Contains(err.Error(), "could not allocate a free agent id") {
			t.Errorf("err=%v, want cap error", err)
		}
	})
}
