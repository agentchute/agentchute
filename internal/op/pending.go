package op

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// PendingReq is the hook-safe peek. ShowBody carries the message bytes; without
// it only frontmatter-derived facts are emitted.
type PendingReq struct {
	ShowBody bool `json:"show_body,omitempty"`
}

// PendingSummary is counts only (D2), plus the one derived fact a caller cannot
// recompute remotely: NeedsBoot.
type PendingSummary struct {
	Unread    int  `json:"unread"`
	Owed      int  `json:"owed"`
	Malformed int  `json:"malformed"`
	NeedsBoot bool `json:"needs_boot,omitempty"`
}

// Pending lists unread inbox messages and outstanding owed obligations without
// archiving, quarantining, or poking peers. Strictly side-effect-free; safe
// from hooks.
//
// Unlike the active ops it does NOT refuse an unregistered agent: it reports
// NeedsBoot instead, because every output mode surfaces that as actionable work
// rather than a failure.
func Pending(cfg *loop.Config, ctx Context, req PendingReq, emit func(Event) error) (PendingSummary, error) {
	var sum PendingSummary
	agentID := ctx.ActorID

	if _, err := os.Stat(cfg.AgentRegistrationPath(agentID)); err != nil {
		if os.IsNotExist(err) {
			sum.NeedsBoot = true
		} else {
			return sum, fmt.Errorf("stat own registration: %w", err)
		}
	}

	// Strictly read-only: last_seen is NOT touched here. `boot` is the
	// lifecycle event that ticks it.
	msgs, skipped, err := loop.ListInboxMessagesWithSkipped(cfg.AgentInboxDir(agentID))
	if err != nil {
		if errors.Is(err, loop.ErrInboxMissing) {
			sum.NeedsBoot = true
			msgs, skipped = nil, nil
		} else {
			return sum, fmt.Errorf("list inbox: %w", err)
		}
	}
	sum.Malformed = len(skipped)

	for _, msg := range msgs {
		ev := MessageEvent{
			Filename: msg.Filename,
			Sender:   msg.Sender,
			Stamp:    msg.Timestamp.UTC().Format(time.RFC3339Nano),
		}
		// Capped at the inbox message limit, matching the consume path: a peer
		// could plant a validly named but oversized file, and reading it
		// unbounded just to inspect frontmatter would let that peer OOM the
		// consumer. An unreadable message is still LISTED (today's behavior) —
		// it simply carries no frontmatter-derived facts.
		if content, rerr := loop.ReadFileLimit(msg.Path, loop.MaxInboxMessageBytes); rerr == nil {
			ev.ReplyRequired = frontmatterReplyRequired(loop.ParseMessageFrontmatter(content))
			if req.ShowBody {
				ev.Body = content
			}
		}
		sum.Unread++
		if eerr := emit(NewMessageEvent(ev)); eerr != nil {
			return sum, eerr
		}
	}

	// Asker-owned obligations — replies WE are owed by peers. NON-BLOCKING. A
	// corrupt `.owed` is not fatal here: gate/boot own blocking, so pending
	// stays a best-effort peek and reports zero owed rather than crashing.
	if owed, oerr := loop.LoadOwedLedger(cfg, agentID); oerr == nil {
		for _, e := range owed.OutstandingOwed() {
			sum.Owed++
			if eerr := emit(NewOwedEvent(owedEventOf(e))); eerr != nil {
				return sum, eerr
			}
		}
	}
	return sum, nil
}
