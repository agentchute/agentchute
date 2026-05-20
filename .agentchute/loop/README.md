# `.agentchute/loop/` — repo-local agent coordination

Live coordination state for agents working on the agentchute codebase. Follows the agentchute convention itself (the protocol spec at [`AGENTCHUTE.md`](../../AGENTCHUTE.md) is at this repo's root).

## Layout

- `README.md` — this file.
- `agents/` — agent registrations (tracked examples + format reference; live registrations gitignored).
- `inbox/<recipient>/` — per-recipient inbox queue (gitignored).
- `archive/` — consumed messages (gitignored).

## Currently registered agents

Live state is gitignored; check `agents/*.md` (not `.example.md`) locally for who's currently in the loop.

Example registrations are tracked:

- `agents/claude-code.example.md` — Claude Code working on the agentchute codebase.
- `agents/codex.example.md` — codex CLI working on the agentchute codebase.
- `agents/gemini-cli.example.md` — gemini-cli working on the agentchute codebase.

## Working in this repo as a new agent

1. Read [`AGENTS.md`](../../AGENTS.md) for the working rules + how to enroll.
2. Read [`AGENTCHUTE.md`](../../AGENTCHUTE.md) for the protocol itself.
3. Build the binary locally: `go build -o agentchute` (or use the pre-built release once published).
4. Register yourself:
   ```sh
   ./agentchute register --as <your-agent-id> --vendor <vendor-slug> --wake-method tmux --wake-target <your-pane-id>
   ```
   Or by hand: copy `agents/<existing-agent>.example.md` to `agents/<your-id>.md` and edit.
5. Inbox is created automatically by `register`.
6. Confirm gitignore: `git check-ignore agents/<your-id>.md` should print the path.

That's it. Everything else is in the spec.
