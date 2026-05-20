<div align="center">

# agentchute

**Shared inboxes for AI agents, so they can hand off work without a human passing messages.**

[![MIT License](https://img.shields.io/badge/license-MIT-2a4a8a.svg)](LICENSE)
[![Go 1.21+](https://img.shields.io/badge/go-1.21+-2a4a8a.svg)](go.mod)
[![Release](https://img.shields.io/github/v/release/agentchute/agentchute?color=b94a26&label=release)](https://github.com/agentchute/agentchute/releases)
[![Spec](https://img.shields.io/badge/spec-AGENTCHUTE.md-545048.svg)](AGENTCHUTE.md)

[Spec](AGENTCHUTE.md) · [Examples](examples/) · [Extensions](EXTENSIONS.md) · [Website](https://agentchute.dev)

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```

</div>

---

agentchute is a small coordination protocol. Each agent owns an inbox; senders write to it; recipients consume on their own cadence. The wire is medium-agnostic — the reference CLI maps the protocol onto Markdown files on a shared filesystem with optional tmux wake pokes, but the same primitives work over a queue, an HTTP endpoint, or any substrate that preserves no-overwrite per-recipient delivery (see [`EXTENSIONS.md`](EXTENSIONS.md)). No server, no broker, no SDK.

<p align="center">
  <a href="https://www.youtube.com/watch?v=jwYzKtcOYl0">
    <img src="https://img.youtube.com/vi/jwYzKtcOYl0/maxresdefault.jpg"
         alt="Real agentchute session: Claude, codex, Gemini in tmux"
         width="640">
  </a>
</p>

<p align="center"><em>Claude Code, codex, and Gemini CLI coordinating in tmux during the v0.1.1 pre-release pass. Real session, 24× speedup, 62 seconds.</em></p>

## Lifecycle hooks

Hooks are the primary integration path for the reference CLI: each wrapper has a hooks file that calls into agentchute at three points per session so you don't run `boot` / `pending` / `gate` by hand. (The protocol doesn't require hooks — they're how the reference CLI keeps the inbox contract visible during a wrapper's lifecycle events.)

| Wrapper | Template |
|---|---|
| **Claude Code** | `examples/hooks/claude-code/.claude/settings.json` |
| **codex CLI** | `examples/hooks/codex/.codex/hooks.json` |
| **Gemini CLI** | `examples/hooks/gemini/.gemini/settings.json` |

To install:

```sh
# from the agentchute repo, into your project's working directory
cp examples/hooks/claude-code/.claude/settings.json .claude/settings.json
# repeat for codex (.codex/hooks.json) and gemini (.gemini/settings.json) as needed
# if the binary isn't on PATH, set this in the env that launches the wrapper:
export AGENTCHUTE_BIN=/path/to/agentchute
```

Restart the wrapper. From then on:

- **SessionStart** runs `boot` — registers the agent, peeks the inbox, surfaces pending-reply obligations as developer context.
- **UserPromptSubmit** (Claude/codex) / **BeforeAgent** (Gemini) runs `pending` — a side-effect-free peek that injects current obligations into the model's context per turn. Claude Code and codex use wrapper-specific JSON modes (`--claude-hook UserPromptSubmit`, `--codex-hook UserPromptSubmit`) so the context lands in the right field; Gemini reads plain text via `--json`.
- **Stop** (Claude/codex) / **BeforeAgent** (Gemini, again) runs `gate --before finish` — refuses to let the agent end the turn while inbox or ledger has outstanding work. Claude and Gemini use exit-code blocking; codex uses its Stop-hook `{"decision":"block"}` JSON. This is the load-bearing one.

> ⚠ **Never use `agentchute check` in a hook.** `check` archives and quarantines; `boot` and `pending` are read-only peeks. Hook templates above only use the peeks.

Run `agentchute doctor --as <id>` when wiring hooks. It validates the loop scaffold, binary resolution, hook files, hook content, registration freshness, inbox/ledger state, and wake target health without consuming mail.

## Quickstart

These are the commands the hooks call. With hooks installed (above), the wrapper runs them at the right lifecycle points for you. Run them by hand to learn the surface, or for the rare manual session.

```sh
agentchute init --yes
```

In each agent's pane:

```sh
agentchute boot --as claude-code --vendor anthropic
agentchute boot --as codex       --vendor openai
```

Send a review request:

```sh
agentchute send --from claude-code --to codex \
  --task "review the diff" --ask --body "look at PR #42"
```

`--ask` writes `reply_required: true` into the message frontmatter and adds a `## ASK` heading. When codex runs `check`, it archives the message and records the obligation in its pending-reply ledger. Codex's `gate --before finish` will then refuse to let it end the turn until it replies:

```sh
agentchute check --as codex
agentchute send --from codex --to claude-code \
  --reply-to <message_id> --status signoff --body "looks good"
```

Or codex can defer the obligation explicitly with `agentchute defer --message <id> --reason "..."` — the gate clears, the original sender gets an automatic deferred-reply ack.

The reference CLI stores the whole loop at `.<namespace>/loop/`:

```text
agents/       live registrations: agent id, vendor, host, wake target
inbox/<id>/   unread messages owned by each recipient
archive/      consumed messages
malformed/    quarantined protocol violations
state/<id>/   per-agent pending-reply ledger
```

## A handoff in 30 seconds

```text
┌──────────────────┐                          ┌──────────────────┐
│  ALICE (Claude)  │                          │  BOB (codex)     │
└──────────────────┘                          └──────────────────┘
        │ 1. deliver message to Bob's inbox (no-overwrite)
        ├─────────────────────────────────────────▶
        │ 2. wake poke (best-effort via declared adapter)
        ├ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ▶
        │                                         │ 3. consume + archive
        │ 4. reply lands in Alice's inbox         │
        │◀─────────────────────────────────────────
```

Delivery is no-overwrite by contract: a sender never replaces an existing message. The wake poke is an optional optimization; if no adapter is reachable, the message waits in the inbox until the recipient's next poll. The reference CLI ships `tmux send-keys` as the default adapter; alternates (HTTP, SSH, notifications) fit the same shape.

## Commands at a glance

| Command | Purpose |
|---|---|
| `init` | Scaffold loop dirs + drop ENROLLMENT block into wrapper files |
| `boot --as <id> --vendor <v>` | Session-start: register + peek inbox + pending-reply summary |
| `send --from <a> --to <b> [--ask] [--reply-to <id>]` | Write to recipient's inbox + wake poke + (optionally) clear ledger |
| `check --as <id>` | Read + archive inbox; record reply obligations; cooperative-wake peers |
| `pending --as <id>` | Side-effect-free peek (inbox + ledger). Hook-safe. |
| `gate --as <id> --before <phase>` | Block declaring done if inbox/ledger has outstanding work |
| `defer --message <id> --reason "..."` | Explicit defer; auto-acks the sender |
| `register --as <id> --vendor <v>` | Write/refresh registration (boot supersedes for most uses) |
| `status [--as <id>]` | Pool overview: inbox depths, `last_seen`, wake targets |
| `doctor [--as <id>] [--json]` | Diagnostic aggregator: scaffold, binary, hook content, registration, inbox/ledger, wake target. Exit nonzero on BLOCKER. |
| `watch --as <id> [--notify] [--print] [--exec <cmd>]` | Recipient-side non-consuming watcher for new mail; useful outside tmux |
| `watchdog --as <id>` | Optional liveness sidecar; pokes peers with stale inboxes |
| `prepare-pool --target <dir>` | Connect sibling folders via pointer files |

Run `agentchute <command> --help` for flags. All commands accept `--control-repo`, `--loop-dir`, and `--json` where applicable.

## No binary required

The binary is convenience, not the protocol. A hand-protocol agent reads [`AGENTCHUTE.md`](AGENTCHUTE.md) §5, writes its registration to `agents/<id>.md`, drops Markdown files into `inbox/<recipient>/` using the filename grammar in §6.1, and maintains its own `state/<id>/pending-replies.json` for reply obligations. The whole protocol fits in one file.

Hand-protocol agents and CLI agents share the same loop directory cleanly — mix and match in the same pool.

## Running without tmux

The protocol's discovery mechanism is **recipient-side polling**. Senders write to your inbox; you are responsible for checking it on your own cadence. Wake pokes are optional optimizations.

Recommended polling tiers:

1. **Native Loops**: If your wrapper supports recurring tasks, use them.
   - **Claude Code**: run `/loop 5m` with a prompt to check inbox.
   - **Codex App**: use native Automations.
2. **Preflighted Scheduler**: For wrappers without a native loop (Gemini, terminal Codex). Use `agentchute doctor --generate-service` to install a persistent scheduler that runs a side-effect-free preflight (`agentchute self-poll --as <id>`) and only launches the wrapper when work exists.
3. **In-Session Catchup**: Active sessions catch new mail at lifecycle boundaries via hooks (e.g., `gate --before continue`).

For regular terminal sessions, use the non-consuming watcher:

```sh
agentchute watch --as <id> --notify
```

Schedule the wrapper, not bare `agentchute check`. `check` consumes mail; `pending`, `boot`, `doctor`, and `watch` are the safe inspection surfaces.

## Limitations

- **Single shared filesystem** (reference CLI v0.1). Multi-machine works if all participants share the volume. Alternate transports (queues, S3, HTTP) are protocol-compatible — see [`EXTENSIONS.md`](EXTENSIONS.md) — but don't ship in v0.1.
- **Cooperative trust**: messages are plain text, no signing or encryption.
- **Optimistic delivery**: no retries, no DLQ. Wake pokes are liveness hints, not read receipts.
- **No concurrency on shared files**: use git for that.
- **POSIX shells**: macOS and Linux. Windows users want WSL.

## Spec, hacking, license

The protocol is [`AGENTCHUTE.md`](AGENTCHUTE.md). The binary is one reference implementation; alternates are welcome.

```sh
git clone https://github.com/agentchute/agentchute
cd agentchute && gofmt -w . && go vet ./... && go test ./... && go build ./...
```

Go 1.21+, standard library only. Behavior changes should start with the spec. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

MIT — see [`LICENSE`](LICENSE).

---

*Built by [reHuman Labs](https://rehumanlabs.com). Let humans do human work, agents do agent work, and stop using humans as a message bus.*
