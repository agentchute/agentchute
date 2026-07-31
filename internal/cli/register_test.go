package cli

import (
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
// no wake state; register's retained behavior is: write the record + explicit-
// id duplicate-owner refusal.

func TestRegister_RMWUnderAgentLock(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
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
		// B1: the heartbeat is lease-gated, so racing it here needs a real
		// serve lease token, not merely a timestamp.
		lease, err := loop.AcquireServeLease(cfg, agentID)
		if err != nil {
			t.Fatal(err)
		}
		template := loop.Registration{
			AgentID:      agentID,
			Vendor:       "anthropic",
			ControlRepo:  cfg.ControlRepo,
			WorkingRepos: []string{cfg.ControlRepo},
		}

		var wg sync.WaitGroup
		errs := make(chan error, 128)
		// Many performRegister merges (read existing → write) concurrently...
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				opts := registerOpts{AgentID: agentID, Vendor: "anthropic", ServeToken: lease.Token}
				if _, err := performRegister(cfg, opts, now.Add(time.Duration(i)*time.Second)); err != nil {
					errs <- err
				}
			}(i)
		}
		// ...racing HeartbeatRegistration on the same registration.
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := loop.HeartbeatRegistration(cfg, template, lease.Token); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent register/heartbeat: %v", err)
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

func TestRegisterWritesProtocolVersion(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := performRegister(cfg, registerOpts{AgentID: "codex", Vendor: "openai"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
		if err != nil {
			t.Fatal(err)
		}
		if reg.ProtocolVersion != loop.CurrentProtocolVersion {
			t.Fatalf("ProtocolVersion = %d, want %d", reg.ProtocolVersion, loop.CurrentProtocolVersion)
		}
		data := string(mustRead(t, cfg.AgentRegistrationPath("codex")))
		if !strings.Contains(data, "\nv: 2\n") {
			t.Fatalf("registration missing v: 2:\n%s", data)
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
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
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

// Pull-only: a successful register writes the record, with NO wake state on it.
func TestRegister_WritesRecord(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"

		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}

		if _, err := loop.ReadRegistration(cfg.AgentRegistrationPath(agentID)); err != nil {
			t.Fatalf("registration unreadable: %v", err)
		}
	})
}

func TestRegisterBioFlagSetsAndOverwritesBody(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))

		if err := cmdRegister([]string{"--as", "test", "--vendor", "test", "--bio", "first bio"}); err != nil {
			t.Fatal(err)
		}

		regPath := filepath.Join(root, ".agentchute", "loop", "agents", "test.md")
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

func TestRegisterRefusesLiveDuplicateID(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		now := time.Now().UTC()
		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, now); err != nil {
			t.Fatalf("seed registration: %v", err)
		}
		lease, err := loop.AcquireServeLease(cfg, agentID)
		if err != nil {
			t.Fatalf("acquire serve lease: %v", err)
		}
		defer loop.ReleaseLease(lease)

		_, err = performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, now.Add(time.Second))
		if err == nil {
			t.Fatal("live duplicate registration returned nil error")
		}
		want := `agent id "claude-code" is live elsewhere; pick a distinct name (--as claude-code-2?)`
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err, want)
		}

		if _, err := os.Stat(cfg.AgentRegistrationPath(agentID + "-2")); !os.IsNotExist(err) {
			t.Fatalf("duplicate registration created suffixed id: stat err = %v", err)
		}
	})
}

func TestRegisterRefusesFreshForeignLeaseWithStaleRow(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		now := time.Now().UTC()
		stale := now.Add(-2 * loop.DefaultStaleAfter)
		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, stale); err != nil {
			t.Fatalf("seed stale registration: %v", err)
		}
		lease, err := loop.AcquireServeLease(cfg, agentID)
		if err != nil {
			t.Fatalf("acquire fresh foreign lease: %v", err)
		}
		defer loop.ReleaseLease(lease)

		_, err = performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, now)
		if err == nil {
			t.Fatal("registration with stale row and fresh foreign lease returned nil error")
		}
		want := `agent id "claude-code" is live elsewhere; pick a distinct name (--as claude-code-2?)`
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err, want)
		}
	})
}

func TestRegisterRefusesFreshForeignLeaseWithoutRow(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		lease, err := loop.AcquireServeLease(cfg, agentID)
		if err != nil {
			t.Fatalf("acquire fresh foreign lease: %v", err)
		}
		defer loop.ReleaseLease(lease)

		_, err = performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, time.Now().UTC())
		if err == nil {
			t.Fatal("registration with no row and fresh foreign lease returned nil error")
		}
		want := `agent id "claude-code" is live elsewhere; pick a distinct name (--as claude-code-2?)`
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err, want)
		}
		if _, err := os.Stat(cfg.AgentRegistrationPath(agentID)); !os.IsNotExist(err) {
			t.Fatalf("foreign lease registration created a row: stat err = %v", err)
		}
	})
}

func TestRegisterCreatesMissingRowWithMatchingLeaseToken(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		lease, err := loop.AcquireServeLease(cfg, agentID)
		if err != nil {
			t.Fatalf("acquire own lease: %v", err)
		}
		defer loop.ReleaseLease(lease)

		result, err := performRegister(cfg, registerOpts{
			AgentID:    agentID,
			Vendor:     "anthropic",
			ServeToken: lease.Token,
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("register under own lease: %v", err)
		}
		if result.ExistingFound {
			t.Fatal("missing-row registration reported an existing row")
		}
		if result.Reg.AgentID != agentID {
			t.Fatalf("agent id = %q, want %q", result.Reg.AgentID, agentID)
		}
	})
}

func TestRegisterMergesOverStaleSameID(t *testing.T) {
	root := t.TempDir()
	withCwd(t, root, func() {
		mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
		mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		const agentID = "claude-code"
		now := time.Now().UTC()
		stale := now.Add(-2 * loop.DefaultStaleAfter)
		if _, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic", Bio: "old", BioProvided: true}, stale); err != nil {
			t.Fatalf("seed stale registration: %v", err)
		}

		result, err := performRegister(cfg, registerOpts{AgentID: agentID, Vendor: "anthropic"}, now)
		if err != nil {
			t.Fatalf("merge stale same id: %v", err)
		}
		if !result.ExistingFound {
			t.Fatal("stale same-id merge did not report existing row")
		}
		if !result.Reg.LastSeen.Equal(now) {
			t.Fatalf("last_seen = %s, want %s", result.Reg.LastSeen, now)
		}
		if !strings.Contains(result.Reg.Body, "old") {
			t.Fatalf("stale merge lost existing body: %q", result.Reg.Body)
		}
	})
}
