package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Path-traversal-shaped agent IDs (--as, --from, --to) must be rejected at
// the command entry point, before any filesystem path resolution. Catches
// regressions of the H1 finding: agent IDs reaching cfg.AgentInboxDir() /
// cfg.AgentRegistrationPath() unvalidated.
func TestCommandsRejectPathTraversalAgentIDs(t *testing.T) {
	bad := []string{
		"../etc",
		"..",
		"foo/bar",
		"foo\\bar",
		".",
		"foo bar",
		"FOO",
		".hidden",
		"-leading-hyphen",
		"",
	}

	root := setupTraversalRepo(t)

	for _, id := range bad {
		t.Run("register/"+id, func(t *testing.T) {
			withCwd(t, root, func() {
				args := []string{"--as", id, "--vendor", "test"}
				if err := cmdRegister(args); err == nil {
					t.Fatalf("expected rejection of --as %q, got nil", id)
				}
			})
		})
		t.Run("send-from/"+id, func(t *testing.T) {
			withCwd(t, root, func() {
				args := []string{"--from", id, "--to", "recipient", "--body", "x"}
				if err := cmdSend(args); err == nil {
					t.Fatalf("expected rejection of --from %q, got nil", id)
				}
			})
		})
		t.Run("send-to/"+id, func(t *testing.T) {
			withCwd(t, root, func() {
				args := []string{"--from", "sender", "--to", id, "--body", "x"}
				if err := cmdSend(args); err == nil {
					t.Fatalf("expected rejection of --to %q, got nil", id)
				}
			})
		})
		t.Run("check/"+id, func(t *testing.T) {
			withCwd(t, root, func() {
				args := []string{"--as", id}
				if err := cmdCheck(args); err == nil {
					t.Fatalf("expected rejection of --as %q, got nil", id)
				}
			})
		})
		t.Run("status/"+id, func(t *testing.T) {
			// Empty --as is legitimate for status (pool-overview mode);
			// v0.1.2 explicitly made --as optional and treats "" the
			// same as "flag omitted". Path-traversal security is
			// preserved: every non-empty bad id is rejected by
			// ValidateAgentID before any filesystem access.
			if id == "" {
				t.Skip("empty --as is pool-overview mode for status (by design)")
			}
			withCwd(t, root, func() {
				args := []string{"--as", id}
				if err := cmdStatus(args); err == nil {
					t.Fatalf("expected rejection of --as %q, got nil", id)
				}
			})
		})
	}

	// Confirm the loop tree itself stays bounded: only files written by the
	// register flow with valid slugs (none here) should appear under agents/
	// or inbox/. Anything escaping the validator would land elsewhere.
	for _, sub := range []string{"agents", "inbox"} {
		dir := filepath.Join(root, ".agentchute", "loop", sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			t.Errorf("unexpected entry under %s: %s", dir, e.Name())
		}
	}
}

// Negative control: a valid slug that simply is not registered should fail
// with a different (post-validation) error path — proves the validator did
// not over-reject.
func TestCommandsAcceptValidUnregisteredAgentID(t *testing.T) {
	root := setupTraversalRepo(t)
	withCwd(t, root, func() {
		err := cmdCheck([]string{"--as", "not-registered-yet"})
		if err == nil {
			return // check succeeds (empty inbox) or fails later — both fine
		}
		if strings.Contains(err.Error(), "must match") {
			t.Fatalf("valid slug rejected as malformed: %v", err)
		}
	})
}

func setupTraversalRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTCHUTE.md"), []byte("# Spec"))
	mustMkdir(t, filepath.Join(root, ".agentchute", "loop"))
	return root
}

func withCwd(t *testing.T, dir string, fn func()) {
	t.Helper()
	t.Setenv("AGENTCHUTE_CONTROL_REPO", "")
	t.Setenv("AGENTCHUTE_LOOP_DIR", "")
	t.Setenv("AGENTCHUTE_AGENT_ID", "")
	t.Setenv("AGENTCHUTE_RUNNER", "")
	t.Setenv("AGENTCHUTE_RUNNER_PID", "")
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("TMUX_PANE", "")
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("restore cwd: %v", err)
		}
	}()
	fn()
}
