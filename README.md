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

agentchute is a small coordination protocol. Each agent owns an inbox; senders write to it; recipients consume on their own cadence. Coordination is **pull-only**: a sender's only job is durable delivery, and it never wakes the recipient. A wrapper with no native polling loop is launched under the runner (`agentchute run`), a per-agent PTY supervisor that polls the agent's own inbox and injects a `check inbox` cue when mail arrives. The wire is medium-agnostic — the reference CLI maps the protocol onto Markdown files on a shared filesystem, but the same primitives work over a queue, an HTTP endpoint, or any substrate that preserves no-overwrite per-recipient delivery (see [`EXTENSIONS.md`](EXTENSIONS.md)). No server, no broker, no SDK.

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

- **Launcher shims + `agentchute run`** own startup, registration, `last_seen`, no-tmux inbox polling, and best-effort prompt injection. `agentchute setup --wake runner` installs namespaced launchers such as `ac-codex` and `ac-gemini`; inside a control repo, those commands route through the runner and pass through unchanged elsewhere.
- **Lifecycle hooks** own model context and gates. Each wrapper has a hooks file that calls into agentchute at session start, prompt submit, and finish so you don't run `boot` / `pending` / `gate` by hand.

The protocol doesn't require shims or hooks; they are the reference CLI's wrapper integration.

| Wrapper | Template |
|---|---|
| **Claude Code** | `examples/hooks/claude-code/.claude/settings.json` |
| **codex CLI** | `examples/hooks/codex/.codex/hooks.json` |
| **Gemini CLI** | `examples/hooks/gemini/.gemini/settings.json` |
| **Grok CLI** | hookless — no hook template; uses the `ac-grok` launcher shim / `agentchute run` for startup enrollment and wake |

The installer runs setup automatically when it has a terminal. To re-run setup later, or to reconfigure an existing repo:

```sh
agentchute setup
```

For automation, choose the wake path explicitly. This is the canonical post-install step:

```sh
agentchute setup --wake runner --wrappers all --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH; the launchers use `ac-*` names, so they do not need to precede real wrapper binaries.

`runner` is the only supported wake path: the pull-only redesign removed the tmux/herdr wake adapters and the runner receive-socket. Agents wake by being launched through the `ac-*` runner launcher, which polls the agent's own inbox and injects `check inbox`; peers deliver by writing the inbox and never poke. `--wake all`/`--wake both` are accepted as deprecated aliases that install runner only; `tmux`/`herdr` are rejected with a pointer to `--wake runner`.
Hookless wrappers such as Grok still get a launcher shim because no lifecycle hook can run startup enrollment for them. Setup is idempotent: same-content re-runs report `already current`, changed setup choices reconcile old setup-managed hooks, shims, PATH blocks, and ENROLLMENT blocks in `AGENTS.md` / wrapper `.md` files, and live `agents/*.md` registrations are cleared so wrappers re-enroll with fresh contextual IDs. To upgrade an existing install and re-sync this folder in one step, run `agentchute update` (see [Updating](#updating)).

Restart the wrapper. From then on:

- The `ac-*` launcher starts `agentchute run` before the wrapper inside initialized pools. The runner acquires a serve lease (id-uniqueness + fencing token), refreshes `last_seen` and `.live` every poll, watches the agent's own inbox, and injects `[agentchute:run] check inbox` when new mail arrives. It publishes no wake target — peers never poke it.
- **SessionStart** runs `poller ensure`, then `boot` for hook-capable wrappers — verifies inbox visibility, registers the agent and active wrapper session, peeks the inbox, surfaces pending-reply obligations as developer context.
- **UserPromptSubmit** (Claude/codex) / **BeforeAgent** (Gemini) first runs `self-check`, then `poller ensure` — refreshes registration / `last_seen` / `.live` and keeps liveness covered by the runner, an active session heartbeat, or a poller heartbeat. There is no wake target to re-prove or rebind (pull-only).
- The same hook then runs `pending` — a side-effect-free peek that injects current obligations into the model's context per turn. Claude Code and codex use wrapper-specific JSON modes (`--claude-hook UserPromptSubmit`, `--codex-hook UserPromptSubmit`) so the context lands in the right field; Gemini reads plain text via `--json`.
- **Stop** (Claude/codex) / **BeforeAgent** (Gemini, again) first runs `ack` (commits the prior turn's claimed mail — phase 2 of the two-phase consume), then `gate --before finish` — refuses to let the agent end the turn while the inbox/ledger has outstanding work (unread mail, malformed inbox files, pending replies, or a corrupt pending-reply ledger). At `finish` (and `continue`) the gate does NOT check `.live` at all — only owed work (unread mail, malformed files, pending replies, an unregistered self) blocks; a stale/absent `.live` blocks the `commit`/`release` gates only (`consensus` does not check `.live` either). Outstanding/expired asker-owned reply obligations (`.owed`) surface as non-blocking warnings (dead-recipient detection). Claude and Gemini use `--json` exit-code blocking; codex uses its Stop-hook `{"decision":"block"}` JSON. This is the load-bearing one.

> ⚠ **Never use `agentchute check` in a hook.** `check` claims (moves mail to `.claimed`) and quarantines — it mutates the inbox; `ack` is what archives. Hook templates use `boot` / `self-check` only for registration and active-session heartbeats, `poller ensure` for non-tmux visibility, and `pending` for read-only inbox peeks.

Run `agentchute doctor --as <id>` after restarting the wrapper. It validates the loop scaffold, binary resolution, hook files, hook content, registration freshness, inbox/ledger state, wake target health, launch provenance, present-but-not-enrolled wrappers, and recipient liveness without consuming mail.

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

`--ask` writes `reply_required: true` into the message frontmatter and adds a `## ASK` heading, and records claude-code's own obligation in its `.owed` ledger ("I am owed a reply"). When codex runs `check`, it CLAIMS the message (moves it to `inbox/codex/.claimed/`) and displays it — it does not archive yet. `reply_required` is **advisory** on the wire: codex's `check` prints a ready-to-paste reply command, and codex replies and `ack`s when it acts — a new `--ask` does **not** block codex's own finish gate. The binding obligation lives with **claude-code (the asker)**: claude-code's `gate --before finish` surfaces the outstanding `.owed` as a **non-blocking warning** until codex's reply (matching `in_reply_to`) clears it.

```sh
agentchute check --as codex     # CLAIM + display (at-least-once); does not archive
agentchute send --from codex --to claude-code \
  --reply-to to-codex_from-claude-code_seq-... --status signoff --body "looks good"
agentchute ack --as codex       # COMMIT: archive the claimed message (the Stop hook does this for you)
```

`check` prints the exact `--reply-to` reference the reply must carry; consuming that reply clears claude-code's `.owed` obligation. Or codex can defer explicitly with `agentchute defer --message <id> --reason "..."` — the gate clears, the original sender gets an automatic deferred-reply ack.

The reference CLI stores the whole loop at `.<namespace>/loop/`:

```text
agents/             live registrations: agent id, vendor, host (no wake fields)
inbox/<id>/         unread messages owned by each recipient
inbox/<id>/.claimed/ phase-1 CLAIMED, not-yet-committed messages (re-delivered on crash)
live/<id>.live      presence fact: last_seen + advisory busy (fresh => alive)
archive/            consumed (committed) messages
malformed/          quarantined protocol violations
state/<id>/         owed.json (asker obligations), seq counters, serve.claim (lease), pending-reply ledger, poller heartbeat
```

## A handoff in 30 seconds

```text
┌──────────────────┐                          ┌──────────────────┐
│  ALICE (Claude)  │                          │  BOB (codex)     │
└──────────────────┘                          └──────────────────┘
        │ 1. deliver message to Bob's inbox (no-overwrite). No poke.
        ├─────────────────────────────────────────▶
        │                                         │ 2. Bob's runner polls his
        │                                         │    own inbox + injects
        │                                         │    "check inbox"
        │                                         │ 3. check CLAIMS + acts; ack COMMITS
        │ 4. reply lands in Alice's inbox         │
        │◀─────────────────────────────────────────
```

Coordination is pull-only: a sender writes the recipient's inbox and never wakes it. Delivery is no-overwrite by contract — a sender never replaces an existing message, and re-delivering the same `(to, from, seq)` is a benign no-op. The message waits in the inbox until the recipient's runner poll (or its native loop) discovers it; consumption is two-phase (`check` claims, `ack` commits) so a crash mid-turn re-delivers instead of losing the work.

## Commands at a glance

| Command | Purpose |
|---|---|
| `init` | Scaffold loop dirs + drop ENROLLMENT block into wrapper files |
| `boot --vendor <v> [--as <id>]` | Session-start: register + peek inbox + pending-reply summary |
| `run --vendor <v> [--as <id>] -- <wrapper>` | Launch a wrapper under the PTY runner with registration, serve lease, inbox polling, and `check inbox` injection (no wake socket — pull-only) |
| `setup [--wake runner]` | One-command control-repo setup: init + clear stale registrations + hooks + runner launcher shims (`runner` is the only supported wake path) |
| `update [--version <tag>]` | Self-update the binary to a release, then re-sync this repo's setup |
| `shims install [--force] [--aliases]` | Install namespaced launcher shims (`ac-*`); `--aliases` also installs legacy same-name aliases |
| `send --to <b> [--from <a>] [--ask] [--reply-to <ref>]` | Write to recipient's inbox (no poke). `--ask` records the asker's `.owed` obligation; `--reply-to` clears a pending-reply ledger entry |
| `check [--vendor <v>] [--as <id>]` | Phase 1: CLAIM + display inbox (at-least-once; re-displays uncommitted `.claimed` residue with a REDELIVERED banner). Does not archive |
| `ack [--vendor <v>] [--as <id>]` | Phase 2: COMMIT — archive the `.claimed` residue `check` claimed. Idempotent; the Stop hook runs it |
| `pending [--vendor <v>] [--as <id>]` | Side-effect-free peek (inbox + ledger). Hook-safe. |
| `self-check --vendor <v> [--as <id>]` | Hook-safe heartbeat: refresh registration / `last_seen` / `.live` (pull-only: no wake target to rebind) |
| `poller ensure --vendor <v> [--as <id>]` | Start/verify a heartbeat-only poller when no runner or active session is keeping `.live` fresh; add `--launch` for autonomous wrapper launch |
| `poller status --as <id>` | Verify fresh `state/<id>/poller.json` heartbeat |
| `gate --before <phase> [--vendor <v>] [--as <id>]` | Block declaring done if inbox/ledger has outstanding work; a stale/absent `.live` blocks `commit`/`release` only (`finish`/`continue`/`consensus` do not check `.live`) |
| `defer --message <id> --reason "..." [--vendor <v>] [--as <id>]` | Explicit defer; auto-acks the sender |
| `register --vendor <v> [--as <id>]` | Write/refresh registration (boot supersedes for most uses) |
| `status [--as <id>]` | Pool overview: inbox depths, `.live` presence/last_seen |
| `doctor [--as <id>] [--json]` | Diagnostic aggregator: scaffold, binary, hook content, registration, inbox/ledger, `.live` presence, launch provenance, unenrolled presence. Exit nonzero on BLOCKER. |
| `watch [--vendor <v>] [--as <id>] [--notify] [--print] [--exec <cmd>]` | Recipient-side non-consuming watcher for new mail |
| `presenced [--once] [--dry-run]` | Opt-in host presence daemon: discover + auto-enroll/repair high-confidence local wrappers; off by default |
| `prepare-pool --target <dir>` | Connect sibling folders via pointer files |
| `self-poll [--vendor <v>] [--as <id>] [--heartbeat]` | "Should I wake?" helper for schedulers; `--heartbeat` proves polling is alive |
| `doctor --generate-service <kind>` | Emit launchd / systemd / shell unit files for the preflighted scheduler (v0.2) |
| `hooks install --wrapper <name>` | Write the canonical hook template into `.claude/` / `.codex/` / `.gemini/` (v0.2.1) |

Run `agentchute <command> --help` for flags. All commands accept `--control-repo`, `--loop-dir`, and `--json` where applicable.

Commands that act as the current agent resolve identity in order: explicit `--as <id>`, then `AGENTCHUTE_AGENT_ID`, then the contextual `<wrapper>-<folder>` default when a vendor/wrapper is known. (Pull-only registrations carry no wake target, so there is no tmux/herdr pane to map back to an identity.) Set `AGENTCHUTE_AGENT_ID` only for a custom stable lane name.

## Updating

`agentchute update` upgrades the **reference CLI** in one step. It is a convenience of this implementation — the [protocol](AGENTCHUTE.md) versions itself independently (Working Draft v1) and is unaffected by a CLI update. One command replaces the old three-step dance (re-install, re-`setup`, restart):

```sh
agentchute update                  # update to the latest release
agentchute update --version v0.6.1 # pin a specific release
agentchute update --dry-run        # show the plan (from→to, agents it would disrupt); change nothing
agentchute update --no-resync      # binary-only update; do not replay setup
```

It runs in two phases:

1. **Self-update the binary.** Resolves the target release, downloads the release archive plus `checksums.txt`, verifies the exact-filename SHA-256 *before* extracting, and atomically replaces the running binary. Any download or verification failure leaves the installed binary untouched. (Pure Go — it never pipes a remote script to a shell.)
2. **Re-sync the control repo.** Re-execs the *new* binary's `setup`, replaying this pool's saved wake mode, wrappers, shim directory, and profile, so hooks, launcher shims, and `ENROLLMENT` blocks re-sync to the new version's layout. A version bump can change that layout (enrollment-block version, shim names, state schema), which is why updating the binary alone is not enough.

> ⚠ **Restart your agents.** Re-running `setup` clears this pool's live registrations. Until each wrapper restarts and re-enrolls, peers cannot wake it. `update` prints the active agents you need to restart.

Standard `update` must run from the real installed binary (not a launcher shim) and requires a prior `agentchute setup` in the repo — it refuses rather than guess your wake mode. Use `--no-resync` for a binary-only update that does not replay setup or require saved setup state. For a first install, use `install.sh` instead:

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```

## No binary required

The binary is convenience, not the protocol. A hand-protocol agent reads [`AGENTCHUTE.md`](AGENTCHUTE.md) §5, writes its registration to `agents/<id>.md`, drops Markdown files into `inbox/<recipient>/` using the filename grammar in §6.1, claims and acks its own inbox, and — when it sends `--ask` — tracks its **asker-owned** reply obligations in `state/<id>/owed.json` (§6.6). (The recipient-owned `state/<id>/pending-replies.json` is legacy compat only.) The whole protocol fits in one file.

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

## Pull-only delivery

Coordination is pull-only: senders write to your inbox and never wake you, so the only question is what drives your poll. Presence is a published `.live` fact with freshness — a fresh `.live` means alive, a stale or absent one means not-alive (no wake target, no reachability cache). `doctor` flags an absent/stale `.live` as a blocker, and the publishing gates `gate --before commit`/`release` block on it; `gate --before finish`/`continue`/`consensus` do NOT check `.live` (those phases block only on owed inbox or reply work).

Polling tiers that keep `.live` fresh:

1. **Runner / launcher shims (primary)**: `agentchute run --vendor <v> -- <wrapper>` launches the wrapper under a PTY, acquires the serve lease, keeps `last_seen` and `.live` fresh, polls the agent's own inbox, and injects `[agentchute:run] check inbox` when work arrives. `agentchute setup --wake runner` installs namespaced launchers (`ac-claude`, `ac-codex`, `ac-gemini`, `ac-grok`) for this path.
2. **Hook-managed poller fallback**: the canonical hooks run `agentchute poller ensure --vendor <v>`. Under the runner or inside a live wrapper session it no-ops; otherwise it starts/verifies a heartbeat-only `agentchute poller run`, keeping `.live` and `state/<id>/poller.json` fresh without launching a wrapper or consuming inbox mail. Use `--launch` only for an explicitly autonomous recipient.
3. **Native loops**: if your wrapper supports recurring tasks, use them only if they update the heartbeat. Claude Code `/loop` or Codex App Automations should call `agentchute self-poll --vendor <v> --heartbeat`.
4. **Preflighted scheduler**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that run `agentchute self-poll --heartbeat` and only launch the wrapper when work exists.
5. **In-session catchup**: active sessions catch new mail at lifecycle boundaries via hooks (e.g., `gate --before continue`).

For regular terminal sessions, use the non-consuming watcher:

```sh
agentchute watch --vendor <v> --notify
```

Schedule the wrapper, not bare `agentchute check`/`ack`. `check`+`ack` consume mail; `self-poll --heartbeat`, `pending`, `boot`, `doctor`, and `watch` are the safe inspection surfaces.

## Limitations

- **Single shared filesystem** (reference CLI). Multi-machine works if all participants share the volume. Alternate transports (queues, S3, HTTP) are protocol-compatible — see [`EXTENSIONS.md`](EXTENSIONS.md) — but don't ship in the reference CLI.
- **Cooperative trust**: messages are plain text, no signing or encryption.
- **Pull-only, optimistic delivery**: senders never wake recipients; no retries, no DLQ, no read receipts. A message waits in the inbox until the recipient polls.
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

See [`CHANGELOG.md`](CHANGELOG.md) and the [releases page](https://github.com/agentchute/agentchute/releases) for the current version and full history. Current highlights: the **pull-only / protocol-v2 redesign** — senders only write files (no wake adapters, no watchdog, no reachability); durable per-`(sender,recipient)` `seq` identity with exact per-sender FIFO; act-then-archive two-phase consume (`check` claims, `ack` commits, at-least-once); `.live` presence with freshness; asker-owned `.owed` reply obligations; and a serve lease + fencing token for id-uniqueness. One-command [`agentchute update`](#updating) still applies. Already installed? Run `agentchute update` to upgrade and re-sync.

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
