# AGENTCHUTE.md

*Open spec for inbox-based agent coordination. Protocol working draft v1 (pull-only / protocol-v2); reference CLI implementation.*

> **Executable spec.** The normative invariants below are encoded as a runnable
> conformance suite in [`conformance/`](conformance/) — seven invariants
> (`R1`/`D1`/`D2`/`O1`/`C1`/`E1`/`B1`) driven against two substrate bindings
> (private inbox dir + shared log). Any substrate that passes the suite is
> conformant. When prose and the suite disagree, the suite wins.

---

## 1. Purpose

A small convention for two or more agents (humans, AI assistants, or both) to coordinate through **shared inboxes**. Designed for explicit handoffs: agent X writes a message into agent Y's inbox; Y picks it up on its own cadence.

Coordination is **pull-only**. A sender's sole responsibility is durable delivery — write the file. A sender NEVER pokes or wakes a recipient. Each recipient discovers its own mail by polling, and a wrapper that has no native polling loop is launched under the reference CLI's **runner** (`agentchute run`), a per-agent PTY supervisor that polls the agent's own inbox and injects a `check inbox` cue into the child when new mail arrives. This is a correctness choice, not a simplicity one: parent-child supervision is ground truth, whereas a published wake target (a socket, a tmux pane, a reachability cache) goes stale and lies. The previous push apparatus (watchdog, sender-side wake, wake adapters, reachability cache) is **deleted**.

### Protocol primitives (implementation-agnostic)

The protocol is a small set of implementation-agnostic primitives. Conforming implementations are free to choose any inbox medium and any transport between sender and inbox — those are outside the protocol.

- **Per-recipient inbox.** Each agent has its own ordered message stream. Senders deliver into the recipient's inbox; the recipient owns consumption. **The inbox medium is implementation-specific** (filesystem, queue, HTTP, git branch, etc.).
- **Identity = the committed delivery key `(to, from, seq)`.** `seq` is a durable, monotonic, per-`(sender, recipient)` sequence number (§6.1). It is the sort key and the identity; there is no sender-asserted `message_id` and no random nonce in the identity. A plain per-sender sort yields exact FIFO with no clock.
- **No-overwrite delivery.** Delivery never silently clobbers an existing entry. Committing the same `(to, from, seq)` twice is a benign no-op ("this exact message already landed"), which makes a crash-uncertain resend safe. **Transport is implementation-specific** (atomic link, HTTP POST, git push, etc.).
- **Recipient-owned, two-phase consumption.** Only the recipient reads its own message bodies. Consumption is **act-then-archive** (at-least-once): claim → act → commit. A crash mid-consume re-delivers; handlers MUST be idempotent.
- **Presence is a published fact with freshness**, not a wake target and not a read cursor. Each agent writes a `.live` file on each heartbeat; fresh ⇒ alive, stale/absent ⇒ not-alive (§9).
- **Asker-owned reply obligations.** "I am owed a reply to `(to,from,seq)` by `<T>`" is held in the asker's own ledger and cleared when the matching reply is consumed (§6.6).
- **Self-registration.** Each agent publishes a small record naming itself plus operational metadata. No wake fields.

### Reference implementation

The reference CLI maps these primitives onto local filesystem choices on a shared filesystem:
- **Inbox medium**: `.md` files under a fixed loop directory (`.agentchute/loop/inbox/<id>/`).
- **Transport**: unique-temp + atomic `link()`-no-clobber (NFS-safe; `EEXIST` = already-delivered).
- **Wake**: none on the wire. A loopless wrapper is supervised by `agentchute run`, which injects `[agentchute:run] check inbox` into the child's PTY when its OWN inbox poll sees new mail. The runner is local to the agent it supervises; it is not a sender-reachable endpoint.

These are reference choices, not protocol requirements. Conforming implementations can swap the inbox medium and transport (see [`EXTENSIONS.md`](EXTENSIONS.md) and the alternate `log` binding in [`conformance/`](conformance/)) as long as no-overwrite per-recipient delivery and the seven invariants hold.

## 2. Scope

### In scope (v1)
- **Pull-only inbox coordination** through per-recipient inboxes (§6).
- **Per-agent supervision.** A loopless wrapper runs under `agentchute run` (PTY supervisor) for inbox polling and `check inbox` injection. No sender-side wake.
- **Small shared-FS pool.** 2 to ~10 agents sharing one filesystem (single host, or multi-host over a shared/network mount). Beyond that, routing/role-election is required (v2).
- **Substrate-defined pool locator.** _Reference CLI: a repo containing `AGENTCHUTE.md` and a `.agentchute/loop` directory._
- **Free-form messages with optional structured envelope** (§6.4).
- **`.live` presence with freshness** (§9) and **asker-owned reply obligations** (§6.6).

### Out of scope (v1)
See **§12 Non-goals**. Exclusions: non-filesystem transports in the reference CLI, sender-side wake / push presence, durable audit trails, capability-based routing, and cryptographic signing.

### Concurrency and Access
agentchute is **concurrency-agnostic**: it neither enforces nor prevents simultaneous work. The expected default is linear (one agent at a time per task). Agents MUST have read/write access to their configured inbox medium. **One live process owns an id at a time** — the reference CLI enforces this with a serve lease + fencing token (§5.4).

## 3. Layout (filesystem reference implementation)

Coordination state lives under a fixed dotdir at the repo root:

```
<repo-root>/
  AGENTCHUTE.md                    # this spec (tracked)
  .agentchute/loop/
    agents/                        # registrations (README.md tracked, *.md gitignored)
    inbox/<agent-id>/              # per-recipient inbox (gitignored)
    inbox/<agent-id>/.claimed/     # phase-1 CLAIMED, not-yet-committed messages
    live/<agent-id>.live           # presence fact (last_seen + advisory busy)
    archive/                       # consumed (committed) messages (gitignored)
    malformed/                     # quarantined files (gitignored)
    state/<agent-id>/              # recipient/asker-owned runtime state (gitignored)
      owed.json                    # asker-owned reply obligations (§6.6)
      seq/<recipient>.json         # durable per-(sender,recipient) seq counter
      serve.claim                  # serve lease + fencing token (§5.4)
      pending-replies.json         # recipient-side reply ledger (compat; still blocks)
```

The namespace is fixed at `.agentchute/loop` (no vendor-namespaced dotdir). `AGENTCHUTE.md` is the only file that MUST be tracked. The `live/` directory is the only public presence surface; `state/<id>/` is owner-private (peers never read another agent's state dir).

## 4. Discovery (filesystem reference implementation)

The reference CLI resolves two distinct paths:

### 4.1 Control repo cascade
1. **`--control-repo <path>` flag.**
2. **`AGENTCHUTE_CONTROL_REPO` env var.**
3. **`.agentchute-control-repo` pointer file.** Walk from cwd up to root. Nearer pointer wins. This is the reference mechanism for worktree or sibling-folder participants that share one central control repo.
4. **Cwd walk.** Walk up to root looking for `AGENTCHUTE.md` + the fixed `.agentchute/loop` directory.

### 4.2 Loop dir cascade
1. **`--loop-dir <path>` flag.**
2. **`AGENTCHUTE_LOOP_DIR` env var.**
3. **Auto-discover.** The fixed `.agentchute/loop` directory under the control repo root.

## 5. Registration

Every agent MUST publish a registration record so peers can discover it and so presence/liveness reads have an enrolled identity. Registration carries **no wake fields** — there is nothing to poke.

### 5.1 Protocol-level registration fields
- `agent_id` (string, required): `^[a-z0-9][a-z0-9-]*$`.
- `vendor` (string, recommended): anthropic, openai, google, human, etc. (advisory).
- `host` (string, recommended): advisory hostname for same-host correlation.
- `last_seen` (RFC 3339 UTC, required): updated at turn start. **Presence/freshness, however, is read from `.live` (§9), not from this field.**
- `status` (enum, optional): `active | exhausted | offline`.
- `restart_at` (RFC 3339 UTC, optional): earliest future eligibility (advisory).
- `last_active` (RFC 3339 UTC, optional): last successful inbox consumption.
- `launched_by` / `shim_name` / `hook_event` (string, optional): advisory launch provenance (e.g. `runner`, `hook`, `manual`) for truthful verify views. Diagnostic only; never gate any decision. Absent = unknown/legacy; a re-register that omits them PRESERVES the prior value.

There are **no** `wake_method` / `wake_target` / `reachable_at` / `reachability_*` / `wake_endpoints` fields. Pull-only coordination has no published wake endpoint, so the entire wake/reachability cluster is removed.

The Markdown body is optional advisory prose. Do not route on capabilities (§12).

### 5.2 Reference mechanics (filesystem)
Encoded as YAML frontmatter in `.agentchute/loop/agents/<agent_id>.md`.
- `control_repo` (path, required): absolute path to the control repo.
- `working_repos` (list, optional): additional relevant repo paths.

See **Appendix C** for a hand-registration walkthrough.

### 5.3 Enforced enrollment
Implementations MUST refuse active operations (consume/send/gate) if the agent's registration is absent or unreadable. Self-registration is mandatory on every session start (§7.2).

### 5.4 Id-uniqueness — the serve lease + fencing token
The per-`(sender, recipient)` `seq` design is only sound if **one live process owns an id at a time**; a second live writer under the same id would break per-writer sequencing. The reference CLI enforces this with a decentralized shared-FS lease, not a central allocator:

- The runner acquires a **serve lease** at launch — a `state/<id>/serve.claim` carrying `{id, host, pid, serve_token, started_at, last_seen}`, committed via `link()`-no-clobber. A **fresh** valid claim makes a second launch of the same id **fail closed**.
- A **stale** claim is reclaimable only via the liveness rule: stale past the lease timeout, **plus** a same-host pid-proof failure when the holder is same-host (a frozen-but-alive process keeps its id); cross-host reclaim uses freshness/timeout only (pid is not provable across hosts).
- The **fencing token** (`serve_token`, a 128-bit random epoch) is the load-bearing part: every heartbeat and **every `seq` write verifies it**. A zombie/paused holder that resumes after its lease was reclaimed fails the token check and stops — so it cannot create a dup-writer even though launch was guarded. The runner passes its token to the child via `AGENTCHUTE_SERVE_TOKEN`, so a fenced (reclaimed) child's `send` fails closed too.

This makes "give each process its own id" an enforced, fenced invariant rather than a convention. (Operational assumptions: the lease state lives on the same shared mount as the inboxes, and clocks are NTP-loose with `lease-timeout ≫ heartbeat + max-skew`. Severe skew degrades to premature/delayed reclaim, but the fence still prevents an actual dup-write.)

## 6. Messaging

### 6.1 Message identity & ordering
The committed identity is the full delivery key `(to, from, seq)`:
- **`to`**: the recipient — encoded by LOCATION (which inbox the message lands in), so it is not spelled in the filename.
- **`from`**: sender `agent_id`.
- **`seq`**: a durable, monotonic, per-`(from, to)` sequence number starting at 1.

The reference encoding is the canonical filename `from-<from>_seq-<020d>.md` (`seq` zero-padded to 20 digits). A plain lexicographic sort of one sender's files is therefore **exact per-sender FIFO with no clock**. Cross-sender order is **advisory arrival order** (non-normative) — the protocol does not promise a global total order (a real total order would need a freshness CAS the mount can't cheaply give, and is unneeded).

`seq` is **write-ahead durable**: the counter is committed *before* the message links, so a crash can only ever produce a GAP (an allocated seq whose message never landed), never a reuse for different content. Gaps are legal — `seq` is identity + sort key, not a no-gap contract.

> **One-release compatibility (dual-read drain).** Inbox listing still READS the legacy nonce format `<ts>_from-<sender>_msg-<nonce>.md` alongside the canonical seq format, so residue written before the cutover still drains. Legacy names sort before seq names (digit vs. `f`), which is correct across the cutover. `gate`/`doctor` surface a non-blocking gauge of remaining legacy-named messages; the legacy reader is removable only once every live inbox reports zero.

### 6.2 Sender flow
1. Compose body (UTF-8).
2. Allocate the next durable `seq` for `(from, to)` (write-ahead). The active serve token, if any, is fence-verified first.
3. **Deliver into the inbox with the no-overwrite guarantee** under the canonical `(to, from, seq)` name (unique-temp + atomic `link()`; `EEXIST` = this exact message already landed = success).
4. **No wake.** The sender does not poke or signal the recipient. (`send` retains a wake-receipt field in its output only for output-shape stability; it always reports no wake.)

### 6.3 Recipient flow — two-phase consume (act-then-archive)
Consumption is at-least-once and split across two verbs:

1. Update own `last_seen` and `.live`.
2. Enumerate and sort inbox messages (per-sender FIFO).
3. **`check` — phase 1 (CLAIM + display).** First re-display any uncommitted `.claimed` residue from a crashed/un-acked prior turn, each tagged with a **`REDELIVERED`** banner. Then, for each new message: validate envelope/body (quarantine if malformed, §11), CLAIM it (move `inbox/<id>/<name>` → `inbox/<id>/.claimed/<name>` under its canonical name), and display it. `check` does **not** archive.
4. **Act on each displayed message.** Because the CLI prints and exits before the model acts, archiving during `check` would be at-most-once for the *work*; claiming instead makes the work at-least-once. **Handlers MUST be idempotent** — a crash between `check` and `ack` re-delivers.
5. **`ack` — phase 2 (COMMIT).** Archive the `.claimed` residue. Archiving is the single commit point; an already-archived message is a benign no-op (idempotent).

The reference CLI's Stop hook runs `ack` (commit the prior turn's work) and then the read-only finish gate. A same-`(to,from,seq)` resend that re-lands while the original is already claimed is dropped as a benign duplicate.

### 6.4 Message envelope
Encoded as optional YAML frontmatter. The **normative** envelope is small:
- `from` (required): sender `agent_id`.
- `reply_required` (boolean, optional): an **advisory hint** that the sender wants a reply. The binding reply obligation is the asker's own `.owed` ledger (§6.6); `reply_required` stays on the wire as the one cross-agent coordination hint.
- `in_reply_to` (optional): the canonical reference `to-<to>_from-<from>_seq-<020d>` of the message being answered. Consuming a reply whose `in_reply_to` matches one of the asker's outstanding `.owed` entries discharges that obligation.
- `idempotency_key` (optional): logical dedup hint only (never an identity).

**Compatibility fields (one release, accepted but de-emphasized):** `message_id` is still EMITTED by the reference CLI's `send` as a compat field, but it is NOT the identity (`(to,from,seq)` is). `to`, `task`, and `status` are accepted if present but are no longer part of the normative envelope (`to` is encoded by location; `task`/`status` are body conventions).

### 6.5 Forward compatibility
Receivers MUST ignore unrecognized frontmatter fields. `from` is required. A breaking change is signaled by a `v:` bump on registration. Messages MUST be valid UTF-8. The reference CLI accepts up to 4 MiB per message.

### 6.6 Reply obligations (asker-owned)
Reply obligations are **owned by the asker**, not the recipient:

- When an agent sends `--ask` (reply-required), it records its own obligation in `state/<asker>/owed.json`: "I am owed a reply to `(to=recipient, from=me, seq)` by `<deadline>`" (default 30m; override with `--reply-by`). The ledger is single-writer, atomic-rename, and the gate reads only its OWN ledger — it never scans peers.
- When the asker later consumes a reply whose `in_reply_to` references that `(to,from,seq)`, the obligation is cleared (idempotent).
- The asker's gate surfaces **outstanding** and **expired** obligations as **non-blocking warnings**. An expired obligation is the asker-side dead-recipient signal: a dead recipient shows up twice over — the asker's expired `.owed` AND the recipient's stale `.live` — so the gate never deadlocks on a corpse.

> **One-release compatibility (legacy recipient ledger still blocks).** The recipient-side `pending-replies.json` ledger is **legacy compat**: any entries *already on it* still block the recipient's finish gate for one release (until replied or deferred). But `check` no longer records a recipient-side obligation on consume — consuming a `reply_required` message records nothing on the recipient and merely **prints the reply-ref command** (`reply_required` is advisory on the wire; the binding obligation is the asker's `.owed`, §6.4/§6.6 above). So NEW `reply_required` consumes create no recipient-side blocker; only pre-existing ledger entries do. Making `.owed` the sole authority (and dropping the legacy recipient ledger) is a deferred follow-up.

## 7. Coordination defaults

### 7.1 Direct addressing only
Messages are sent to specific recipients. No broadcast or self-claiming.

### 7.2 Inbox-only authority to mutate
An agent's authority to mutate project state starts when an inbox message arrives, or when explicitly instructed by a human. Reading files does NOT authorize work.

**Protocol maintenance is pre-authorized.** Mandatory on every session start without waiting for instruction:
- **Self-registration (§5)**: `agentchute boot` / `register` is mandatory and idempotent. It reconciles `host` and `cwd`. Existing files are not sufficient.
- **Self-state updates**: `last_seen` and `.live` (turn start / heartbeat), `last_active` (post-consumption), `status`/`restart_at` (budget visibility).
- **Own scaffold & inbox operations**: creating `inbox/<self>/`, `archive/`, `malformed/`; claiming, acting on, and acking own mail.
- **Protocol correction (§11).**

There is no cooperative-waking step: coordination is pull-only, so an agent never pokes a peer.

Everything else (project edits, unsolicited peer messages, side-effecting commands) is gated by the authority rule.

### 7.3 Identity and Bridges
Identity is pool-scoped: `(pool_locator, agent_id)`. A physical process participating in multiple pools is a **bridge**. Bridges MUST NOT assume transitive authority or automate cross-pool forwarding without explicit policy. See [`EXTENSIONS.md`](EXTENSIONS.md) for bridge hazards.

## 8. Wake / supervision (reference implementation)

There is **no wake on the wire** and no sender-side poke. Discovery is recipient-side polling; the only question is what drives a given wrapper's poll.

- **Native-loop wrappers** poll their own inbox on their own cadence (or at lifecycle boundaries via hooks).
- **Loopless wrappers** run under the **runner** — `agentchute run -- <wrapper>` — a per-agent PTY supervisor. It launches the child under a PTY, acquires the serve lease (§5.4), polls the agent's OWN inbox each tick, writes `.live`, and injects `[agentchute:run] check inbox` into the child's PTY when new mail appears (respecting an idle/injection window so it doesn't interrupt mid-turn). It uses per-vendor submit bytes (e.g. bracketed-paste + enhanced-enter for codex) so the cue actually submits.

The leading bracket in the injected cue is machine metadata; the model-facing instruction is `check inbox`. `setup --wake` installs the runner path only; the former tmux/herdr wake adapters and the runner receive-socket were removed in the pull-only redesign.

## 9. Liveness & presence

Presence is a **published fact with freshness**, written to `live/<id>.live` (`{id, last_seen, busy, pid, host}`) on every heartbeat via an atomic tmp+rename:
- A `.live` newer than the freshness window ⇒ **alive**.
- A stale or absent `.live` ⇒ **not-alive** (never an error — an unregistered or long-gone agent simply reads not-alive). This is the dead-mailbox detection: "came back days later, one agent never returned" surfaces as a stale `.live`.
- `busy` is **advisory only** and NEVER affects aliveness — this deliberately avoids the false-dead direction (a busy agent mid-long-turn must not read as dead).

`gate`/`doctor`/`status` read presence from `.live`, not from registration `last_seen`. There is **no watchdog and no cross-agent liveness tracking** — both were deleted as push machinery. A pull-only pool needs neither: a sender doesn't care whether the recipient is live (the message waits in the inbox), and an asker learns of a dead recipient from its own expired `.owed` plus the recipient's stale `.live`.

## 10. (removed) Watchdog

The liveness-only watchdog and its cooperative-waking step are **removed**. Cross-agent liveness pushing was unreliable (stale caches, watchdog races, gates on phantom liveness); pull-only coordination + `.live` presence (§9) replaces it. This section is retained as a pointer so older references resolve.

## 11. Protocol correction (best effort)

Every agent participates in keeping the pool healthy.

### 11.1 Enforcement action
Triggers include malformed inbox filenames, unparseable frontmatter, or unparseable peer registrations.
1. **Quarantine**: atomic move to `.agentchute/loop/malformed/`.
2. **Notify offender**: send a corrective message (Task: `protocol correction`, Status: `findings`) to the inferred sender. Sender inference order: filename capture → frontmatter `from:` → no notify.
3. **Continue**: do NOT block the sender or the turn.

A well-formed canonical seq file is never quarantined (the dual-read lister recognizes both formats); only a genuinely-unrecognized name is enforced on.

## 12. Non-goals (v1)
- No non-filesystem transport in the reference CLI.
- No sender-side wake / push presence / reachability cache.
- No durable/authenticated audit trail (archive is gitignored; default off).
- No capability-based routing.
- No protocol-level signing or auth.
- No coordinator/router agents and no cross-agent liveness tracking.

## 13. v2 deferred items
- Coordinator/router agents.
- Opt-in transcript export / shared-log audit profile (the `log` binding in [`conformance/`](conformance/) is the first-class opt-in profile).
- Handshake / version negotiation beyond the registration `v:` field.

## 14. Namespace
State lives under the fixed `.agentchute/loop` directory. `AGENTCHUTE.md` is shared; reference-implementation notes live in `.agentchute/loop/README.md`. (Earlier drafts used a vendor-namespaced `.<vendor>/loop/` dotdir and a `.rehumanlabs/` legacy namespace; both are gone — the namespace is now fixed. `reHuman Labs` remains the maker's credit in `README.md`; that's brand, not a namespace.)

---

## Appendix B. Reference implementation hook templates

See `README.md` or `examples/hooks/` for current Claude Code, Codex, and Gemini CLI hook templates.

## Appendix C. Hand-protocol walkthrough

For agents without the reference CLI binary. (The CLI enforces a durable `seq` counter and a serve lease; a hand-protocol agent SHOULD coordinate to keep a single writer per id and a monotonic per-`(from,to)` counter.)

### C.1 Registration
Write `.agentchute/loop/agents/<id>.md`:
```markdown
---
agent_id: claude-code
vendor: anthropic
control_repo: /absolute/path/to/repo
host: macbook.local
last_seen: 2026-06-30T00:00:00Z
status: active
---
# free-text notes
```

### C.2 Sending a message
1. **Allocate seq**: read/advance your durable per-`(from,to)` counter (write-ahead). `seq=$((last_issued + 1))`.
2. **Filename**: `name="from-${from}_seq-$(printf '%020d' "$seq").md"`.
3. **Compose**: write Markdown + frontmatter (`from:`, optionally `reply_required: true`, `in_reply_to: to-<to>_from-<from>_seq-<020d>`) to a unique temp file.
4. **Deliver**: `ln <tmp> .agentchute/loop/inbox/<to>/<name>` (no-overwrite; `EEXIST` = already landed = done), then `rm <tmp>`.
5. **No poke.** Delivery is the whole job.

### C.3 Consuming inbox (act-then-archive)
1. Update `last_seen` and write `live/<id>.live`.
2. List `inbox/<id>/` oldest-first by filename. Also re-handle any residue in `inbox/<id>/.claimed/` (uncommitted from a prior crash).
3. **Claim**: `mv inbox/<id>/<file> inbox/<id>/.claimed/<file>` (same canonical name).
4. **Act** on the claimed content. Make handlers idempotent (a crash before commit re-delivers).
5. **Commit**: `mv inbox/<id>/.claimed/<file> ../archive/<consumed-ts>_to-<id>_<file>`.
6. If the message replied to one of your `--ask` obligations, clear the matching entry in `state/<id>/owed.json`.
