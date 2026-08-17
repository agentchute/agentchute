package op

import (
	"fmt"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// CleanOwedReq mirrors the shipped `--yes` flag's polarity exactly: Apply
// true = actually prune. An inverted DryRun is a defect waiting to happen.
type CleanOwedReq struct {
	Apply bool `json:"apply,omitempty"`
}

// CleanOwedResp is the plan/apply outcome, with the shipped JSON tags.
//
// Pruned lists the SAME refs whether planning or applying; Applied
// distinguishes whether they were actually removed. It is a REF LIST, not a
// count: the text output prints one line per ref and `--json` serializes the
// list. Pruned is initialized to `[]string{}` and never nil, so `--json`
// renders `[]` rather than `null`.
type CleanOwedResp struct {
	Agent   string   `json:"agent"`
	Pruned  []string `json:"pruned"`
	Applied bool     `json:"applied"`
}

// CleanOwed prunes (or, without Apply, plans pruning) the asker's own expired
// `.owed` entries.
//
// Plan mode is a plain, LOCK-FREE read: WithAgentLock's ensurePrivateDir side
// effect creates state/<id>/ even when nothing is written, which would break
// the "plan mutates nothing" promise. A lock-free read of an atomically written
// ledger can never observe a torn file, so this loses no safety.
//
// Apply mode runs under WithAgentLock(asker) so a concurrent
// RecordOwed/ClearOwed (from the agent's own check/send) cannot race the
// read-modify-write.
func CleanOwed(cfg *loop.Config, ctx Context, req CleanOwedReq) (CleanOwedResp, error) {
	now := time.Now().UTC()
	resp := CleanOwedResp{Agent: ctx.ActorID, Pruned: []string{}}

	if !req.Apply {
		ledger, err := loop.LoadOwedLedger(cfg, ctx.ActorID)
		if err != nil {
			return resp, fmt.Errorf("load owed ledger: %w", err)
		}
		for _, e := range ledger.ExpiredOwed(now) {
			resp.Pruned = append(resp.Pruned, e.Key().RefString())
		}
		return resp, nil
	}

	err := loop.WithAgentLock(cfg, ctx.ActorID, func() error {
		ledger, err := loop.LoadOwedLedger(cfg, ctx.ActorID)
		if err != nil {
			return fmt.Errorf("load owed ledger: %w", err)
		}
		expired := ledger.ExpiredOwed(now)
		// Keyed by OwedKey, not counted: RecordOwed is idempotent per key and
		// cannot create two entries with the same identity, so this can never
		// observe a real duplicate through normal use.
		expiredKeys := make(map[loop.OwedKey]bool, len(expired))
		for _, e := range expired {
			resp.Pruned = append(resp.Pruned, e.Key().RefString())
			expiredKeys[e.Key()] = true
		}
		if len(expired) == 0 {
			return nil
		}
		kept := make([]loop.OwedEntry, 0, len(ledger.Owed))
		for _, e := range ledger.Owed {
			if expiredKeys[e.Key()] {
				continue
			}
			kept = append(kept, e)
		}
		ledger.Owed = kept
		return loop.SaveOwedLedger(cfg, ctx.ActorID, ledger)
	})
	if err != nil {
		return resp, err
	}
	resp.Applied = req.Apply && len(resp.Pruned) > 0
	return resp, nil
}
