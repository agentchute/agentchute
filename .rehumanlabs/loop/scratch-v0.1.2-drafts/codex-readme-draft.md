# codex README draft for v0.1.2

Context: this draft assumes the final Go fixes from codex's `d73d4dd` review land before the release is cut. Keep this as a surgical README update, not a rewrite.

## Proposed README changes

### 1. Rename the hooks heading

```diff
-## Lifecycle hooks (v0.1.1)
+## Lifecycle hooks (v0.1.2)
```

### 2. Tighten the UserPromptSubmit bullet

Replace the current bullet:

```md
- **UserPromptSubmit** (Claude/codex) / **BeforeAgent** (Gemini) runs `pending` — side-effect-free peek that injects current obligations into the model's context per turn.
```

with:

```md
- **UserPromptSubmit** (Claude/codex) / **BeforeAgent** (Gemini) runs `pending` — a side-effect-free peek that injects current obligations into the model's context per turn. Claude Code and codex use wrapper-specific JSON modes (`--claude-hook UserPromptSubmit`, `--codex-hook UserPromptSubmit`) so the context lands in the right field.
```

### 3. Add a one-line doctor check after the hook warning

Insert after the "Never use `agentchute check` in a hook" warning:

```md
Run `agentchute doctor --as <id>` when wiring hooks. It checks the loop scaffold, binary resolution, hook files, hook content, registration freshness, inbox/ledger state, and wake target health without consuming mail.
```

### 4. Update Commands at a glance

Add these rows near `status` / `watchdog`:

```md
| `doctor [--as <id>] [--json]` | Diagnostic aggregator: scaffold, binary, hook content, registration, inbox/ledger, wake target |
| `watch --as <id> [--notify] [--print] [--exec <cmd>]` | Recipient-side non-consuming watcher for new mail; useful outside tmux |
```

Keep the existing `status [--as <id>]` row as-is.

### 5. Replace "Running without tmux"

Current section is still mostly v0.1.1. Replace it with:

````md
## Running without tmux

The CLI ships one peer wake adapter in v0.1: `tmux send-keys`. Without tmux, delivery still works; messages wait in the recipient's inbox until the recipient polls.

For regular terminals, v0.1.2 adds a recipient-side watcher:

```sh
agentchute watch --as codex --notify
```

`watch` is non-consuming. It peeks for new mail and can fire an OS notification (`--notify`), print a line (`--print`), or run an operator-owned command (`--exec <cmd>`). It does **not** archive, quarantine, or make mail visible to the model by itself. After the notification, the human or wrapper still runs the normal recipient flow.

Per-wrapper polling patterns:

- **Claude Code**: use `/loop check`, or install the hooks above.
- **codex CLI / Gemini CLI**: use an operator-owned scheduler that invokes the wrapper with an inbox-processing prompt.
- **Plain terminal / no wrapper hooks**: run `agentchute watch --as <id> --notify` next to the session.

Schedule the wrapper, not bare `agentchute check`. `check` consumes mail; `pending`, `boot`, `doctor`, and `watch` are the safe inspection surfaces.
````

### 6. Optional small subsection before Limitations

If the README feels too dense after the replacement above, add this short section before `Limitations` instead of expanding the tmux section too much:

```md
## Diagnostics

`agentchute doctor [--as <id>]` is the first stop when a loop feels wrong. It reports BLOCKER / WARN / OK / SKIP checks for the loop scaffold, hook files, hook content, PATH / `AGENTCHUTE_BIN`, registration freshness, inbox state, reply ledger, and wake target. Unread mail and pending replies are warnings in doctor; lifecycle blocking still belongs to `gate`.
```
