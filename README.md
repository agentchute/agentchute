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

To install (v0.2.1+):

```sh
# one command, all three wrappers
agentchute hooks install --wrapper all

# or one wrapper at a time
agentchute hooks install --wrapper claude-code

# if the binary isn't on PATH, set this in the env that launches the wrapper:
export AGENTCHUTE_BIN=/path/to/agentchute
```

`hooks install` is idempotent — same-content re-runs report `already current` and do nothing. By default `--scope repo` anchors at the control-repo root (the dir holding `AGENTCHUTE.md`), so install works from any subdirectory. Use `--scope user` to install under `$HOME` instead, `--dry-run` to preview, `--force` to overwrite a diverged hook file (a `.bak` backup is written first).

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
| `self-poll --as <id>` | Side-effect-free "should I wake?" helper for schedulers (v0.2) |
| `gate --before continue --gemini-hook AfterAgent` | In-session catchup decision JSON (v0.2) |
| `doctor --generate-service <kind>` | Emit launchd / systemd / shell unit files for the preflighted scheduler (v0.2) |
| `hooks install --wrapper <name>` | Write the canonical hook template into `.claude/` / `.codex/` / `.gemini/` (v0.2.1) |

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

- **Single shared filesystem** (reference CLI). Multi-machine works if all participants share the volume. Alternate transports (queues, S3, HTTP) are protocol-compatible — see [`EXTENSIONS.md`](EXTENSIONS.md) — but don't ship in the reference CLI.
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

## Releases

### v0.2.1 (2026-05-20)

The **enforced enrollment** release. Self-registration was normative in the spec (§5) but the reference CLI treated it as optional; agents could call `send` and `check` forever without ever publishing a registration record. v0.2.1 closes the gap end-to-end.

- **AGENTCHUTE.md §5.7** (new, normative): conforming implementations MUST refuse to perform active agent operations (consume, send, gate completion) without a registration record. Read-only diagnostics MAY surface a needs-registration signal instead of refusing, so wrappers can detect and remediate the gap.
- **Active commands refuse on missing self-registration**: `check`, `send --from`, `watch`, `status --as`, and `gate --before finish|continue` now exit with a clear pointer to `agentchute boot --as <id> --vendor <vendor>`. The previous silent-tolerance behavior is gone.
- **`internal/loop.ErrInboxMissing` sentinel**: `ListInboxMessages*` now distinguishes "inbox dir doesn't exist" from "inbox is empty". Callers act-as-agent map to `needs_boot`; peer-liveness paths (watchdog) skip without failing the pass.
- **`pending` surfaces `needs_boot`** in text / `--json` / `--claude-hook UserPromptSubmit` / `--codex-hook UserPromptSubmit` modes (read-only, always exit 0 for hook envelopes; `--fail-if-any` exits 2 because that mode IS a scheduler preflight).
- **`agentchute hooks install --wrapper <name>`** (new): writes the canonical hook template into `.claude/settings.json` / `.codex/hooks.json` / `.gemini/settings.json`. Atomic temp+rename, 0600/0700 modes, idempotent re-runs, `--scope repo|user`, `--dry-run`, `--force` with `.bak` backup. Makes the v6 enrollment block's "run this once" property real.
- **v6 enrollment block**: drops the "If hooks are configured, this runs automatically" hedge; leads with `agentchute hooks install` as the primary path.

**Breaking change**: any caller running `send` / `check` / `watch` / `status --as` / `gate finish|continue` without a prior `boot` now gets a loud error pointing at the fix. Pools where every agent has already booted at least once are unaffected.

### v0.2.0 (2026-05-20)

The **no-tmux release**. Recipient-side polling becomes the canonical discovery mechanism; tmux is demoted to one optional convenience adapter.

- **§8.2 wake responsibility** (AGENTCHUTE.md): normative text declaring that recipients MUST discover unread mail through their own inbox scans on their own cadence. Wake adapters are best-effort latency optimizations.
- **`agentchute self-poll --as <id>`**: side-effect-free "should I wake the wrapper?" helper for schedulers and launch prompts. Exits 2 on unread mail, pending replies, malformed inbox files, or first-run `needs_boot`. JSON output for schedulers; `--prompt-text` for model-facing launch fragments (with prompt-injection guard on peer-supplied metadata).
- **`agentchute gate --before continue`**: in-session continuation gate. Sibling of `--before finish` with wrapper-specific output framing (`--gemini-hook AfterAgent` emits `{"decision":"deny|allow","reason":"..."}`, always exit 0).
- **`agentchute doctor --generate-service <kind>`**: emits launchd / systemd-service / systemd-timer / portable shell-script unit files for the preflighted-scheduler pattern. Single-flight via POSIX `mkdir`-as-lock, strict input validation on `--as` / `--vendor` / `--repo` (shell-injection-safe), plain-text wrapper prompts.
- **Three-tier polling model** (AGENTCHUTE.md §8.1): native loop (Claude `/loop`, Codex App Automations) / preflighted scheduler / finish-hook continuation.
- **v5 enrollment block**: `agentchute init` writes the new three-tier polling guidance into `CLAUDE.md` / `CODEX.md` / `GEMINI.md` / `GROK.md` / `AGENTS.md`.

### v0.1.3 (2026-05-20)

- **`watch` dedupe by filename**: two distinct files sharing a `message_id` no longer suppress the second notification (§6.4.1 compliance fix).
- **`AGENTCHUTE_BIN` executable check**: `doctor` now requires the override to be a real regular file with the executable bit set; directories and non-executable files are rejected.

### v0.1.2 (2026-05-20)

- **`agentchute doctor`**: diagnostic aggregator with severity-tagged checks (scaffold, hook content, registration, ledger, wake target).
- **`agentchute watch --as <id> --notify`**: non-consuming watcher; OS notification / print / exec on new mail. Recipient-side watcher (§10.9).
- **`agentchute status` without `--as`**: prints the pool overview as a side-effect-free read.
- **Claude Code `UserPromptSubmit` hook JSON**: `pending --claude-hook UserPromptSubmit` emits the nested `hookSpecificOutput.additionalContext` contract.

### v0.1.1 (2026-05-19)

- **Lifecycle primitives**: `boot`, `pending`, `gate`, `defer` for mechanical protocol compliance.
- **Universal hook templates**: Claude Code, codex, Gemini CLI session-start and turn-gate hooks.
- **Pending-reply ledger**: durable local state at `<loop>/state/<agent>/pending-replies.json` tracking `reply_required` obligations.
- **Protocol additions**: `reply_required`, `priority`, `in_reply_to` frontmatter fields (AGENTCHUTE.md §6.4).
- **`AGENTCHUTE_BIN` env override** for binary discovery.

### v0.1.0 (2026-05-13)

Initial reference CLI release.
