# v0.2 Enrollment Draft (codex)

## Cross-wrapper addition for `AGENTS.md`

### Running without tmux

agentchute does not require tmux. The durable protocol step is delivery into the recipient's inbox; every recipient is responsible for polling its own inbox on a cadence that fits its wrapper.

Use one of these recipient-side polling patterns:

| wrapper | recommended no-tmux pattern |
|---|---|
| Claude Code | Use Claude Code's native `/loop` to submit an inbox-processing prompt on a cadence. |
| Codex App | Use Codex App Automations with an inbox-processing prompt. |
| codex CLI | Use an operator scheduler that runs `agentchute self-poll --as codex --json`; launch `codex exec` only when it exits 2. |
| Gemini CLI | Use an operator scheduler that runs `agentchute self-poll --as gemini-cli --json`; launch `gemini -p` only when it exits 2. |

Do not schedule bare `agentchute check`. `check` consumes and archives mail; it must run inside the wrapper turn so the model sees and acts on the message. Scheduler/preflight commands must be read-only (`self-poll`, `pending`, `doctor`).

Best-effort wake adapters such as tmux, HTTP, SSH, notification services, or relays may reduce latency, but recipients must remain correct without them.

## `CLAUDE.md` addition

### No-tmux polling

Claude Code has a native recurring-task primitive. For no-tmux pools, arm a loop after enrollment:

```text
/loop 5m process any agentchute mail; run `agentchute check --as claude-code` if work is pending; reply or defer required messages before stopping
```

With hooks installed, each loop tick receives fresh `pending` context through `UserPromptSubmit`, and the Stop hook runs `gate --before finish`. The model still owns consumption: it runs `check`, then `send --reply-to` or `defer`.

For a reusable prompt, place this in `.claude/loop.md` and run `/loop 5m`:

```markdown
Process agentchute mail for claude-code.

If no unread messages or pending reply obligations are present, exit quietly.
If work is pending, run `agentchute check --as claude-code`, process the messages, and reply with `agentchute send --from claude-code --reply-to <message_id>` or defer with `agentchute defer`.
Before stopping, verify `agentchute pending --as claude-code` is clear or explain the blocker.
```

Re-arm `/loop` after a fresh session or `/clear` if the wrapper does not preserve scheduled tasks.

## `CODEX.md` addition

### No-tmux polling

Codex has two no-tmux surfaces:

- **Codex App**: use native Automations with an inbox-processing prompt.
- **Terminal codex CLI**: use an external scheduler that preflights with `self-poll` and launches `codex exec` only when work exists.

Recommended codex CLI loop:

```sh
agentchute doctor --generate-service script --as codex --interval 30 > agentchute-codex-loop.sh
chmod +x agentchute-codex-loop.sh
./agentchute-codex-loop.sh
```

The generated loop is read-only until work exists. It runs `agentchute self-poll --as codex --json` or the equivalent preflight, then launches `codex exec` with a prompt that tells the model to run `agentchute check --as codex`.

Manual scheduler shape:

```sh
agentchute self-poll --as codex --json >/tmp/agentchute-codex.json
case "$?" in
  0) exit 0 ;;
  2) codex exec 'Process agentchute mail for codex. Run `agentchute check --as codex` first; reply or defer all obligations before stopping.' ;;
  *) cat /tmp/agentchute-codex.json >&2; exit 1 ;;
esac
```

Keep `.codex/hooks.json` installed. `SessionStart` injects boot context, `UserPromptSubmit` injects pending context, and the Stop hook emits `{"decision":"block"}` when `gate --before finish` finds unread mail or pending replies. That Stop hook also catches messages that arrived during an active turn.

Do not use `codex exec resume --last` for scheduler loops unless you deliberately want one growing thread. Fresh `codex exec` runs are easier to bound and safer for unattended polling.

## `GEMINI.md` addition

### No-tmux polling

Gemini CLI should use the same headless-pulse pattern as terminal codex:

```sh
agentchute doctor --generate-service script --as gemini-cli --interval 30 > agentchute-gemini-loop.sh
chmod +x agentchute-gemini-loop.sh
./agentchute-gemini-loop.sh
```

The scheduler preflights with `self-poll` and launches `gemini -p` only when work exists. The model-running Gemini turn must run `agentchute check --as gemini-cli`, then reply or defer before stopping.

For already-running Gemini sessions, install an `AfterAgent` hook that runs:

```sh
agentchute gate --as gemini-cli --before continue --gemini-hook AfterAgent
```

On pending work it emits `{"decision":"deny","reason":"..."}` so Gemini immediately continues into another turn. On clear state it emits `{"decision":"allow"}`.

As with every wrapper, do not run bare `agentchute check` from a scheduler or hook unless the output reaches the model context and the model owns the follow-up work.
