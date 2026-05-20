<div align="center">

# agentchute

**Shared Markdown inboxes for AI agents, so they can hand off work without a human passing messages.**

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

You're running Claude Code, codex, and Gemini against the same repo. They have things to say to each other. Today you ferry every message between them by hand.

agentchute is a mailbox-on-filesystem protocol. One agent writes a Markdown file into another's inbox directory; that's the entire wire format. A small reference CLI plus three lifecycle hooks turn it into a working coordination layer — no server, no broker, no SDK.

<p align="center">
  <a href="https://www.youtube.com/watch?v=jwYzKtcOYl0">
    <img src="https://img.youtube.com/vi/jwYzKtcOYl0/maxresdefault.jpg"
         alt="Real agentchute session: Claude, codex, Gemini in tmux"
         width="640">
  </a>
</p>

<p align="center"><em>Claude Code, codex, and Gemini CLI coordinating in tmux during the v0.1.1 pre-release pass. Real session, 24× speedup, 62 seconds.</em></p>

## Who it's for

- You run two or more agent CLIs on the same repo.
- You want agents to ask each other for review, sign-off, or follow-up work.
- You want coordination over local files, not a server, broker, SDK, or framework.
- You're comfortable with cooperative-trust local tooling.

agentchute is not a queue, auth layer, audit log, task router, or multi-agent framework. It's the small mailbox layer underneath those things.

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

Delivery is an atomic, no-overwrite file write. The wake poke is best-effort (`tmux send-keys` is the v0.1 reference adapter); when it fails, the message simply waits in the inbox until the recipient's next poll.

The reference CLI stores the whole loop at `.<namespace>/loop/`:

```text
agents/       live registrations: agent id, vendor, host, wake target
inbox/<id>/   unread messages owned by each recipient
archive/      consumed messages
malformed/    quarantined protocol violations
state/<id>/   per-agent pending-reply ledger
```

## Quickstart

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

## Lifecycle hooks (v0.1.1)

Each wrapper has a hooks file that calls into agentchute at three points per session. Copy the template, restart the wrapper, done.

| Wrapper | Template |
|---|---|
| **Claude Code** | `examples/hooks/claude-code/.claude/settings.json` |
| **codex CLI** | `examples/hooks/codex/.codex/hooks.json` |
| **Gemini CLI** | `examples/hooks/gemini/.gemini/settings.json` |

What each hook does:

- **SessionStart** runs `boot` — registers the agent, peeks the inbox, surfaces pending-reply obligations as developer context.
- **UserPromptSubmit** (Claude/codex) / **BeforeAgent** (Gemini) runs `pending` — side-effect-free peek that injects current obligations into the model's context per turn.
- **Stop** (Claude/codex) / **BeforeAgent** (Gemini, again) runs `gate --before finish` — refuses to let the agent end the turn (exit code 2) while inbox or ledger has outstanding work. This is the load-bearing one.

If the binary isn't on the wrapper's PATH, set `AGENTCHUTE_BIN=/path/to/agentchute` in the environment that launches the wrapper. The templates honor `${AGENTCHUTE_BIN:-agentchute}`.

> ⚠ **Never use `agentchute check` in a hook.** `check` archives and quarantines; `boot` and `pending` are read-only peeks. Hook templates above only use the peeks.

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
| `register --as <id> --vendor <v>` | Write/refresh the agent's registration (boot supersedes for most uses) |
| `status --as <id>` | Pool overview: inbox depths, `last_seen`, wake targets |
| `watchdog --as <id>` | Optional liveness sidecar; pokes peers with stale inboxes |
| `prepare-pool --target <dir>` | Connect sibling folders to one control repo via pointer files |

Run `agentchute <command> --help` for flags. All commands accept `--control-repo`, `--loop-dir`, and `--json` where applicable.

## No binary required

The binary is convenience, not the protocol. A hand-protocol agent reads [`AGENTCHUTE.md`](AGENTCHUTE.md) §5, writes its registration to `agents/<id>.md`, drops Markdown files into `inbox/<recipient>/` using the filename grammar in §6.1, and maintains its own `state/<id>/pending-replies.json` for reply obligations. The whole loop fits in one file.

Hand-protocol agents and CLI agents share the same loop directory cleanly — mix and match in the same pool.

## Running without tmux

The CLI ships one wake adapter in v0.1: `tmux send-keys`. Without tmux, delivery still works; recipients just need a polling cadence that invokes the wrapper (not bare `check` — that drains mail without the model seeing it). Per-wrapper patterns we've verified:

- **Claude Code**: use `/loop check` — Claude's built-in recurring task.
- **codex CLI / Gemini CLI**: an operator-owned `while`-loop that invokes the wrapper with an inbox-processing prompt.

Schedule the wrapper, not the CLI.

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
