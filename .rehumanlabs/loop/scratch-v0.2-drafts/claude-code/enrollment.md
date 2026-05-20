# v0.2 enrollment block additions

## CLAUDE.md — append under the existing "Claude-specific notes" section

```md
## Tier-1 self-poll (no tmux)

If your install can't share a tmux server with peer agents, set up a
recipient-side polling loop instead. Claude Code has a built-in scheduled-
task primitive: `/loop`. From the working repo, run:

    /loop 5m Run `agentchute check --as claude-code`; process any obligations.

That's the entire setup. Each tick fires UserPromptSubmit so the existing
boot/gate ritual runs unchanged. 5m is a reasonable default — adjust to
trade latency for token budget.

Pair `/loop` with the hook in `.claude/settings.json` (already wired in
the v0.1 enrollment block) so SessionStart still calls
`agentchute boot --as claude-code --vendor anthropic` on each tick.
```

## CODEX.md — append under the codex-specific notes

```md
## Tier-1 (Codex App) and Tier-2 (codex CLI) self-poll

**Codex App (desktop)** has native Automations. Create one that runs:

    Run `agentchute check --as codex`; process obligations; do not declare
    done until your inbox is empty.

Cadence: 5m is a reasonable default. The Automation re-prompts on each
tick; the existing UserPromptSubmit hook (v0.1) still calls
`agentchute boot`.

**Codex CLI (terminal)** has no native scheduler. Use tier 2 — an
operator-owned preflighted scheduler. Generate the launchd or systemd
unit:

    agentchute doctor --generate-service launchd --as codex \
      --out ~/Library/LaunchAgents/com.agentchute.codex.plist

The generated unit polls `agentchute pending --as codex --fail-if-any`
every 30s (side-effect-free); when work exists it launches
`codex exec` with an inbox-processing prompt under flock for
single-flight. See AGENTCHUTE.md §8.1 for the three-tier model.

Tier-3 continuation (mid-session catchup) is already wired via the
Stop hook from v0.1.
```

## GEMINI.md — append

```md
## Tier-2 self-poll + tier-3 AfterAgent continuation

Gemini CLI has no native loop. Use the v0.2 preflighted scheduler:

    agentchute doctor --generate-service systemd-service --as gemini-cli \
      --out ~/.config/systemd/user/agentchute-gemini-cli.service
    agentchute doctor --generate-service systemd-timer --as gemini-cli \
      --out ~/.config/systemd/user/agentchute-gemini-cli.timer
    systemctl --user enable --now agentchute-gemini-cli.timer

For mid-session catchup (tier 3), wire the AfterAgent decision hook in
`.gemini/settings.json`:

    {
      "hooks": {
        "AfterAgent": [
          {
            "command": "agentchute gate --before continue --as gemini-cli --gemini-hook AfterAgent"
          }
        ]
      }
    }

The hook emits `{"decision":"deny","reason":"..."}` when new mail
arrived during the turn — Gemini continues into another turn to
process it. On a clean inbox it emits `{"decision":"allow"}` and the
session ends normally.
```

## AGENTS.md — cross-wrapper summary section

```md
## Running without tmux

The protocol does not require tmux. Recipient-side polling is the
canonical discovery mechanism (AGENTCHUTE.md §8.2). Three tiers:

| Tier | Mechanism | When |
|------|-----------|------|
| 1. Native loop | `/loop` (Claude Code), Automations (Codex App) | Wrapper has a built-in scheduler |
| 2. Preflighted scheduler | launchd / systemd / cron + `pending --fail-if-any` preflight | No native loop (codex-cli, gemini-cli) |
| 3. Finish-hook continuation | `gate --before continue` in Stop/AfterAgent hooks | In-session mid-turn catchup |

Generate tier-2 unit files via `agentchute doctor --generate-service`.
Tier 3 is wired in the standard hook templates installed by
`agentchute init`. See the per-wrapper section above for specifics.

tmux remains supported as a convenience adapter (sender-side wake hint)
but is no longer the primary path.
```
