<div align="center">

# agentchute

**Agents share inboxes via markdown files, so they can hand off work without a human passing messages.**

[![MIT License](https://img.shields.io/badge/license-MIT-2a4a8a.svg)](LICENSE)
[![Go 1.21+](https://img.shields.io/badge/go-1.21+-2a4a8a.svg)](go.mod)
[![Release](https://img.shields.io/github/v/release/agentchute/agentchute?color=b94a26&label=release)](https://github.com/agentchute/agentchute/releases)
[![Spec](https://img.shields.io/badge/spec-AGENTCHUTE.md-545048.svg)](AGENTCHUTE.md)

[Website](https://agentchute.dev) ·
[Spec](AGENTCHUTE.md) ·
[Examples](examples/) ·
[Extensions](EXTENSIONS.md)

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```

</div>

---

You're running Claude Code, codex, and Gemini against the same repo. They have things to say to each other. Today you ferry every message between them by hand.

agentchute is a small Markdown protocol that gives each agent an inbox directory. One agent writes a file into another's inbox; that's the entire wire format. A reference CLI plus three lifecycle hooks turn it into a working coordination layer — no server, no broker, no SDK.

<p align="center">
  <a href="https://www.youtube.com/watch?v=jwYzKtcOYl0">
    <img src="https://img.youtube.com/vi/jwYzKtcOYl0/maxresdefault.jpg"
         alt="Three CLI agents coordinating in tmux"
         width="640">
  </a>
</p>

<p align="center"><em>Claude Code, codex, Gemini CLI coordinating in tmux during the v0.1.1 pre-release pass. Real session, 24× speedup, 62 seconds.</em></p>

## A handoff in 30 seconds

```text
┌──────────────────┐                          ┌──────────────────┐
│  ALICE (Claude)  │                          │  BOB (codex)     │
└──────────────────┘                          └──────────────────┘
        │ 1. write inbox/bob/<ts>_from-alice_msg-<nonce>.md
        ├─────────────────────────────────────────▶
        │ 2. tmux send-keys "check" (wake poke)
        ├ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ▶
        │                                         │ 3. read + archive
        │ 4. reply lands in inbox/alice/          │
        │◀─────────────────────────────────────────
```

Everything is local files. Atomic create-temp + rename for delivery, `tmux send-keys` for the wake, recipient archives after reading.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```

Pre-built binaries on the [releases page](https://github.com/agentchute/agentchute/releases) for macOS (arm64/amd64) and Linux (amd64/arm64). Windows: use WSL.

Or run protocol-only — no binary required. See [Hand-protocol mode](#hand-protocol-mode) below.

## Get a two-agent pool running

```sh
# Once per repo
agentchute init --yes

# In each agent's tmux pane (or shell), once per session
agentchute boot --as claude-code --vendor anthropic
agentchute boot --as codex       --vendor openai
```

`boot` registers the agent, peeks the inbox, and surfaces any pending reply obligations. Run it once at the start of every session; hooks (next section) automate this.

Send a message:

```sh
agentchute send --from claude-code --to codex \
  --task "review this diff" --ask --body "look at PR #42"
```

`--ask` marks the message as `reply_required: true` and adds a `## ASK` heading. The recipient's `agentchute check` archives the message, records a pending-reply obligation, and codex's `gate --before finish` will refuse to let it end the turn until it replies via `send --reply-to <msg-id>` or defers with `agentchute defer`.

## Lifecycle hooks (v0.1.1)

Each wrapper has a hooks file that calls into agentchute at three points per session. Copy the template, restart the wrapper, done.

| Wrapper | Template | SessionStart | UserPromptSubmit | Stop / BeforeAgent |
|---|---|---|---|---|
| **Claude Code** | `examples/hooks/claude-code/.claude/settings.json` | `boot --context-only` | `pending` | `gate --before finish` |
| **codex CLI** | `examples/hooks/codex/.codex/hooks.json` | `boot --codex-hook SessionStart` | `pending --codex-hook UserPromptSubmit` | `gate --codex-hook Stop` |
| **Gemini CLI** | `examples/hooks/gemini/.gemini/settings.json` | `boot --context-only` | (in `BeforeAgent`) `pending --json` | (in `BeforeAgent`) `gate --before finish --json` |

The Stop hook is the load-bearing one: it sees the agent's pending-reply ledger and forces a turn to continue (exit code 2) until the obligation is cleared. Without it, an agent can read a `--ask` message and silently never reply.

> ⚠ **Never use `agentchute check` in a hook.** `check` archives and quarantines; `boot` and `pending` are read-only peeks. Hook templates above only use the peeks.

## Commands at a glance

| Command | What it does |
|---|---|
| `agentchute init` | Scaffold loop directories + drop ENROLLMENT block into each wrapper file |
| `agentchute boot --as <id> --vendor <v>` | Session-start: register + peek inbox + pending-reply summary |
| `agentchute send --from <a> --to <b> [--ask] [--reply-to <id>]` | Write to recipient's inbox + wake poke + (optionally) clear ledger |
| `agentchute check --as <id>` | Read + archive inbox; record reply obligations; cooperative-wake peers |
| `agentchute pending --as <id>` | Side-effect-free peek (inbox + ledger). Hook-safe. |
| `agentchute gate --as <id> --before <phase>` | Block declaring done if inbox/ledger has outstanding work |
| `agentchute defer --message <id> --reason "..."` | Explicit defer of a reply obligation; auto-acks the sender |
| `agentchute status --as <id>` | Pool overview: per-agent inbox depth, `last_seen`, wake target |
| `agentchute watchdog --as <id>` | Optional liveness sidecar; pokes peers with stale inboxes |

All commands accept `--control-repo`, `--loop-dir`, and `--json` where it makes sense.

## Hand-protocol mode

Don't want the binary? The protocol is a directory layout and a filename format. Read [`AGENTCHUTE.md`](AGENTCHUTE.md) §5 — the whole loop fits in one file. Agents follow these steps with `mv` and `cat`. The v0.1.1 lifecycle (`boot`/`gate`/`defer`) reduces to inspecting and updating `<loop>/state/<self>/pending-replies.json`; the spec shows the exact transitions.

Hand-protocol agents and CLI agents share the same loop directory cleanly — mix and match in the same pool.

## Limitations

- **Single-filesystem (reference CLI only)**: the v0.1 inbox medium is files on a shared filesystem. Multi-machine works as long as everyone shares the volume. Network transports are protocol-compatible — see [`EXTENSIONS.md`](EXTENSIONS.md) — but don't ship in v0.1.
- **Cooperative trust**: messages are plain text, no signing. Don't use for untrusted peers.
- **Optimistic delivery**: no retries, no DLQ. If the wake fails, the message waits in the inbox.
- **No concurrency on shared files**: use git for that.
- **POSIX shells**: macOS and Linux. Windows users want WSL.

## How it compares

The honest one-liner: agentchute is the protocol-not-platform sibling of [mcp_agent_mail](https://github.com/Dicklesworthstone/mcp_agent_mail) (MCP broker) and Anthropic's [Claude Code Channels](https://github.com/anthropics/claude-code-action) (vendor-locked IM relay). No server, no SDK, runs over a substrate you already have. Closest functional sibling in the MCP world is mcp_agent_mail; closest tool-locked sibling is Claude Code subagents (in-process, single wrapper). agentchute is across-wrapper, across-vendor, broker-free.

## Spec / hacking / license

- **Protocol**: [`AGENTCHUTE.md`](AGENTCHUTE.md) is the spec. The binary is one reference implementation; alternates are welcome.
- **Hacking**: `git clone`, `gofmt -w . && go vet ./... && go test ./... && go build ./...`. No third-party deps. See [`CONTRIBUTING.md`](CONTRIBUTING.md).
- **License**: MIT.

---

*Built by [reHuman Labs](https://rehumanlabs.com). Let humans do human work, agents do agent work, and stop using humans as a message bus.*
