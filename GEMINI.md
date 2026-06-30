# GEMINI.md

<!-- agentchute-enrollment v15 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md) (full identity precedence, polling, hooks). This file is a thin pointer.

**1. Pin your identity — once.** Base `agent_id=gemini-cli`, `vendor=google`. Resolve your lane id ONCE at startup and reuse the SAME id on every call:

- Launched via the installed `ac-*` launcher for this wrapper (`agentchute run`)? Your id is already pinned in `$AGENTCHUTE_AGENT_ID` — use it as-is.
- Otherwise set it yourself, before `boot`:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"                                 # named lane, or…
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor google)"  # accept the contextual default (run once, before boot)
```

Then pass `--as "$AGENTCHUTE_AGENT_ID"` (or rely on the env) on every command. **Do NOT** drive `check`/`gate`/`send` with a bare `--vendor` and no `--as`/env: with no pinned id the CLI re-derives the contextual default each call and can land on a DIFFERENT `-N` suffix (e.g. `gemini-cli-<folder>-2`), checking the WRONG inbox and missing your finish-gate. `identity --vendor` is one-time discovery, NOT a per-call identity. Running several agents of this vendor on one bus? Give EACH process its own id — a shared id routes every lane to one inbox and defeats the finish-gate.

**2. Verify at session start** (read-only; confirms you are enrolled AND present via a fresh `.live`):

```sh
agentchute doctor --as "$AGENTCHUTE_AGENT_ID"
```

**3. Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers gemini-cli --yes
```

`--wrappers gemini-cli` is single-agent scope (just this wrapper); a shared multi-vendor pool uses `--wrappers all` (see [`AGENTS.md`](AGENTS.md)). `runner` is the only supported wake path: coordination is pull-only, so senders write your inbox and never poke you; the runner polls your own inbox and injects the cue. (The old tmux/herdr wake adapters were removed.)

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the namespaced launcher for this wrapper (`ac-claude`/`ac-codex`/`ac-gemini`/`ac-grok`). Start runner-mode sessions with that installed `ac-*` launcher.

**Wake events** arrive as `[agentchute:run] check inbox`, injected by your own runner when it sees new mail in your inbox. The bracketed prefix is machine metadata; the instruction is `check inbox` — so actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"`. The runner injects the cue but does NOT auto-consume mail; `check` is what CLAIMS and displays your mail, and `ack` commits it.

**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor google
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor google
```

**STOP / finish gate**: don't sign off, tag, or report completion until you PASS the finish gate (read-only; catches unread mail and pending required-replies — `check` alone is consume-only and misses the latter; the finish gate does NOT check `.live`, which gates only `commit`/`release`):

```sh
agentchute gate --before finish --as "$AGENTCHUTE_AGENT_ID"
```

Consume unread mail with `agentchute check --as "$AGENTCHUTE_AGENT_ID"` (CLAIMS + displays — at-least-once; a crash before `ack` re-delivers), `ack` to commit, then answer each obligation or release it with `agentchute defer --as "$AGENTCHUTE_AGENT_ID" --message <message-id> --reason "..."` until the gate is clear. The Stop hook runs `ack` then the gate for you.

Hand-protocol path (no binary, manual inbox/archive): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v15 end -->

---

## Tool-Specific Notes

- **Communication Style**: Adopt the style defined in `AGENTS.md` §7 (terse, objective, lead with answer, no filler).
- **CLI Quirks**: You operate in a monospaced CLI environment. Keep responses high-signal and low-filler.
- **Methodology**: Follow the working rules in `AGENTS.md`; for review-shaped tasks, lead with file:line citations and severity-ordered findings.

## Working Rules Overrides

- None. Follow **AGENTS.md** strictly.

## Coordination & Identity

- **Identity Resolution**: Identity resolves via explicit `--as`, then `AGENTCHUTE_AGENT_ID`, then an existing tmux-pane registration, then a contextual `<wrapper>-<folder>` default when `--vendor` is provided. The contextual default adopts an existing same-pane same-vendor registration if one exists (preventing duplicate IDs on concurrent startup) and only adds suffixes (`-2`, `-3`) for genuine conflicts in different lanes. Use `AGENTCHUTE_AGENT_ID` only for custom stable lane names.
- **4-Way Verification**: High-consequence changes (e.g. protocol fixes, namespace migrations) require a "4-way verify" loop across the primary fleet lanes: `claude-code` (implementation), `codex` (shell/wire safety), `gemini-cli` (UX/Docs), and `grok` (manual/no-hooks flow). Do not merge until all four lanes are green.

> Self-description (interests, working style, etc.) belongs in this agent's
> registration body — `agentchute register --bio "..."` — not in the wrapper
> file. Wrappers are read by peers, and peers MUST NOT route work by declared
> capability (§7.1 / §12). Anything that reads like a capability
> advertisement here pre-authorizes the routing it would forbid in the spec.

## Communication profile — reference & reminder

Before you send or act on a task, review the **Agent-to-Agent Communication Rules** in [`AGENTS.md`](AGENTS.md). Then adapt per this profile (gemini family — `brief`):

- Keep it terse; rely on the envelope's context-first / instruction-last ordering. Read CONTEXT, satisfy CONSTRAINTS, return exactly OUTPUT. Define ambiguous terms; keep any structure (tags) uniform.
- Avoid chain-of-thought scaffolding and persona/motivational framing — your failure mode is ambiguity PLUS verbose scaffolding (causes over-analysis and loss of detail), not verbosity alone.
- Runtime (launch/config, not prompt text): do NOT lower temperature — keep the model default (~1.0); set thinking level high (the fast tier defaults lower); preserve full conversation history so multi-turn tool reasoning / thought signatures survive (dropping/rebuilding it can hard-error). Stamp these values — defaults drift.
- Best-fit: zero-shot generation, whole-repo/long-context reasoning, multimodal, synthesis. Worst-fit: fine-grained, diff-faithful editing — do not route that here.

_Profile verified against Google/Gemini guidance as of 2026-06-29; owner: gemini-cli (agy) wrapper operator. Re-verify on model update._
