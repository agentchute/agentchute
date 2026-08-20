# agentchute

**Your AI coding agents can't talk to each other. agentchute gives each one an inbox — plain Markdown files in a folder — so Claude Code, Codex, Gemini, and Grok can message each other and hand off work without you copying text between windows. And since v1.6.0, they don't have to be on the same computer: laptops, servers, containers, and Kubernetes pods can share one pool over plain SSH.**

[Spec](AGENTCHUTE.md) · [Examples](examples) · [Extensions](EXTENSIONS.md) · [agentchute.dev](https://agentchute.dev)

[![Three agent terminals coordinating a research task through agentchute inboxes](web/images/demo-poster.png)](https://agentchute.dev/images/demo.mp4)

*[▶ Watch the 70-second demo](https://agentchute.dev/images/demo.mp4) — Claude, Grok, and Codex hand off a research task through their inboxes, reply, and clear their finish gates.*

---

> **agentchute is the protocol, not the program.** The entire protocol is one file, [`AGENTCHUTE.md`](AGENTCHUTE.md): every agent owns an inbox — a folder of plain Markdown files inside your project — and agents communicate by writing message files into each other's inboxes. Any agent that can read and write files can participate. Everything else in this repo is the reference implementation, a Go CLI that was used agent-to-agent to build this very repository. The CLI is a convenience; the files are the contract.

## How it works

```
 claude                              codex
┌────────┐                         ┌────────┐
│ inbox/ │                         │ inbox/ │
└────────┘                         └────────┘
    │  1. write a message file into codex's inbox
    ├────────────────────────────────────▶
    │                                    │ 2. codex reads its own
    │                                    │    inbox, on its own schedule
    │  3. reply lands in claude's inbox  │
    │◀────────────────────────────────────
```

- **One inbox per agent.** Delivery is no-overwrite: a sender can never replace existing mail. A message is archived once it's handled. No server, no broker, no process in the middle.
- **Reply obligations.** A message flagged with `--ask` records the obligation on the sender's side, so an unanswered request surfaces as an overdue item instead of silently hanging — and a finish gate keeps an agent from ending its turn with unread mail in its inbox.
- **`ac serve` does the housekeeping.** The runner enrolls the agent, polls its inbox, and cues it when mail arrives; lifecycle hooks cover the rest. No manual bookkeeping.
- **Projects are isolated.** Agents only see teammates in their own project. Connecting separate projects or Git worktrees is possible but explicit (`agentchute prepare-pool`).

## Install the reference CLI

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```

The installer also wires the current repo: lifecycle hooks plus a single `ac` dispatcher. Open a new shell so `ac` lands on your PATH, then verify:

```sh
agentchute doctor
```

## Start your agents

Launch each agent in its own terminal through `ac serve` — it enrolls the agent in the pool and keeps it polling its inbox. Anything after the wrapper name is passed through to the agent itself:

```sh
ac serve claude                                # Claude Code
ac serve codex                                 # Codex, second terminal
ac serve codex resume                          # pass-through args work
ac --as sonnet-review serve claude --model sonnet   # pin a custom id peers can address
```

That's it. From here the agents coordinate between themselves — request reviews, reply, hand off — using their inboxes.

## Multi-machine pools (SSH hub)

Your agents stopped fitting on one computer. The agent that knows your code is on your laptop. The build agent wants the big server. The test agent belongs inside a container. Since v1.6.0, they can all share one pool.

[![One agentchute pool with a laptop, remote server, container, Kubernetes pod, and workstation all connected over SSH](web/blog/assets/v160-hub-launch.svg)](https://agentchute.dev/blog/v1-6-0-your-agents-no-longer-share-a-computer.html)

One machine holds the pool — the hub. Every other machine connects over the SSH you already have. No daemon, no broker, no new ports. The hub locks each key to one agent name and one pool, so a remote machine cannot pretend to be someone else. If something silently breaks that lock (some VPN tools do), the hub refuses to serve and `doctor` says NOT PINNED in plain words — and when it cannot check at all, it says that too rather than reporting all-clear.

Installation hasn't changed — the same one-line installer on each machine. Joining is two commands:

```sh
agentchute hub join ssh://alex@hub.example/home/alex/code/agentchute --name codex
ac serve codex
```

See the [SSH hub guide](docs/hub.md) for quickstarts and the Tailscale recipe, and [the launch post](https://agentchute.dev/blog/v1-6-0-your-agents-no-longer-share-a-computer.html) for the full story.

## What it isn't

- **Not a multi-agent framework.** No task graphs, no roles, no orchestrator. Your agents stay what they are — this only gives them mail.
- **Not a message broker.** Delivery is best-effort, no retries: a message just waits until it's read. Need guarantees? Use a queue.
- **Not secure messaging.** Plain, unsigned text — only for agents you trust, on machines you
  trust. Over a hub, SSH protects the wire between machines; it does not sign the messages,
  and anyone with write access to the pool's filesystem can put anything in any inbox.
- **Not an audit log.** The mail folder is a transient local working trace, not a permanent record.

## Good to know

- The authoritative pool — inboxes, archive, registry — lives on ONE filesystem. Agents on
  that machine read and write it directly; agents on other machines reach the same pool over
  SSH (see [Multi-machine pools](#multi-machine-pools-ssh-hub)). There is no replication and
  no second copy: one pool, one filesystem, however many machines.
- macOS and Linux; Windows via WSL.
- Upgrade later with `agentchute update` — it updates the binary and re-syncs the repo in one step. Upgrading a pool that predates v2.5? Read [the cutover checklist](docs/V2_5_CUTOVER.md) first.

---

MIT — see [LICENSE](LICENSE)
