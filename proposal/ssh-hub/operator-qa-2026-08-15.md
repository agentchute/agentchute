# Operator Q&A — decisions and clarifications (Alex ⇄ claude-code, 2026-08-15)

Post-SHIP walkthrough of the design with the operator. Recorded for reviewer
awareness before implementation starts. Numbers reference DESIGN.md sections
where applicable.

## Clarified (no design change — restating what the doc already says)

1. **Hub is chosen, not elected.** The operator picks the machine that holds
   the pool folder; no election, no failover; hub down = pool paused
   (fail-stop by design).
2. **No A→B proxy hop.** Every inbox is a file on the hub's disk; A's send
   writes B's inbox file there, B pulls it over its own connection. Agents
   never talk to each other, only to the hub's files.
3. **One binary everywhere.** Install is identical on hub and agent
   machines; only one-time setup differs (hub: today's pool setup + stable
   binary path; agent machine: `hub join` then `serve`).
4. **Two commands, one shell** on the agent machine: `hub join ssh://…`
   once, then regular `agentchute serve <wrapper>`. The "new shell" note is
   only for the optional `ac` alias.
5. **Join is per-agent, not per-machine**; run it once per id, any time —
   no upfront count of agents.
6. **Multiple lanes, one remote user**: per-agent keypairs; sshd picks the
   authorized_keys line by offered key; forced command pins the id. The
   shared unix account carries no identity.
7. **Multi-hub / multi-pool topology confirmed**: one hub machine may host
   many pools; each remote checkout points at exactly one pool; a team may
   span machines by joining the same pool from each; pools are isolated.
8. **`@` in ids rejected**: id grammar is `[a-z0-9][a-z0-9-]*`
   (inbox.go:40), load-bearing in message-filename parsing; the same
   convention is spelled with hyphens (`codex-tiny`).

## Decided (design change, round 8 — hostname-suffix naming default)

`hub join` without `--as` defaults the id to `<local-name>-<hostname>`
(sanitized to the id grammar). Rationale, both operator-stated:
(a) uniqueness becomes local-only from the operator's view — pick `codex`,
`codex-2` locally, never remember other machines' names, list local ids
locally; (b) traffic becomes human-legible — every filename/roster row
carries the origin machine (`from-codex-tiny`), so who-talks-to-whom-from-
where is visible with no lookup. `--as` stays the verbatim override;
duplicate-id refusal (with a "hostnames collide, use --as" hint) covers
same-hostname edge; the suffix is minted once at join, never re-derived.

## Discussed, NOT decided (explicitly not in the design)

- `serve --join <url>` sugar (join-if-needed on first serve). Viable because
  join is idempotent; caveats noted (join state is per-folder so all lanes
  must agree on the URL; first run may need the operator present). Parked
  until Alex asks for it.

## Status

Implementation is expected to start soon (M1, operation seam, per DESIGN.md
§11). This file + the round-8 delta are the awareness package for codex and
grok.
