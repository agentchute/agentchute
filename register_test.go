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

// Pull-only (simple-again Gate 6c): the wake-autodetect / wake-preserve /
// tmux-pane-dedup / same-pane-prune / pane-lock / defer-to-existing register
// tests were removed with the apparatus they exercised. A registration carries
// no wake state; register's retained behavior is: write the record + the initial
// `.live` presence + the -N contextual-id-collision suffix retry.

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
// the lock the read and write are atomic, so the recorded last_active survives.
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
// inbox dir exists; the inbox/state dirs are created BEFORE the registration file
// is published, so a peer can never observe a live registration with no inbox.
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
		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err != nil {
			t.Fatalf("registration unreadable: %v", err)
		}
	})
}

// Pull-only: a successful register writes the record AND publishes the initial
// `.live` presence fact (Gate 3), with NO wake state on the record.
func TestRegister_WritesRecordAndInitialLive(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"

		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}

		// The registration record is written.
		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err != nil {
			t.Fatalf("registration unreadable: %v", err)
		}
		// The initial `.live` presence fact is published.
		liveSeen, ok := loop.LiveLastSeen(cfg, agentID)
		if !ok || liveSeen.IsZero() {
			t.Fatalf("register did not publish an initial .live presence fact (ok=%v seen=%v)", ok, liveSeen)
		}
	})
}

// Re-running register on an agent that was previously marked exhausted or offline
// must reset Status to active and clear RestartAt.
func TestRegisterClearsStaleStatusAndRestartAt(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test"}); err != nil {
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

		if err := cmdRegister([]string{"--as", "test-agent", "--vendor", "test"}); err != nil {
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

func TestRegisterBioFlagSetsAndOverwritesBody(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))

		if err := cmdRegister([]string{"--as", "test", "--vendor", "test", "--bio", "first bio"}); err != nil {
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
		if err := cmdRegister([]string{"--as", "test", "--vendor", "test"}); err != nil {
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
		if err := cmdRegister([]string{"--as", "test", "--vendor", "test", "--bio", "second bio"}); err != nil {
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

// Pull-only: a contextual register (no --as) whose base id is already held by a
// fresh, active registration suffixes to the next free `<base>-N` and still
// publishes its own initial `.live`. This is the retained -N collision behavior.
func TestRegister_ContextualCollisionSuffixesAndWritesLive(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".examplecorp", "loop"))
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		base := "claude-code-" + getFolderSlug(root)

		// First contextual register claims the base id.
		if err := cmdRegister([]string{"--vendor", "anthropic"}); err != nil {
			t.Fatalf("first contextual register: %v", err)
		}
		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(base)); err != nil {
			t.Fatalf("base registration not written: %v", err)
		}

		// Second contextual register collides with the fresh base id and suffixes.
		if err := cmdRegister([]string{"--vendor", "anthropic"}); err != nil {
			t.Fatalf("second contextual register: %v", err)
		}
		suffixed := base + "-2"
		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(suffixed)); err != nil {
			t.Fatalf("collision did not suffix to %q: %v", suffixed, err)
		}
		// The suffixed lane gets its own initial `.live`.
		liveSeen, ok := loop.LiveLastSeen(cfg, suffixed)
		if !ok || liveSeen.IsZero() {
			t.Fatalf("suffixed lane %q missing initial .live (ok=%v seen=%v)", suffixed, ok, liveSeen)
		}
	})
}

// WI-8: nextContextualAgentIDByFilesystem and callers must error (not collide) past cap.
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

// Regression (codex WI-8 review): the cap must error even when the past-cap
// candidate is FREE. Occupy base-2..base-100 but leave base-101 ABSENT; the
// allocator must NOT hand out base-101 — it must return the cap error.
func TestNextContextualAgentIDByFilesystem_ErrorsPastCapWhenCandidateFree(t *testing.T) {
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

		// base-2..base-100 occupied; base-101 deliberately left FREE.
		for i := 2; i <= 100; i++ {
			id := fmt.Sprintf("%s-%d", base, i)
			path := cfg.AgentRegistrationPath(id)
			_ = os.MkdirAll(filepath.Dir(path), 0700)
			_ = os.WriteFile(path, []byte("{}"), 0644)
		}

		got, err := nextContextualAgentIDByFilesystem(cfg, base, base)
		if err == nil {
			t.Fatalf("past cap with free base-101 returned id %q, want cap error", got)
		}
		if !strings.Contains(err.Error(), "could not allocate a free agent id") {
			t.Errorf("err=%v, want cap error", err)
		}
	})
}
