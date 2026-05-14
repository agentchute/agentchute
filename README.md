# agentchute

**Running multiple AI agents on a shared project is easy. Coordinating them isn't.**

**agentchute is a small Markdown protocol that gives AI agents shared inboxes, so they can message each other, request review, and hand off work without a human acting as the relay.** Humans stay in the loop for decisions and steering, not for ferrying messages between terminals.

**No central server or broker required. No SDK. No compiled code required.** When direct wake-up is available (e.g., `tmux send-keys`), senders can also poke recipients directly. At the protocol level, it's just [`AGENTCHUTE.md`](AGENTCHUTE.md) plus a short enrollment block your agents read and follow.

Install the optional reference CLI:

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```

[![Real agentchute session: Claude, codex, Gemini in tmux (24x speedup, 62 seconds)](https://img.youtube.com/vi/jwYzKtcOYl0/maxresdefault.jpg)](https://www.youtube.com/watch?v=jwYzKtcOYl0)

*Three agents coordinating in tmux during the final pre-release cleanup pass — real working session, 24x speedup. Click for the 62-second clip.*

The protocol doesn't assume agents are local, doesn't define delivery latency or polling cadence, and doesn't lock you to any particular substrate. Inbox medium, sender-to-inbox transport, and wake mechanism are implementation choices. The protocol boundary is the inbox contract; location and timing are properties of whatever implementation you pick.

The v0.1 reference CLI makes one concrete choice per axis: filesystem inbox + atomic-rename transport + `tmux send-keys` peer wake + the wrapper's own self-poll loop as recipient fallback (Claude Code's `/loop`; codex CLI and Gemini CLI use an operator-owned scheduler). That implementation worked well enough to be the exclusive coordination layer for the team that built the protocol. Alternates (queues, object stores, HTTP, git-backed transports, other wake adapters, other self-poll mechanisms) are protocol-compatible and discussed in [`EXTENSIONS.md`](EXTENSIONS.md#alternate-transports) (with a worked git-as-transport sketch).

- **The protocol primitive is the shared inbox.** Per-recipient ordered message stream, no-overwrite delivery, recipient-owned consumption, optional wake. See AGENTCHUTE.md §1.
- **Per-agent inbox** at `.<vendor>/loop/inbox/<id>/` in the v0.1 reference implementation; messages are timestamped markdown files. Frontmatter is optional/recommended metadata.
- **Cooperative waking.** Every `agentchute check` cycle opportunistically runs the watchdog algorithm against peer inboxes (metadata only) and pokes any stale peer with unread mail. Best-effort distributed liveness when at least one peer is actively polling — no dedicated process required for that path.
- **Registration** at `.<vendor>/loop/agents/<id>.md` declares name, vendor, `host`, optional `wake_method` + `wake_target`, `last_seen`.
- **Wake-up is pluggable.** Agents declare `wake_method` + `wake_target`; the reference CLI ships the tmux adapter today. With the filesystem-backed inbox, agents in different terminals (or different machines sharing one filesystem) can share one pool. If a sender can't invoke a recipient's wake method, the message still lands — the recipient catches it on its next poll.
- **Best-effort protocol correction (§11).** Malformed inbox file → quarantine + corrective message. No refusal mode.
- **Message bodies are recipient-owned.** Liveness checks (watchdog or cooperation) use inbox metadata only; no peer consumes another peer's message body.
- **No retries, no acks, no signing, no routing.** Fire-and-forget; if peer trust isn't a given, don't run them on your machine.
- **Reference CLI is filesystem-backed.** All participants of a v0.1 pool read/write one shared filesystem. Non-filesystem inbox transports (Redis/NATS queues, S3-prefixed inboxes, HTTP endpoints) are protocol-compatible and discussed in [`EXTENSIONS.md`](EXTENSIONS.md#alternate-transports), but do not ship in v0.1.
- **Reference CLI is optional.** ~4000 lines of stdlib Go; ships the tmux wake adapter. Other multiplexers (wezterm, kitty, iTerm2, Terminal.app) are protocol-compatible and await community CLI adapters — see [`EXTENSIONS.md`](EXTENSIONS.md).

It's not a multi-agent framework — no task graphs, no retries, no role election. It covers the common case: agents sharing a project and handing work off. The hard requirement is just an inbox-checking cadence; the v0.1 reference CLI additionally needs a shared filesystem. **The simplicity is the point.**

## Required local access (v0.1 reference CLI)

The v0.1 reference CLI needs:

- Read/write access to its configured inbox medium. For the reference CLI that's `<repo>/.agentchute/loop/` and its subdirectories on the shared filesystem; an alternate implementation needs equivalent access to whatever stores its inbox.
- **`tmux`** — the v0.1 reference CLI ships one peer-wake adapter: `tmux send-keys`. Install tmux and run agents in addressable tmux panes if you want senders to wake recipients immediately after delivery. New to tmux? The [tmux starter kit](examples/tmux-quickstart.md) is a 5-minute walkthrough from install to running pool. Without tmux, delivery still works — messages land in the recipient's inbox — but recipients must poll manually, use a wrapper-provided self-loop (see "Running without tmux" below), or run an operator-owned scheduler that invokes the agent.
- `curl` once, only to fetch `AGENTCHUTE.md` when bootstrapping a new project.

No agentchute server or app-level network service at runtime; network use is limited to whatever your inbox medium or wake adapter requires. No security guarantees — peers share the configured inbox medium (and, when a wake convention is in use, the substrate the wake adapter uses); the threat model is cooperative.

### Running without tmux

The v0.1 reference CLI works without tmux — peer wake is just unavailable. Recipients then need a polling cadence. **Do not just wrap `agentchute check` in a shell loop**: that consumes and archives messages without an actual agent acting on them. The loop has to invoke the wrapper so the model sees the message.

Per-wrapper patterns we've verified:

- **Claude Code**: use `/loop check` — Claude's built-in recurring task feature. The agent itself sees each tick, so it processes whatever lands in its inbox.
- **codex CLI** (`codex-cli 0.130.0` — no built-in self-loop): use an operator-owned scheduler that invokes `codex exec` with an inbox-processing prompt. Example shape:
  ```sh
  while true; do
    codex exec -C /path/to/repo \
      "Follow AGENTS.md. Run ./agentchute check --as codex, process any messages addressed to codex, reply if needed, then stop."
    sleep 30
  done
  ```
  Sandbox and approval flags depend on your local codex config; the loop is operator-owned and not blessed for arbitrary write privileges.
- **Gemini CLI** (no built-in self-loop): same shape as the codex pattern — an operator-owned `while`-loop that invokes `gemini` with an inbox-processing prompt and exits.

If your wrapper isn't in the list, the rule is the same: schedule the wrapper, not the bare CLI. A recurring task that asks the agent to "process my inbox and reply if needed" preserves the protocol; a bare `agentchute check` loop drains messages into the archive without comprehension.

## What it isn't

agentchute does not have: a web UI, a database, a server, a queue, a message broker, a webhook, a cloud component, an SDK, a SaaS pricing tier, a Slack bot, a Discord integration, telemetry, accounts, login, billing, "AI", or a roadmap with seventeen quarters of feature dates.

It is also explicitly **not** a:

- **Distributed transport with cryptographic guarantees.** The v0.1 reference CLI runs over a shared filesystem and provides no signing, no acks, no exactly-once. (The protocol itself is medium-agnostic; alternate inbox transports are compatible — see [`EXTENSIONS.md#alternate-transports`](EXTENSIONS.md#alternate-transports) — they just don't ship in v0.1.) Within the filesystem-backed pool, multi-machine works only when all participants share one filesystem; wake delivery remains local per recipient. For fully distributed, cryptographically-audited coordination, use a signed-envelope protocol; the v0.1 reference CLI is the local, unsigned, cooperative-trust sibling.
- **Message broker with delivery guarantees.** No retries, no acknowledgments, no exactly-once. If you need queues, use a queue.
- **Identity / auth system.** Messages are unsigned plain-text. If you don't trust your peers, don't run them on your machine.
- **Routing or task-assignment engine.** Agents are peers; senders pick recipients explicitly. There is no wildcard inbox, no broadcast, no role election.
- **Audit log.** The archive is local and gitignored by default. Loop messages are a transient operational trace, not an authenticated record.

If you wanted any of those, this is the wrong tool, and that's fine.

## Who this is for

- You run two or more agent CLIs in the same repo (Claude Code, codex, Gemini, Aider, …).
- You want explicit handoffs and review requests without becoming the message bus yourself.
- You do not want a server, SDK, broker, or new agent framework.

## How it compares

Short, factual reads of adjacent tools. None of these are wrong; they answer different questions.

| Tool | What it is | When to use it instead |
|---|---|---|
| **OpenClaw** | A full personal-assistant runtime: gateway daemon, channels (Telegram/Slack/etc.), apps, workspace, skills, an always-on control plane. | You want a managed assistant platform with chat-app integrations and a control plane, not a coordination protocol between local CLIs. |
| **Claude Code Channels** | Pub/sub for Claude Code agents via external IM brokers (Telegram, Discord, iMessage). Vendor-locked to Anthropic. | All your agents are Claude Code and you want IM-app delivery. agentchute coordinates Claude, codex, Gemini, and any wrapper that can run in a terminal — no broker required. |
| **Claude Code subagents** (Claude's `/dispatch`) | In-session subagents inside one Claude Code process. Same model, parallel scratchpads, single decision-maker. | You want one agent to spawn helpers inside its own context. agentchute is across-agent across-process — different wrappers, different model providers, talking to each other. |
| **mcp_agent_mail** | An MCP server that gives any MCP-compatible agent an inbox + delivery semantics. Closest functional sibling. | You're already in the MCP world and want a hosted broker doing the inbox work. agentchute is broker-free — the inbox is a directory on a filesystem (or any substrate that preserves the primitives). |

The agentchute pitch — protocol-not-platform, no broker, no SDK, runs over a substrate you already have — is what these four don't deliver in combination.

## Quickstart (no binary)

This is the protocol-only path: a short shell snippet scaffolds the loop, plus one manual step per agent file. No `agentchute` binary required.

```sh
# 1. Fetch the spec.
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/AGENTCHUTE.md -O

# 2. Scaffold the loop directories at owner-only perms.
mkdir -p .agentchute/loop/{agents,inbox,archive,malformed}
chmod 700 .agentchute/loop/{agents,inbox,archive,malformed}

# 3. Ignore live state in git.
cat >> .gitignore <<'EOF'
.agentchute/loop/agents/*.md
.agentchute/loop/inbox/
.agentchute/loop/archive/
.agentchute/loop/malformed/
EOF
```

**Step 4 (manual):** drop the ENROLLMENT block into each agent's discovered file. The block is the same template with different values per wrapper:

- `CLAUDE.md` (Claude Code) — render `templates/enrollment/wrapper.md` with `agent_id=claude-code`, `vendor=anthropic`. Or copy `CLAUDE.md`'s marked block from this repo.
- `CODEX.md` (codex CLI) — same template, `agent_id=codex`, `vendor=openai`. Or copy `CODEX.md`'s marked block.
- `GEMINI.md` (Gemini CLI) — same template, `agent_id=gemini-cli`, `vendor=google`. Or copy `GEMINI.md`'s marked block.
- `AGENTS.md` (universal) — different template: use `templates/enrollment/agents.md` (the version with the wrapper-to-id table). Or copy `AGENTS.md`'s marked block.

The block teaches each agent how to register, check, send, and participate in protocol correction using `mv` (plus an optional wake poke via the adapter declared in the recipient's `wake_method` — `tmux send-keys` for `wake_method: tmux`, the only adapter shipped in the reference CLI) per AGENTCHUTE.md §5 / §6 / §11. Don't cross-paste a wrapper's block into a different wrapper file — the `agent_id` / `vendor` values are per-tool.

Once those four files have the ENROLLMENT block at the top, start your agents — in tmux panes if you want the direct wake poke, or in any other shell if you don't. Each one reads its discovered file, follows the steps, and registers itself by writing `.agentchute/loop/agents/<its-id>.md` per the spec's canonical template. Sending and checking are `mv` (plus an optional wake poke when the recipient's `wake_method` adapter is reachable from this host); agents without a reachable wake method just check their inbox on their own cadence.

If you'd rather skip step 4 (and let the binary manage the marked blocks idempotently), use the CLI path below.

## Optional: install the CLI

If you'd rather not have your agents hand-write timestamps and nonces, install the reference CLI:

```sh
# One-line install:
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh

# Or, with Go on PATH:
go install github.com/agentchute/agentchute@latest
```

Pre-built binaries are on the [releases page](https://github.com/agentchute/agentchute/releases) for macOS (arm64, amd64) and Linux (amd64, arm64). Windows isn't supported directly — the reference tmux adapter and the toolchain are built for POSIX shells. WSL works.

What the CLI buys you:

- `agentchute init [--yes] [--namespace <slug>] [--dry-run]` — does the scaffold above PLUS the ENROLLMENT block drop into each wrapper file (the manual Step 4). Idempotent, shows a confirmation prompt by default.
- `agentchute register --as <id> --vendor <vendor> [--host <name>] [--wake-method <adapter>] [--wake-target <addr>] [--bio "..."] [--announce]` — writes your registration. `--host` defaults to `os.Hostname()`; `--wake-method` + `--wake-target` are auto-detected from `$TMUX_PANE` when run from inside a tmux pane. `--announce` is operator-gated and sends an enrollment notification to every peer; it is not part of mandatory session-start registration.
- `agentchute check --as <id> [--limit <n>] [--no-archive]` — pulls your inbox, archives processed messages, enforces §11 (quarantines malformed files, notifies the offender), and runs §10.5 cooperative waking (pokes any stale peer with unread mail; may write a `watchdog.log` line). `--no-archive` is a dry-run mode: it still reads and prints valid messages, but does not archive, quarantine, send corrections, or poke peers.
- `agentchute send --from <id> --to <peer> --task "..." [--body <text> | < body.md] [--reply-to <msg-id>] [--status <status>]` — writes the message file and dispatches the wake poke via the recipient's declared `wake_method`.
- `agentchute prepare-pool --target <folder> [--target <folder> …] [--replace-pointer] [--update-gitignore]` — scaffolds cross-folder enrollment in one or more sibling folders: writes a `.agentchute-control-repo` pointer file plus ENROLLMENT-rendered wrapper files in each target. Idempotent and atomic; preflight rejects symlink targets and writes to a temp file before rename.
- `agentchute status --as <id>` — registry overview, inbox depths, `last_seen` freshness, `HOST` and `WAKE` columns.
- `agentchute watchdog --as <id>` — optional liveness daemon (also implementable as a self-waking wrapper task, where the wrapper has that capability, or a human checking timestamps; see §10.5 / §10).

All commands accept `--control-repo <path>` and `--loop-dir <path>` to override the discovery cascade (flag > env > pointer > cwd; see `.agentchute-control-repo` below).

Both paths produce the same on-disk state. You can mix: hand-protocol on one pane, CLI on another, in the same loop.

### Cross-folder pools (the `.agentchute-control-repo` pointer)

If you want one repo (the *control repo*, holding `AGENTCHUTE.md` and the loop directory) to coordinate agents across sibling working folders, drop a one-line file named `.agentchute-control-repo` in each target folder containing the absolute or relative path to the control repo. Discovery walks `cwd` to filesystem root looking for the pointer; nearest wins. Override order: `--control-repo` flag > `AGENTCHUTE_CONTROL_REPO` env > pointer file > `cwd` (the cascade above). `agentchute prepare-pool` writes the pointer plus ENROLLMENT wrappers in each target for you. Full detail in [`EXTENSIONS.md`](EXTENSIONS.md) and AGENTCHUTE.md §4.1 / §4.2.

## How it works

### The Handoff: simple two-agent message flow

```text
┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │                        │ BOB (codex)      │
│ inbox: 0         │                        │ inbox: 0         │
│ status: active   │                        │ status: active   │
└──────────────────┘                        └──────────────────┘
         │                                            ▲
         │ 1. message lands in inbox/bob/ (bob: 1)    │
         └────────────────────────────────────────────┘
         │
         │ 2. wake poke (tmux adapter)
         └─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─▶
                                                      │
                                                      │ 3. check
                                                      │    read + archive
                                                      ▼
┌──────────────────┐                        ┌──────────────────┐
│ ALICE (Claude)   │◀───────────────────────│ BOB (codex)      │
│ inbox: 1         │ 4. reply               │ inbox: 0         │
│ status: active   │                        │ status: active   │
└──────────────────┘                        └──────────────────┘
```

### The Review: multi-agent coordination with liveness sidecar

```text
┌────────────────────────────────────────────────────────────┐
│ SHARED INBOX MEDIUM                                        │
│ ref CLI stores it at .<vendor>/loop/                       │
└────────────────────────────────────────────────────────────┘

Task traffic (solid)                         Wake pokes (dashed)
────────────────────                         ─ ─ ─ ─ ─ ─ ─ ─ ─

┌────────────────┐      ┌────────────────┐      ┌────────────────┐
│ CLAUDE-CODE    │      │ CODEX          │      │ GEMINI-CLI     │
│ inbox: 0→2     │      │ inbox: 0→1→0→1 │      │ inbox: 0→1→0→1 │
└───────┬────────┘      └───────▲────────┘      └───────▲────────┘
        │ 1. request review     │                       │
        ├──────────────────────▶│                       │
        ├──────────────────────────────────────────────▶│
        │ 2. wake               │                       │
        ├─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─▶│                       │
        ├─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─▶│
        │                       │ 3. findings           │ 3. findings
        │◀──────────────────────┤                       │
        │◀──────────────────────────────────────────────┤
        │ 4. consolidate + ask for sign-off              │
        ├──────────────────────▶│                       │
        └──────────────────────────────────────────────▶│

Liveness sidecar (metadata only)
        ┌────────────────┐
        │ WATCHDOG       │
        │ liveness §10   │
        └───────┬────────┘
                │ 5. stale unread inbox?
                └─ ─ ─ ─ ─ ─ ─ ─▶ wake peer
```

### Walkthrough

Each agent has a registration file (`.<vendor>/loop/agents/<id>.md`) with its name, vendor, `host`, optional `wake_method` + `wake_target`, and a `last_seen` timestamp. When an agent sends a message, it writes a markdown file into the recipient's inbox directory (filename is `<timestamp>_from-<sender>_msg-<nonce>.md` per §6.1.2 — the reference filename encoding of the §6.1.1 identity tuple). If the recipient is pokable, the sender dispatches via the wake adapter named in `wake_method`. For `wake_method: tmux`, that means typing the literal string `check` into the recipient's pane using two `tmux send-keys` calls separated by a short sleep (chained `'check' Enter` is unreliable across tmux versions). The recipient sees `check` arrive (or, without a reachable wake method, discovers the file on its next poll), reads its inbox per §6.3, prints the message content, and moves the file to an archive. Done.

After own-inbox work, every `agentchute check` cycle also runs **cooperative waking** (§10.5): it walks peer inbox metadata and pokes any stale peer with unread mail (filenames + timestamps only — never bodies). That's how pools self-heal without a dedicated watchdog process when at least one agent is actively polling.

The whole **protocol** is a small set of medium-agnostic primitives (per-recipient inboxes, ordered messages, no-overwrite delivery, recipient-owned consumption, optional wake — see AGENTCHUTE.md §1). The **reference implementation** maps those onto filename conventions, atomic-rename writes, and (optionally) a wake poke on a shared filesystem. No magic in either layer. The spec at [`AGENTCHUTE.md`](AGENTCHUTE.md) is the source of truth.

> **wake_target lifecycle note.** `tmux` pane IDs (`%N`) are monotonic and never reused. When a pane (or tab/window) closes, its ID is gone permanently. A new pane gets a fresh ID. Re-register from inside the new pane so your `wake_target` reflects reality — `agentchute register --as <id> --vendor <vendor>` auto-detects via `$TMUX_PANE`.

## Protocol correction

agentchute is no-retransmit: there are no acknowledgements, no retries, no central authority. To stay functional, every agent participates in best-effort protocol correction (§11). If a recipient finds a malformed file in its inbox — bad filename, broken frontmatter — it quarantines the file to `.agentchute/loop/malformed/` and sends a one-message correction to the inferred offender: *"malformed item: X; reason: Y; action: re-send per §6.1."* Three lines, compiler-error shape. No refusal mode; subsequent valid messages from the same sender process normally.

The reference CLI's `agentchute check` does this automatically. Hand-protocol agents do it per §6.3 step 7 and §11.

## Watchdog (optional)

Most pools don't need a dedicated watchdog: every `agentchute check` cycle already runs cooperative waking (§10.5), which pokes any stale peer with unread mail. For 24/7 setups where no agent is reliably polling — or as belt-and-suspenders for high-uptime pools — there are two complementary options:

- **Wrapper self-loop.** If any agent's wrapper supports a recurring task (Claude Code's `/loop` is the example we use), that session runs `agentchute watchdog --once --as <id>` each tick. Zero extra processes, no daemon to manage. Works only for wrappers that ship this capability.

- **Standalone daemon.** Run `agentchute watchdog --as watchdog &` as a background process — same algorithm, separate process. Works regardless of which wrappers are in the pool. Logs to `<vendor>/loop/watchdog.log`.

Detail in `AGENTCHUTE.md` §10. Watchdog (and cooperative waking) is liveness-only — neither routes, ranks, nor interprets tasks.

## Why does this exist

The honest version: in early 2026 it became normal to run two or three AI agents at once for the same project (Claude Code, codex CLI, gemini, whatever). The "coordinate by typing in the chat" workflow ate hours per week, and existing solutions either didn't exist (codex has no built-in coordination) or shipped a server, a database, and a Helm chart for what is structurally a 200-line problem.

agentchute is what's left when you remove every layer that didn't have to be there. The spec is short. The binary is small. The only moving parts are local files and, when available, tmux for direct wake-ups.

## Limitations

In approximate order of "how often will this bite you":

- **Filesystem-backed inboxes (reference CLI only).** The v0.1 reference CLI puts every pool's inboxes on one shared filesystem. Single-machine is the common case; multi-machine works as long as all participants share that filesystem (NFS, SSHFS, shared volume, etc.). Wake delivery remains local per recipient — cross-host pools depend on each machine having its own wake mechanism (peer cooperation, local watchdog, recipient self-polling). Non-filesystem inbox transports (queues, S3, HTTP) are protocol-compatible — see [`EXTENSIONS.md`](EXTENSIONS.md#alternate-transports) — but don't ship in v0.1; if you need cryptographic audit on top of distributed transport, use a signed-envelope protocol.
- **Optimistic delivery.** No retries, no DLQs. If the recipient is pokable and the poke fails (or the recipient isn't pokable at all), the message sits in the inbox until the recipient's next poll — its wrapper loop, the watchdog, or a manual `check`. Cooperative waking on every `check` cycle, plus the optional standalone watchdog, catches stale inboxes after a delay.
- **No concurrency coordination.** agentchute doesn't know if two agents are editing the same file. Use git for that.
- **Plain text.** No signing, no encryption. Don't use agentchute to coordinate workloads where peer trust is not a given.
- **Linux/macOS only.** Windows users: WSL.

## Hacking on it

```sh
git clone https://github.com/agentchute/agentchute
cd agentchute
gofmt -w .
go vet ./...
go test ./...
go build ./...
```

The codebase is intentionally small. `internal/loop/` has the data layer (config, registration, inbox, message routing, pointer discovery, wake adapters, tmux adapter). The seven command files (`init.go`, `prepare_pool.go`, `register.go`, `send.go`, `check.go`, `status.go`, `watchdog.go`) are dispatched from `main.go`. No third-party dependencies.

PRs welcome, especially for: edge cases in the watchdog, additional integration tests, fixes for filesystem quirks. The spec is the source of truth; if a PR changes behavior, propose the spec change first.

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for details.

## License

MIT. See [`LICENSE`](LICENSE).

## Spec

Full protocol is in [`AGENTCHUTE.md`](AGENTCHUTE.md). Read that if you're implementing agentchute in another language or just want to know what the binary is doing under the hood.

---

*agentchute is built by [reHuman Labs](https://rehumanlabs.com). The umbrella thesis: let humans do human work, let agents do agent work, give humans more time to think instead of coordinating and copy-pasting.*
