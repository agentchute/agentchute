package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentchute/agentchute/internal/loop"
)

// The CLI half of the consume fence (review 2026-10-08, S2): check, ack and
// turn-end hand AGENTCHUTE_SERVE_TOKEN to the op, and turn-end does NOT archive
// when the op refuses. This is the sealed-pool reproduction verbatim: bob's live
// serve holds a fresh claim; a process with a stale token (or none) claimed and
// archived bob's mail while its send from the same env was already fenced.

const consumeForeignToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// liveBob returns the consume fixture with a real serve lease held for bob and
// one message from alice in bob's inbox.
func liveBob(t *testing.T) (string, *loop.Config, *loop.ServeLease) {
	t.Helper()
	root, cfg := setupConsumeFixture(t)
	lease, err := loop.AcquireServeLease(cfg, "bob")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })
	withCwd(t, root, func() {
		if err := cmdSend([]string{"--from", "alice", "--to", "bob", "--body", "work item for the live lane"}); err != nil {
			t.Fatalf("cmdSend: %v", err)
		}
	})
	return root, cfg, lease
}

func TestCheckUnderAForeignTokenDoesNotClaimTheLiveLanesMail(t *testing.T) {
	root, cfg, _ := liveBob(t)
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", consumeForeignToken)
		t.Setenv("AGENTCHUTE_GUARD", "1")
		out, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) })
		if !errors.Is(err, loop.ErrFenced) {
			t.Fatalf("check err = %v, want ErrFenced\n%s", err, out)
		}
		if got := countDirFiles(t, cfg.AgentClaimedDir("bob")); got != 0 {
			t.Fatalf(".claimed = %d files, want none", got)
		}
		if _, err := loop.ReadGuardLatch(cfg, "bob"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused check armed a latch (err=%v); nothing was shown, so nothing is held", err)
		}
	})
}

func TestTurnEndDoesNotArchiveTheLiveLanesClaimedMail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		guard string
		want  error
	}{
		{"foreign token", consumeForeignToken, "1", loop.ErrFenced},
		{"no token", "", "", loop.ErrLeaseHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg, lease := liveBob(t)
			withCwd(t, root, func() {
				// The live lane claims its own mail mid-turn. It is unguarded
				// (a grok lane, or one whose latch write failed), so no latch
				// stands between another process's turn-end and the archive:
				// the fence is the only thing that does.
				t.Setenv("AGENTCHUTE_SERVE_TOKEN", lease.Token)
				t.Setenv("AGENTCHUTE_GUARD", "")
				if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
					t.Fatalf("live lane's own check: %v", err)
				}

				// Another process's end-of-turn hook fires for the same id.
				t.Setenv("AGENTCHUTE_SERVE_TOKEN", tc.token)
				t.Setenv("AGENTCHUTE_GUARD", tc.guard)
				out, err := captureStdout(t, func() error {
					return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
				})
				if !errors.Is(err, tc.want) {
					t.Fatalf("turn-end err = %v, want %v\n%s", err, tc.want, out)
				}
				if got := countDirFiles(t, cfg.AgentClaimedDir("bob")); got != 1 {
					t.Fatalf(".claimed = %d files, want the live lane's claimed message left in place", got)
				}

				// The live lane's own turn-end still commits.
				t.Setenv("AGENTCHUTE_SERVE_TOKEN", lease.Token)
				t.Setenv("AGENTCHUTE_GUARD", "")
				if _, err := captureStdout(t, func() error {
					return cmdTurnEnd([]string{"--as", "bob", "--vendor", "openai", "--json"})
				}); err != nil {
					t.Fatalf("live lane's own turn-end: %v", err)
				}
				if got := countDirFiles(t, cfg.AgentClaimedDir("bob")); got != 0 {
					t.Fatalf(".claimed = %d files after the owner's turn-end, want 0", got)
				}
			})
		})
	}
}

func TestAckWithoutTheServeTokenRefusesWhileTheServeIsLive(t *testing.T) {
	root, cfg, lease := liveBob(t)
	withCwd(t, root, func() {
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", lease.Token)
		if _, err := captureStdout(t, func() error { return cmdCheck([]string{"--as", "bob"}) }); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AGENTCHUTE_SERVE_TOKEN", "")
		if _, err := captureStdout(t, func() error { return cmdAck([]string{"--as", "bob"}) }); !errors.Is(err, loop.ErrLeaseHeld) {
			t.Fatalf("tokenless ack err = %v, want ErrLeaseHeld", err)
		}
		if got := countDirFiles(t, cfg.AgentClaimedDir("bob")); got != 1 {
			t.Fatalf(".claimed = %d files, want 1", got)
		}
	})
}

func countDirFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == ".md" {
			n++
		}
	}
	return n
}
