package cli

import (
	"path/filepath"
	"testing"
)

func TestResolveAgentIDRejectsTraversal(t *testing.T) {
	t.Setenv("AGENTCHUTE_AGENT_ID", "")
	for _, bad := range []string{
		"../../etc/x",
		"../sibling",
		"a/b",
		"foo/../bar",
		"UPPER",
		".hidden",
		"-leading-dash",
	} {
		if got, err := resolveAgentID(bad); err == nil {
			t.Errorf("resolveAgentID(--as=%q) = %q, want error", bad, got)
		}
	}

	t.Setenv("AGENTCHUTE_AGENT_ID", "../../etc/passwd")
	if got, err := resolveAgentID(""); err == nil {
		t.Errorf("resolveAgentID(env=../../etc/passwd) = %q, want error", got)
	}
}

func TestResolveAgentIDExplicitOnly(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "env-id")
		got, err := resolveAgentID("explicit-id")
		if err != nil {
			t.Fatal(err)
		}
		if got != "explicit-id" {
			t.Fatalf("got %q, want explicit-id", got)
		}
	})

	t.Run("env fallback", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "env-id")
		got, err := resolveAgentID("")
		if err != nil {
			t.Fatal(err)
		}
		if got != "env-id" {
			t.Fatalf("got %q, want env-id", got)
		}
	})

	t.Run("missing has exact fix hint", func(t *testing.T) {
		t.Setenv("AGENTCHUTE_AGENT_ID", "")
		_, err := resolveAgentID("")
		if err == nil {
			t.Fatal("missing identity returned nil error")
		}
		if err.Error() != missingAgentIdentityHint {
			t.Fatalf("error = %q, want %q", err, missingAgentIdentityHint)
		}
	})
}

func TestCommandsWithoutIdentityFailWithHint(t *testing.T) {
	root := setupBootFixture(t)
	t.Setenv("AGENTCHUTE_AGENT_ID", "")
	t.Setenv("AGENTCHUTE_SERVE_TOKEN", "")
	withCwd(t, root, func() {
		cases := []struct {
			name string
			run  func() error
		}{
			{"boot", func() error { return cmdBoot([]string{"--vendor", "anthropic"}) }},
			{"serve", func() error { return cmdServe([]string{"--vendor", "openai", "--", filepath.Join(root, "codex")}) }},
			{"check", func() error { return cmdCheck(nil) }},
			{"send", func() error { return cmdSend([]string{"--to", "recipient", "--body", "body"}) }},
			{"status", func() error { return cmdStatus(nil) }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.run()
				if err == nil {
					t.Fatal("command returned nil error")
				}
				if err.Error() != missingAgentIdentityHint {
					t.Fatalf("error = %q, want %q", err, missingAgentIdentityHint)
				}
			})
		}
	})
}
