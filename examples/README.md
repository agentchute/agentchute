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
| Grok CLI | hookless — `ac serve grok` / `agentchute serve` handle startup + wake and set `GROK_CLAUDE_HOOKS_ENABLED=0` so grok does not run the Claude Code template above |
| Antigravity CLI (`agy`, what `ac serve gemini` runs where Gemini CLI is absent) | no template yet — launches UNGUARDED (it does not read the Gemini CLI file); commit with `agentchute ack` |

**codex's shared daemon.** codex 0.161+ hosts every session on one per-user background
process (`codex app-server --managed-daemon`), forked by the first `codex` launch and
outliving it. The hooks above, and the agent's own shell commands, run as children of
that daemon and inherit *its* environment — the identity and serve token of whichever
`agentchute serve` launched codex first, not the serve that owns the current session.
That makes `send` fence ("serve lease fenced (token mismatch)") and `turn-end` exit 1,
across every repo. `ac serve codex` / `agentchute serve -- codex` therefore pass
`--no-daemon` automatically when the installed codex advertises it (the session then
runs its app-server in-process), except for `queue`, `agents` and `--remote`, which codex
refuses to combine with it; pass it yourself on an older `agentchute`. `agentchute
doctor` reports a running daemon whose token or control repo is not this pool's
(`codex_daemon_env`); clear one with `codex app-server daemon stop` and relaunch.

## Running a pool (pull-only)

Coordination is **pull-only**: senders write to an inbox and never poke a recipient. Each
agent runs under the `ac` dispatcher (`ac serve <wrapper>` → `agentchute serve`), a per-agent supervisor that polls
the agent's own inbox and injects `check inbox`. There is no tmux/herdr wake and no
watchdog — those were removed in 0.8.

```sh
# install + wire the repo once
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
agentchute setup --wake runner --wrappers all --yes

# start each agent in its own terminal, with a pinned id
AGENTCHUTE_AGENT_ID=claude-code ac serve claude
AGENTCHUTE_AGENT_ID=codex       ac serve codex
agentchute doctor --as codex            # sanity-check
```

## When in doubt

Read [`../AGENTCHUTE.md`](../AGENTCHUTE.md). It's short.
