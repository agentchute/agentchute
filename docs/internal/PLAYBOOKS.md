# Playbooks — the recurring bus rituals

Companion to the **Working efficiently on this bus** rules in [`AGENTS.md`](../../AGENTS.md) (E1–E10). Each playbook is the step sequence; the E-rules say when it's mandatory. `$ID` is your pinned lane id. **Each playbook's imperative actions bind the lane performing that role** — in `gate-review`, the pre-verdict refresh governs reviewer-side verdict timing and never tells the implementer when to commit, push, or hold. A playbook may still state conditions on the *ask* it receives (pinned SHAs, an `AUTHORIZATION:` line, a delta pinned to the reviewer's named surface); those bind the asker.

## bus-turn (per wake cue — E8)

1. `agentchute check --as $ID` — once. Claims + displays everything waiting.
2. Batch ALL state verification you need into one pass now (doctor/status/git — only what this mail actually requires).
3. Act: do the work, or reply `NEEDS-INFO` (R2). Replies use `--reply-to <ref>` so the asker's obligation discharges.
4. Commit the turn yourself: `agentchute ack --as $ID`, then `agentchute pending --as $ID` if you carry asks, then `agentchute gate --before finish --as $ID`.
5. Stop. No speculative `check` until the next cue. Wrapper Stop hooks are a backstop only — `check` claims, `ack` commits, and a crash before `ack` redelivers.

## fleet-brief (any brief to 3+ recipients — E1)

1. Write the brief ONCE to `proposal/<program>/<name>.md` on the shared checkout (evidence, background, shared constraints all live there).
2. Per recipient, send a pointer message: file path + that lane's own `GOAL / ACCEPTANCE / ACTION MODE` and any per-lane delta — ≤10 lines total (S6 still applies: each recipient gets its own contract).
3. Collect replies; write the synthesis to the same directory; the next round's messages point at it.

## gate-review (fresh PR gate)

1. Freeze first (E3): the ask must pin base SHA, head SHA, changed files, allowed delta. Unpinned ask → `NEEDS-INFO`, no work.
2. `git fetch` + diff base..head; confirm the file scope matches the stated scope exactly — surplus files are a finding on their own.
3. Pick the verification tier (E5) by surface; run it. Substantive claims — numbers, protocol semantics, file scope, rendered assets — get re-derived from the tree/binary, not trusted from the PR text. Render any touched image/binary. **Then read for what no checklist enumerates**: once every mandated item verifies, re-read each changed section start-to-finish and test its own summary and scoping sentences — the universals, "never X", "always Y", "only Z", "carries no W" — against the body they summarize. Those sentences are un-enumerated by construction, so an item-by-item pass is structurally blind to them, and in normative text they are exactly what a downstream implementer builds a codec against. Cost of skipping it (2026-08-17, PR #146 M2 spec): a section asserted that terminal frames "carry counts, never arrays" two paragraphs above a clause mandating an always-present array field, plus two more inline arrays elsewhere. Every enumerated item passed; the primary reviewer approved past the contradiction and the second gate caught it.
4. **Own the checkout, own the cleanup — same turn, before the verdict, every lane.** If the review needed a separate pinned-SHA checkout, verify it's still exactly what you left it — `git status --porcelain` empty AND `git rev-parse HEAD` equals the SHA you were reviewing — then `git worktree remove <path>`. Either check failing means a stray commit landed in the checkout since — stop and preserve it, do not remove. This binds regardless of wrapper — 27 stale `review-pr*` checkouts accumulated from BOTH Claude and Codex review paths before this line existed anywhere every lane actually reads. Skip only when the review used no separate checkout. Do NOT delete a local branch here — that's Rule 5 / the tier table's call (which row applies depends on whether the branch was ever pushed), not this step's.
5. **Re-check immediately before you send, not just before you start** — with commands that actually run mid-turn. A verdict is a snapshot, and a long review turn means the branch may have moved under it. Right before delivering: re-run `gh pr view <n> --json headRefOid`, then run the read-only `agentchute gate --before continue --as <id>`. **Not a second `check`** — the guard denies `check` under any spelling until `turn-end` (§Guard), and E8 forbids a speculative repeat consume, so "check your inbox again" is not an executable instruction on a guarded lane. The continue gate is the documented in-session catch-up primitive (AGENTS.md §4) and stays available while latched: it blocks on *unread* mail and merely warns about your own claimed-but-unacked mail. If it blocks, end the turn, process the resulting cue, and deliver the verdict after it; if it is clear, send. Never build a finding on a peer's *local* worktree state either — a commit sitting in their checkout but not on the PR head may simply not have been pushed yet, and by the time you finish writing it usually has been. Both failures happened in one turn on PR #146: a FIX went out for something already fixed and pushed, criticising a push-discipline gap that had already closed.
6. Verdict: `SHIP` or `FIX` with file:line evidence, delivered on the bus — the bus reply IS the gate signal. Mirror to `gh pr comment` ONLY if the gate ask's `AUTHORIZATION:` line names that PR comment (E9/R4: an external message is irreversible work; no authorization = bus-only). When mirroring is authorized, use `gh pr comment` — the shared token self-blocks `gh pr review`.

## delta-regate (re-gate after a fix — E4)

1. Require the new pinned head + the prior reviewed head.
2. `git diff --name-status <prior-head>..<new-head>` — confirm the delta is exactly the claimed files; anything extra → `FIX`.
3. Re-verify only the changed claims at the tier the change warrants: a substantive fix gets re-derivation; a one-line mechanical fix gets a spot-check (E5). Do not re-review unchanged files.
4. Same checkout-cleanup obligation as gate-review step 4: if this re-gate needed its own pinned-SHA checkout, remove it before the verdict too.
5. Verdict with the delta range cited — and re-check `headRefOid` right before sending, per gate-review step 5. A re-gate is the likeliest place for the head to move again while you work.

## release-notes (before every tag)

The release workflow uses `docs/releases/<tag>.md` **as the GitHub release body**, and smokes
`test -s` on it before publishing. Two things follow, and both have bitten:

1. **The file must exist before the tag is pushed.** A tag without one fails the release job at
   its smoke gate, after the tag exists. That is recoverable only because nothing was published
   yet — delete the tag, add the file, re-tag.
2. **The file must be final at tag time, not a working draft.** Whatever it says becomes the
   published release body verbatim. `docs/releases/v1.6.0.md` opened with "**In progress.** …it
   is not a published release" and that sentence went live on the GitHub release, because the
   file was written as an accumulating scratchpad during the milestone and nobody finalised it.

So, before pushing any `v*` tag:

1. `test -s docs/releases/<tag>.md` — the same check the workflow runs, run where it is cheap.
2. Read the first paragraph as an operator would, on the release page. Strip any in-progress or
   accumulating-notes framing; it is a published document the moment the tag lands.
3. Confirm it states the upgrade obligation explicitly — what an operator must do, and what is
   unchanged (protocol, wire version, whether hub and joiners must match).
4. Only then tag.

If a milestone wants a scratchpad, keep it somewhere that is not the notes file the workflow
publishes.

## owed-audit (when `pending` grows stale — E7)

1. `agentchute pending --as $ID` — list outstanding obligations with their `by` deadlines.
2. For each overdue ref, grep the archive for a reply: `grep -l "in_reply_to: <ref>" .agentchute/loop/archive/*.md` — a matching reply means it discharged late or out-of-band; a missing one means the peer never answered.
3. Classify: still-needed (re-ask with a fresh `--ask`), answered-out-of-band (drop), obsolete (drop — the program moved past it).
4. Drop by pruning the entry from your own `state/$ID/owed.json` (your lane's state; operator-grade edit, keep the JSON valid). Record what you dropped in the program log or your report.
5. Going forward, keep the ledger honest at the source: `--ask` only when you need the answer to proceed.
