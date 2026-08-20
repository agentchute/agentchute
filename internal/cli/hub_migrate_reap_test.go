package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentchute/agentchute/internal/hubclient"
	"github.com/agentchute/agentchute/internal/loop"
)

// grok's #167 sweep: reapHubMigrationMux discarded every reap error with `_ =`,
// so the one case that matters — a master this process could NOT reach, which
// may still be alive and still pinned to the old forced-command snapshot —
// looked exactly like the common case of there being nothing to reap.
func TestMigrationReapReportsWhatItCouldNotClose(t *testing.T) {
	original := hubMigrationReapOne
	t.Cleanup(func() { hubMigrationReapOne = original })

	t.Run("a failed reap is reported, naming the agent and the reason", func(t *testing.T) {
		hubMigrationReapOne = func(*loop.RemoteConfig, string, string, string) error {
			return errors.New("permission denied on the control socket")
		}
		stderr := captureStderr(t, func() {
			reapHubMigrationMux(&hubclient.HubConfig{
				URL:      "ssh://alex@hub44/srv/pool",
				JoinedAs: []string{"codex-tiny"},
			}, t.TempDir())
		})
		if !strings.Contains(stderr, "codex-tiny") {
			t.Fatalf("the warning does not name the agent whose master survived:\n%s", stderr)
		}
		if !strings.Contains(stderr, "permission denied on the control socket") {
			t.Fatalf("the warning does not carry the reason:\n%s", stderr)
		}
		// It must say what the surviving connection COSTS, or it reads as noise
		// about an internal detail and gets ignored.
		if !strings.Contains(stderr, "forced-command") {
			t.Fatalf("the warning does not say what a surviving master means:\n%s", stderr)
		}
	})

	// The control. A successful reap — including the common "there was no socket"
	// case — must stay silent, or every migration prints a warning and operators
	// learn to skip them.
	t.Run("a successful reap says nothing", func(t *testing.T) {
		hubMigrationReapOne = func(*loop.RemoteConfig, string, string, string) error { return nil }
		stderr := captureStderr(t, func() {
			reapHubMigrationMux(&hubclient.HubConfig{
				URL:      "ssh://alex@hub44/srv/pool",
				JoinedAs: []string{"codex-tiny"},
			}, t.TempDir())
		})
		if strings.TrimSpace(stderr) != "" {
			t.Fatalf("a clean reap printed a warning:\n%s", stderr)
		}
	})

	// And a failure must NOT abort the migration: the write-lock contract is what
	// keeps a live lane and a migration apart, and this reap is cleanup.
	t.Run("a failed reap still reaps the remaining agents", func(t *testing.T) {
		var seen []string
		hubMigrationReapOne = func(_ *loop.RemoteConfig, agentID, _, _ string) error {
			seen = append(seen, agentID)
			return errors.New("boom")
		}
		_ = captureStderr(t, func() {
			reapHubMigrationMux(&hubclient.HubConfig{
				URL:      "ssh://alex@hub44/srv/pool",
				JoinedAs: []string{"codex-tiny", "opus-high"},
			}, t.TempDir())
		})
		if len(seen) != 2 {
			t.Fatalf("reaped %v; a failure on the first agent stopped the rest", seen)
		}
	})
}
