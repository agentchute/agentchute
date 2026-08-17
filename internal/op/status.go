package op

import (
	"os"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// StatusReq has no fields: `status` is pool-wide and read-only.
type StatusReq struct{}

// StatusAgent is one registration row as `status` renders it — a
// status-SPECIFIC view, not the full registration. Vendor, ControlRepo,
// WorkingRepos and Body are deliberately absent: nothing in `status` prints
// them, and Body alone (a bio bounded only by loop.MaxRegistrationBytes) would
// make a status frame unframable on the wire. The serve claim is absent for the
// same reason — its only surviving fact is the Status label below.
//
// The four registration-sourced fields keep the frontmatter's own key names, so
// one field is never spelled two ways across views.
type StatusAgent struct {
	AgentID         string    `json:"agent_id"`
	LastSeen        time.Time `json:"last_seen"`
	Host            string    `json:"host"`
	ProtocolVersion int       `json:"v"`
	InboxDepth      int       `json:"inbox_depth"` // hub-derived: a remote client cannot stat the hub's inboxes
	Status          string    `json:"status"`      // hub-derived: reads the serve claim and the pool's stale_after
}

// StatusResp is the whole pool, in `status`'s own sort order.
//
// The op NEVER truncates and always returns Truncated:false. Both framing
// budgets (the 64 KiB encoded-line budget and the 64-row cap, §4.4.3) belong to
// the WIRE producer: in local mode there is no line, so capping here would only
// drop rows silently in the one mode that never needed it — `printStatus`
// renders no truncation notice of any kind. The sort stays here so truncation
// is deterministic wherever it is applied.
//
// Now is the evaluation instant: AGE and the STATUS label derive from ONE
// clock, and on a remote lane that clock must be the hub's.
type StatusResp struct {
	Agents    []StatusAgent `json:"agents"`
	Truncated bool          `json:"truncated"`
	Now       time.Time     `json:"now"`
}

// Status reads every registration in the pool leniently: one corrupt row must
// not abort `status` for the whole pool.
//
// Read errors STREAM as warn notes before the response returns, rather than
// riding on it: the lenient read appends one error per malformed *.md under the
// agents dir, so the list is unbounded (a directory of junk .md files yields one
// string each) and §4.4.3 keeps unbounded lists off a control frame. A note
// necessarily precedes the terminal response, so both the bytes and their
// position (before the table) are today's, by construction.
func Status(cfg *loop.Config, ctx Context, _ StatusReq, emit func(Event) error) (StatusResp, error) {
	now := time.Now().UTC()

	// v0.2.1 enforced enrollment (§5.3): status acts AS the identified agent.
	// This confirms enrollment; it never refreshes liveness.
	if err := requireRegistered(cfg, ctx.ActorID); err != nil {
		return StatusResp{}, err
	}

	regList, errs := loop.ReadRegistrationsLenient(cfg.AgentsDir())
	for _, e := range errs {
		if err := emit(NewNoteEvent(NoteWarn, e.Error())); err != nil {
			return StatusResp{}, err
		}
	}

	regs := make(map[string]*loop.Registration, len(regList))
	for _, reg := range regList {
		regs[reg.AgentID] = reg
	}
	resp := StatusResp{Now: now, Agents: make([]StatusAgent, 0, len(regs))}
	for _, id := range loop.RegistrationsByAgentID(regs) {
		reg := regs[id]
		resp.Agents = append(resp.Agents, StatusAgent{
			AgentID:         reg.AgentID,
			LastSeen:        reg.LastSeen,
			Host:            reg.Host,
			ProtocolVersion: reg.ProtocolVersion,
			InboxDepth:      inboxDepth(cfg.AgentInboxDir(id)),
			Status:          registrationStatusLabel(cfg, reg, now),
		})
	}
	return resp, nil
}

// inboxDepth counts the deliverable messages sitting in an inbox dir. The CLI
// keeps its own copy (countInbox) because in M1 the local renderer still
// derives the column itself; the two converge when WI-4.5 switches the renderer
// onto StatusAgent, which is the only way a REMOTE client can learn a depth it
// cannot stat.
func inboxDepth(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") && !strings.HasPrefix(entry.Name(), ".tmp_") {
			count++
		}
	}
	return count
}

// registrationStatusLabel derives the display STATUS for a row. A live serve
// claim is the strongest signal — a fresh lease immunizes an old-looking row
// (C12: never call a row stale-would-sweep while a claim still owns it, since
// the sweep itself would back off too) — checked first, mirroring
// SweepStaleRegistrations' own claimProvablyDead ordering. Absent that, the
// row's own age vs the pool's stale_after decides fresh vs stale. Single
// hyphenated token so whitespace column parsing never misaligns.
func registrationStatusLabel(cfg *loop.Config, reg *loop.Registration, now time.Time) string {
	if claim, err := loop.ReadServeClaim(cfg, reg.AgentID); err == nil && !loop.ClaimIsStale(claim, now) {
		return "lease-held"
	}
	age := now.Sub(reg.LastSeen)
	if age < 0 {
		age = 0
	}
	if age > loop.StaleAfter(cfg) {
		return "stale-would-sweep"
	}
	return "fresh"
}
