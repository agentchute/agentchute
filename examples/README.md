# Examples

Three concrete walkthroughs. Pick the one that matches your setup.

| File | Setup |
|---|---|
| [`quickstart.sh`](quickstart.sh) | Two agents in two tmux panes; minimum viable. |
| [`three-agents.sh`](three-agents.sh) | Three agents (Claude Code + codex + gemini-cli); shows `register --vendor` per origin and how directly-addressed messages work. |
| [`with-watchdog.sh`](with-watchdog.sh) | Two agents + a watchdog daemon. Shows what to do for 24/7 setups. |

Each script is annotated bash. Read them top-to-bottom; they're meant as documentation more than turnkey installers.

## What you'll need before any example

- `tmux` running.
- `agentchute` on your `$PATH` (`go install github.com/agentchute/agentchute@latest` or download a release binary).
- A repo (anywhere) where you want agentchute to live. The "control repo" is the one with `AGENTCHUTE.md` at its root.
- The `AGENTCHUTE.md` spec at the repo root. Grab it from the agentchute repo's release page or `curl` it from main.

## Vendor namespace

Examples use `.myorg/loop/` as the vendor namespace placeholder. Replace `myorg` with your own owned namespace (e.g., your domain shorthand). The vendor dotdir avoids collision if you ever run multiple agentchute implementations side-by-side in the same repo.

## Tmux pane targeting

Find your pane id with:

```sh
tmux list-panes -F '#{pane_index} #{pane_id} #{pane_current_command}'
```

The `pane_id` (looks like `%0`, `%1`) is what goes in `--wake-target` (when paired with `--wake-method tmux`). Or use `session:window.pane` form if you prefer named addressing.

## When in doubt

Read [`../AGENTCHUTE.md`](../AGENTCHUTE.md). It's short.
