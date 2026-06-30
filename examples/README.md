# Examples

Per-wrapper hook templates, plus pointers for running a pool. The fastest path is the
[root README quickstart](../README.md) and the spec, [`AGENTCHUTE.md`](../AGENTCHUTE.md).

## Hook templates

[`hooks/`](hooks/) holds the per-wrapper lifecycle hook templates the installer wires for
you on `agentchute setup`. They run `boot` / `pending` / `gate` at the right lifecycle
points so you don't call them by hand:

| Wrapper | Template |
|---|---|
| Claude Code | [`hooks/claude-code/.claude/settings.json`](hooks/claude-code/.claude/settings.json) |
| codex CLI | [`hooks/codex/.codex/hooks.json`](hooks/codex/.codex/hooks.json) |
| Gemini CLI | [`hooks/gemini/.gemini/settings.json`](hooks/gemini/.gemini/settings.json) |
| Grok CLI | hookless — uses the `ac-grok` launcher / `agentchute run` for startup + wake |

## Running a pool (pull-only)

Coordination is **pull-only**: senders write to an inbox and never poke a recipient. Each
agent runs under its `ac-*` launcher (`agentchute run`), a per-agent supervisor that polls
the agent's own inbox and injects `check inbox`. There is no tmux/herdr wake and no
watchdog — those were removed in 0.8.

```sh
# install + wire the repo once
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
agentchute setup --wake runner --wrappers all --yes

# start each agent in its own terminal, with a pinned id
AGENTCHUTE_AGENT_ID=claude-code ac-claude
AGENTCHUTE_AGENT_ID=codex       ac-codex
agentchute doctor --as codex            # sanity-check
```

## When in doubt

Read [`../AGENTCHUTE.md`](../AGENTCHUTE.md). It's short.
