# Fleet-efficiency retrospective — 5-way synthesis (round 1)

Individual reports: claude (`claude-report.md` here), codex (bus seq-84), sonnet (bus seq-70), gemini (her artifact + bus seq-39 summary), grok (bus seq-30). All five were produced independently; convergence below is genuine.

## Shared counter-finding (all lanes, protect it)
The review machinery is fast and load-bearing: median reply 30 s, median PR merge 7–9 min, 68% zero-fix PRs, 11 releases/50 h through full gates. Sonnet's hub-vs-lane comparison proved double verification catches real defects a single pass missed (the v:2 capture claim on #70; the gray presence dot on #71 that survived THREE prior reviews). Nothing below reduces gate substance.

## Consolidated proposal set (S1–S14 + parked)

**S1 [process+skill] Briefs by reference** *(claude P3 + codex #2 + sonnet #1 — three independent #1-rankings; 24–36% of archive bytes are duplicate copies, 49 duplicate groups, all from the hub).* Program briefs, synthesis docs, and review evidence are written ONCE to `proposal/<program>/` in the shared checkout; bus messages carry a pointer + per-lane GOAL/ACCEPTANCE/ACTION-MODE delta of ≤10 lines. A `fleet-brief` skill generates the per-recipient envelopes.

**S2 [role/process] Pair-owned gate loops** *(claude P4 + codex #1).* Implementer ↔ assigned reviewer run their delta loops DIRECTLY; the hub receives a one-line SHIP/FIX tally. Hub-owned remains: merge/release/tag, synthesis rounds, conflicts, Alex-facing reports. (Today: 14 of 529 messages bypass the hub.)

**S3 [AGENTS.md+skill] Review-freeze + delta-regate** *(codex #3 + sonnet skill-2; PR #70/#71 evidence).* Every gate ask carries a REVIEW-FREEZE block: base SHA, head SHA, changed files, allowed delta. A `delta-regate` skill: diff prior-head..new-head, confirm delta scope, re-verify ONLY changed claims — no re-derivation of unchanged conclusions.

**S4 [AGENTS.md/process] Verification tiers** *(codex #4 + sonnet #3 + grok fix-1 + the recorded "substance not re-clicks" lesson).* Implementer runs the full ritual pre-PR. Reviewers: docs/prose gate = diff + targeted checks, no test-suite rerun; localized code = targeted tests + CI; protocol/runtime/release surface = one senior independent full run (not all). Substantive claims (numbers, semantics, file scope, rendered assets) get full independent re-derivation; single-line mechanical fixes get a spot-check.

**S5 [skill] `bus-turn` cue discipline** *(claude P1/P2 + grok fix-1; 4.1 checks per cue, 42.5% of hub Bash = hygiene; AMENDED per codex round-1 objection).* One `check` per cue, drain, act/reply, then the bus-turn sequence itself commits: `ack → pending → gate`. Wrapper Stop hooks are a BACKSTOP, not the primary commit path — `check` only claims, and hookless lanes or crash-before-stop would otherwise redeliver handled mail. All state verification batched into ONE upfront pass; no speculative checks.

**S6 [AGENTS.md+skill] Executed-true bus content + fact linter** *(gemini #2/#4 + claude P8/P9).* CLI snippets in any brief/report must be verified against `--help`/a run before sending. A small `tools/fact-sweep` script pins the launch constants (269, −8,262, 9 vectors, versions) against the tree; run pre-tag and pre-copy.

**S7 [process+skill] Owed hygiene** *(codex #5; number independently confirmed by sonnet in round 1, superseding her earlier count-only check).* The hub's `.owed` ledger: 22 entries, **17 (77%) past their `by` deadline**, oldest 2+ days — the overdue signal has degraded into background noise, which defeats the ledger's purpose as a warning. Fix: `--ask` only when a reply is genuinely needed; an `owed-audit` helper lists stale obligations with matching-reply detection; the hub clears or expires the current backlog as part of adopting this.

**S8 [AGENTS.md] AUTHORIZATION field** *(codex #6).* Irreversible work (push/PR/merge/tag/publish/install) carries an explicit AUTHORIZATION line: exact commands/targets/expiry. Absent = local-only. Codifies the safety pattern codex already enforces, removes the infer-and-stall tax.

**S9 [role] Lane codification refresh** *(codex #7, consistent with the ratified operating model).* codex = implementation + deterministic gates + release mechanics when authorized; sonnet = senior design/security/runtime gates, not routine small diffs; gemini = prose/docs/web copy, NO git mechanics; grok = optional bench, narrow pinned targets only; claude = integrator/release owner + synthesis, delegates pair loops (S2).

**S10 [per-lane habits, self-directed]** claude: ranged Reads (>400-line files: Grep→ranged Read; never re-Read post-edit), background CI watch instead of sleep loops (5.2 h recovered), two-strike rule on failing commands (the 58× boot retry). grok: first-turn deliverable, quote-and-cite from brief instead of re-verification loops. gemini: verify her send path emits the CURRENT envelope (2 malformed legacy-field messages in quarantine — sonnet #4).

**S11 [process/repo] Environment hygiene** *(gemini #1/#3).* Test invocations strip `AGENTCHUTE_*` env (already a recorded lesson — make it a written rule); scratch/proposal dirs carry their own go.mod or stay outside `go test ./...` reach.

**S12 [role, Alex's call] RFC-writer bus lane** *(claude P10).* Ends Alex-as-relay for the RFC writer's drops and reviews.

**S13 [process] Hub session lifecycle** *(claude P11; the 65 MB session behind 237 full-context cue reloads).* After each shipped program: handoff memory → fresh session.

**S14 [repo config, Alex's Cloudflare] Scope Pages preview builds to `web/**`** *(sonnet #5; 64 of 90 PR comments are the CF bot on non-web PRs).*

**Parked (post-1.0 product round only):** `check --ack` one-shot CLI verb — adds surface, touches two-phase semantics; goes to the certification-round agenda, not this retro.

## Deliverable shape (on Alex's ratification)
1. `AGENTS.md`: one new section ("Working efficiently on this bus") carrying S3/S4/S6/S7/S8 + the S1/S2 routing rules — additive, ~40 lines.
2. Skills, per-lane where each harness supports them: `bus-turn`, `fleet-brief`, `gate-review`, `delta-regate`, `owed-audit` + `tools/fact-sweep.sh`.
3. `team_operating_model` memory + AGENTS.md role table refresh (S9).
4. Self-directed habit items (S10) recorded per-lane; no artifact needed.
5. Alex-side: S12 (RFC lane), S14 (Cloudflare setting).

## Weighting note
Seniors (claude, codex, sonnet) carried the deep reviews and their rankings agree on S1–S5 as the top block; gemini/grok items are incorporated where they survived senior evidence (S6, S11, and grok's own S5/S10 contributions). Per Alex: senior opinions weigh more in this loop.
