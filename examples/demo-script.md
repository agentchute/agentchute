# agentchute demo script

A walkthrough you can record from with [asciinema](https://asciinema.org/),
[VHS](https://github.com/charmbracelet/vhs), or a screen-capture tool. Two
agents in two tmux panes; ~90 seconds of footage if you don't pause. Uses the
current `wake_method` / `wake_target` registration flow.

## Setup before recording

1. **Fresh repo / temp dir** with `AGENTCHUTE.md` at root.
   ```sh
   mkdir /tmp/agentchute-demo && cd /tmp/agentchute-demo
   curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/AGENTCHUTE.md -O
   ```

2. **Two stub agent wrappers** so the demo doesn't require real Claude Code /
   codex API calls. Save these in the demo dir:

   ```sh
   cat > claude-stub.sh <<'EOF'
   #!/bin/sh
   echo "[claude] online. waiting for messages."
   while read -r line; do
     case "$line" in
       check) agentchute check --as claude-code ;;
       quit) exit 0 ;;
       *) echo "[claude] $line" ;;
     esac
   done
   EOF
   cat > codex-stub.sh <<'EOF'
   #!/bin/sh
   echo "[codex] online. waiting for messages."
   while read -r line; do
     case "$line" in
       check) agentchute check --as codex ;;
       quit) exit 0 ;;
       *) echo "[codex] $line" ;;
     esac
   done
   EOF
   chmod +x claude-stub.sh codex-stub.sh
   ```

3. **agentchute init** to scaffold the loop:
   ```sh
   agentchute init --yes
   ```

4. **Open two tmux panes side by side**:
   ```sh
   tmux new-session -s demo \; split-window -h
   ```

## The script (numbered for VHS / asciinema markers)

Each numbered block is one "scene". Pause briefly between scenes if you're
narrating.

### Scene 1 — start both agents (~10s)

```
# pane 0 (left):
./claude-stub.sh

# pane 1 (right):
./codex-stub.sh
```

Both panes print `[claude] online` / `[codex] online`.

### Scene 2 — register both agents (~10s)

The wrappers' enrollment block normally runs this on session start; we run
it manually for the demo so it's visible.

This demo uses explicit `--as` names so the cast stays stable on camera.
In normal wrapper setup, `--vendor` is enough for contextual IDs such as
`codex-<folder>`.

```
# pane 0:
agentchute register --as claude-code --vendor anthropic

# pane 1:
agentchute register --as codex --vendor openai
```

Each command prints the resolved `host`, `wake_method: tmux`, and
`wake_target: %N` (the auto-detected pane id).

### Scene 3 — list the registry (~5s)

In either pane:

```
agentchute status --as claude-code
```

Output shows both agents active, both inboxes at 0, fresh `last_seen`.

### Scene 4 — claude sends to codex (~15s)

In pane 0:

```
agentchute send --from claude-code --to codex \
    --task "review the demo" \
    --body "Take a look at the README opener — does the lede land?"
```

**The moment.** Pane 1 (codex) wakes within a fraction of a second — the
literal string `check` appears in its prompt, codex's `agentchute check`
runs, the message is read aloud (or printed), then archived.

### Scene 5 — codex replies (~15s)

In pane 1:

```
agentchute send --from codex --to claude-code \
    --task "review findings" \
    --status findings \
    --body "Lede reads cleanly. Two nits in the bullets — comment inline."
```

Pane 0 (claude) wakes the same way. Symmetric flow.

### Scene 6 — show the on-disk state (~10s)

In either pane:

```
tree .agentchute/loop/
```

Viewer sees:

```
.agentchute/loop/
├── agents/
│   ├── claude-code.md
│   └── codex.md
├── archive/
│   ├── 2026-...Z_to-claude-code_2026-...Z_from-codex_msg-XXXX.md
│   └── 2026-...Z_to-codex_2026-...Z_from-claude-code_msg-YYYY.md
├── inbox/
│   ├── claude-code/
│   └── codex/
└── malformed/
```

Inboxes are empty (both consumed); archive has both round-trip messages.
That's the whole on-disk surface — markdown files in directories.

### Scene 7 — closing slide (~5s)

```
echo "agentchute — protocol at AGENTCHUTE.md. spec at https://agentchute.dev/spec.html"
```

(or whatever closing line lines up with the recording flow)

## Recording notes

- **Total length**: target 60-90 seconds. Anything over 2 min loses HN audience.
- **Font size**: minimum 16pt for asciinema, 18pt for VHS GIF embed. Anything
  smaller is unreadable in a tweet/HN post.
- **Pane split**: 50/50 vertical; that's what shows the wake-poke "magic" best —
  the right pane visibly responds to the left pane's send.
- **VHS preferred** for the embedded GIF (autoplays inline on HN/Twitter). Use
  asciinema for the README link (text-selectable, replays at controlled speed).
- **Captions vs voiceover**: captions only. Voiceover doesn't translate to
  embedded GIF and adds production cost.

## What the demo proves

In ~90 seconds: two agents in two panes, one send, one wake, one reply, the
on-disk state. No server, no SDK, no compiled code other than the
single-binary `agentchute`. The simplicity is the point — let the viewer see
how little is happening.
