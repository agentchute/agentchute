# EXTENSIONS.md

*The protocol is agnostic by design. This document is the home for everything that's pluggable but not shipped in the v0.1 reference CLI.*

[`AGENTCHUTE.md`](AGENTCHUTE.md) defines the semantics — ordered messages, named inboxes, atomic delivery, sender-recipient pokes — and stays silent on *how* you implement them. The reference CLI picks the simplest concrete substrate (shared filesystem + tmux). Anything that preserves the protocol semantics is a valid agentchute extension.

---

## What the reference CLI ships in v0.1

- **Transport.** Shared local filesystem with atomic create/rename semantics (POSIX `rename(2)` or equivalent). Works on local disks, NFS, SSHFS, and other mounted filesystems as long as atomicity holds across writers — see the cross-host caveats below.
- **Wake adapter.** `wake_method: tmux`, using `tmux send-keys` to deliver `check` + `Enter` to a target pane. Targets accept `%pane_id` and `session:window.pane` (panes in different tmux windows on the same server work as long as they share one tmux server).
- **Watchdog / cooperative waking.** Filesystem-walk over `.<vendor>/loop/agents/`, per-peer poke via the tmux adapter when stale + has-unread. Cooperation is run by every `agentchute check` cycle; the dedicated `agentchute watchdog` daemon is the unattended fallback.

If your setup matches that — single filesystem, agents in tmux panes on one server, polling via `agentchute check` or the watchdog — you don't need anything in this document.

---

## Extension dimensions

These exist in the protocol's design space but are out of v0.1 reference scope. Implementers MAY add them in their own forks/distributions; the protocol welcomes it.

### Alternate transports

The shared "inbox" doesn't have to be a directory on a local filesystem. Anything providing ordered, per-recipient append + listing + atomic move can carry agentchute messages. Candidates:

- **Git as transport.** Commits as messages, branches or directories as inboxes. Solves cross-machine and audit trail in one move; trades atomicity and latency for it.
- **Object stores.** S3, GCS, Azure Blob. Per-recipient prefixes as inboxes. Atomicity model varies; some have no atomic-rename equivalent.
- **Message queues.** SQS, Kafka, RabbitMQ, Redis Streams. Per-recipient queue/stream. Different ordering and persistence guarantees than a filesystem; would require the implementer to preserve agentchute's at-least-once / oldest-first / no-overwrite semantics.
- **HTTP endpoints.** Per-recipient PUT/GET surface on any HTTP server with `If-None-Match` and `If-Match` headers (or equivalent ETag semantics). The cleanest mapping for distributed pools; no shared filesystem required.

None of these ship in the reference CLI. They are conceptually compatible.

#### Worked example: agentchute on git

Pedagogical sketch. No implementation ships under this name; the point is to show concretely how the protocol's primitives map onto a non-filesystem substrate.

- **Inbox medium.** Each agent has a branch (e.g., `agentchute/inbox/claude-code`) tracked in a shared bare repo. Messages are files under that branch; you preserve the §6.1.1 identity tuple semantics, and the §6.1.2 filename encoding is a natural choice for a file-based substrate (reuse it directly, or roll a substrate-native equivalent).
- **Sender→inbox transport.** Sender creates the file in its local checkout, commits, and pushes with a compare-and-swap on the inbox ref (`--force-with-lease` is the portable approximation; modern git supports `--push-option=cas=<oid>=<ref>` on some hosts). If the push is rejected because someone else's message landed first, the sender regenerates the nonce and retries — exactly the same retry shape the filesystem implementation uses for `rename(2)` collisions.
- **Ordering.** Filename timestamp remains authoritative; commit time is advisory metadata.
- **Recipient flow.** Recipient pulls its inbox branch, processes messages oldest-first by filename, then moves consumed files under `agentchute/archive/` (or a parallel archive branch), commits, pushes. Quarantine works the same way under `agentchute/malformed/`.
- **Wake.** A git server hook (GitHub Actions, GitLab webhook, self-hosted `post-receive`) fires when the inbox ref advances. The wake adapter owns the webhook URL the same way the tmux adapter owns a pane id; `wake_target` becomes the endpoint URL.
- **Registration.** Same shape as the filesystem: `agentchute/agents/<id>.md` on a dedicated branch.

Trade-offs an implementer should be honest about:

- **Latency.** Git push round-trips are 100ms-1s, not microseconds. The protocol is speed-agnostic (§1), so this is a property of the implementation, not a conformance issue — but operators should know.
- **Atomicity granularity.** Only the ref update is atomic. Two simultaneous pushes mean one is rejected; the rejection is the no-overwrite primitive working, just at a coarser granularity than `rename(2)`.
- **Audit trail bonus.** Every message is signed by whoever pushed (if the remote enforces signed pushes). Cross-machine works for free. Pool compaction is `git gc`.

The same shape applies to HTTP-, object-store-, and queue-backed implementations. The primitives carry; the substrate's atomicity model decides what "no-overwrite" looks like.

#### Conformance for transport extensions

If you ship a non-filesystem transport (git, S3, queue, HTTP, etc.) under the agentchute name, the implementation MUST preserve:

- **Identity tuple semantics** (AGENTCHUTE.md §6.1.1). The `(timestamp, sender, nonce)` tuple is the protocol-level identity; uniqueness within a recipient inbox and oldest-first ordering with deterministic tie-breaking are required regardless of how the tuple is encoded. If your substrate uses files, you MAY reuse the §6.1.2 filename encoding directly; otherwise, encode the tuple however your substrate prefers as long as the invariants hold.
- **Per-recipient ownership.** Only the recipient consumes its own inbox. Senders write only.
- **Ordering.** Oldest-first delivery by identity tuple; no reordering.
- **No-overwrite delivery.** If two messages arrive with the same identity tuple, the second MUST NOT overwrite the first. The substrate's atomicity model decides the mechanism (atomic rename on filesystems, ref compare-and-swap on git, `If-None-Match` on HTTP, queue dedupe on Kafka/SQS).
- **Logical envelope fields** (AGENTCHUTE.md §6.4.1). Preserve `message_id`, `from`, `to`, `in_reply_to`, `task`, `status` with their stated semantics. The YAML-in-Markdown encoding (§6.4.2) is one realization; you may encode the same logical fields in JSON, queue attributes, or whatever your substrate offers.
- **Best-effort wake semantics.** Wake delivery may fail; message delivery must not.

Transport extensions MUST identify themselves as "agentchute with X transport adapter" — not as "v0.1 reference agentchute." The reference v0.1 means the filesystem transport described in [`AGENTCHUTE.md`](AGENTCHUTE.md). Any other transport is a compatible extension, not the reference.

### Alternate wake adapters

The tmux adapter is one instance of a generic "deliver a keystroke or trigger to the recipient's pane" capability. Other multiplexers ship the same capability under different CLIs:

| Multiplexer / OS | Wake trigger | Status |
|---|---|---|
| **tmux** | `tmux send-keys` | shipped in reference CLI |
| **macOS** | `osascript` (notification) | shipped in v0.1.2 (notifies human) |
| **Linux** | `notify-send` | shipped in v0.1.2 (notifies human) |
| **wezterm** | `wezterm cli send-text` | protocol-compatible, awaits CLI adapter |
| **kitty** | `kitty @ send-text` | protocol-compatible, awaits CLI adapter |
| **iTerm2** | AppleScript / Python API | protocol-compatible, awaits CLI adapter (macOS-only) |
| **Terminal.app** | AppleScript | protocol-compatible, awaits CLI adapter (macOS-only) |
| ghostty | none currently | no IPC for this |
| alacritty | none | anti-IPC by design |
| Windows Terminal | none | lacks the CLI |

Beyond multiplexers, "wake adapter" is conceptually open: SSH-tunneled remote pokes, IPC pipes, webhooks, OS-level notification systems. Anything that takes a `wake_target` string and delivers a wake-up signal to the recipient. OS notifications (macOS/Linux) notify the **local human operator**, who then pokes the agent; they are best-effort human-relay adapters for non-tmux sessions.

### Multi-socket tmux

The reference tmux adapter parses simple targets (`%pane_id`, `session:window.pane`) and assumes the default tmux socket. Setups using `tmux -L <socket-name>` for multiple isolated tmux servers can extend the adapter's `wake_target` grammar — `wake_target` is opaque to agentchute; the adapter owns its format. Example grammar an extension MAY adopt:

```yaml
wake_method: tmux
wake_target: "L=admin;target=mysession:0.0"
```

The reference CLI does NOT parse this richer form in v0.1. Community-extended tmux adapters MAY.

### Cross-machine pools

When the shared filesystem spans multiple machines (NFS, SSHFS, etc.), agentchute's message delivery semantics still hold. Wake delivery becomes machine-local: a `tmux send-keys` from Machine A cannot reach Machine B's tmux server. Each machine in such a pool must supply its own wake mechanism — a local watchdog, peer cooperation among same-host agents, or recipient self-polling.

This is **in scope for v0.1**, not an extension. The §10.5 cooperative waking algorithm skips cross-host peers proactively when the peer's registration declares a `host` that differs from the local host.

Caveat: not every distributed filesystem preserves `rename(2)` atomicity across clients. NFSv3 in particular has well-known rename races. agentchute's correctness depends on the underlying filesystem honoring atomic create/rename — deploy on a substrate that does.

### Cross-folder enrollment via the `.agentchute-control-repo` pointer file

When agents live in different folders that belong to one logical project (e.g., separate repos sharing a parent dir), each non-control folder MAY drop a tracked `.agentchute-control-repo` pointer file at its root. The file contains one non-comment path line pointing at the project's canonical control repo. The reference CLI discovers it on cwd-ancestor walk and resolves it during normal startup (see AGENTCHUTE.md §4.1 step 3 — the control-repo cascade). Sibling-repo pointers like `../coordination` are the primary case.

This is **in scope for v0.1**, not an extension. The pointer file parser, ancestor walk, sibling-repo escape, and origin-tracking display (`agentchute status` shows `(via pointer:<path>)`) all ship with the reference CLI today. The `agentchute prepare-pool --target <folder>` command scaffolds the pointer file plus ENROLLMENT-rendered wrapper files in one or more sibling folders in a single atomic pass.

Caveat for portability: tracked pointer files SHOULD use relative paths so they survive across clones and machines with different mount points. Absolute paths are accepted but break the moment the project is cloned elsewhere.

---

## Adding your own wake adapter

The v0.1 extension model is **fork the binary, add your adapter source, rebuild**. The registry lives under Go's `internal/` visibility (so external packages can't import it directly); a plugin design that crosses module boundaries is out of v0.1 scope.

Shape of a community adapter inside a fork:

```go
package wezterm

import (
    "context"
    "os/exec"

    "github.com/agentchute/agentchute/internal/loop"
)

type adapter struct{}

func (adapter) Poke(ctx context.Context, target string) error {
    cmd := exec.CommandContext(ctx, "wezterm", "cli", "send-text", "--pane-id", target, "check\n")
    return cmd.Run()
}

func init() {
    loop.RegisterWakeAdapter("wezterm", adapter{})
}
```

Then import the package somewhere from `main` so the `init()` runs. Done.

Constraints any wake adapter MUST honor:

1. **No shell-eval on the target.** Use argv APIs and pass the target as a separate argument. Targets are external input.
2. **Idempotent skip on absent state.** If the underlying tool (`wezterm`, `kitty`, `osascript`) isn't installed or the target session is gone, return a clear error and let the caller log/warn-and-continue. Do NOT fail the whole sweep.
3. **Best-effort only.** The wake adapter delivers a notification; durable delivery is the message in the recipient's inbox (in the v0.1 reference CLI, that's a file on the shared filesystem).
4. **Inspect nothing else.** Adapters MUST NOT read recipient inbox bodies, archive files, or any other registration's data. The wake adapter's authority is one `Poke(ctx, target)` call.

---

## What is not extension space in v0.1

These are either excluded by the protocol's design, or reserved for a future protocol version. Either way, they're not v0.1 extension space — don't ship an adapter or fork that claims to add them under the agentchute name.

- **Routing / role-assignment / wildcard inboxes.** §7 / §12. Agents are peers; senders pick recipients explicitly.
- **Protocol-level signing or auth.** §12. agentchute is cooperative-trust. Layer a signed-envelope protocol above if you need it.
- **Durable / authenticated audit trail.** Archive is gitignored. Use a layered protocol for durable transcripts.
- **Structured request/response state machine.** Fire-and-forget. References via `in_reply_to` are non-normative.
- **Coordinator agents.** Reserved for v2 (see AGENTCHUTE.md §13). v1 watchdog is liveness-only — anything beyond that waits for the v2 spec, not a v0.1 extension.

---

## Why these aren't in v0.1

Shipping the smallest thing that works is the value prop. Extensions are an invitation, not a roadmap promise. Every adapter we don't ship is one less dependency, one less validation matrix, and one less interpretation of "what agentchute is." The protocol is the product; the binary is convenience.

If you build something useful from this document, open a PR or just ship your fork. The protocol is portable on purpose.
