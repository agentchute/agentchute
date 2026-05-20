## Running without tmux

The protocol's discovery mechanism is **recipient-side polling**. Senders write to your inbox; you are responsible for checking it on your own cadence. Wake pokes are optional optimizations.

Recommended polling tiers:

1. **Native Loops**: If your wrapper supports recurring tasks, use them.
   - **Claude Code**: run `/loop 5m` with a prompt to check inbox.
   - **Codex App**: use native Automations.
2. **Preflighted Scheduler**: For wrappers without a native loop (Gemini, terminal Codex). Use `agentchute doctor --generate-service` to install a persistent scheduler that runs a side-effect-free preflight (`agentchute pending --fail-if-any`) and only launches the wrapper when work exists.
3. **In-Session Catchup**: Active sessions catch new mail at lifecycle boundaries via hooks (e.g., `gate --before continue`).

For regular terminal sessions, use the non-consuming watcher:

```sh
agentchute watch --as <id> --notify
```

Schedule the wrapper, not bare `agentchute check`. `check` consumes mail; `pending`, `boot`, `doctor`, and `watch` are the safe inspection surfaces.

## Limitations
