# Playbooks — the recurring bus rituals

Companion to the **Working efficiently on this bus** rules in [`AGENTS.md`](../../AGENTS.md) (E1–E9). Each playbook is the step sequence; the E-rules say when it's mandatory. `$ID` is your pinned lane id.

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
3. Pick the verification tier (E5) by surface; run it. Substantive claims — numbers, protocol semantics, file scope, rendered assets — get re-derived from the tree/binary, not trusted from the PR text. Render any touched image/binary.
4. Verdict: `SHIP` or `FIX` with file:line evidence. Deliver on the bus (the real gate signal) and mirror to `gh pr comment` (shared-token self-block prevents `gh pr review`).

## delta-regate (re-gate after a fix — E4)

1. Require the new pinned head + the prior reviewed head.
2. `git diff --name-status <prior-head>..<new-head>` — confirm the delta is exactly the claimed files; anything extra → `FIX`.
3. Re-verify only the changed claims at the tier the change warrants: a substantive fix gets re-derivation; a one-line mechanical fix gets a spot-check (E5). Do not re-review unchanged files.
4. Verdict with the delta range cited.

## owed-audit (when `pending` grows stale — E7)

1. `agentchute pending --as $ID` — list outstanding obligations with their `by` deadlines.
2. For each overdue ref, grep the archive for a reply: `grep -l "in_reply_to: <ref>" .agentchute/loop/archive/*.md` — a matching reply means it discharged late or out-of-band; a missing one means the peer never answered.
3. Classify: still-needed (re-ask with a fresh `--ask`), answered-out-of-band (drop), obsolete (drop — the program moved past it).
4. Drop by pruning the entry from your own `state/$ID/owed.json` (your lane's state; operator-grade edit, keep the JSON valid). Record what you dropped in the program log or your report.
5. Going forward, keep the ledger honest at the source: `--ask` only when you need the answer to proceed.
