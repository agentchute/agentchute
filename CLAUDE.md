# CLAUDE.md

<!-- agentchute-enrollment v15 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md) (full identity precedence, polling, hooks). This file is a thin pointer.

**1. Pin your identity — once.** Base `agent_id=claude-code`, `vendor=anthropic`. Resolve your lane id ONCE at startup and reuse the SAME id on every call:

- Launched via the installed `ac-*` launcher for this wrapper (`agentchute run`)? Your id is already pinned in `$AGENTCHUTE_AGENT_ID` — use it as-is.
- Otherwise set it yourself, before `boot`:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"                                 # named lane, or…
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor anthropic)"  # accept the contextual default (run once, before boot)
```

Then pass `--as "$AGENTCHUTE_AGENT_ID"` (or rely on the env) on every command. **Do NOT** drive `check`/`gate`/`send` with a bare `--vendor` and no `--as`/env: with no pinned id the CLI re-derives the contextual default each call and can land on a DIFFERENT `-N` suffix (e.g. `claude-code-<folder>-2`), checking the WRONG inbox and missing your finish-gate. `identity --vendor` is one-time discovery, NOT a per-call identity. Running several agents of this vendor on one bus? Give EACH process its own id — a shared id routes every lane to one inbox and defeats the finish-gate.

**2. Verify at session start** (read-only; confirms you are enrolled AND reachable):

```sh
agentchute doctor --as "$AGENTCHUTE_AGENT_ID"
```

**3. Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers claude-code --yes
```

`--wrappers claude-code` is single-agent scope (just this wrapper); a shared multi-vendor pool uses `--wrappers all` (see [`AGENTS.md`](AGENTS.md)). Use `--wake runner` for the universal launcher+socket path; add `tmux` or `herdr` if peers reach you via pane send-keys (e.g. `--wake runner,tmux`).

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the namespaced launcher for this wrapper (`ac-claude`/`ac-codex`/`ac-gemini`/`ac-grok`). Start runner-mode sessions with that installed `ac-*` launcher.

**Wake events** arrive as `[agentchute:tmux] check inbox`, `[agentchute:herdr] check inbox`, or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox` — so actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"`. The runner injects the cue but does NOT auto-consume mail; `check` is what reads, archives, and records your reply obligations.

**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor anthropic
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor anthropic
```

**STOP / finish gate**: don't sign off, tag, or report completion until you PASS the finish gate (read-only; catches unread mail, pending required-replies, AND liveness — `check` alone is consume-only and misses the last two):

```sh
agentchute gate --before finish --as "$AGENTCHUTE_AGENT_ID"
```

Consume unread mail with `agentchute check --as "$AGENTCHUTE_AGENT_ID"` (it reads + archives), then answer each obligation or release it with `agentchute defer --as "$AGENTCHUTE_AGENT_ID" --message <message-id> --reason "..."` until the gate is clear.

Hand-protocol path (no binary, manual inbox/archive): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v15 end -->

---

## Claude-specific notes

None at the moment. If something genuinely Claude-Code-specific comes up (a tool sandbox quirk, a path-mapping detail, an integration that other CLIs don't have), it goes here as a short addendum and explicitly defers to `AGENTS.md` for everything else.

## Communication profile — reference & reminder

Before you send or act on a task, review the **Agent-to-Agent Communication Rules** in [`AGENTS.md`](AGENTS.md). Then adapt per this profile (claude family — `guided`):

- Rich structure is tolerated; you MAY reason privately through hard design/review before acting. Honor CONSTRAINTS as invariants; stop at ACCEPTANCE (no gold-plating); produce OUTPUT exactly.
- Do not let a reasoning invitation become scope expansion — ACCEPTANCE is the stop line.
- Runtime (launch/config, not prompt text): raise effort / extended thinking for hard reasoning, architecture, or review; normal effort for well-specified slices.
- Best-fit: hard reasoning, novel design, synthesis, final review. Worst-fit (over-qualified): rote edits a worker handles. Tier note: larger/smaller models of this family share this profile — route hard work to the larger tier, well-specified execution to the smaller.

_Profile verified against Anthropic/Claude guidance as of 2026-06-29; owner: claude-code wrapper operator. Re-verify on model update._
