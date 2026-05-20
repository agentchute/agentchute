# claude-code wrapper-native loop R&D, Round 1

Topic: how Claude Code's `/loop` should be the no-tmux baseline for agentchute on the Claude side.

---

## 1. What exists today in Claude Code

**Native recurring-task primitive: `/loop`.** Documented at https://code.claude.com/docs/en/scheduled-tasks.md.

- **Syntax**: `/loop [interval] [prompt]`. Both optional; order flexible. Trailing-format works too (`/loop check the inbox every 5 minutes`).
- **Interval units**: `s`, `m`, `h`, `d`. Sub-minute rounds to nearest minute (cron granularity).
- **Per-tick shape**: each tick submits a fresh user prompt. **UserPromptSubmit hook fires per tick**. SessionStart hook does NOT — that's a session-level event, fires only on startup/resume/clear/compact.
- **Permissions / tools**: each tick inherits the session's permissions, MCP servers, tool access. No sandbox isolation.
- **Cancel**: Esc while waiting between iterations.
- **Persistence**: auto-deletes 7 days after creation. Survives `/resume`. Lost on fresh session start.
- **Default prompt file**: `.claude/loop.md` if present; otherwise a default maintenance prompt.
- **Cost shape**: each tick = one full model turn. 5-minute interval over 24h ≈ 288 turns/day. Operator's call on the cadence-vs-cost trade-off.

### How this lines up with v0.1.1 hooks

- **SessionStart** fires once → runs `boot --context-only` → model knows enrolled state at start.
- **Per /loop tick → UserPromptSubmit** fires → runs `pending` (now with `--claude-hook UserPromptSubmit` JSON) → injects current inbox + ledger state into the model's context for THIS tick.
- **Per tick → model turn** → model sees the context, decides what to do (run `check`, send a reply, defer, or just acknowledge "nothing to do").
- **Stop hook fires at end of turn** → `gate --before finish --json` → if obligations remain, exit 2, Claude continues for another turn.

The hooks already do the right thing on every /loop tick. **The /loop primitive is the wake mechanism; the hooks are the awareness mechanism. Together they're a complete recipient-side story for Claude Code with zero new agentchute infrastructure.**

---

## 2. Recommended agentchute integration (3 candidates)

### Recommended default: `/loop 5m process any agentchute mail`

```
/loop 5m process any agentchute mail; reply or defer if anything is reply-required; stop when done
```

- 5m balances responsiveness and cost. 288 ticks/day at 5m feels reasonable for an active project; 15m or 30m is fine for less time-critical pools.
- The terse prompt is enough because UserPromptSubmit's `pending --claude-hook UserPromptSubmit` injects the inbox state directly — the model already sees "you have 2 unread, 1 pending reply" in context before it processes the prompt.
- The model executes by calling `agentchute check` itself (via its Bash tool / shell). Not a separate `agentchute self-poll` command — see the cross-wrapper question below.
- Stop hook is the guardrail: if the model tries to end the turn with obligations open, the gate blocks it. /loop's "fresh prompt per tick" model means this is automatic.

### Lower-token alternate: `/loop 5m` with `.claude/loop.md`

Move the prompt into a file:

```markdown
# .claude/loop.md
Run `agentchute pending --as claude-code` quietly. If there are unread messages
or pending replies, process them via `check` + `send --reply-to`. Otherwise
exit silently. Do not produce conversational output unless action is needed.
```

Then the operator just types `/loop 5m` — Claude reads `.claude/loop.md` as the default prompt. Saves a few tokens per tick by encouraging silent acknowledgment on empty-inbox ticks.

### High-responsiveness alternate: `/loop 30s` with strict silent-on-empty

```
/loop 30s check inbox silently; if nothing to do, exit with one word "ok"; if there's work, process and reply
```

For pools where seconds of latency matter. 30s × 24h = 2880 turns/day — meaningfully more cost. Worth it for active multi-agent sessions where peers expect sub-minute response; overkill for casual coordination.

---

## 3. Cross-wrapper standardization

**My recommendation: NO canonical `agentchute self-poll` command.** Each wrapper's loop should call **the wrapper itself with an inbox-processing prompt** — the model owns the consumption decision.

Reasoning, drawn from the v0.1.1 silent-drain lesson:

> "Never use `agentchute check` in a hook unless you're certain the hook output reaches the model's context."

The same applies to recurring tasks. If a recurring task runs `agentchute check` directly (in a shell loop, in a wrapper-side scheduler that doesn't invoke the model), mail gets consumed by a process that doesn't have a model in the loop. The model never sees the message; the obligation is silently discharged.

The right shape is: **the wrapper's recurring task invokes the wrapper itself**. The model is always in the loop. The model decides whether to call `check`, when, and what to do after.

What COULD agentchute provide to make this consistent across wrappers? A canonical **prompt template** that the enrollment block teaches each wrapper. Something like:

```
# in CLAUDE.md / CODEX.md / GEMINI.md enrollment block:
Recurring task pattern: run `agentchute pending` quietly. If unread or
pending-reply, run `agentchute check` and process; reply via `agentchute
send --reply-to` or defer via `agentchute defer`. Otherwise acknowledge
silently. Do not generate output if nothing to do.
```

Each wrapper picks its native scheduling primitive (`/loop` for Claude, operator scheduler for codex-cli, whatever for gemini) but runs the same prompt template. Cross-wrapper consistency without cross-wrapper code.

---

## 4. Failure modes

| Failure | What happens | Mitigation |
|---|---|---|
| Fresh Claude session (`/clear`) | /loop is gone; agent won't poll until manually re-armed. | SessionStart hook's `boot --context-only` could include a "you may want to re-arm /loop" line in its context. Or the enrollment block teaches "re-arm /loop on first prompt after a fresh session." |
| /loop tick generates output even when inbox is empty | Token waste + chat noise. | Prompt explicitly says "exit silently if nothing to do"; verify in real-bake. |
| Multiple /loop active (e.g., one for agentchute, one for something else) | Each tick is independent; both fire. | No conflict per se, but the user's total turn rate doubles. Operator's call. |
| Model decides to skip a tick's work | Obligation stays in ledger; next tick's UserPromptSubmit re-injects it. | Self-healing — Stop hook on the skipping tick would have blocked anyway if there's a reply-required outstanding. |
| Rate limits / API outage | /loop fires but model errors out. Claude shows the error; loop continues next tick. | Claude Code's own rate-limit handling applies; nothing agentchute-side. |
| Cost runaway | 30s loop with verbose model = a lot of tokens. | Doctor could surface "you have an active /loop at <interval> — daily tick budget ≈ X turns" as a WARN when wiring is checked. |

---

## Open question for the team

Should agentchute (the binary) gain a small new command like `agentchute init --claude-loop-recipe` that prints / installs the recommended `.claude/loop.md` content? Or is leaving it as a docs recipe better? I lean docs-only — the recipe is short enough that operators can paste it from the README without needing a binary helper.
