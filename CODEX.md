# CODEX.md

<!-- agentchute-enrollment v17 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md) (full identity precedence, polling, hooks). This file is a thin pointer.

**1. Pin your identity — once.** Base `agent_id=codex`, `vendor=openai`. Resolve your lane id ONCE at startup and reuse the SAME id on every call:

- Launched via the `ac` dispatcher (`ac run <wrapper>`)? Your id is already pinned in `$AGENTCHUTE_AGENT_ID` — use it as-is.
- Otherwise set it yourself, before `boot`:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"                                 # named lane, or…
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor openai)"  # accept the contextual default (run once, before boot)
```

Then pass `--as "$AGENTCHUTE_AGENT_ID"` (or rely on the env) on every command. **Do NOT** drive `check`/`gate`/`send` with a bare `--vendor` and no `--as`/env: with no pinned id the CLI re-derives the contextual default each call and can land on a DIFFERENT `-N` suffix (e.g. `codex-<folder>-2`), checking the WRONG inbox and missing your finish-gate. `identity --vendor` is one-time discovery, NOT a per-call identity. Running several agents of this vendor on one bus? Give EACH process its own id — a shared id routes every lane to one inbox and defeats the finish-gate.

**2. Verify at session start** (read-only; confirms you are enrolled AND present via a fresh `.live`):

```sh
agentchute doctor --as "$AGENTCHUTE_AGENT_ID"
```

**3. Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers codex --yes
```

`--wrappers codex` is single-agent scope (just this wrapper); a shared multi-vendor pool uses `--wrappers all` (see [`AGENTS.md`](AGENTS.md)). `runner` is the only supported wake path: coordination is pull-only, so senders write your inbox and never poke you; the runner polls your own inbox and injects the cue. (The old tmux/herdr wake adapters were removed.)

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the single `ac` dispatcher. Start runner-mode sessions with `ac run <wrapper>`.

**Wake events** arrive as `[agentchute:run] check inbox`, injected by your own runner when it sees new mail in your inbox. The bracketed prefix is machine metadata; the instruction is `check inbox` — so actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"`. The runner injects the cue but does NOT auto-consume mail; `check` is what CLAIMS and displays your mail, and `ack` commits it.

**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor openai
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor openai
```

**STOP / finish gate**: don't sign off, tag, or report completion until you PASS the finish gate (read-only; blocks on unread/malformed mail or an unregistered self — `check` claims mail but the gate is the read-only STOP verdict; the finish gate does NOT check `.live`, which gates only `commit`/`release`):

```sh
agentchute gate --before finish --as "$AGENTCHUTE_AGENT_ID"
```

Consume unread mail with `agentchute check --as "$AGENTCHUTE_AGENT_ID"` (CLAIMS + displays — at-least-once; a crash before `ack` re-delivers), then `ack` to commit — that clears the finish gate (which blocks only on unread/malformed mail). Reply to any message that needs one with `agentchute send --reply-to <ref>`; reply obligations are asker-owned (`.owed`) and never block the recipient. The Stop hook runs `ack` then the gate for you.

Hand-protocol path (no binary, manual inbox/archive): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v17 end -->

---

## Codex-Specific Notes

- Default posture: review first. Identify bugs, scope creep, behavioral regressions, missing tests, and unclear spec/code mismatches before drafting.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Keep patches narrow and use the standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Do not reach into the chorus-protocol sibling repo from this repo (see HANDOFF.md for context). agentchute is independent.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules; codex's review posture (concise, file:line cited, severity-ordered findings) flows from the rules there.

## Communication profile — reference & reminder

Before you send or act on a task, review the **Agent-to-Agent Communication Rules** in [`AGENTS.md`](AGENTS.md). Then adapt per this profile (codex family — `outcome`):

- Treat GOAL + ACCEPTANCE as the outcome and choose your own steps. Do not write an upfront plan or status preamble before executing (it can cause early stop). Respect `review-only` vs `implement` — do not turn a request into edits unless the mode says so.
- Durable repo conventions live in [`AGENTS.md`](AGENTS.md); treat envelope CONSTRAINTS as task-specific additions and don't restate durable rules in the task.
- Verify against ACCEPTANCE (run tests/build) before declaring done; cite what you ran.
- Runtime: scale reasoning effort to difficulty (medium default, higher for hard, long-horizon work).
- Best-fit: autonomous multi-file execution, hard refactors, long-horizon agentic coding, review. Worst-fit: tight step-by-step human supervision.
- **How to compose tasks FOR me (presentation preference, not a schema):** keep it concise and outcome-first — Goal / Context / Constraints / Done-when wording WITHIN the canonical contract; do NOT ask me for an upfront plan (it can trigger an early stop); durable repo rules live in [`AGENTS.md`](AGENTS.md), not the task. This only reshapes how the SAME canonical contract (GOAL/CONTEXT/CONSTRAINTS/ACCEPTANCE/OUTPUT/ACTION MODE) is presented; it never adds, drops, or renames required sections.

_Profile verified against OpenAI/Codex guidance as of 2026-06-29; owner: codex wrapper operator. Re-verify on model update._
