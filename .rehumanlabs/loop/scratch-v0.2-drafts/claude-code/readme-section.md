# README "Running without tmux" — rewritten for v0.2

## Running without tmux (the canonical path)

agentchute's protocol is best-effort recipient polling. Senders write
files into recipient inboxes; recipients discover them on their own
cadence. tmux is one optional convenience adapter that lets a sender
poke a recipient's pane; it is no longer the primary integration path.

Three polling tiers cover every wrapper, from zero-infrastructure to
operator-managed services.

### Tier 1 — Native recurring task

If your wrapper has a built-in scheduler, use it.

**Claude Code:**
```
/loop 5m Run `agentchute check --as claude-code`; process any obligations.
```

**Codex App:** create a 5-minute Automation with the equivalent prompt.

That's the whole setup. Each tick fires the wrapper's normal
SessionStart hook, which runs `agentchute boot` for you.

### Tier 2 — Preflighted scheduler (codex-cli, gemini-cli, scripted)

For wrappers without a native loop, run an external scheduler. The v0.2
generator writes the unit file for you:

```
# macOS
agentchute doctor --generate-service launchd --as gemini-cli \
  --out ~/Library/LaunchAgents/com.agentchute.gemini-cli.plist
launchctl load ~/Library/LaunchAgents/com.agentchute.gemini-cli.plist

# Linux (systemd user)
agentchute doctor --generate-service systemd-service --as codex \
  --out ~/.config/systemd/user/agentchute-codex.service
agentchute doctor --generate-service systemd-timer --as codex \
  --out ~/.config/systemd/user/agentchute-codex.timer
systemctl --user enable --now agentchute-codex.timer

# Portable shell loop (cron, tmux pane, manual sh)
agentchute doctor --generate-service script --as gemini-cli \
  --out ~/bin/agentchute-gemini-cli.sh
```

Every 30 seconds the unit runs:

```
agentchute pending --as <id> --fail-if-any
```

This is a side-effect-free read (no archiving, no last_seen mutation).
On exit code 2 (work exists) the unit launches the wrapper under flock
single-flight. On exit code 0 (idle) nothing happens.

### Tier 3 — Finish-hook continuation

When the wrapper is already running and new mail arrives mid-session,
the Stop / AfterAgent hook catches it via `gate --before continue` and
the wrapper continues into another turn. This pairs with tiers 1 and 2;
it is not a wake on its own.

### tmux: still supported as a convenience adapter

If both agents share a tmux server, senders can opt into a low-latency
wake poke. Set `wake_method: tmux` in your registration; this remains a
fully supported path but is no longer required. See AGENTCHUTE.md §8
for the adapter contract.
