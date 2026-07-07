---
description: Write a dated restart-handoff file and tell the rest of the live roster to do the same
---

# Save Context

Produces a single handoff file for this lane and broadcasts the same instruction to every
other live agent, so a fleet restart has one consistent artifact per lane instead of an
ad hoc reconstruction each time.

## Instructions

1. Gather state (read-only, don't skip any):
   - `git status --short` and current branch/worktree path.
   - Any open PRs authored or being reviewed by this lane: `gh pr list --author @me` and
     `gh pr status`; note gate/verify status (SHIP/FIX, waiting-on) for each.
   - `agentchute doctor --as "$AGENTCHUTE_AGENT_ID"` and `agentchute pending --as "$AGENTCHUTE_AGENT_ID"`
     (enrollment health + outstanding reply obligations).
   - The next planned step in your own words — one or two sentences, not a full plan dump.
2. Write `.tmp/handoffs/<ISO-date>.md` (e.g. `.tmp/handoffs/2026-07-07.md`; if one already
   exists for today, append a new dated section rather than overwriting) with:
   - `## Repo state` — branch, worktree path, uncommitted changes if any.
   - `## Open work` — PRs with their gate status, anything mid-review.
   - `## Agentchute state` — doctor summary, pending obligations.
   - `## Next step` — the one/two-sentence plan for after restart.
   Do NOT put this under `docs/internal/` — that collides with the existing curated
   `docs/internal/HANDOFF.md`. `.tmp/` is gitignored; this file is local scratch, not a
   commit.
3. Broadcast: list the live roster (`agentchute status`, excluding yourself) and
   `agentchute send --from "$AGENTCHUTE_AGENT_ID" --to <peer> --body "save context, restarting"`
   to each one. This is a non-task status message (AGENTS.md S5) — no GOAL/ACCEPTANCE
   envelope needed.
4. Report back to the user: the handoff file path, and confirmation the broadcast went out.
