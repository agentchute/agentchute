## Running without tmux

agentchute does not require tmux. The protocol is the inbox: a sender writes a message, and the recipient polls its own inbox on a cadence. Wake adapters such as tmux are best-effort latency helpers, not correctness requirements.

For no-tmux setups, run a recipient-side polling loop that wakes the wrapper, not the `agentchute` consumer:

```sh
agentchute doctor --generate-service script --as codex --interval 30 > agentchute-codex-loop.sh
chmod +x agentchute-codex-loop.sh
./agentchute-codex-loop.sh
```

The generated script uses a read-only preflight (`self-poll`) on each tick. If the inbox and pending-reply ledger are clear, it does nothing. If work exists, it launches the wrapper with an inbox-processing prompt. The model then runs `agentchute check --as <id>` inside the wrapper turn, reads the mail, archives it, and replies or defers.

Per-wrapper patterns:

| Wrapper | No-tmux polling pattern |
|---|---|
| **Claude Code** | Use `/loop 5m ...` or `.claude/loop.md`; hooks inject pending context and gate the turn. |
| **Codex App** | Use native Automations with an inbox-processing prompt. |
| **codex CLI** | Use `doctor --generate-service` or your scheduler of choice to preflight with `self-poll` and launch `codex exec` only on work. |
| **Gemini CLI** | Use `doctor --generate-service` or your scheduler of choice to preflight with `self-poll` and launch `gemini -p` only on work. Add `gate --before continue --gemini-hook AfterAgent` for in-session catchup. |
| **Plain terminal** | Use `agentchute watch --as <id> --notify` for human notifications, or `--exec` to launch the wrapper. |

The rule is simple: schedule the wrapper, not bare `agentchute check`. `check` consumes and archives mail. Safe scheduler and hook surfaces are read-only: `self-poll`, `pending`, `boot --context-only`, `doctor`, and `watch`.

### Manual codex CLI scheduler

```sh
while true; do
  agentchute self-poll --as codex --json >/tmp/agentchute-codex.json
  case "$?" in
    0) ;;
    2)
      codex exec 'Process agentchute mail for codex. Run `agentchute check --as codex` first. Reply with `agentchute send --from codex --reply-to <message_id>` or defer with `agentchute defer`. Do not stop until `agentchute pending --as codex` is clear.'
      ;;
    *)
      cat /tmp/agentchute-codex.json >&2
      ;;
  esac
  sleep 30
done
```

Use a single-flight lock in production so two wrapper instances do not race on the same inbox. The generated service artifacts include that guard.

### tmux is still useful

The v0.1 reference CLI still supports `tmux send-keys` as a wake adapter. It is convenient when agents share one tmux server, but it is optional. If the poke fails, delivery still succeeded; the recipient's next polling tick will pick up the message.
