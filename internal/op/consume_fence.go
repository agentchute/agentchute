package op

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentchute/agentchute/internal/loop"
)

// consumeFence decides whether this caller may claim or commit agentID's mail.
//
// Send has been fenced since protocol v2 (MintSendStamp's VerifyFence); the
// consume path never was, so a process carrying a stale or foreign env — a
// codex 0.161 shared daemon forked by an earlier serve, or a hand-run session
// that exported the lane's id — could claim a LIVE lane's inbox, show that
// mail to the wrong agent, and archive it at its own end of turn, while its
// sends from the very same env already failed closed (review 2026-10-08, S2).
//
// The rule is registrationLiveElsewhere's: only a FRESH serve claim owns the
// id. No claim, or a stale one (its serve died), leaves nothing to steal from,
// and the caller consumes as it always has. Against a fresh claim, a token
// that differs is ErrFenced and no token at all is ErrLeaseHeld. An unreadable
// claim is not evidence of a live owner, matching that rule.
//
// Unfenced is the hub's compatibility arm for a request frame that carries no
// serve_token key at all (a client that predates this fence); nothing local
// sets it.
func consumeFence(cfg *loop.Config, agentID, token string, unfenced bool, now time.Time) error {
	if unfenced {
		return nil
	}
	claim, err := loop.ReadServeClaim(cfg, agentID)
	if err != nil || loop.ClaimIsStale(claim, now) {
		return nil
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("%w: agent %q is owned by a live serve (pid %d on %s) and this process carries no serve token, so it may not claim or commit that lane's mail; run it from that lane, or peek with `agentchute check --no-archive`",
			ErrLeaseHeld, agentID, claim.PID, claim.Host)
	}
	if claim.ServeToken != token {
		return fmt.Errorf("%w: agent %q is owned by a live serve (pid %d on %s) whose token is not this process's AGENTCHUTE_SERVE_TOKEN — this process inherited another runner's env (a shared daemon?) or that lane was relaunched; it may not claim or commit that lane's mail",
			ErrFenced, agentID, claim.PID, claim.Host)
	}
	return nil
}

// fencedMutation is where the fence is authoritative. A preflight consumeFence
// is one read; a lane restart that reclaims the lease after it would let an
// in-flight check or ack go on claiming or archiving the NEW owner's mail for
// the rest of its batch (PR #211 gate, codex + grok). So every consume
// mutation — the claim move, the archive, a quarantine — re-checks the fence
// under the agent lock that AcquireServeLease and RenewLease write the claim
// under, and mutates inside the same critical section: no reclaim can land
// between the check and the move. This is MintSendStamp's check-within-lock
// (loop/floor.go). The lock is non-reentrant, so mutate must not take it, and
// callers emit only after this returns.
//
// before is an invocation-scoped test hook (ClaimReq/AckReq.beforeMutation),
// run outside the lock; nil in production.
func fencedMutation(cfg *loop.Config, agentID, token string, unfenced bool, before func(), mutate func() error) error {
	if before != nil {
		before()
	}
	return loop.WithAgentLock(cfg, agentID, func() error {
		if err := consumeFence(cfg, agentID, token, unfenced, time.Now().UTC()); err != nil {
			return err
		}
		return mutate()
	})
}
