package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

func TestCleanRegisteredInDispatch(t *testing.T) {
	if commandHandlers["clean"] == nil {
		t.Fatal(`"clean" is not registered in commandHandlers`)
	}
}

func TestCleanRequiresOwedOrMailbox(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		if err := cmdClean([]string{}); err == nil {
			t.Fatal("expected an error when neither --owed nor --mailbox is given")
		} else if !strings.Contains(err.Error(), "--owed or --mailbox") {
			t.Fatalf("err = %v, want a usage error naming --owed/--mailbox", err)
		}
	})
}

func TestCleanOwedAndMailboxMutuallyExclusive(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		err := cmdClean([]string{"--as", "claude-code", "--owed", "--mailbox", "codex"})
		if err == nil {
			t.Fatal("expected an error when both --owed and --mailbox are given")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("err = %v, want a mutually-exclusive usage error", err)
		}
	})
}

// --owed without --yes prints a plan (what would be pruned) and mutates
// nothing: the ledger on disk must still contain the expired entry.
func TestCleanOwedPlanDoesNotMutate(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		seedExpiredOwed(t, cfg, "claude-code", "codex", 1)

		out, err := captureStdout(t, func() error {
			return cmdClean([]string{"--as", "claude-code", "--owed"})
		})
		if err != nil {
			t.Fatalf("clean --owed (plan) err = %v", err)
		}
		if !strings.Contains(out, "would prune") {
			t.Fatalf("plan output = %q, want a would-prune line", out)
		}

		ledger, err := loop.LoadOwedLedger(cfg, "claude-code")
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger.Owed) != 1 {
			t.Fatalf("plan mode mutated the ledger: got %d entries, want 1 (unchanged)", len(ledger.Owed))
		}
	})
}

// --owed --yes prunes ONLY the expired entries, keeping a still-outstanding
// (not yet past deadline) obligation untouched.
func TestCleanOwedAppliesPrunesOnlyExpired(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		seedExpiredOwed(t, cfg, "claude-code", "codex", 1)
		now := time.Now().UTC()
		outstandingKey := loop.MsgID{To: "gemini-cli", From: "claude-code", Seq: 2}
		if err := loop.RecordOwed(cfg, "claude-code", outstandingKey, now.Add(30*time.Minute), now); err != nil {
			t.Fatal(err)
		}

		out, err := captureStdout(t, func() error {
			return cmdClean([]string{"--as", "claude-code", "--owed", "--yes"})
		})
		if err != nil {
			t.Fatalf("clean --owed --yes err = %v", err)
		}
		if !strings.Contains(out, "pruned") {
			t.Fatalf("apply output = %q, want a pruned line", out)
		}

		ledger, err := loop.LoadOwedLedger(cfg, "claude-code")
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger.Owed) != 1 {
			t.Fatalf("got %d remaining entries, want 1 (only the outstanding one)", len(ledger.Owed))
		}
		if !ledger.Owed[0].Key().Equal(outstandingKey) {
			t.Fatalf("remaining entry = %+v, want the outstanding (non-expired) one", ledger.Owed[0])
		}
	})
}

func TestCleanOwedNoExpiredIsNoop(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		out, err := captureStdout(t, func() error {
			return cmdClean([]string{"--as", "claude-code", "--owed", "--yes"})
		})
		if err != nil {
			t.Fatalf("clean --owed --yes on an empty ledger err = %v", err)
		}
		if !strings.Contains(out, "no expired reply obligations") {
			t.Fatalf("output = %q, want a no-op message", out)
		}
	})
}

func TestCleanMailboxRefusesOnLiveRegistration(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		inbox := cfg.AgentInboxDir("abandoned")
		mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\n---\n\nhi\n"))
		reg := &loop.Registration{
			AgentID:     "abandoned",
			Vendor:      "test",
			ControlRepo: cfg.ControlRepo,
			LastSeen:    time.Now().UTC().Add(-2 * time.Hour), // stale age, but the row EXISTS
			Status:      loop.StatusActive,
		}
		if err := loop.WriteRegistration(cfg.AgentRegistrationPath("abandoned"), reg); err != nil {
			t.Fatal(err)
		}

		err = cmdClean([]string{"--mailbox", "abandoned"})
		if err == nil {
			t.Fatal("expected a refusal error when the target has a registration row")
		}
		if !strings.Contains(err.Error(), "registration row") {
			t.Fatalf("err = %v, want a registration-row refusal", err)
		}
		if _, statErr := os.Stat(inbox); statErr != nil {
			t.Fatalf("mailbox was removed despite refusal: %v", statErr)
		}
	})
}

func TestCleanMailboxRefusesOnFreshServeClaim(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		inbox := cfg.AgentInboxDir("abandoned")
		mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\n---\n\nhi\n"))
		// No registration row, but a FRESH serve lease.
		lease, err := loop.AcquireServeLease(cfg, "abandoned")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = loop.ReleaseLease(lease) }()

		err = cmdClean([]string{"--mailbox", "abandoned"})
		if err == nil {
			t.Fatal("expected a refusal error when the target has a fresh serve lease")
		}
		if !strings.Contains(err.Error(), "serve lease") {
			t.Fatalf("err = %v, want a serve-lease refusal", err)
		}
		if _, statErr := os.Stat(inbox); statErr != nil {
			t.Fatalf("mailbox was removed despite refusal: %v", statErr)
		}
	})
}

// No registration row and a STALE serve claim (not absent — stale): the
// removal must proceed. ClaimIsStale, not mere presence, is the guard.
func TestCleanMailboxAllowsOnStaleServeClaim(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		inbox := cfg.AgentInboxDir("abandoned")
		mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\n---\n\nhi\n"))
		lease, err := loop.AcquireServeLease(cfg, "abandoned")
		if err != nil {
			t.Fatal(err)
		}
		staleBackdateClaim(t, cfg, "abandoned")
		_ = lease // the claim file on disk is what mailboxCleanRefusal reads

		if err := cmdClean([]string{"--mailbox", "abandoned", "--yes"}); err != nil {
			t.Fatalf("clean --mailbox --yes with a stale claim err = %v, want nil", err)
		}
		if _, statErr := os.Stat(inbox); !os.IsNotExist(statErr) {
			t.Fatalf("mailbox still present after apply: stat err = %v", statErr)
		}
	})
}

func TestCleanMailboxPlanDoesNotMutate(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		inbox := cfg.AgentInboxDir("abandoned")
		mustWriteSeqInbox(t, inbox, "peer", 1, []byte("---\nfrom: peer\n---\n\nhi\n"))

		out, err := captureStdout(t, func() error {
			return cmdClean([]string{"--mailbox", "abandoned"})
		})
		if err != nil {
			t.Fatalf("clean --mailbox (plan) err = %v", err)
		}
		if !strings.Contains(out, "would remove") {
			t.Fatalf("plan output = %q, want a would-remove line", out)
		}
		if _, statErr := os.Stat(inbox); statErr != nil {
			t.Fatalf("plan mode removed the mailbox: %v", statErr)
		}
	})
}

// --mailbox --yes must remove ONLY the target's own inbox tree (including
// .claimed, which lives under it) — every other agent's mailbox, and every
// other loop directory (agents/, archive/, state/), must survive untouched.
func TestCleanMailboxAppliesRemovesOnlyTargetInboxTree(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		targetInbox := cfg.AgentInboxDir("abandoned")
		mustWriteSeqInbox(t, targetInbox, "peer", 1, []byte("---\nfrom: peer\n---\n\nhi\n"))
		claimedFile := filepath.Join(cfg.AgentClaimedDir("abandoned"), "from-peer_seq-00000000000000000002.md")
		mustWrite(t, claimedFile, []byte("---\nfrom: peer\n---\n\nclaimed\n"))

		otherInbox := cfg.AgentInboxDir("still-here")
		mustWriteSeqInbox(t, otherInbox, "peer", 3, []byte("---\nfrom: peer\n---\n\nhi\n"))
		seedExpiredOwed(t, cfg, "still-here", "codex", 9)

		if err := cmdClean([]string{"--mailbox", "abandoned", "--yes"}); err != nil {
			t.Fatalf("clean --mailbox --yes err = %v", err)
		}

		if _, err := os.Stat(targetInbox); !os.IsNotExist(err) {
			t.Fatalf("target inbox (incl. .claimed) still present: stat err = %v", err)
		}
		if _, err := os.Stat(otherInbox); err != nil {
			t.Fatalf("unrelated agent's inbox was disturbed: %v", err)
		}
		ledger, err := loop.LoadOwedLedger(cfg, "still-here")
		if err != nil {
			t.Fatal(err)
		}
		if len(ledger.Owed) != 1 {
			t.Fatalf("unrelated agent's state was disturbed: owed entries = %d, want 1", len(ledger.Owed))
		}
	})
}

func TestCleanJSONShape(t *testing.T) {
	root := setupBootFixture(t)
	withCwd(t, root, func() {
		cfg, err := loop.Discover(loop.DiscoverOpts{Cwd: root})
		if err != nil {
			t.Fatal(err)
		}
		seedExpiredOwed(t, cfg, "claude-code", "codex", 1)

		out, err := captureStdout(t, func() error {
			return cmdClean([]string{"--as", "claude-code", "--owed", "--json"})
		})
		if err != nil {
			t.Fatalf("clean --owed --json err = %v", err)
		}
		var owedResult cleanOwedResult
		if jerr := json.Unmarshal([]byte(out), &owedResult); jerr != nil {
			t.Fatalf("--owed --json did not decode: %v\noutput: %s", jerr, out)
		}
		if owedResult.Agent != "claude-code" || len(owedResult.Pruned) != 1 || owedResult.Applied {
			t.Fatalf("owed JSON shape = %+v, want agent=claude-code, 1 pruned, applied=false", owedResult)
		}

		mustWriteSeqInbox(t, cfg.AgentInboxDir("abandoned"), "peer", 1, []byte("---\nfrom: peer\n---\n\nhi\n"))
		out, err = captureStdout(t, func() error {
			return cmdClean([]string{"--mailbox", "abandoned", "--json"})
		})
		if err != nil {
			t.Fatalf("clean --mailbox --json (plan) err = %v", err)
		}
		var mailboxResult cleanMailboxResult
		if jerr := json.Unmarshal([]byte(out), &mailboxResult); jerr != nil {
			t.Fatalf("--mailbox --json did not decode: %v\noutput: %s", jerr, out)
		}
		if mailboxResult.Target != "abandoned" || mailboxResult.Applied || mailboxResult.Refused != "" {
			t.Fatalf("mailbox JSON shape = %+v, want target=abandoned, applied=false, refused=\"\"", mailboxResult)
		}
	})
}

// seedExpiredOwed records an obligation whose deadline is already in the
// past, so ExpiredOwed(now) picks it up immediately.
func seedExpiredOwed(t *testing.T, cfg *loop.Config, asker, recipient string, seq uint64) {
	t.Helper()
	now := time.Now().UTC()
	key := loop.MsgID{To: recipient, From: asker, Seq: seq}
	if err := loop.RecordOwed(cfg, asker, key, now.Add(-time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// staleBackdateClaim back-dates an existing serve claim's LastSeen so
// ClaimIsStale reports true, without going through the reclaim machinery.
func staleBackdateClaim(t *testing.T, cfg *loop.Config, agentID string) {
	t.Helper()
	claim, err := loop.ReadServeClaim(cfg, agentID)
	if err != nil {
		t.Fatal(err)
	}
	claim.LastSeen = time.Now().UTC().Add(-time.Hour)
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(cfg.AgentStateDir(agentID), "serve.claim"), data)
}
