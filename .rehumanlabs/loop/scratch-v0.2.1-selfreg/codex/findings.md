# Self-registration investigation — codex

## Summary

Claude's hypothesis is basically right, with one important sharpening: self-registration is normative in the spec, but the reference CLI does not enforce it at the main "agent is operating" entry points.

`boot` and `register` are the only paths that create or refresh `agents/<id>.md` and `inbox/<id>/`. If hooks are absent, broken, or only partially installed, the model is expected to read the enrollment block and voluntarily run `boot`. That is the unreliable part. Once that step is missed, several commands make the failure look like "nothing is wrong" instead of "you are not registered."

## What works today

- Spec says every agent MUST publish a registration: `AGENTCHUTE.md` §5 says every agent in a pool must publish a registration record, and §7.2 says self-registration on every session start is mandatory protocol overhead.
- `boot` calls `performRegister`, then peeks inbox state. `performRegister` writes `agents/<id>.md` and creates `inbox/<id>/`.
- Hook templates do use `boot` on SessionStart:
  - Claude: `boot --as claude-code --vendor anthropic --context-only`
  - codex: `boot --as codex --vendor openai --codex-hook SessionStart`
  - Gemini: `boot --as gemini-cli --vendor google --context-only`
- `self-poll` already detects missing registration or missing inbox as `needs_boot` and exits 2. `doctor --as <id>` already reports missing registration as a BLOCKER.

So the primitive is implemented, but only if the SessionStart path actually runs.

## Silent failure paths

1. `agentchute init` installs enrollment text, not hooks.

   `init.go` writes or updates `CLAUDE.md` / `CODEX.md` / `GEMINI.md` / `AGENTS.md` and loop directories. It does not copy `examples/hooks/...` into `.claude/`, `.codex/`, or `.gemini/`. The README still has a manual hook-copy step. In a no-hook setup, the only thing forcing `boot` is a natural-language instruction in the enrollment block.

2. Missing inbox reads as empty.

   `internal/loop/inbox.go` treats `os.ErrNotExist` in `ListInboxMessagesWithSkipped` as `(nil, nil, nil)`. That makes sense for read-only peeks in some contexts, but it means an unregistered agent can run `pending`, `check`, `gate`, or `watch` and see an empty inbox instead of a "you have not booted" error.

3. `check` does not require registration.

   `check.go` updates `last_seen` only if `agents/<self>.md` exists. If it is missing, it ignores `os.IsNotExist` and continues to list `inbox/<self>/`. Since a missing inbox is "empty", `check` can print `(inbox empty)` for an agent that never registered.

4. `send` allows unregistered senders.

   `send.go` also updates sender `last_seen` only if the sender registration exists. If `agents/<from>.md` is missing, it continues. Therefore an unregistered sender can deliver to any already-registered recipient. The outbound message has `from: <id>`, but peers cannot enumerate or wake that sender from the registry, and replies will fail if the sender's own inbox was never created.

5. `gate --before finish` and `gate --before continue` do not block on missing registration.

   `gate` only checks stale/missing registration for `commit` and `release`. The comment says `consensus` and `finish` skip registration freshness because they only care about inbox/reply work. That means the normal Stop/AfterAgent gate can allow an unregistered session to end cleanly.

6. `pending` hook context does not surface `needs_boot`.

   If a pool has UserPromptSubmit/BeforeAgent hooks but no working SessionStart `boot`, `pending` will say there are no unread messages and no pending reply obligations. `self-poll` has the correct `needs_boot` logic, but the normal hook-safe context path does not reuse it.

7. The fallback docs are too weak for LLM reliability.

   `templates/enrollment/wrapper.md` says "Run `agentchute boot ...`. (If hooks are configured, this runs automatically)." That is semantically correct, but in practice agents skip or forget one-time imperative instructions unless the toolchain enforces them. Alex's "it never worked" report fits this failure mode: the contract exists, but it is not load-bearing unless hooks/schedulers are installed.

## Other possible failure reasons checked

- File modes are not the likely cause. `performRegister` writes through `loop.WriteRegistration` and creates the inbox via `EnsurePrivateDir`. `init` creates the top-level loop dirs at 0700.
- Validation is not silently dropping valid registrations. `performRegister` errors on invalid `agent_id`, missing vendor, malformed existing registration, or bad write/create operations.
- Stale `last_seen` is a secondary symptom, not the root. `check`, `send`, and `status --as` refresh `last_seen` only when a registration already exists; they do not create one.
- `register --announce` already exists, but it is operator-gated and not part of mandatory session-start registration. Broadcast/hello is therefore not the missing enforcement mechanism.

## Proposed v0.2.1 changes

### 1. Make missing self-registration block normal lifecycle gates

Change `gate` so `finish` and `continue` also treat missing self-registration as a blocker. Keep age-based stale registration checks scoped to `commit`/`release` if desired, but missing registration should block every phase because an unenrolled agent should not be able to declare completion.

Effect: if SessionStart boot failed but Stop/AfterAgent hooks exist, the agent gets forced into another turn with a clear "run boot" reason.

Suggested text shape:

```text
no registration on disk (run `agentchute boot --as <id> --vendor <vendor>` first)
```

### 2. Teach read-only context paths to surface `needs_boot`

Port the `self-poll` missing-registration/missing-inbox detection into `pending` hook output, or share a helper used by both `pending` and `self-poll`.

For text/hook output, if `needs_boot` is true, emit model-visible context like:

```text
agentchute: registration/inbox missing for <id>; run `agentchute boot --as <id> --vendor <vendor>` before processing mail.
```

For `pending --json`, add `needs_boot: true` without changing the command into a mutating command. Keep `pending` read-only.

Effect: partially-installed hooks no longer report "all clear" when the agent never booted.

### 3. Add a self-registration guard to active agent commands

For commands that act as an agent identity, fail fast when the caller's registration is missing:

- `check --as <id>`
- `send --from <id>`
- `watch --as <id>`
- `status --as <id>` acting-agent mode

Do not auto-boot by default. Auto-boot needs a vendor and may guess wrong for non-canonical agents. A hard error is safer and makes the missing setup visible:

```text
agent <id> is not registered; run `agentchute boot --as <id> --vendor <vendor>` first
```

Optional ergonomic follow-up: add `AGENTCHUTE_VENDOR` and a small canonical-ID vendor map so wrappers can opt into `--auto-boot` later. But the first fix should be explicit failure, not silent mutation.

## Hook/install improvement

This is not a replacement for the guards above, but it is the operator-facing fix: add an installer path for hook templates.

Possible surface:

```sh
agentchute init --install-hooks
agentchute hooks install --wrapper codex --as codex --vendor openai
agentchute doctor --fix-hooks
```

Current `init` makes the docs present; it does not make the wrapper run `boot`. If we want self-registration to "just work", there needs to be a command that writes the wrapper hook files, not just instructions telling the model to run `boot`.

## Recommendation

Do the three guard/context changes first. They convert silent no-registration into a visible, actionable failure without changing the protocol's inbox wire format. Then add hook installation ergonomics so users can make the reliable path the default.
