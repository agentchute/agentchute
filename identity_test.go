package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"agentchute", "agentchute"},
		{"AgentChute", "agentchute"},
		{"agent-chute", "agent-chute"},
		{"agent_chute", "agent-chute"},
		{"agent chute", "agent-chute"},
		{"agent.chute", "agent-chute"},
		{"-agent-chute-", "agent-chute"},
		{"!@#agent$%^", "agent"},
	}
	for _, c := range cases {
		got := slugify(c.in)
		if got != c.want {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveAgentID_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "proj")
	_ = os.MkdirAll(cwd, 0o700)
	origCwd, _ := os.Getwd()
	_ = os.Chdir(cwd)
	defer os.Chdir(origCwd)
	t.Setenv("AGENTCHUTE_AGENT_ID", "")

	// Hostile explicit --as values must be rejected structurally — never
	// returned for downstream filesystem resolution.
	flagAttacks := []string{
		"../../etc/x",
		"../sibling",
		"a/b",
		"foo/../bar",
		"UPPER",
		".hidden",
		"-leading-dash",
	}
	for _, bad := range flagAttacks {
		if got, err := resolveAgentID(bad, "anthropic", nil); err == nil {
			t.Errorf("resolveAgentID(--as=%q) = %q, want error", bad, got)
		}
	}

	// A hostile AGENTCHUTE_AGENT_ID must be rejected too.
	t.Setenv("AGENTCHUTE_AGENT_ID", "../../etc/passwd")
	if got, err := resolveAgentID("", "anthropic", nil); err == nil {
		t.Errorf("resolveAgentID(env=../../etc/passwd) = %q, want error", got)
	}
	t.Setenv("AGENTCHUTE_AGENT_ID", "")

	// The empty-input → contextual-default path must still succeed and yield a
	// valid id.
	got, err := resolveAgentID("", "anthropic", nil)
	if err != nil {
		t.Fatalf("contextual default errored: %v", err)
	}
	if err := loop.ValidateAgentID(got); err != nil {
		t.Fatalf("contextual default %q is not a valid agent id: %v", got, err)
	}
}

func TestResolveAgentID(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "my-Project")
	_ = os.MkdirAll(cwd, 0700)

	// Set CWD for the test
	origCwd, _ := os.Getwd()
	_ = os.Chdir(cwd)
	defer os.Chdir(origCwd)

	t.Run("ExplicitFlagWins", func(t *testing.T) {
		got, err := resolveAgentID("explicit-id", "anthropic", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != "explicit-id" {
			t.Errorf("got %q, want 'explicit-id'", got)
		}
	})

	t.Run("EnvVarWinsOverDefault", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "env-id")
		got, err := resolveAgentID("", "anthropic", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != "env-id" {
			t.Errorf("got %q, want 'env-id'", got)
		}
	})

	t.Run("ContextualDefault", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		got, err := resolveAgentID("", "anthropic", nil)
		if err != nil {
			t.Fatal(err)
		}
		// "my-Project" -> "my-project", "anthropic" -> "claude-code"
		if got != "claude-code-my-project" {
			t.Errorf("got %q, want 'claude-code-my-project'", got)
		}
	})

	t.Run("ConflictDetectionSuffixes", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("TMUX_PANE", "%99")

		cfg := &loop.Config{
			LoopDir: filepath.Join(root, ".agentchute", "loop"),
		}
		agentsDir := cfg.AgentsDir()
		_ = os.MkdirAll(agentsDir, 0700)

		// Create an active registration for the base ID
		reg := &loop.Registration{
			AgentID:    "claude-code-my-project",
			Vendor:     "anthropic",
			LastSeen:   time.Now().UTC(),
			WakeMethod: "tmux",
			WakeTarget: "%1", // Different pane
			Status:     loop.StatusActive,
		}
		hostname, _ := os.Hostname()
		reg.Host = hostname
		reg.ControlRepo = root
		_ = loop.WriteRegistration(filepath.Join(agentsDir, reg.AgentID+".md"), reg)

		got, err := resolveAgentID("", "anthropic", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got != "claude-code-my-project-2" {
			t.Errorf("got %q, want 'claude-code-my-project-2' (collision with active different-pane ID)", got)
		}
	})

	t.Run("ReuseSamePaneID", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("TMUX_PANE", "%1")

		cfg := &loop.Config{
			LoopDir: filepath.Join(root, ".agentchute", "loop"),
		}
		agentsDir := cfg.AgentsDir()
		_ = os.MkdirAll(agentsDir, 0700)

		// Create an active registration for the base ID in the SAME pane
		reg := &loop.Registration{
			AgentID:    "claude-code-my-project",
			Vendor:     "anthropic",
			LastSeen:   time.Now().UTC(),
			WakeMethod: "tmux",
			WakeTarget: "%1", // SAME pane
			Status:     loop.StatusActive,
		}
		hostname, _ := os.Hostname()
		reg.Host = hostname
		reg.ControlRepo = root
		_ = loop.WriteRegistration(filepath.Join(agentsDir, reg.AgentID+".md"), reg)

		got, err := resolveAgentID("", "anthropic", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got != "claude-code-my-project" {
			t.Errorf("got %q, want 'claude-code-my-project' (reused from same pane)", got)
		}
	})

	t.Run("ReuseStaleID", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("TMUX_PANE", "%99")

		cfg := &loop.Config{
			LoopDir: filepath.Join(root, ".agentchute", "loop"),
		}
		agentsDir := cfg.AgentsDir()
		_ = os.MkdirAll(agentsDir, 0700)

		// Create a STALE registration for the base ID
		reg := &loop.Registration{
			AgentID:  "claude-code-my-project",
			Vendor:   "anthropic",
			LastSeen: time.Now().UTC().Add(-10 * time.Minute), // Stale
			Status:   loop.StatusActive,
		}
		hostname, _ := os.Hostname()
		reg.Host = hostname
		reg.ControlRepo = root
		_ = loop.WriteRegistration(filepath.Join(agentsDir, reg.AgentID+".md"), reg)

		got, err := resolveAgentID("", "anthropic", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got != "claude-code-my-project" {
			t.Errorf("got %q, want 'claude-code-my-project' (reused stale ID)", got)
		}
	})

	t.Run("PollerOnlyRegistrationDoesNotReserveContextualID", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		t.Setenv("TMUX_PANE", "")

		cfg := &loop.Config{
			LoopDir: filepath.Join(root, ".agentchute", "loop"),
		}
		agentsDir := cfg.AgentsDir()
		_ = os.MkdirAll(agentsDir, 0700)

		reg := &loop.Registration{
			AgentID:     "claude-code-my-project",
			Vendor:      "anthropic",
			ControlRepo: root,
			Host:        "test-host",
			LastSeen:    time.Now().UTC(),
			Status:      loop.StatusActive,
		}
		_ = loop.WriteRegistration(filepath.Join(agentsDir, reg.AgentID+".md"), reg)
		mustWriteFreshPollerHeartbeat(t, cfg, reg.AgentID)

		got, err := resolveAgentID("", "anthropic", cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got != "claude-code-my-project" {
			t.Errorf("got %q, want 'claude-code-my-project' (poller heartbeat is liveness, not a distinct lane)", got)
		}
	})
}

// TestAvailableContextualAgentID_ErrorsPastCap: per WI-8, past cap must error
// rather than return a colliding id (e.g. base-101 when 100 taken).
func TestAvailableContextualAgentID_ErrorsPastCap(t *testing.T) {
	root := t.TempDir()
	cfg := &loop.Config{
		LoopDir: filepath.Join(root, ".agentchute", "loop"),
	}
	agentsDir := cfg.AgentsDir()
	_ = os.MkdirAll(agentsDir, 0700)

	base := "claude-code-testcap"
	now := time.Now().UTC()
	hostname, _ := os.Hostname()

	// Pre-create registrations for base and base-2 through base-101.
	// All must reserve identity so loop exhausts candidates.
	for i := 0; i <= 101; i++ {
		id := base
		if i >= 2 {
			id = fmt.Sprintf("%s-%d", base, i)
		} else if i == 1 {
			// skip 1, cap logic uses -2+
			continue
		}
		reg := &loop.Registration{
			AgentID:     id,
			Vendor:      "anthropic",
			LastSeen:    now,
			WakeMethod:  "tmux",
			WakeTarget:  "%99",
			Status:      loop.StatusActive,
			Host:        hostname,
			ControlRepo: root,
		}
		_ = loop.WriteRegistration(filepath.Join(agentsDir, id+".md"), reg)
	}

	// Must error, not return e.g. base-102 or any suffixed colliding id.
	_, err := availableContextualAgentID(cfg, base, now)
	if err == nil {
		t.Fatalf("availableContextualAgentID past cap returned no error (would have collided)")
	}
	if !strings.Contains(err.Error(), "could not allocate a free agent id") {
		t.Errorf("error = %v, want message about allocation failure", err)
	}
}
