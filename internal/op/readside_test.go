package op

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// ---------------------------------------------------------------- gate + ack

func TestGateBlocksOnUnreadMailAndClearsAfterAck(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "body")

	actor := Context{ActorID: "claude-code"}
	blocked, err := Gate(cfg, actor, GateReq{Phase: GatePhaseFinish})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.Blocked || blocked.UnreadCount != 1 {
		t.Fatalf("gate = %+v, want blocked on 1 unread", blocked)
	}

	var c collector
	if _, err := Claim(cfg, actor, ClaimReq{}, c.emit); err != nil {
		t.Fatal(err)
	}
	// Claimed-but-unacked residue is a WARNING, never a blocking reason — ack
	// must always be able to commit its own just-claimed mail.
	afterClaim, err := Gate(cfg, actor, GateReq{Phase: GatePhaseFinish})
	if err != nil {
		t.Fatal(err)
	}
	if afterClaim.Blocked {
		t.Fatalf("gate = %+v, want claimed residue to warn, not block", afterClaim)
	}
	if afterClaim.ClaimedResidue != 1 || len(afterClaim.Warnings) == 0 {
		t.Fatalf("gate = %+v, want the residue surfaced as a warning", afterClaim)
	}
}

// The gate is the SINGLE source of the blocking decision: FinishGateClear must
// agree with Gate over the finish phase, or `ack` and `gate --before finish`
// could disagree about whether a turn may end.
func TestFinishGateClearAgreesWithGate(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "body")

	actor := Context{ActorID: "claude-code"}
	now := time.Now().UTC()
	status, err := Gate(cfg, actor, GateReq{Phase: GatePhaseFinish})
	if err != nil {
		t.Fatal(err)
	}
	clear, reasons, err := FinishGateClear(cfg, actor, now)
	if err != nil {
		t.Fatal(err)
	}
	if clear == status.Blocked {
		t.Fatalf("FinishGateClear=%v vs gate.Blocked=%v", clear, status.Blocked)
	}
	if strings.Join(reasons, "|") != strings.Join(status.Reasons, "|") {
		t.Fatalf("reasons = %v, want %v", reasons, status.Reasons)
	}
}

// commit/release consult registration freshness; consensus/finish do not.
func TestGateStaleRegistrationBlocksOnlyCommitPhases(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	backdate(t, cfg, "claude-code", time.Now().UTC().Add(-2*StaleRegThreshold))

	actor := Context{ActorID: "claude-code"}
	for phase, wantBlocked := range map[string]bool{
		GatePhaseFinish:  false,
		GatePhaseCommit:  true,
		GatePhaseRelease: true,
	} {
		got, err := Gate(cfg, actor, GateReq{Phase: phase})
		if err != nil {
			t.Fatal(err)
		}
		if got.Blocked != wantBlocked {
			t.Fatalf("phase %s: blocked = %v, want %v (%+v)", phase, got.Blocked, wantBlocked, got.Reasons)
		}
	}
	// The acknowledgment only counts when the caller asked to be double-checked.
	acked, err := Gate(cfg, actor, GateReq{Phase: GatePhaseCommit, RequireConfirm: true, AckStaleReg: true})
	if err != nil {
		t.Fatal(err)
	}
	if acked.Blocked {
		t.Fatalf("gate = %+v, want an acknowledged stale registration to clear", acked)
	}
}

// The gate's JSON shape is a de facto interface (the codex Stop hook and
// turn-end --json both embed it). This pins the tags rather than the struct.
func TestGateRespKeepsItsShippedJSONShape(t *testing.T) {
	data, err := json.Marshal(GateResp{Agent: "a", Phase: "finish", Blocked: true, Reasons: []string{"r"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"agent"`, `"phase"`, `"unread_count"`, `"malformed_count"`, `"stale_reg"`, `"blocked"`, `"reasons"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("gate JSON %s is missing %s", data, key)
		}
	}
}

func TestAckCommitsEveryClaimedMessageAndReportsTheGate(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "one")
	deliver(t, cfg, "codex", "claude-code", "two")

	actor := Context{ActorID: "claude-code"}
	var claimed collector
	if _, err := Claim(cfg, actor, ClaimReq{}, claimed.emit); err != nil {
		t.Fatal(err)
	}

	var c collector
	sum, err := Ack(cfg, actor, AckReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	c.assertUnionInvariant(t)
	if sum.Acked != 2 || !sum.GateClear {
		t.Fatalf("summary = %+v, want 2 acked and a clear gate", sum)
	}
	if n := countFiles(t, cfg.AgentClaimedDir("claude-code")); n != 0 {
		t.Fatalf(".claimed = %d files, want everything committed", n)
	}
	for _, ev := range c.events {
		if ev.Ack == nil || ev.Ack.ArchivePath == "" {
			t.Fatalf("event = %+v, want an ack item with its archive path", ev)
		}
		if _, err := os.Stat(ev.Ack.ArchivePath); err != nil {
			t.Fatalf("archived file missing: %v", err)
		}
	}

	// Idempotent: re-acking an empty .claimed is a no-op success.
	var again collector
	sum2, err := Ack(cfg, actor, AckReq{}, again.emit)
	if err != nil || sum2.Acked != 0 {
		t.Fatalf("re-ack = (%+v, %v), want a no-op success", sum2, err)
	}
}

// The commit is UNCONDITIONAL: an unrelated blocker arriving between check and
// ack must not withhold mail this agent already handled. The gate is reported,
// never enforced.
func TestAckCommitsEvenWhileTheGateBlocks(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "handled")

	actor := Context{ActorID: "claude-code"}
	var claimed collector
	if _, err := Claim(cfg, actor, ClaimReq{}, claimed.emit); err != nil {
		t.Fatal(err)
	}
	// A third party drops new mail after the claim.
	deliver(t, cfg, "codex", "claude-code", "unrelated")

	var c collector
	sum, err := Ack(cfg, actor, AckReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Acked != 1 {
		t.Fatalf("summary = %+v, want the claimed batch committed anyway", sum)
	}
	if sum.GateClear || len(sum.BlockReasons) == 0 {
		t.Fatalf("summary = %+v, want the unrelated blocker REPORTED", sum)
	}
}

// ---------------------------------------------------------------- status

// T1a: the op never truncates. Both framing budgets are the wire producer's, and
// a cap here would silently drop rows in local mode, which renders no truncation
// notice of any kind.
func TestStatusReturnsEveryRowPastTheFramingCaps(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	for i := 0; i < 70; i++ {
		enroll(t, cfg, fmt.Sprintf("peer-%02d", i))
	}

	var c collector
	resp, err := Status(cfg, Context{ActorID: "claude-code"}, StatusReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Agents) != 71 {
		t.Fatalf("agents = %d, want every row (71)", len(resp.Agents))
	}
	if resp.Truncated {
		t.Fatal("the op truncated; truncation is a framing concern, not an op one")
	}
	// Sorted, so truncation is deterministic wherever it IS applied.
	for i := 1; i < len(resp.Agents); i++ {
		if resp.Agents[i-1].AgentID > resp.Agents[i].AgentID {
			t.Fatalf("rows are not in the pool's sort order at %d", i)
		}
	}
}

// The local mirror of the same rule: a single row larger than the wire's byte
// budget is still returned intact, because the budget is framing-only.
func TestStatusReturnsARowBiggerThanTheFramingBudget(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	reg, err := loop.ReadRegistration(cfg.AgentRegistrationPath("codex"))
	if err != nil {
		t.Fatal(err)
	}
	reg.Host = strings.Repeat("h", 70*1024)
	if err := loop.WriteRegistration(cfg.AgentRegistrationPath("codex"), reg); err != nil {
		t.Fatal(err)
	}

	var c collector
	resp, err := Status(cfg, Context{ActorID: "claude-code"}, StatusReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Truncated {
		t.Fatal("the op truncated an over-budget row")
	}
	var found bool
	for _, a := range resp.Agents {
		if a.AgentID == "codex" {
			found = true
			if len(a.Host) != 70*1024 {
				t.Fatalf("host = %d bytes, want it intact", len(a.Host))
			}
		}
	}
	if !found {
		t.Fatal("the over-budget row was dropped")
	}
}

// R4/T1c: read errors are unbounded, so they STREAM as warn notes before the
// response — which is also what puts them on stderr ahead of the table.
func TestStatusStreamsCorruptRowWarningsBeforeReturning(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	if err := os.WriteFile(cfg.AgentRegistrationPath("codex"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	var c collector
	// Recording the stream length at return time is how "before" is asserted:
	// a warning collected onto the response would arrive after this point.
	resp, err := Status(cfg, Context{ActorID: "claude-code"}, StatusReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	warns := c.notes(NoteWarn)
	if len(warns) != 1 || !strings.Contains(warns[0], "codex.md") {
		t.Fatalf("warn notes = %v, want the corrupt row reported", warns)
	}
	if strings.HasPrefix(warns[0], "warning:") {
		t.Fatal("the note carries its own level prefix; the renderer supplies it")
	}
	// The healthy rows still render.
	if len(resp.Agents) != 1 || resp.Agents[0].AgentID != "claude-code" {
		t.Fatalf("agents = %+v, want every readable row", resp.Agents)
	}
}

// The row carries the two facts a remote client cannot derive for itself.
func TestStatusRowCarriesHubDerivedDepthAndLabel(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "claude-code", "codex", "body")
	backdate(t, cfg, "codex", time.Now().UTC().Add(-100*24*time.Hour))

	var c collector
	resp, err := Status(cfg, Context{ActorID: "claude-code"}, StatusReq{}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]StatusAgent{}
	for _, a := range resp.Agents {
		byID[a.AgentID] = a
	}
	if byID["codex"].InboxDepth != 1 {
		t.Fatalf("codex inbox depth = %d, want 1", byID["codex"].InboxDepth)
	}
	if byID["codex"].Status != "stale-would-sweep" {
		t.Fatalf("codex status = %q, want stale-would-sweep", byID["codex"].Status)
	}
	if byID["claude-code"].Status != "fresh" {
		t.Fatalf("claude-code status = %q, want fresh", byID["claude-code"].Status)
	}
	if resp.Now.IsZero() {
		t.Fatal("the evaluation instant is what a remote lane renders AGE from")
	}
}

func TestStatusRefusesAnUnregisteredActor(t *testing.T) {
	cfg := newPool(t)
	var c collector
	if _, err := Status(cfg, Context{ActorID: "claude-code"}, StatusReq{}, c.emit); !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

// ---------------------------------------------------------------- pending

// Pending is the hook-safe peek: strictly read-only, and it reports NeedsBoot
// rather than refusing, because that is actionable work in every output mode.
func TestPendingIsReadOnlyAndReportsNeedsBoot(t *testing.T) {
	cfg := newPool(t)
	var c collector
	sum, err := Pending(cfg, Context{ActorID: "claude-code"}, PendingReq{}, c.emit)
	if err != nil {
		t.Fatalf("pending refused an unbooted agent: %v", err)
	}
	if !sum.NeedsBoot {
		t.Fatalf("summary = %+v, want NeedsBoot", sum)
	}
	if _, serr := os.Stat(cfg.AgentInboxDir("claude-code")); !os.IsNotExist(serr) {
		t.Fatalf("pending created state: %v", serr)
	}
}

func TestPendingStreamsMessagesThenOwedAndHonoursShowBody(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	deliver(t, cfg, "codex", "claude-code", "body text")
	recordExpiredOwed(t, cfg, "claude-code", "codex")

	for _, showBody := range []bool{false, true} {
		var c collector
		sum, err := Pending(cfg, Context{ActorID: "claude-code"}, PendingReq{ShowBody: showBody}, c.emit)
		if err != nil {
			t.Fatal(err)
		}
		c.assertUnionInvariant(t)
		if sum.Unread != 1 || sum.Owed != 1 || sum.NeedsBoot {
			t.Fatalf("summary = %+v", sum)
		}
		if got := c.kinds(); len(got) != 2 || got[0] != "message" || got[1] != "owed" {
			t.Fatalf("stream = %v, want the message then the obligation", got)
		}
		body := c.messages()[0].Body
		if showBody && !strings.Contains(string(body), "body text") {
			t.Fatalf("--show-body dropped the body: %q", body)
		}
		if !showBody && body != nil {
			t.Fatalf("body = %q, want nothing carried without --show-body", body)
		}
		// The obligation carries the full ledger entry: `pending --json`
		// serializes every field of it.
		owed := c.events[1].Owed
		if owed.To != "codex" || owed.From != "claude-code" || owed.By.IsZero() || owed.RecordedAt.IsZero() || owed.Ref == "" {
			t.Fatalf("owed event = %+v, want the full entry", owed)
		}
	}
}

// A message that cannot be read is still LISTED — it simply carries no
// frontmatter-derived facts. Dropping it would hide unread mail.
func TestPendingListsAnUnreadableMessage(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	enroll(t, cfg, "codex")
	id := deliver(t, cfg, "codex", "claude-code", "body")
	if err := os.Chmod(filepath.Join(cfg.AgentInboxDir("claude-code"), id.Filename()), 0o000); err != nil {
		t.Fatal(err)
	}

	var c collector
	sum, err := Pending(cfg, Context{ActorID: "claude-code"}, PendingReq{ShowBody: true}, c.emit)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unread != 1 || len(c.messages()) != 1 {
		t.Fatalf("summary = %+v / messages = %d, want the message listed", sum, len(c.messages()))
	}
	if c.messages()[0].ReplyRequired {
		t.Fatal("an unreadable message must not claim frontmatter facts")
	}
}

// ---------------------------------------------------------------- clean --owed

func TestCleanOwedPlansThenApplies(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")
	recordExpiredOwed(t, cfg, "claude-code", "codex")
	actor := Context{ActorID: "claude-code"}

	plan, err := CleanOwed(cfg, actor, CleanOwedReq{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Pruned) != 1 || plan.Applied {
		t.Fatalf("plan = %+v, want one ref listed and nothing applied", plan)
	}
	ledger, err := loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Owed) != 1 {
		t.Fatal("planning mutated the ledger")
	}

	applied, err := CleanOwed(cfg, actor, CleanOwedReq{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Pruned) != 1 || !applied.Applied {
		t.Fatalf("apply = %+v", applied)
	}
	ledger, err = loop.LoadOwedLedger(cfg, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Owed) != 0 {
		t.Fatalf("ledger = %+v, want the expired entry pruned", ledger.Owed)
	}
}

// The empty shape is load-bearing for byte-identical JSON: `[]`, never `null`.
func TestCleanOwedEmptyPrunedRendersAsAnEmptyList(t *testing.T) {
	cfg := newPool(t)
	enroll(t, cfg, "claude-code")

	resp, err := CleanOwed(cfg, Context{ActorID: "claude-code"}, CleanOwedReq{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Applied {
		t.Fatalf("resp = %+v, want Applied=false with nothing pruned", resp)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"pruned":[]`) {
		t.Fatalf("json = %s, want an empty list rather than null", data)
	}
}
