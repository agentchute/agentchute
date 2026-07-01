<!-- agentchute-enrollment v17 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md) (full identity precedence, polling, hooks). This file is a thin pointer.

**1. Pin your identity — once.** Base `agent_id={{AGENT_ID}}`, `vendor={{VENDOR}}`. Resolve your lane id ONCE at startup and reuse the SAME id on every call:

- Launched via the `ac` dispatcher (`ac run <wrapper>`)? Your id is already pinned in `$AGENTCHUTE_AGENT_ID` — use it as-is.
- Otherwise set it yourself, before `boot`:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"                                 # named lane, or…
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor {{VENDOR}})"  # accept the contextual default (run once, before boot)
```

Then pass `--as "$AGENTCHUTE_AGENT_ID"` (or rely on the env) on every command. **Do NOT** drive `check`/`gate`/`send` with a bare `--vendor` and no `--as`/env: with no pinned id the CLI re-derives the contextual default each call and can land on a DIFFERENT `-N` suffix (e.g. `{{AGENT_ID}}-<folder>-2`), checking the WRONG inbox and missing your finish-gate. `identity --vendor` is one-time discovery, NOT a per-call identity. Running several agents of this vendor on one bus? Give EACH process its own id — a shared id routes every lane to one inbox and defeats the finish-gate.

**2. Verify at session start** (read-only; confirms you are enrolled AND present via a fresh `.live`):

```sh
agentchute doctor --as "$AGENTCHUTE_AGENT_ID"
```

**3. Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers {{AGENT_ID}} --yes
```

`--wrappers {{AGENT_ID}}` is single-agent scope (just this wrapper); a shared multi-vendor pool uses `--wrappers all` (see [`AGENTS.md`](AGENTS.md)). `runner` is the only supported wake path: coordination is pull-only, so senders write your inbox and never poke you; the runner polls your own inbox and injects the cue. (The old tmux/herdr wake adapters were removed.)

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the single `ac` dispatcher. Start runner-mode sessions with `ac run <wrapper>`.

**Wake events** arrive as `[agentchute:run] check inbox`, injected by your own runner when it sees new mail in your inbox. The bracketed prefix is machine metadata; the instruction is `check inbox` — so actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"`. The runner injects the cue but does NOT auto-consume mail; `check` is what CLAIMS and displays your mail, and `ack` commits it.

**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor {{VENDOR}}
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor {{VENDOR}}
```

**STOP / finish gate**: don't sign off, tag, or report completion until you PASS the finish gate (read-only; catches unread mail and pending required-replies — `check` alone is consume-only and misses the latter; the finish gate does NOT check `.live`, which gates only `commit`/`release`):

```sh
agentchute gate --before finish --as "$AGENTCHUTE_AGENT_ID"
```

Consume unread mail with `agentchute check --as "$AGENTCHUTE_AGENT_ID"` (CLAIMS + displays — at-least-once; a crash before `ack` re-delivers), `ack` to commit, then answer each obligation or release it with `agentchute defer --as "$AGENTCHUTE_AGENT_ID" --message <message-id> --reason "..."` until the gate is clear. The Stop hook runs `ack` then the gate for you.

Hand-protocol path (no binary, manual inbox/archive): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v17 end -->
