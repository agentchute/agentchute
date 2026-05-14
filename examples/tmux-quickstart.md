# tmux quickstart for agentchute pools

Five minutes from "no tmux on this machine" to "three agents coordinating
through a shared inbox". No prior tmux experience required.

## 1. Install tmux

| OS                | Command                       |
|-------------------|-------------------------------|
| macOS             | `brew install tmux`           |
| Debian / Ubuntu   | `sudo apt install tmux`       |
| Fedora / RHEL     | `sudo dnf install tmux`       |
| Arch              | `sudo pacman -S tmux`         |
| Alpine            | `sudo apk add tmux`           |

## 2. Drop the starter config

[`examples/tmux.conf`](tmux.conf) is a minimal, opinion-free starter — five
settings, no plugins, no theme. Copy to `~/.tmux.conf`:

```sh
cp examples/tmux.conf ~/.tmux.conf
```

If you already have a `~/.tmux.conf`, skip this step — your existing setup
is fine. The starter just sets sane defaults (mouse on, friendlier split
keys, larger scrollback, and `prefix + r` to reload).

## 3. Start tmux and split into panes

```sh
tmux                     # new session
# inside tmux:
Ctrl-b |                 # split current pane vertically (right)
Ctrl-b |                 # split again — three panes total
Ctrl-b <arrow>           # move between panes
Ctrl-b z                 # zoom current pane (full-screen) / unzoom
```

`Ctrl-b` is the default tmux *prefix* key. Press it, release, then press
the next key. (Most tmux documentation you'll find online assumes this
default — don't remap it on day one.)

## 4. Start your agents

In each pane, start one agent wrapper:

```sh
# pane 1
claude

# pane 2
codex

# pane 3
gemini
```

(Replace with the actual command that launches Claude Code, codex CLI, or
Gemini CLI on your system.)

## 5. Have each agent register

On session start, have each agent read its wrapper instructions
(`CLAUDE.md`, `CODEX.md`, `GEMINI.md`, or `AGENTS.md`) and run the
enrollment command. `agentchute register --as <id> --vendor <vendor>`
auto-detects `$TMUX_PANE`, so the wake target is correct for that pane.

## 6. Test the loop

Have one agent send to another:

```sh
agentchute send --from claude-code --to codex \
    --task "smoke test" \
    --body "hello, can you reply?"
```

codex's pane should wake with the literal string `check` appearing in its
prompt within a fraction of a second. codex reads its inbox, processes the
message, replies; claude-code's pane wakes the same way. That's the whole
loop.

## How to exit (without losing your work)

New users often feel "trapped" inside tmux. Two ways out:

- **Detach** (recommended): `Ctrl-b d`. Your agents keep running in the
  background. Re-attach later with `tmux attach`. This is how you close
  the terminal window without killing the agents.
- **Exit each pane**: type `exit` in each pane individually. When the last
  pane closes, the tmux session ends.

To list running sessions:

```sh
tmux list-sessions
```

To kill a session entirely (closes all panes inside it):

```sh
tmux kill-session -t <session-name>
```

## Troubleshooting

- **`Ctrl-b |` doesn't split** — the config didn't load. Either you didn't
  copy `tmux.conf` to `~/.tmux.conf`, or you haven't restarted tmux since.
  Run `tmux kill-server` and start a fresh `tmux` session.
- **Pane stops receiving wakes after closing and reopening** — tmux pane
  IDs (`%0`, `%1`, …) are monotonic and never reused. A new pane has a
  new ID, so the agent in it must re-register so its `wake_target`
  reflects current reality. Re-run `agentchute register --as <id>
  --vendor <vendor>` from inside the new pane; `$TMUX_PANE` is auto-
  detected.
- **Agents in different tmux sessions** — they can still coordinate as
  long as they share the same filesystem (and the same loop directory).
  Use the canonical `session:window.pane` form for `wake_target` when the
  panes live in different sessions on the same tmux server.

That's the whole setup. The agentchute protocol doesn't know or care about
tmux specifically — it only knows about `wake_method` and `wake_target` in
each agent's registration. tmux is the v0.1 reference adapter because it's
terminal-native, widely packaged, and lets every agent run as its own
addressable pane with one install.
