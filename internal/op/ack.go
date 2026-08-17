package op

import (
	"fmt"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// AckReq has no fields: ack commits everything `check` claimed, unconditionally.
type AckReq struct{}

// AckSummary is the post-commit report. BlockReasons is the one inline list
// kept on a summary — a fixed-small set of gate reason strings (§4.4.3).
type AckSummary struct {
	Acked        int      `json:"acked"`
	GateClear    bool     `json:"gate_clear"`
	BlockReasons []string `json:"block_reasons,omitempty"`
}

// Ack is phase 2 of the two-phase consume: the COMMIT. It archives every
// message `check` CLAIMED (moved into inbox/<id>/.claimed) but did not commit,
// emitting each as it commits, then evaluates the finish gate.
//
// UNCONDITIONAL COMMIT: archiving is the caller's OWN state (mail it already
// claimed and acted on), not pool hygiene, so it never waits on the gate. The
// gate is evaluated AFTER committing, purely to REPORT whether other
// obligations remain — post-commit, so the reasons never stale-cite the batch
// just archived.
//
// Idempotent: an already-archived message (a partial prior ack) is success, and
// an empty .claimed is a no-op. An emit error aborts after the current item;
// what committed stays committed and re-acking is a no-op.
func Ack(cfg *loop.Config, ctx Context, _ AckReq, emit func(Event) error) (AckSummary, error) {
	var sum AckSummary
	now := time.Now().UTC()

	residue, err := loop.ListClaimedMessages(cfg.AgentClaimedDir(ctx.ActorID))
	if err != nil {
		return sum, fmt.Errorf("list claimed residue: %w", err)
	}
	for _, msg := range residue {
		dest, aerr := loop.ArchiveMessage(msg, cfg.ArchiveDir(), ctx.ActorID, now)
		if aerr != nil {
			return sum, fmt.Errorf("ack (archive) %s: %w", msg.Filename, aerr)
		}
		sum.Acked++
		if eerr := emit(NewAckItemEvent(AckItemEvent{Filename: msg.Filename, ArchivePath: dest})); eerr != nil {
			return sum, eerr
		}
	}

	clear, reasons, err := FinishGateClear(cfg, ctx, now)
	if err != nil {
		return sum, fmt.Errorf("evaluate finish gate: %w", err)
	}
	sum.GateClear = clear
	if !clear {
		sum.BlockReasons = reasons
	}
	return sum, nil
}
