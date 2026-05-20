# claude-code's v0.1.2 README diff (round 1)

Surgical edits on top of the current main README. Three patches:

## Patch 1: Commands at a glance table — add doctor + watch

Replace the existing table with:

```markdown
| Command | Purpose |
|---|---|
| `init` | Scaffold loop dirs + drop ENROLLMENT block into wrapper files |
| `boot --as <id> --vendor <v>` | Session-start: register + peek inbox + pending-reply summary |
| `send --from <a> --to <b> [--ask] [--reply-to <id>]` | Write to recipient's inbox + wake poke + (optionally) clear ledger |
| `check --as <id>` | Read + archive inbox; record reply obligations; cooperative-wake peers |
| `pending --as <id>` | Side-effect-free peek (inbox + ledger). Hook-safe. |
| `gate --as <id> --before <phase>` | Block declaring done if inbox/ledger has outstanding work |
| `defer --message <id> --reason "..."` | Explicit defer; auto-acks the sender |
| `register --as <id> --vendor <v>` | Write/refresh the agent's registration (boot supersedes for most uses) |
| `status [--as <id>]` | Pool overview: inbox depths, `last_seen`, wake targets |
| `doctor [--as <id>]` | Diagnostic aggregator: scaffold, hook content, registration, ledger, wake target |
| `watch --as <id>` | Recipient-side watcher: `--notify` / `--print` / `--exec` on new mail (no-tmux fallback) |
| `watchdog --as <id>` | Optional liveness sidecar; pokes peers with stale inboxes |
| `prepare-pool --target <dir>` | Connect sibling folders to one control repo via pointer files |
```

(Two new rows: `doctor` and `watch`.)

## Patch 2: Promote watch into "Running without tmux"

Current "Running without tmux" section (lines 158-166) reads as the polling-fallback. v0.1.2 added a real first-class watcher; let's name it:

```markdown
## Running without tmux

The CLI ships one wake adapter in v0.1: `tmux send-keys`. Without tmux, recipients need an arrival-notification path that doesn't depend on a tmux pane being addressable.

**v0.1.2's recipient-side watcher** (`agentchute watch --as <id>`) is the supported path:

- `--notify`: OS notification (macOS `osascript`, Linux `notify-send`). Wakes the **operator**, not the agent — pair with a wrapper-launching keystroke if you want the model to react.
- `--print`: stdout line per new message. Pipe into anything.
- `--exec <cmd>`: shell command per new message, with `AGENTCHUTE_MSG_ID` / `AGENTCHUTE_FROM` / `AGENTCHUTE_TASK` as env vars. The only path that can wake an agent without tmux — operator-owned automation.

`watch` is non-consuming: it never archives, quarantines, or wakes peers. Pair with a wrapper that runs `check` when the model is actually ready to process mail.

For wrappers that ship a recurring-task primitive, the simpler path still works:

- **Claude Code**: `/loop check` — Claude's built-in recurring task.
- **codex CLI / Gemini CLI**: an operator-owned `while`-loop that invokes the wrapper with an inbox-processing prompt.
```

(Replaces the existing paragraph + bullet list. Adds the v0.1.2 watcher as the lead path; keeps the wrapper-recurring-task fallback below it.)

## Patch 3: Add a one-paragraph "Diagnostics" pointer near the limitations

Right above "## Limitations" (line 167), insert:

```markdown
## Diagnostics

`agentchute doctor` is the health check. Run it after `init`, after installing hooks, or after a wrapper update to confirm everything resolves:

```sh
agentchute doctor --as claude-code
```

Reports each check with a severity (BLOCKER / WARN / OK / SKIP). Non-zero exit when any BLOCKER is found — safe to wire into a CI step or operator script. Hook templates are validated for the silent-drain antipattern (`agentchute check` in a hook), binary resolution (PATH vs `AGENTCHUTE_BIN`), and scaffold integrity.

```

---

## Why this shape

- The Commands table update is the bare minimum — surfaces the new commands to first-time readers.
- Promoting watch into "Running without tmux" makes the section accurate again. The current version still implies "without tmux, you're on your own with polling"; v0.1.2 has a real answer.
- A short Diagnostics section answers "how do I tell if my setup is broken?" — the question doctor was built to answer. Placing it above Limitations means the reader sees the resolution path before reading what's NOT supported.

Length impact: +~30 lines vs. current 188.
