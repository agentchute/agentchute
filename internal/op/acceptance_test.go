package op

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// Identity arrives OUT OF BAND (C1): neither loop.Config nor any request struct
// carries it, so where the actor id came from cannot change what an op does.
// Locally it comes from the CLI's own resolution (flag, else env); on the hub it
// is the forced command's pinned key id. This drives every actor-scoped op
// through BOTH constructors against identical pools and asserts the state
// effects match — which is the property the whole seam rests on, because the hub
// dispatcher will build its Context the second way for every request it serves.
func TestEveryActorScopedOpIsIdenticalUnderBothContextConstructors(t *testing.T) {
	constructors := []struct {
		name string
		ctx  func(t *testing.T, id string) Context
	}{
		{"pinned", func(_ *testing.T, id string) Context {
			// What the hub session does: build it once, from the pinned
			// --agent, and reuse it for every dispatch.
			return Context{ActorID: id}
		}},
		{"resolved", func(t *testing.T, id string) Context {
			// What the local CLI does: resolve from the environment when no
			// flag was given.
			t.Setenv("AGENTCHUTE_AGENT_ID", id)
			return Context{ActorID: os.Getenv("AGENTCHUTE_AGENT_ID")}
		}},
	}

	type effects struct {
		Claimed     int
		Redelivered int
		Acked       int
		GateClear   bool
		Unread      int
		Owed        int
		Agents      int
		Pruned      int
		ArchiveDir  int
		InboxDir    int
		ClaimedDir  int
		Registered  bool
	}

	run := func(t *testing.T, ctxOf func(t *testing.T, id string) Context) effects {
		t.Helper()
		cfg := newPool(t)
		actor := ctxOf(t, "claude-code")
		peer := ctxOf(t, "codex")

		// Register — the write path, through the seam for both actors.
		vendor := "anthropic"
		now := time.Now().UTC().Truncate(time.Second)
		if _, err := Register(cfg, actor, RegisterReq{Vendor: &vendor, Host: "box"}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := Register(cfg, peer, RegisterReq{Vendor: &vendor, Host: "box"}, now); err != nil {
			t.Fatal(err)
		}

		// Send — one plain message and one ask (whose obligation is expired on
		// purpose, so CleanOwed has something to prune).
		if _, err := Send(cfg, peer, SendReq{To: "claude-code", Content: loop.ComposeMessage("codex", "", "one")}); err != nil {
			t.Fatal(err)
		}
		recordExpiredOwed(t, cfg, "claude-code", "codex")

		var out effects

		// Pending — read-only peek, before anything is consumed.
		var peek collector
		pendingSum, err := Pending(cfg, actor, PendingReq{}, peek.emit)
		if err != nil {
			t.Fatal(err)
		}
		peek.assertUnionInvariant(t)
		out.Unread, out.Owed = pendingSum.Unread, pendingSum.Owed

		// Claim → Ack: the two-phase consume.
		var claimed collector
		claimSum, err := Claim(cfg, actor, ClaimReq{}, claimed.emit)
		if err != nil {
			t.Fatal(err)
		}
		claimed.assertUnionInvariant(t)
		out.Claimed, out.Redelivered = claimSum.Claimed, claimSum.Redelivered

		var acked collector
		ackSum, err := Ack(cfg, actor, AckReq{}, acked.emit)
		if err != nil {
			t.Fatal(err)
		}
		acked.assertUnionInvariant(t)
		out.Acked, out.GateClear = ackSum.Acked, ackSum.GateClear

		// Status — the pool-wide read.
		var status collector
		statusResp, err := Status(cfg, actor, StatusReq{}, status.emit)
		if err != nil {
			t.Fatal(err)
		}
		out.Agents = len(statusResp.Agents)

		// CleanOwed — the prune.
		cleanResp, err := CleanOwed(cfg, actor, CleanOwedReq{Apply: true})
		if err != nil {
			t.Fatal(err)
		}
		out.Pruned = len(cleanResp.Pruned)

		// The state effects on disk, which is what "identical" has to mean.
		out.ArchiveDir = countFiles(t, cfg.ArchiveDir())
		out.InboxDir = countFiles(t, cfg.AgentInboxDir("claude-code"))
		out.ClaimedDir = countFiles(t, cfg.AgentClaimedDir("claude-code"))
		_, statErr := os.Stat(cfg.AgentRegistrationPath("claude-code"))
		out.Registered = statErr == nil
		return out
	}

	results := make([]effects, len(constructors))
	for i, c := range constructors {
		t.Run(c.name, func(t *testing.T) {
			results[i] = run(t, c.ctx)
		})
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("pinned = %+v\nresolved = %+v\nthe actor's provenance changed the op's effect", results[0], results[1])
	}
	// A matrix that asserted equality over two no-ops would be vacuous.
	if want := (effects{Claimed: 1, Acked: 1, GateClear: true, Unread: 1, Owed: 1, Agents: 2, Pruned: 1, ArchiveDir: 1, Registered: true}); results[0] != want {
		t.Fatalf("effects = %+v, want %+v", results[0], want)
	}
}
