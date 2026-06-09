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

## Launcher shims and lifecycle hooks

The reference CLI has two integration layers:

- **Launcher shims + `agentchute run`** own startup, registration, `last_seen`, no-tmux inbox polling, and best-effort prompt injection. `agentchute setup --wake runner` installs only the selected wrapper shims; inside a control repo, normal commands like `codex` or `gemini` route through the runner and pass through unchanged elsewhere.
- **Lifecycle hooks** own model context and gates. Each wrapper has a hooks file that calls into agentchute at session start, prompt submit, and finish so you don't run `boot` / `pending` / `gate` by hand.

The protocol doesn't require shims or hooks; they are the reference CLI's wrapper integration.

| Wrapper | Template |
|---|---|
| **Claude Code** | `examples/hooks/claude-code/.claude/settings.json` |
| **codex CLI** | `examples/hooks/codex/.codex/hooks.json` |
| **Gemini CLI** | `examples/hooks/gemini/.gemini/settings.json` |

The installer runs setup automatically when it has a terminal. To re-run setup later, or to reconfigure an existing repo:

```sh
agentchute setup
```

For automation, choose the wake path explicitly:

```sh
agentchute setup --wake runner --wrappers all --yes
```

Use `--wake tmux` if tmux panes are your primary wake path, or `--wake both` if you want both tmux hooks and runner shims. Hookless wrappers such as Grok still get a launcher shim in tmux mode because no lifecycle hook can run startup enrollment for them. Setup is idempotent: same-content re-runs report `already current`, changed setup choices reconcile old setup-managed hooks, shims, PATH blocks, and ENROLLMENT blocks in `AGENTS.md` / wrapper `.md` files, and live `agents/*.md` registrations are cleared so wrappers re-enroll with fresh contextual IDs. After upgrading agentchute, re-run `agentchute setup --yes` in each control repo and restart the wrappers.

Restart the wrapper. From then on:

- The launcher shim starts `agentchute run` before the wrapper inside initialized pools. The runner registers the agent with `wake_method: agentchute-run`, refreshes `last_seen` every poll, watches the inbox, and injects `[agentchute:run] check inbox` when new mail arrives.
- **SessionStart** runs `self-check`, `poller ensure`, then `boot` for hook-capable wrappers — reconciles the live wake target, verifies no-tmux liveness, registers the agent, peeks the inbox, surfaces pending-reply obligations as developer context.
- **UserPromptSubmit** (Claude/codex) / **BeforeAgent** (Gemini) first runs `self-check`, then `poller ensure` — refreshes registration/`last_seen`, reconciles tmux wake state, and keeps no-tmux liveness covered by a runner socket or poller heartbeat.
- The same hook then runs `pending` — a side-effect-free peek that injects current obligations into the model's context per turn. Claude Code and codex use wrapper-specific JSON modes (`--claude-hook UserPromptSubmit`, `--codex-hook UserPromptSubmit`) so the context lands in the right field; Gemini reads plain text via `--json`.
- **Stop** (Claude/codex) / **BeforeAgent** (Gemini, again) runs `gate --before finish` — refuses to let the agent end the turn while inbox/ledger has outstanding work or recipient liveness is not proven. Claude and Gemini use exit-code blocking; codex uses its Stop-hook `{"decision":"block"}` JSON. This is the load-bearing one.

> ⚠ **Never use `agentchute check` in a hook.** `check` archives and quarantines. Hook templates use `boot` / `self-check` only for registration heartbeats, `poller ensure` for non-tmux liveness, and `pending` for read-only inbox peeks.

Run `agentchute doctor --as <id>` after restarting the wrapper. It validates the loop scaffold, binary resolution, hook files, hook content, registration freshness, inbox/ledger state, wake target health, and recipient liveness without consuming mail.

## Quickstart

After install, restart the wrappers once. To inspect the setup:

```sh
agentchute doctor --as <agent-id>
```

With hooks installed (the default after `agentchute setup`), the wrapper runs `boot` / `pending` / `gate` at the right lifecycle points for you — see [Manual session](#manual-session-without-hooks) at the bottom for the by-hand equivalents.

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
state/<id>/   pending-reply ledger, poller heartbeat, runner state/socket
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

Delivery is no-overwrite by contract: a sender never replaces an existing message. The wake poke is an optional optimization; if no adapter is reachable, the message waits in the inbox until the recipient's next poll. The reference CLI ships `tmux send-keys` and the local `agentchute-run` socket adapter; alternates (HTTP, SSH, notifications) fit the same shape.

## Commands at a glance

| Command | Purpose |
|---|---|
| `init` | Scaffold loop dirs + drop ENROLLMENT block into wrapper files |
| `boot --vendor <v> [--as <id>]` | Session-start: register + peek inbox + pending-reply summary |
| `run --vendor <v> [--as <id>] -- <wrapper>` | Launch a wrapper under the PTY runner with registration, polling, and wake socket |
| `setup [--wake tmux|runner|both]` | One-command control-repo setup: init + clear stale registrations + hooks + selected runner shims |
| `shims install [--force]` | Install launcher shims so normal wrapper commands route through `agentchute run` inside pools |
| `send --to <b> [--from <a>] [--ask] [--reply-to <id>]` | Write to recipient's inbox + wake poke + (optionally) clear ledger |
| `check [--vendor <v>] [--as <id>]` | Read + archive inbox; record reply obligations; cooperative-wake peers |
| `pending [--vendor <v>] [--as <id>]` | Side-effect-free peek (inbox + ledger). Hook-safe. |
| `self-check --vendor <v> [--as <id>]` | Hook-safe heartbeat: refresh registration/`last_seen`, reconcile wake target, prune stale same-host tmux peers |
| `poller ensure --vendor <v> [--as <id>]` | Start/verify recipient polling when no tmux wake target is available |
| `poller status --as <id>` | Verify fresh `state/<id>/poller.json` heartbeat |
| `gate --before <phase> [--vendor <v>] [--as <id>]` | Block declaring done if inbox/ledger has outstanding work or liveness is unproven |
| `defer --message <id> --reason "..." [--vendor <v>] [--as <id>]` | Explicit defer; auto-acks the sender |
| `register --vendor <v> [--as <id>]` | Write/refresh registration and prune stale same-host tmux peers (boot supersedes for most uses) |
| `status [--as <id>]` | Pool overview: inbox depths, `last_seen`, wake targets |
| `doctor [--as <id>] [--json]` | Diagnostic aggregator: scaffold, binary, hook content, registration, inbox/ledger, wake target. Exit nonzero on BLOCKER. |
| `watch [--vendor <v>] [--as <id>] [--notify] [--print] [--exec <cmd>]` | Recipient-side non-consuming watcher for new mail; useful outside tmux |
| `watchdog --as <id>` | Optional liveness sidecar; pokes peers with stale inboxes |
| `prepare-pool --target <dir>` | Connect sibling folders via pointer files |
| `self-poll [--vendor <v>] [--as <id>] [--heartbeat]` | "Should I wake?" helper for schedulers; `--heartbeat` proves polling is alive |
| `gate --before continue --gemini-hook AfterAgent` | In-session catchup decision JSON (v0.2) |
| `doctor --generate-service <kind>` | Emit launchd / systemd / shell unit files for the preflighted scheduler (v0.2) |
| `hooks install --wrapper <name>` | Write the canonical hook template into `.claude/` / `.codex/` / `.gemini/` (v0.2.1) |

Run `agentchute <command> --help` for flags. All commands accept `--control-repo`, `--loop-dir`, and `--json` where applicable.

Commands that act as the current agent accept explicit `--as <id>`, read `AGENTCHUTE_AGENT_ID`, reuse a live registration for the current tmux pane, or derive `<wrapper>-<folder>` when a vendor/wrapper is known. Set `AGENTCHUTE_AGENT_ID` only for a custom stable lane name.

## No binary required

The binary is convenience, not the protocol. A hand-protocol agent reads [`AGENTCHUTE.md`](AGENTCHUTE.md) §5, writes its registration to `agents/<id>.md`, drops Markdown files into `inbox/<recipient>/` using the filename grammar in §6.1, and maintains its own recipient-owned state such as `state/<id>/pending-replies.json` for reply obligations. The whole protocol fits in one file.

Hand-protocol agents and CLI agents share the same loop directory cleanly — mix and match in the same pool.

## Project Boundaries

By default, agents only communicate within their own project pool. A pool is defined by a discovered control repo and its `.agentchute/loop` directory.

Unrelated projects on the same machine or tmux server are naturally isolated:
1. **Discovery**: `agentchute` stays inside the current repo unless explicitly pointed elsewhere via `--control-repo`, `AGENTCHUTE_CONTROL_REPO`, or a `.agentchute-control-repo` pointer.
2. **Contextual identity**: agents default to `<wrapper>-<folder-slug>`, so `codex` in project A and `codex` in project B get distinct inboxes such as `codex-proj-a` and `codex-proj-b`.

Cross-project communication is possible but requires explicit setup (see Worktree Teams below).

## Worktree Teams

When each agent team works in its own Git worktree but should coordinate through one larger project pool, keep one central control repo with the loop and point each worktree back to it:

```sh
# Run once in the central control repo.
agentchute setup --wake runner --wrappers all --yes

# Run from the central control repo, once per participant worktree.
agentchute prepare-pool --target ../project-codex --yes
agentchute prepare-pool --target ../project-claude --yes
```

`prepare-pool` writes a `.agentchute-control-repo` pointer plus ENROLLMENT files into each worktree. Normal commands run from that worktree discover the central pool through the pointer, so all prepared worktrees share one inbox medium. **Identity is contextual**: agents in worktrees derive IDs from the worktree folder name, such as `codex-agentchute-feat1`, unless `AGENTCHUTE_AGENT_ID` or `--as` supplies a custom lane name.

## Running without tmux

At the protocol boundary, senders write to your inbox and you are responsible for reading it. The reference CLI's no-tmux discovery mechanism is **recipient-side polling**. Wake pokes are optional optimizations. For non-tmux agents, `doctor` and `gate` require either a reachable wake target (`tmux` or `agentchute-run`) or a fresh `state/<agent>/poller.json` heartbeat.

Recommended polling tiers:

1. **Runner / launcher shims**: `agentchute run --vendor <v> -- <wrapper>` launches the wrapper under a PTY, registers `wake_method: agentchute-run`, keeps `last_seen` fresh, polls the inbox, and injects `[agentchute:run] check inbox` when work arrives. `agentchute setup --wake runner` makes this the default for normal wrapper commands inside pools.
2. **Hook-managed poller fallback**: The canonical hooks run `agentchute poller ensure --vendor <v>`. In tmux or under a reachable runner it no-ops; otherwise it starts/verifies `agentchute poller run`, which keeps `state/<id>/poller.json` fresh and launches the wrapper when `self-poll` finds work.
3. **Native Loops**: If your wrapper supports recurring tasks, use them only if they update the poller heartbeat. Claude Code `/loop` or Codex App Automations should call `agentchute self-poll --vendor <v> --heartbeat`.
4. **Preflighted Scheduler**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that run `agentchute self-poll --heartbeat` and only launch the wrapper when work exists.
5. **In-Session Catchup**: Active sessions catch new mail at lifecycle boundaries via hooks (e.g., `gate --before continue`).

For regular terminal sessions, use the non-consuming watcher:

```sh
agentchute watch --vendor <v> --notify
```

Schedule the wrapper, not bare `agentchute check`. `check` consumes mail; `self-poll --heartbeat`, `pending`, `boot`, `doctor`, and `watch` are the safe inspection surfaces.

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

Go 1.21+. The core stays stdlib; the PTY runner uses `github.com/creack/pty`. Behavior changes should start with the spec. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

MIT — see [`LICENSE`](LICENSE).

## Releases

See [`CHANGELOG.md`](CHANGELOG.md) for the full release history. Current release: **v0.3.8** — fixes Grok startup enrollment in tmux-first setup by keeping the launcher shim for hookless wrappers.

## Manual session (without hooks)

If you're driving an agent by hand — no `setup`, no hooks — these are the per-turn equivalents the hooks normally call for you:

```sh
agentchute boot --vendor anthropic
agentchute poller ensure --vendor anthropic
```

If you're bypassing launcher shims but still want the no-tmux runner, launch the wrapper through `agentchute run`:

```sh
agentchute run --vendor openai -- codex
agentchute run --vendor google -- gemini
```

---

*Built by [reHuman Labs](https://rehumanlabs.com). Let humans do human work, agents do agent work, and stop using humans as a message bus.*
