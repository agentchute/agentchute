# GROK.md

<!-- agentchute-enrollment v16 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md) (full identity precedence, polling, hooks). This file is a thin pointer.

**1. Pin your identity — once.** Base `agent_id=grok`, `vendor=xai`. Resolve your lane id ONCE at startup and reuse the SAME id on every call:

- Launched via the `ac` dispatcher (`ac run <wrapper>`)? Your id is already pinned in `$AGENTCHUTE_AGENT_ID` — use it as-is.
- Otherwise set it yourself, before `boot`:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"                                 # named lane, or…
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor xai)"  # accept the contextual default (run once, before boot)
```

Then pass `--as "$AGENTCHUTE_AGENT_ID"` (or rely on the env) on every command. **Do NOT** drive `check`/`gate`/`send` with a bare `--vendor` and no `--as`/env: with no pinned id the CLI re-derives the contextual default each call and can land on a DIFFERENT `-N` suffix (e.g. `grok-<folder>-2`), checking the WRONG inbox and missing your finish-gate. `identity --vendor` is one-time discovery, NOT a per-call identity. Running several agents of this vendor on one bus? Give EACH process its own id — a shared id routes every lane to one inbox and defeats the finish-gate.

**2. Verify at session start** (read-only; confirms you are enrolled AND present via a fresh `.live`):

```sh
agentchute doctor --as "$AGENTCHUTE_AGENT_ID"
```

**3. Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers grok --yes
```

`--wrappers grok` is single-agent scope (just this wrapper); a shared multi-vendor pool uses `--wrappers all` (see [`AGENTS.md`](AGENTS.md)). `runner` is the only supported wake path: coordination is pull-only, so senders write your inbox and never poke you; the runner polls your own inbox and injects the cue. (The old tmux/herdr wake adapters were removed.)

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the single `ac` dispatcher. Start runner-mode sessions with `ac run <wrapper>`.

**Wake events** arrive as `[agentchute:run] check inbox`, injected by your own runner when it sees new mail in your inbox. The bracketed prefix is machine metadata; the instruction is `check inbox` — so actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"`. The runner injects the cue but does NOT auto-consume mail; `check` is what CLAIMS and displays your mail, and `ack` commits it.

**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor xai
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor xai
```

**STOP / finish gate**: don't sign off, tag, or report completion until you PASS the finish gate (read-only; catches unread mail and pending required-replies — `check` alone is consume-only and misses the latter; the finish gate does NOT check `.live`, which gates only `commit`/`release`):

```sh
agentchute gate --before finish --as "$AGENTCHUTE_AGENT_ID"
```

Consume unread mail with `agentchute check --as "$AGENTCHUTE_AGENT_ID"` (CLAIMS + displays — at-least-once; a crash before `ack` re-delivers), `ack` to commit, then answer each obligation or release it with `agentchute defer --as "$AGENTCHUTE_AGENT_ID" --message <message-id> --reason "..."` until the gate is clear. The Stop hook runs `ack` then the gate for you.

Hand-protocol path (no binary, manual inbox/archive): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v16 end -->

---

## Grok-Specific Notes

- **Startup/enrollment runs through the `ac` dispatcher, not lifecycle hooks.** The grok CLI has no repo hook system (no `settings.json`/`hooks.json`, no SessionStart/UserPromptSubmit/Stop events), so `agentchute setup --wrappers grok` installs the `ac` dispatcher and `ac run grok` routes through `agentchute run` to enroll you — there is no hook install. setup still installs the `ac` dispatcher for grok precisely because no lifecycle hook can run startup enrollment. `agentchute hooks install` has no grok target by design.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules.

## Communication profile — reference & reminder

Before you send or act on a task, review the **Agent-to-Agent Communication Rules** in [`AGENTS.md`](AGENTS.md). Then adapt per this profile (grok family — `sealed`):

- Assume no memory beyond the message: make the task self-contained before acting — if CONTEXT names files by path, read them and carry the needed excerpts/facts (do not blindly inline whole files; that bloats). Make tool usage explicit to yourself (which tool serves which step); keep step-by-step reasoning internal and return exactly OUTPUT. The same holds when a sender composes tasks FOR me (presentation preference, not a schema): each message should be fully self-contained / stateless — repeat every needed fact, name tools explicitly, assume no session memory. This only reshapes how the SAME canonical contract (GOAL/CONTEXT/CONSTRAINTS/ACCEPTANCE/OUTPUT/ACTION MODE) is presented; it never adds, drops, or renames required sections.
- Avoid relying on implicit prior context, open-ended scope, and vague "make it better" asks.
- Runtime: use the reasoning variant for code; some sampling params are unsupported — don't rely on them. Tools explicit; use real-time search only when the task needs current information or verification.
- Best-fit: real-time/current-info gathering and verification, cheap high-volume iteration, decorrelated third-opinion review. Worst-fit: the hardest autonomous coding where other families lead.

_Profile verified against xAI/Grok guidance as of 2026-06-29; owner: grok wrapper operator. Re-verify on model update._
