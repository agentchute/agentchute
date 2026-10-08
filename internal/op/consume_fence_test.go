package op

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// The consume path (Claim, Ack) is fenced the way send already is. These rows are
// the sealed-pool reproduction from the 2026-10-08 review (S2), moved into the
// suite: a LIVE serve owns "lane"; a process that carries a different serve token
// (a shared daemon's stale env) or none at all (a hand-run session that exported
// the id) must not claim or commit that lane's mail. Before the fix, Claim
// moved the message into .claimed and Ack archived it, while send from the same
// process was already refused with ErrFenced.

const foreignToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// liveLane registers "lane", acquires its real serve lease (a fresh claim), and
// delivers one message from "peer". It returns the live lease.
func liveLane(t *testing.T) (*loop.Config, *loop.ServeLease) {
	t.Helper()
	cfg := newPool(t)
	enroll(t, cfg, "lane")
	enroll(t, cfg, "peer")
	lease, err := loop.AcquireServeLease(cfg, "lane")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loop.ReleaseLease(lease) })
	deliver(t, cfg, "peer", "lane", "work item for the live lane")
	return cfg, lease
}

func TestClaimRefusesAForeignTokenWhileTheServeIsLive(t *testing.T) {
	cfg, _ := liveLane(t)

	var c collector
	_, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{ServeToken: foreignToken}, c.emit)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("Claim with a foreign token = %v, want ErrFenced", err)
	}
	if got := countFiles(t, cfg.AgentInboxDir("lane")); got != 1 {
		t.Fatalf("inbox = %d files, want the message left for the live lane", got)
	}
	if got := countFiles(t, cfg.AgentClaimedDir("lane")); got != 0 {
		t.Fatalf(".claimed = %d files, want nothing claimed by the foreign process", got)
	}
	if got := c.kinds(); len(got) != 0 {
		t.Fatalf("a refused claim emitted %v; it must show the foreign process nothing", got)
	}
}

func TestClaimRefusesNoTokenWhileTheServeIsLive(t *testing.T) {
	cfg, _ := liveLane(t)

	var c collector
	_, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{}, c.emit)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("Claim with no token = %v, want ErrLeaseHeld", err)
	}
	if got := countFiles(t, cfg.AgentInboxDir("lane")); got != 1 {
		t.Fatalf("inbox = %d files, want the message left for the live lane", got)
	}
}

func TestClaimWithTheLiveTokenClaims(t *testing.T) {
	cfg, lease := liveLane(t)

	var c collector
	sum, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{ServeToken: lease.Token}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Claimed != 1 || countFiles(t, cfg.AgentClaimedDir("lane")) != 1 {
		t.Fatalf("summary = %+v; the lane's own token must claim its mail", sum)
	}
}

// --no-archive stays the read-only peek an operator can always take.
func TestClaimNoArchivePeekIsAllowedWhileTheServeIsLive(t *testing.T) {
	cfg, _ := liveLane(t)

	var c collector
	if _, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{NoArchive: true}, c.emit); err != nil {
		t.Fatalf("peek = %v, want allowed", err)
	}
	if got := c.kinds(); len(got) != 1 || got[0] != "message" {
		t.Fatalf("peek stream = %v, want the one message displayed", got)
	}
	if got := countFiles(t, cfg.AgentInboxDir("lane")); got != 1 {
		t.Fatalf("inbox = %d files; a peek must leave the message in place", got)
	}
}

// No live owner: a stale claim (the serve died) leaves nothing to steal from,
// so a tokenless or foreign process consumes as it always has.
func TestClaimIsUnfencedWhenTheServeClaimIsStale(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "lane")
	enroll(t, cfg, "peer")
	writeStaleClaim(t, cfg, "lane", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	deliver(t, cfg, "peer", "lane", "work item")

	var c collector
	for _, token := range []string{"", foreignToken} {
		if _, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{ServeToken: token}, c.emit); err != nil {
			t.Fatalf("Claim(token=%q) against a stale claim = %v, want allowed", token, err)
		}
	}
	if got := countFiles(t, cfg.AgentClaimedDir("lane")); got != 1 {
		t.Fatalf(".claimed = %d files, want the message claimed", got)
	}
}

// The hub's compatibility arm: a frame from a client that predates the fence
// carries no serve_token key at all, and the hub marks the request Unfenced.
func TestClaimUnfencedRequestSkipsTheFence(t *testing.T) {
	cfg, _ := liveLane(t)

	var c collector
	if _, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{Unfenced: true}, c.emit); err != nil {
		t.Fatalf("Unfenced Claim = %v, want allowed", err)
	}
}

func TestAckRefusesToCommitAnotherLiveLanesClaimedMail(t *testing.T) {
	cfg, lease := liveLane(t)
	var c collector
	if _, err := Claim(cfg, Context{ActorID: "lane"}, ClaimReq{ServeToken: lease.Token}, c.emit); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		token string
		want  error
	}{
		{"foreign token", foreignToken, ErrFenced},
		{"no token", "", ErrLeaseHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c collector
			sum, err := Ack(cfg, Context{ActorID: "lane"}, AckReq{ServeToken: tc.token}, c.emit)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Ack = %v, want %v", err, tc.want)
			}
			if sum.Acked != 0 || countFiles(t, cfg.AgentClaimedDir("lane")) != 1 {
				t.Fatalf("summary = %+v; the live lane's claimed mail must stay in .claimed", sum)
			}
		})
	}

	sum, err := Ack(cfg, Context{ActorID: "lane"}, AckReq{ServeToken: lease.Token}, c.emit)
	if err != nil || sum.Acked != 1 {
		t.Fatalf("Ack with the live token = %+v, %v; want the one message committed", sum, err)
	}
}

func writeStaleClaim(t *testing.T, cfg *loop.Config, id, token string) {
	t.Helper()
	dir := cfg.AgentStateDir(id)
	if err := loop.EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	host, _ := os.Hostname()
	body := `{"id":"` + id + `","host":"` + host + `","pid":0,"serve_token":"` + token + `","started_at":"` + old + `","last_seen":"` + old + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "serve.claim"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
