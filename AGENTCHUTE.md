# AGENTCHUTE.md

*Open spec for inbox-based agent coordination. Protocol working draft v1; reference CLI implementation.*

---

## 1. Purpose

A small convention for two or more agents (humans, AI assistants, or both) to coordinate through **shared inboxes**. Designed for explicit handoffs: agent X writes a message into agent Y's inbox; if Y declared a reachable `wake_method` and `wake_target`, X also pokes Y via the corresponding wake adapter for direct wake-up. Without a reachable wake method, Y picks up the message via its own polling cadence (wrapper loop, watchdog, or manual `check`).

### Protocol primitives (implementation-agnostic)

The protocol is a small set of implementation-agnostic primitives. Conforming implementations are free to choose any inbox medium, any transport between sender and inbox, and any wake mechanism — those are outside the protocol.

- **Per-recipient inbox.** Each agent has its own ordered message stream. Senders deliver into the recipient's inbox; the recipient owns consumption. **The inbox medium is implementation-specific** (filesystem, queue, HTTP, git branch, etc.).
- **Ordered, identified messages.** Each carries a unique `(timestamp, sender, nonce)` identity tuple (§6.1.1). Receivers MUST process entries oldest-first by timestamp with deterministic tie-breaking.
- **No-overwrite delivery.** Delivery never silently clobbers an existing entry. If identities collide, the sender retries with a fresh nonce. **Transport is implementation-specific** (atomic rename, HTTP POST, git push, etc.).
- **Recipient-owned consumption.** Only the recipient reads its own message bodies. Liveness checks use inbox metadata only.
- **Optional wake.** Senders MAY signal the recipient via a pluggable wake adapter. Wake is a latency hint; recipient-side polling is the correctness mechanism (§8.2). **The wake mechanism is implementation-specific.**
- **Self-registration.** Each agent publishes a record naming itself, its wake method/target (if any), and operational metadata.

### Reference implementation

The reference CLI maps these primitives onto local filesystem choices:
- **Inbox medium**: `.md` files under a vendor loop directory (`.<vendor>/loop/inbox/`).
- **Transport**: atomic create-temp + rename.
- **Wake**: `tmux send-keys` injection of `[agentchute:tmux] check inbox` (peer-to-peer) or `agentchute-run` (PTY runner injection).

These are reference choices, not protocol requirements. Conforming implementations can swap any axis (see [`EXTENSIONS.md`](EXTENSIONS.md) for git-backed and HTTP-based sketches).

## 2. Scope

### In scope (v1)
- **Inbox-based coordination** through per-recipient inboxes (§6.1.1).
- **Pluggable wake adapters.** Senders look up `wake_method` and dispatch the poke. Non-pokable agents participate via polling.
- **Small pools.** 2 to ~10 agents. Beyond that, routing/role-election is required (v2).
- **Substrate-defined pool locator.** _Reference CLI: a repo containing `AGENTCHUTE.md` and a loop directory._
- **Free-form messages with optional structured envelope** (§6.4.1).
- **Liveness-only watchdog** (§10).

### Out of scope (v1)
See **§12 Non-goals**. Exclusions: non-filesystem transports in the reference CLI, durable audit trails, capability-based routing, and cryptographic signing.

### Concurrency and Access
agentchute is **concurrency-agnostic**: it neither enforces nor prevents simultaneous work. The expected default is linear (one agent at a time per task). Agents MUST have read/write access to their configured inbox medium.

## 3. Layout (filesystem reference implementation)

Coordination state lives under a vendor-namespaced dotdir at the repo root:

```
<repo-root>/
  AGENTCHUTE.md                    # this spec (tracked)
  .<vendor>/loop/                  # e.g., .agentchute/loop/
    agents/                        # registrations (README.md tracked, *.md gitignored)
    inbox/<agent-id>/              # per-recipient inbox (gitignored)
    archive/                       # consumed messages (gitignored)
    malformed/                     # quarantined files (gitignored)
    state/<agent-id>/              # recipient-owned runtime state (gitignored)
```

The vendor prefix (e.g., `.agentchute/`, `.examplecorp/`) anchors the namespace to a specific domain. `AGENTCHUTE.md` is the only file that MUST be tracked.

## 4. Discovery (filesystem reference implementation)

The reference CLI resolves two distinct paths:

### 4.1 Control repo cascade
1. **`--control-repo <path>` flag.**
2. **`AGENTCHUTE_CONTROL_REPO` env var.**
3. **`.agentchute-control-repo` pointer file.** Walk from cwd up to root. Nearer pointer wins. This is the reference mechanism for worktree or sibling-folder participants that share one central control repo.
4. **Cwd walk.** Walk up to root looking for `AGENTCHUTE.md` + a loop directory.

### 4.2 Loop dir cascade
1. **`--loop-dir <path>` flag.**
2. **`AGENTCHUTE_LOOP_DIR` env var.**
3. **Auto-discover.** Exactly one match for `.<vendor>/loop/` at the control repo root.

## 5. Registration

Every agent MUST publish a registration record for discovery and waking.

### 5.1 Protocol-level registration fields
- `agent_id` (string, required): `^[a-z0-9][a-z0-9-]*$`.
- `vendor` (string, required): anthropic, openai, google, human, etc.
- `host` (string, recommended): advisory hostname for wake-locality checks.
- `wake_method` (string, conditional): adapter name (e.g., `tmux`, `herdr`, `agentchute-run`).
- `wake_target` (string, conditional): adapter-specific address (e.g., `%0`).
- `last_seen` (RFC 3339 UTC, required): updated at turn start.
- `status` (enum, optional): `active | exhausted | offline`.
- `restart_at` (RFC 3339 UTC, optional): earliest future poke eligibility.
- `last_active` (RFC 3339 UTC, optional): last successful inbox consumption.

The Markdown body is optional advisory prose. Do not route on capabilities (§12).

### 5.2 Reference mechanics (filesystem)
Encoded as YAML frontmatter in `.<vendor>/loop/agents/<agent_id>.md`.
- `control_repo` (path, required): absolute path to the control repo.
- `working_repos` (list, optional): additional relevant repo paths.

See **Appendix C** for a hand-registration walkthrough.

### 5.3 Enforced enrollment
Implementations MUST refuse active operations (consume/send/gate) if the agent's registration is absent or unreadable. Mandatory on every session start (§7.2).

## 6. Messaging

### 6.1 Message identity
Every message carries a `(timestamp, sender, nonce)` identity tuple.
- **Timestamp**: UTC microsecond precision.
- **Sender**: `agent_id`.
- **Nonce**: 4-character hex suffix for microsecond disambiguation.

Inbox messages MUST be presented oldest-first by timestamp. The reference encoding uses the filename: `<timestamp>_from-<sender>_msg-<nonce>.md`. See **Appendix C** for filename construction.

### 6.2 Sender flow
1. Compose body (UTF-8).
2. Generate identity tuple.
3. **Deliver into inbox with no-overwrite guarantee.** Retry nonce on collision.
4. Signal via `wake_method` / `wake_target` if reachable (§8.2).

### 6.3 Recipient flow
1. Update `last_seen` / `restart_at` in own registration.
2. Enumerate and sort inbox messages oldest-first.
3. For each message:
   a. Validate envelope/body. Reject (quarantine) if malformed (§11).
   b. Consistently archive or consume to remove from the live stream.
   c. Act on message.
4. Update `last_active`.
5. Perform cooperative waking for peers (§10.5).

### 6.4 Message envelope
Encoded as optional YAML frontmatter (§6.4.2):
- `message_id`: Sender-provided reference handle (recommended RFC 3339 UTC).
- `from` / `to`: agent_id.
- `in_reply_to`: parent `message_id`.
- `reply_required` (boolean): when true, places a **reply obligation** (§6.4.3).
- `task`: short descriptor.
- `status`: `request | findings | signoff | request-changes | info`.

### 6.5 Forward compatibility
Receivers MUST ignore unrecognized frontmatter fields. Senders MUST NOT redefine v1 fields. Messages MUST be valid UTF-8. The reference CLI accepts up to 4 MiB per message.

## 7. Coordination defaults

### 7.1 Direct addressing only
Messages are sent to specific recipients. No broadcast or self-claiming.

### 7.2 Inbox-only authority to mutate
An agent's authority to mutate project state starts when an inbox message arrives, or when explicitly instructed by a human. Reading files does NOT authorize work.

**Protocol maintenance is pre-authorized.** Mandatory on every session start without waiting for instruction:
- **Self-registration (§5)**: `agentchute boot` / `register` is mandatory and idempotent. It reconciles `host`, `wake_target`, and `cwd`. Existing files are not sufficient.
- **Self-state updates**: `last_seen` (turn start), `last_active` (post-consumption), `status` and `restart_at` (budget visibility).
- **Same-host tmux cleanup**: The reference CLI MAY remove peer registrations if: peer is not self, host matches, `wake_method: tmux`, and the pane is unreachable.
- **Own scaffold & inbox operations**: Creating `inbox/<self>/`, `archive/`, `malformed/`. Consuming own mail.
- **Cooperative waking (§10.5)** and protocol correction (§11).

Everything else (project edits, unsolicited peer messages, side-effecting commands) is gated by the authority rule.

### 7.3 Identity and Bridges
Identity is pool-scoped: `(pool_locator, agent_id)`. A physical process participating in multiple pools is a **bridge**. Bridges MUST NOT assume transitive authority or automate cross-pool forwarding without explicit policy. See [`EXTENSIONS.md`](EXTENSIONS.md) for bridge hazards.

## 8. Wake adapters

Wake pokes are latency hints; recipient-side polling is the correctness baseline (§8.2). The reference adapters are `tmux` and `herdr`.

### 8.1 Reference wake patterns
- **tmux**: `tmux send-keys -t <wake_target> '[agentchute:tmux] check inbox'` followed by `Enter`.
- **herdr**: `herdr agent send <wake_target> '[agentchute:herdr] check inbox\r'`.

The leading bracket is machine metadata; the model-facing instruction is `check inbox`.

### 8.2 Wake responsibility
The protocol's discovery mechanism is **recipient-side polling**. Senders are responsible for durable delivery; recipients are responsible for reading. Senders MAY attempt wake; failure is ignored.

No-tmux environments follow the five-tier polling model described in `README.md` (runner shims, poller fallbacks, native loops, preflighted schedulers, and finish hooks). Heartbeat-only pollers prove the inbox medium is visible but MUST NOT consume mail or launch wrappers unless explicitly configured for autonomous launch. Active wrapper sessions prove liveness with `state/<agent>/session.json`. Always schedule the wrapper (which invokes the model), not a bare `agentchute check`.

## 9. Liveness

Agents update `last_seen` at turn start. For pools > 3 agents, use the watchdog (§10).

## 10. Watchdog

The watchdog is a liveness-only latency accelerator. It monitors agent inboxes and pokes stale recipients.

### 10.1 Watchdog algorithm
1. Update own `last_seen`.
2. Enumerate peer registrations.
3. For each agent:
   a. If unread message age > `POKE_THRESHOLD` (90s) AND `last_seen` > `FRESH_THRESHOLD` (5min):
   b. Check `status` and `restart_at` (skip if exhausted/offline/deferred).
   c. Poke via `wake_method`.
4. Sleep `POLL_CADENCE` (60s).

### 10.2 Cooperative waking
The reference CLI performs §10.1 during every `check` cycle. Peer inspection is **metadata-only** (filenames/timestamps). Agents MUST NOT open peer inbox bodies. If a peer's `host` differs from the local host, skip the poke (cross-host pokes are not reachable).

## 11. Protocol correction (best effort)

Every agent participates in keeping the pool healthy.

### 11.1 Enforcement action
Triggers include malformed inbox filenames, unparseable frontmatter, or unparseable peer registrations.
1. **Quarantine**: Atomic rename to `<loop>/malformed/`.
2. **Notify offender**: Send a corrective message (Task: `protocol correction`, Status: `findings`) to the inferred sender (§11.4).
3. **Continue**: Do NOT block the sender or the turn.

## 12. Non-goals (v1)
- No non-filesystem transport in the reference CLI.
- No durable/authenticated audit trail (archive is gitignored).
- No capability-based routing.
- No protocol-level signing or auth.
- No coordinator/router agents (liveness-only in v1).

## 13. v2 deferred items
- Coordinator/router agents.
- Opt-in transcript export.
- Remote notifications (Slack, webhooks).
- Handshake / version negotiation.

## 14. Vendor namespace conventions
Implementations namespace state under a vendor-owned identifier (e.g., `.agentchute/`, `.examplecorp/`). `AGENTCHUTE.md` is shared; vendor-specific notes live in `.<vendor>/loop/README.md`.

> **Legacy namespace migration.** The reference implementation previously used the `.rehumanlabs/` namespace (renamed to `.agentchute/` in v0.2.2). Because discovery (§4.2) auto-matches any `.<vendor>/loop/`, a leftover `.rehumanlabs/loop` makes discovery ambiguous. `agentchute setup`/`init` migrate it for the current control repo: rename when the canonical namespace is absent, move a scaffold-only legacy loop aside, or promote legacy state over an empty canonical loop. If **both** hold live state it refuses and asks the operator to consolidate by hand — never an automatic merge. (`reHuman Labs` remains the maker's credit in `README.md`; that's brand, not a namespace.)

---

## Appendix B. Reference implementation hook templates

See `README.md` or `examples/hooks/` for current Claude Code, Codex, and Gemini CLI hook templates.

## Appendix C. Hand-protocol walkthrough

For agents without the reference CLI binary.

### C.1 Registration
Write `.<vendor>/loop/agents/<id>.md`:
```markdown
---
agent_id: claude-code
vendor: anthropic
control_repo: /absolute/path/to/repo
host: macbook.local
wake_method: tmux
wake_target: "%0"
last_seen: 2026-05-22T00:00:00Z
status: active
---
# free-text notes
```

### C.2 Sending a message
1. **Filename**: `ts=$(date -u +%Y-%m-%dT%H-%M-%S-000000Z)`, `nonce=$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')`.
2. **Compose**: Write Markdown + frontmatter to `.tmp_<filename>`.
3. **Deliver**: `ln .tmp_<filename> <filename>` (ensures no-overwrite), then `rm .tmp_<filename>`.
4. **Poke**:
   - tmux: `tmux send-keys -t <target> '[agentchute:tmux] check inbox' Enter`
   - herdr: `herdr agent send <target> '[agentchute:herdr] check inbox\r'`.

### C.3 Checking inbox
1. Update `last_seen` in registration.
2. List inbox, sort oldest-first.
3. For each file: `mv <file> ../archive/<consumed-ts>_to-<id>_<file>`.
4. Process archived content.
