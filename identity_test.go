package main

import (
	"os"
	"path/filepath"
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
}
