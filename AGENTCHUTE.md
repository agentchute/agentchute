# AGENTCHUTE.md
<!-- agentchute-spec v1 -->

*Open spec for inbox-based agent coordination. **Protocol v2 (pull-only) was declared final at CLI v1.0.0.** That declaration held for the primitives (§1), the envelope (§6.4), and the lifecycle guarantees (§6.3, §11.1) — none of those changed. It did **not** hold for the filename/identity grammar (§6.1): v2.5 deliberately replaces the per-`(sender,recipient)` sequence counter with a timestamp+random-suffix identity — a real wire break, the exact kind the 1.0 covenant said would only happen through Protocol v3. It ships as "2.5," not "3.0," on purpose; the reasoning for both the break and that naming choice is recorded in `docs/decisions/agentchute-v2.5-proposal.md` and on the project blog, not left implicit. The reference CLI writes integer `v: 3` on registration rows and renders that wire value as Protocol v2.5; the release history is recorded in [`CHANGELOG.md`](CHANGELOG.md). Conformance invariants remain covenants that change only through the versioned deprecation process in [`CONTRIBUTING.md`](CONTRIBUTING.md).*

> **Executable spec.** The normative invariants below are encoded as runnable,
> language-neutral conformance vectors in [`conformance/`](conformance/) — seven
> core invariants (`R1`/`D1`/`D2`/`O1`/`C1`/`E1`/`B1`), the malformed-quarantine
> vector (`Q1`, inbox-profile), the v2.5 wire-break vectors
> (`TS1`/`TS2`/`TS3`/`DR1`, `DR1` inbox-profile), and the frontmatter-grammar
> vectors (`FM1`/`FM2`), driven against two substrate bindings (private inbox
> dir + shared log). Any substrate that passes its applicable vectors is
> conformant. When prose and the suite disagree, the suite wins.

---

## 1. Purpose

A small convention for two or more agents (humans, AI assistants, or both) to coordinate through **shared inboxes**. Designed for explicit handoffs: agent X writes a message into agent Y's inbox; Y picks it up on its own cadence.

Coordination is **pull-only**. A sender's sole responsibility is durable delivery — write the file. A sender NEVER pokes or wakes a recipient. Each recipient discovers its own mail by polling, and a wrapper that has no native polling loop is launched under the reference CLI's **runner** (`agentchute serve`), a per-agent PTY supervisor that polls the agent's own inbox and injects a `check inbox` cue into the child when new mail arrives. This is a correctness choice, not a simplicity one: parent-child supervision is ground truth, whereas a published wake target (a socket, a tmux pane, a reachability cache) goes stale and lies. The previous push apparatus (watchdog, sender-side wake, wake adapters, reachability cache) is **deleted**.

### Protocol primitives (implementation-agnostic)

The protocol is a small set of implementation-agnostic primitives. Conforming implementations are free to choose any inbox medium and any transport between sender and inbox — those are outside the protocol.

- **Per-recipient inbox.** Each agent has its own ordered message stream. Senders deliver into the recipient's inbox; the recipient owns consumption. **The inbox medium is implementation-specific** (filesystem, queue, HTTP, git branch, etc.).
- **Identity = the committed delivery key `(to, from, timestamp, random-suffix)`.** `timestamp` is a fixed-width, microsecond-precision UTC stamp minted under a durable per-**sender** monotonic floor — never reissued at or before that sender's own last-issued stamp, even across a restart or clock regression (§6.1). `random-suffix` is a 128-bit value whose only job is making a same-instant mint from the same sender collision-free; it is not itself a load-bearing identity component. There is no sender-asserted `message_id` and no per-`(sender,recipient)` counter — the floor is per-sender, not per-pair. A plain per-sender sort of the timestamp is exact FIFO with no clock reads needed at consume time.
- **No-overwrite delivery.** Delivery never silently clobbers an existing entry: a collision at the committed identity is refused, never a silent dedup no-op — the sender retries under a fresh identity. Delivery is **at-most-once**; there is no sender-asserted idempotency key or delivery-side dedup backstop (the covenant is handler idempotency at consume, §6.3). **Transport is implementation-specific** (atomic link, HTTP POST, git push, etc.).
- **Recipient-owned, two-phase consumption.** Only the recipient reads its own message bodies. Consumption is **act-then-archive** (at-least-once): claim → act → commit. A crash mid-consume re-delivers; handlers MUST be idempotent.
- **Presence is a published fact with freshness, read directly from the registration row** — not a wake target, not a read cursor, and not a separate file. A registration row's own `last_seen` field IS the presence record: a live supervising process advances it every heartbeat tick under a fencing token; fresh ⇒ alive, stale (or the row absent) ⇒ not-alive (§9).
- **Asker-owned reply obligations.** "I am owed a reply to `<to,from,identity>` by `<T>`" is held in the asker's own ledger and cleared when a reply whose reference matches the outstanding entry is consumed (§6.6).
- **Self-registration.** Each agent publishes a small record naming itself plus operational metadata. No wake fields.

### Reference implementation

The reference CLI maps these primitives onto local filesystem choices on a shared filesystem:
- **Inbox medium**: `.md` files under a fixed loop directory (`.agentchute/loop/inbox/<id>/`).
- **Transport**: unique-temp + atomic `link()`-no-clobber (`EEXIST` = a collision — the sender retries under a fresh identity; delivery is at-most-once); or, for a remote lane, framed operations over SSH to the pool host (the hub); the substrate is unchanged.
- **Wake**: none on the wire. A loopless wrapper is supervised by `agentchute serve`, which injects `[agentchute] check inbox` into the child's PTY when its OWN inbox poll sees new mail. The runner is local to the agent it supervises; it is not a sender-reachable endpoint.

These are reference choices, not protocol requirements. Conforming implementations can swap the inbox medium and transport (see [`EXTENSIONS.md`](EXTENSIONS.md) and the alternate `log` binding in [`conformance/`](conformance/)) as long as no-overwrite per-recipient delivery and the applicable conformance vectors (see the executable-spec note above) hold.

## 2. Scope

### In scope
- **Pull-only inbox coordination** through per-recipient inboxes (§6).
- **Per-agent supervision.** A loopless wrapper runs under `agentchute serve` (PTY supervisor) for inbox polling and `check inbox` injection. No sender-side wake.
- **Small shared-FS pool.** 2 to ~10 agents sharing one filesystem pool — **single-host, and a hub pool (one pool host + SSH remote lanes, §13), are the supported, CI-tested configurations** ("Tested targets" below). A pool spanning hosts over a shared network mount is inherited compatibility, not a newly-endorsed target. Beyond ~10 agents, routing/role-election would be required regardless of host count (a future protocol major; a non-goal today, §12).
- **Substrate-defined pool locator.** _Reference CLI: a repo containing `AGENTCHUTE.md` and a `.agentchute/loop` directory._
- **Free-form messages with optional structured envelope** (§6.4).
- **Registration presence with freshness** (§9) and **asker-owned reply obligations** (§6.6).

### Out of scope
See **§12 Non-goals**. Exclusions: non-filesystem **state substrates** in the reference CLI (the hub's SSH channel is a transport for operations, not a second substrate), sender-side wake / push presence, durable audit trails, capability-based routing, and cryptographic signing.

### Tested targets and assumptions

The reference implementation makes specific assumptions about its runtime environment.

**Single-host, and a hub pool (one pool host + SSH remote lanes), are the supported and CI-tested configurations.** Every lock (`flock`), lease, and fence in this spec executes on **one** kernel — the pool host's. Locally that is the machine the agents share; remotely it is the hub. That is what CI exercises. A new multi-host deployment should join remotes to a hub (§13), not share the loop directory over a network mount.

**A network-mounted loop directory across hosts is inherited compatibility, one notch further deprecated.** Some pools run that way; this project's own pool has been one of them at points in its history (a second host's registration has appeared and aged out under the lazy sweep, §9), even though its current live pool happens to be colocated. The reference CLI does not refuse that shape, and two paths stay deliberately host-aware so an already-cross-host pool fails CLOSED rather than silently corrupting shared state: the serve lease's reclaim rule falls back to freshness/timeout alone when the holder is on another host, since a pid can't be proven cross-host (§5.4), and `agentchute setup --wipe-state` refuses to wipe a bus a foreign host's fresh serve claim still owns. They are compatibility and safety behavior, not a claim that mount-based cross-host correctness has been verified under concurrent writes, network-filesystem mount-cache edge cases, or clock skew beyond the NTP-loose assumption already stated (§5.4). Earlier drafts of this spec offered specific mount-flag guidance (`actimeo`, `noac`, `lookupcache`) for this case; that guidance is removed, not replaced — it was never CI-verified. **If your pool already spans hosts on a shared mount**, keep doing so on the fail-closed behavior above — nothing here asks you to colocate or migrate before upgrading — but a fresh deployment should default to single-host or a hub, and a pool considering a mount-based cross-host move for the first time should not expect the same tested guarantees.

| Axis | Tested Configuration | Real, Unverified | Out of Scope |
| :--- | :--- | :--- | :--- |
| **Operating System** | macOS, Linux (CI verified) | — | Native Windows (WSL required for run) |
| **Filesystem Layout** | Single-host shared directory (tested local disk); hub pool (one pool host + SSH remote lanes, §13) | Network-mounted loop directory shared across hosts (fail-closed safety only; no correctness guarantee; new multi-host deployments should use a hub) | Multi-disk, non-POSIX layouts |
| **PTY / Supervision** | Unix-like PTY (SIGWINCH propagation, best-effort) | — | Non-PTY shell wrappers, raw Windows Cmd/PowerShell PTY emulation |
| **Security Model** | Cooperative local trust (POSIX file permissions); remote lanes add per-key SSH pinning (§13.9, §15) | — | Cryptographic signing of message bodies, host-isolation bypass |

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
    archive/                       # consumed (committed) messages (gitignored; caller-managed, §6.3)
    malformed/                     # quarantined files (gitignored; caller-managed, §6.3)
    state/                         # pool-wide non-runtime scaffold (gitignored)
      setup.json                   # pool settings; preserved across --wipe-state
      pool.id                      # durable pool identity (12 lowercase hex + LF); preserved across --wipe-state (§13.9)
    state/<agent-id>/              # owner-private runtime state (gitignored)
      owed.json                    # asker-owned reply obligations — sole reply mechanism (§6.6)
      send.floor                   # durable per-sender monotonic timestamp floor (§6.1)
      serve.claim                  # serve lease + fencing token (§5.4)
      guard.latch                  # per-session guard latch, guarded vendors only (§15)
```

The namespace is fixed at `.agentchute/loop` (no vendor-namespaced dotdir). `AGENTCHUTE.md` is the only file that MUST be tracked. There is no separate presence directory: a registration row under `agents/` (public, one per agent) IS the presence record (§9). `state/<id>/` is owner-private — peers never read another agent's state dir.

**`setup --reset --wipe-state` preservation.** Wipe deletes runtime loop state (inbox/archive/`.claimed`/malformed/scratch/`state/<id>/` contents) and preserves non-runtime scaffold. Two `state/` files are preserved, named in all three places the wipe surface reports them — the wipe plan, the post-wipe rescan, and the dry-run/preserved output:

- `state/setup.json` — pool settings (already preserved).
- `state/pool.id` — the pool's durable identity. Wiping it would remint a new identity on the next `hub authorize` and silently invalidate every existing key binding for that pool. A post-wipe rescan must not flag it as a leftover.

Any other `state/` survivor is still flagged by the rescan. Behavior of the `pool.id` exemption lands with `hub authorize`; this paragraph is the contract the implementer and this spec agree on.

## 4. Discovery (filesystem reference implementation)

The reference CLI resolves two distinct paths:

### 4.1 Control repo cascade
1. **`--control-repo <path-or-url>` flag.**
2. **`AGENTCHUTE_CONTROL_REPO` env var.**
3. **`.agentchute-control-repo` pointer file.** Walk from cwd up to root. Nearer pointer wins. This is the reference mechanism for worktree or sibling-folder participants that share one central control repo, and for a remote lane that has joined a hub.
4. **Cwd walk.** Walk up to root looking for `AGENTCHUTE.md` + the fixed `.agentchute/loop` directory.

Each of arms 1–3 also accepts an `ssh://` locator (a remote lane's pointer at a hub):

```
ssh://[user@]host[:port]/absolute/path/to/control-repo
```

`user` and `host` match `[A-Za-z0-9._-]+` and must not begin with `-`; `port` is 1–65535 or omitted (ssh default); the path is absolute on the hub. The `ssh://` prefix is recognized **before** local-directory checks. Remoteness is discovered from this locator — there is no `--hub` / `--remote` flag. An `ssh://` locator combined with an explicit `--loop-dir` / `AGENTCHUTE_LOOP_DIR` is a hard error (one authority for where local state lives). The local state for a joined hub lives under `~/.agentchute/hub/<hub-id>/` (shadow loop dir, keys, known_hosts); mail never does.

### 4.2 Loop dir cascade
1. **`--loop-dir <path>` flag.**
2. **`AGENTCHUTE_LOOP_DIR` env var.**
3. **Auto-discover.** The fixed `.agentchute/loop` directory under the control repo root (local), or the per-hub shadow loop dir (remote; derived from the `ssh://` locator, never supplied as a second authority).

## 5. Registration

Every agent MUST publish a registration record so peers can discover it and so presence/liveness reads have an enrolled identity. Registration carries **no wake fields** — there is nothing to poke.

### 5.1 Protocol-level registration fields
- `agent_id` (string, required): `^[a-z0-9][a-z0-9-]*$`.
- `vendor` (string, recommended): anthropic, openai, google, human, etc. (advisory).
- `host` (string, recommended): advisory hostname for same-host correlation.
- `last_seen` (RFC 3339 UTC, required): **this field IS the presence/freshness signal (§9)** — there is no separate presence file. A live `serve` process advances it unconditionally every heartbeat tick, gated by its fencing token; a session that registers explicitly (`boot`/`register`) sets it once at that moment, without a lease.

**Retired (v2.5 plan B5; tolerate-on-read only):** `status`, `restart_at`, `last_active`, `launched_by`, `shim_name`, `hook_event` are no longer written by the reference CLI. An older in-flight row may still carry one or more of them; a reader simply parses and ignores them like any other unrecognized field (§6.5) — this tolerance is a migration courtesy, not a standing part of the schema. There are also **no** `wake_method` / `wake_target` / `reachable_at` / `reachability_*` / `wake_endpoints` fields; pull-only coordination has no published wake endpoint, so the entire wake/reachability cluster never existed as a live field.

The Markdown body is optional advisory prose. Do not route on capabilities (§12).

### 5.2 Reference mechanics (filesystem)
Encoded as YAML frontmatter in `.agentchute/loop/agents/<agent_id>.md`.
- `control_repo` (path, required): absolute path to the control repo.
- `working_repos` (list, optional): additional relevant repo paths.

See **Appendix C** for a hand-registration walkthrough.

### 5.3 Enforced enrollment
Implementations MUST refuse active operations (consume/send/gate) if the agent's registration is absent or unreadable. Self-registration is mandatory on every session start (§7.2).

### 5.4 Id-uniqueness — the serve lease + fencing token
The reference CLI's per-sender monotonic timestamp floor (§6.1) and its unconditional `last_seen` heartbeat (§9) are only sound if **one live process owns an id at a time**; a second live writer under the same id would break monotonicity for that sender and could resurrect a stale-looking row out from under a legitimate sweep. The reference CLI enforces this with a decentralized shared-FS lease, not a central allocator:

- The runner acquires a **serve lease** at launch — a `state/<id>/serve.claim` carrying `{id, host, pid, serve_token, started_at, last_seen, boot_ref}`, committed via `link()`-no-clobber. A **fresh** valid claim makes a second launch of the same id **fail closed**. For a remote lane the lease holder is the **hub-side session process** (its own hub pid sits in the claim); channel loss releases the lease token-checked. Pid-proof is therefore same-host again — the session and the claim live on the hub.
- A **stale** claim is reclaimable only via the liveness rule: stale past the lease timeout, **plus** a same-host pid-proof failure when the holder is same-host (a frozen-but-alive process keeps its id); cross-host reclaim uses freshness/timeout only (pid is not provable across hosts). The same-host pid-proof gains **boot-reference corroboration**: the claim records a per-boot identifier (`boot_ref`) at acquire, and a stale claim whose recorded ref **differs** from this host's current one is provably dead whatever its pid says (a recycled pid after a reboot). Three properties bound this — the one behavioral amendment to existing lease semantics, and it also applies to local pools:
  - the comparison is **equality only, never ordering** — refs are never compared for age or parsed;
  - the reference has **no wall-clock component**, so a clock step cannot forge a difference (Linux: `/proc/sys/kernel/random/boot_id`; Darwin: `kern.bootsessionuuid`; anything else: empty);
  - the reference is **host-scoped** — `/proc/sys/kernel/random/boot_id` and `kern.bootsessionuuid` change on a host **reboot** and on nothing else, so a container/VM restart, a service restart, or a namespace re-create on a host that did **not** reboot leaves the ref identical and the claim behaves exactly as today. A restart without a reboot is unchanged from today. This fixes the post-reboot wedge; it does **not** solve pid reuse in general, and the manual remediation (inspect the pid; delete the claim if it is unrelated) remains the answer everywhere else.
  An **absent ref means unchanged, pre-existing behavior** (a claim written by a pre-upgrade binary, or a host whose ref is unreadable). Freshness is still checked first: a not-yet-stale claim is refused before any pid or boot reasoning.
- The **fencing token** (`serve_token`, a 128-bit random epoch) is the load-bearing part: every heartbeat, every mint of a new timestamp identity, and every registration write from `serve` verifies it. A zombie/paused holder that resumes after its lease was reclaimed fails the token check and stops — so it cannot create a dup-writer even though launch was guarded. The runner passes its token to the child via `AGENTCHUTE_SERVE_TOKEN`, so a fenced (reclaimed) child's `send` fails closed too.

This makes "give each process its own id" an enforced, fenced invariant rather than a convention. (Operational assumption: clocks are NTP-loose with `lease-timeout ≫ heartbeat + max-skew`. Severe skew degrades to premature/delayed reclaim. The lease state typically lives on the same single-host filesystem as the inboxes — the CI-tested configuration, §2 — but the cross-host reclaim branch above is real, shipped behavior, not dead code: some pools run the loop directory on a shared network mount across more than one host, and this project's own pool has too, at points in its history. That path is fail-closed compatibility, not a verified guarantee — see §2's "Tested targets" for the honest boundary between the two. A new multi-host deployment should use a hub, §13.)

## 6. Messaging

### 6.1 Message identity & ordering
The committed identity is the full delivery key `(to, from, timestamp, random-suffix)`:
- **`to`**: the recipient — encoded by LOCATION (which inbox the message lands in), so it is not spelled in the filename.
- **`from`**: sender `agent_id`.
- **`timestamp`**: a fixed-width, microsecond-precision UTC stamp, `YYYYMMDDTHHMMSSffffffZ` (e.g. `20260730T182415123456Z`), minted under a durable floor kept PER SENDER — not per recipient, not per `(sender,recipient)` pair. Mint rule: `stamp = now; if stamp <= floor { stamp = floor + 1 microsecond }`; the new floor is persisted BEFORE the delivery attempt (write-ahead), so a crash between persisting the floor and landing the message can only ever waste a stamp, never reissue one.
- **`random-suffix`**: 128 bits of `crypto/rand`, lowercase hex. Its only job is distinguishing two mints from the SAME sender in the same microsecond; a genuine suffix collision is astronomically unlikely and, on the rare link collision, is handled exactly like any other identity collision (below) — it is never itself the source of uniqueness.

The reference encoding is the canonical filename `<timestamp>_from-<from>_r<32hex>.md`. Because the timestamp is fixed-width, a plain lexicographic sort of one sender's files is **exact per-sender FIFO with no clock reads at sort time**. Cross-sender order is **advisory arrival order** (non-normative) — the protocol does not promise a global total order (a real total order would need a freshness CAS the mount can't cheaply give, and is unneeded).

On a filename collision at the committed identity (an already-landed file at the identical `(to,from,timestamp,suffix)` — a same-microsecond mint colliding on suffix too, astronomically rare, or a genuinely resent attempt), the sender retries under a FRESH random suffix — `to`/`from`/`timestamp` held fixed — for a small bounded number of attempts, then hard-errors. A collision is **never** treated as "already delivered, done": that would silently drop whichever attempt lost the race. This is the wire-level meaning of at-most-once (§1) — delivery either lands under a genuinely fresh identity, or it does not land at all; there is no delivery-side dedup to fall back on.

**Migration note (v2.5 dual-read window).** The reference implementation reads BOTH this timestamp grammar and the earlier per-`(from,to)` sequence-counter grammar (`from-<from>_seq-<020d>.md`) everywhere a name or reference is parsed — lister, claimed-lister, reply references, `.owed` matching, archive naming — and writes ONLY the new grammar. Per-sender FIFO is preserved across the cutover by sorting a sender's legacy-grammar messages before its timestamp-grammar messages, then sorting each grammar by its own monotonic key. Known migration-window limitation: if a pool writes timestamp-format messages, rolls back to writing legacy messages, then rolls forward again, the old-first ordering rule can place those rollback writes ahead of earlier timestamp writes — a two-transition edge case with no dedicated ordering machinery added for it (file mtime would be a worse cross-grammar key; copying and archiving perturb it). The old-format read paths are removed no earlier than the first post-2.5 minor release.

A name that parses as neither grammar is unrecognized: skipped by the lister, quarantined by `check` (§11.1).

### 6.2 Sender flow
1. Compose body (UTF-8).
2. Mint a timestamp under the sender's OWN monotonic floor (write-ahead persist, §6.1), then RELEASE that lock — the active serve token, if any, is fence-verified inside this same step, before the floor is persisted.
3. **Deliver into the inbox with the no-overwrite guarantee** (unique-temp + atomic `link()`), now under the RECIPIENT's lock (taken only after the sender's own lock is released — the two are never held together, so a self-send or a pair of agents sending to each other at the same moment cannot deadlock): re-check the recipient's freshness, fence-verify again if a token is active, then link. A collision at the committed identity is refused, not a silent success — the sender retries under a fresh random suffix (bounded attempts, §6.1). A sender crash before the link completes loses that message as a legal gap — delivery is **at-most-once**; there is no idempotency key and no delivery-side dedup (v2.5 plan B7 deleted both the old per-`(from,to)` allocator and the `--idempotency-key` opt-in that rode it).
4. **No wake.** The sender does not poke or signal the recipient — it only writes the message into the recipient's inbox. The recipient discovers it on its own poll.

### 6.3 Recipient flow — two-phase consume (act-then-archive)
Consumption is at-least-once and split across two verbs:

1. Enumerate and sort inbox messages (per-sender FIFO).
2. **`check` — phase 1 (CLAIM + display).** First re-display any uncommitted `.claimed` residue from a crashed/un-acked prior turn, each tagged with a **`REDELIVERED`** banner. Then, for each new message: validate envelope/body (quarantine if malformed, §11), CLAIM it (move `inbox/<id>/<name>` → `inbox/<id>/.claimed/<name>` under its canonical name), and display it. `check` does **not** archive, and it does **not** touch presence — a registration row's freshness (§9) is refreshed independently, by a live `serve` process's own heartbeat tick (or once, at explicit `boot`/`register`), never by a read/consume CLI touch.
3. **Act on each displayed message.** Because the CLI prints and exits before the model acts, archiving during `check` would be at-most-once for the *work*; claiming instead makes the work at-least-once. **Handlers MUST be idempotent** — a crash between `check` and `ack` re-delivers.
4. **`ack` — phase 2 (COMMIT).** Archive the `.claimed` residue (moving `.claimed` → `archive/` is a mutation of the recipient's own state only), then report any outstanding finish-gate blockers rather than withholding the commit. For anyone the guard (§15) does not apply to — a human, an unguarded/hookless session, or a latch belonging to no session or a foreign/dead one — this commit is unconditional. **It is not unconditional for a guarded session whose OWN current-session latch is armed**: that call is refused, directing the caller to `agentchute turn-end` instead (the only path that may commit claimed mail and clear that latch together, §15) — a deliberate exception, not an oversight, since letting a bare `ack` bypass the ordered handler would let it commit without ever clearing the latch. Archiving is the single commit point regardless of which path reaches it; an already-archived message is a benign no-op (idempotent).

**Retention model.** `archive/` and `malformed/` are **caller-managed**. They grow without bound by design and are **not** part of the delivery guarantee. The delivery contract ends at claim (`check`) / commit (`ack`); `archive/` is an audit residue only. This includes malformed/ — §11.1's never-silently-dropped guarantee binds the reader (quarantine, don't drop); subsequent disposal is the caller's retention choice.

Operators may clean with this documented one-liner (verify paths against §3 layout; targets `archive/`, `malformed/`, and stale delivery temps — never live inbox messages, `state/` records, or `.claimed/` residue):

    find .agentchute/loop/archive -type f -mtime +30 -delete && find .agentchute/loop/malformed -type f -mtime +30 -delete && find .agentchute/loop -name '.tmp_*' -type f -mtime +1 -delete

(`.tmp_*` files are crashed in-flight writes — deliveries, registrations, lease claims — that were never linked into place; `doctor` reports any older than an hour.)

**Backpressure.** Coordination is pull-only, so a dead or inactive recipient's inbox grows without bound by design — senders apply no backpressure. Operators should watch inbox depths (`status`); the remedy for a permanently-retired agent is removing its inbox directory by hand after confirming the registration is gone (the cleanup one-liner above deliberately never touches inboxes).

On guarded vendors (§15), the end-of-turn hook runs a single `agentchute turn-end`, which self-repairs the agent's own registration, archives what THIS session's `check` claimed, and evaluates the read-only finish gate, all in one ordered process (§9, §15) — it replaces what were once three separate hook entries. Unguarded or hookless sessions commit with `agentchute ack` directly. If a resend under the identical committed identity as an already-claimed message ever lands in the inbox (a narrow race), `check`'s claim step treats it as a benign duplicate and drops it — this is a claim-time safeguard, not a general delivery-side dedup guarantee (delivery itself is at-most-once).

### 6.4 Message envelope
Encoded as optional frontmatter — this section's own flat key:value grammar (below), not general YAML. The **normative** envelope is small:
- `from` (required **information**): the sender `agent_id`. In the filesystem binding this is satisfied by the canonical filename (`<timestamp>_from-<from>_r<32hex>`, strictly parsed, §6.1); a frontmatter `from` field, when present, is display/inference-grade metadata, and a body-only message with a canonical filename is well-formed.
- `reply_required` (boolean, optional): an **advisory hint** that the sender wants a reply. The binding reply obligation is the asker's own `.owed` ledger (§6.6); `reply_required` stays on the wire as the one cross-agent coordination hint.
- `in_reply_to` (optional): the canonical reference `to-<to>_from-<from>_<timestamp>_r<32hex>` of the message being answered. Consuming a reply whose `in_reply_to` matches one of the asker's outstanding `.owed` entries discharges that obligation.

**Compatibility fields:** `message_id` is no longer emitted (removed in v0.9.0); the identity is `(to,from,timestamp,random-suffix)` (§6.1) and reply threading rides `in_reply_to` (the canonical reference above). A `message_id` on an older in-flight message is still tolerated on read (ignored — never the identity). `to`, `task`, and `status` are no longer part of the envelope or the reference CLI at all (`to` is encoded by location; a message's subject, if any, is a body convention — the first Markdown line — not a typed field). They carry no special-case compat handling anymore; a stray `task:`/`status:`/`to:` line on an old in-flight message is simply an unrecognized field, ignored per §6.5 like any other. A message using the earlier `to-<to>_from-<from>_seq-<020d>` reference form is still read during the dual-read window (§6.1).

**Frontmatter grammar (v2.5 plan B8).** One parser (`parseFrontmatter`, the reference CLI's `internal/loop/registration.go`) implements this for both message envelopes here and registration rows (§5.2) — one engine, not a per-context dialect, closing a historical validator/recorder skew where a message's envelope was gated by one parser and its fields read by another. It is a flat key:value format, not general YAML. The steps below are exact — verified line-for-line against `parseFrontmatter` and against the independent conformance reimplementation (`conformance/fm.go`); the two agree with each other on every point:

1. The opening line and the next line whose TRIMMED content is exactly `---` delimit the block; whitespace around the delimiter itself is tolerated. A block with no closing `---` is a hard parse error for the whole message.
2. Scan the lines strictly between the delimiters, top to bottom:
   - A blank line (after trimming) is skipped.
   - A line starting with a literal space or tab is a hard parse error for the **whole block** — UNLESS it is a list item continuation (see below); a list is the only place indentation is expected or tolerated.
   - Every other line MUST contain a `:`. The KEY is everything before the *first* `:`, trimmed; it may be **any non-empty text** — there is no charset restriction (hyphens, spaces, Unicode all pass) beyond "not empty after trimming." In practice every field name in this protocol is a bare identifier (`agent_id`/`from`/`reply_required`/…), but the parser itself does not enforce that shape. A line with no `:` at all — including a `#`-prefixed comment, since this grammar has no comment syntax — is a hard parse error for the whole block.
   - A key may appear at most once; a repeated key is a hard parse error for the whole block (no last-write-wins).
   - The VALUE is everything after that first `:`, trimmed (further `:` or `#` characters inside it are literal value content, not new fields or comments).
   - A non-empty value makes the field a SCALAR: its text runs through the cleanup in step 3.
   - An empty value (`key:` with nothing after it, or only whitespace) OPENS A LIST: each following line is inspected — a blank line is skipped and does **not** end the list; a line whose content, after trimming **any** amount of leading whitespace (zero, one, a tab, or more — there is no fixed indent width), starts with `- ` (dash, single space) is consumed as a list item, its text being everything after that `- ` prefix, run through the same cleanup as a scalar; the first line that is neither blank nor `- `-prefixed ends the list — that line is **not** consumed, and is re-scanned as an ordinary top-level line (subject to every rule above, including the indentation check) — and leaves the key with an empty scalar.
3. Scalar/item cleanup: `strconv.Unquote` is tried first (interprets backslash escapes inside a double-quoted value, e.g. `"a\tb"` → `a`+TAB+`b`); failing that, one layer of surrounding matched `"` or `'` is stripped verbatim (no escape interpretation); the literal values `null` and `~` collapse to the empty string.

Unknown keys are always tolerated (§6.5) — the grammar's strictness is about **shape** (a `:`-bearing key per line, no stray text, no indentation outside a list, no duplicate keys), never about which key names or how many list-item spaces appear.

### 6.5 Forward compatibility
Receivers MUST ignore unrecognized frontmatter fields. `from` is required information (§6.4). Conforming v2.5 registrations emit integer `v: 3` in the protocol-version field; the reference CLI renders that value as `v2.5`. Absent `v:` implies a silent legacy/unknown state with no warnings generated. A genuine protocol-version mismatch (where `v:` is present and does not equal 3) surfaces as a diagnostic warning (doctor/status) with pool-wide update/restart guidance, but is never a delivery blocker. Messages MUST be valid UTF-8. The reference CLI accepts up to 4 MiB per message.

### 6.6 Reply obligations (asker-owned only)
Reply obligations are **asker-owned only**. The asker's `.owed` ledger is the **sole** reply-obligation mechanism (non-blocking warning + expiry). **Recipients are never blocked at finish by a `reply_required` message** — delivery is best-effort pull, with no forcing function once delivered.

- When an agent sends `--ask` (reply-required), it records its own obligation in `state/<asker>/owed.json`: "I am owed a reply to `(to=recipient, from=me, seq)` by `<deadline>`" (default 30m; override with `--reply-by`). The ledger is single-writer, atomic-rename, and the gate reads only its OWN ledger — it never scans peers.
- When the asker later consumes a reply whose `in_reply_to` references that `(to,from,seq)`, the obligation is cleared only if the consumed reply's canonical sender is the agent that owed the reply (idempotent).
- The asker's gate surfaces **outstanding** and **expired** obligations as **non-blocking warnings**. An expired obligation is the asker-side dead-recipient signal: a dead recipient shows up twice over — the asker's expired `.owed` AND the recipient's own stale registration row (§9) — so the asker never waits on a corpse.
- On the recipient side, consuming a `reply_required` message records **nothing** and merely **prints the reply-ref command** (`reply_required` is advisory on the wire). There is no recipient-side ledger and no `defer` command; both were removed in v0.9.0 (the `.owed` redesign). A reply is a normal `send --reply-to <ref>`, which discharges the asker's `.owed` when the asker consumes it.

## 7. Coordination defaults

### 7.1 Direct addressing only
Messages are sent to specific recipients. No broadcast or self-claiming.

### 7.2 Inbox-only authority to mutate
An agent's authority to mutate project state starts when an inbox message arrives, or when explicitly instructed by a human. Reading files does NOT authorize work.

**Protocol maintenance is pre-authorized.** Mandatory on every session start without waiting for instruction:
- **Self-registration (§5)**: `agentchute boot` / `register` is mandatory and idempotent. It reconciles `host` and `cwd`. Existing files are not sufficient.
- **Self-state updates**: the registration's own `last_seen` (§5.1) is the sole freshness signal — set once at explicit `boot`/`register`, and, when running under `agentchute serve`, advanced continuously by its lease-gated heartbeat (§9). The retired `status`/`restart_at`/`last_active` fields (§5.1) are no longer written.
- **Own scaffold & inbox operations**: creating `inbox/<self>/`, `archive/`, `malformed/`; claiming, acting on, and acking own mail.
- **Protocol correction (§11).**

There is no cooperative-waking step: coordination is pull-only, so an agent never pokes a peer.

Everything else (project edits, unsolicited peer messages, side-effecting commands) is gated by the authority rule.

### 7.3 Identity and Bridges
Identity is pool-scoped: `(pool_locator, agent_id)`. A physical process participating in multiple pools is a **bridge**. Bridges MUST NOT assume transitive authority or automate cross-pool forwarding without explicit policy. See [`EXTENSIONS.md`](EXTENSIONS.md) for bridge hazards.

## 8. Wake / supervision (reference implementation)

There is **no wake on the wire** and no sender-side poke. Discovery is recipient-side polling; the only question is what drives a given wrapper's poll.

- **Native-loop wrappers** poll their own inbox on their own cadence (or at lifecycle boundaries via hooks).
- **Loopless wrappers** run under the **runner** — `agentchute serve -- <wrapper>` — a per-agent PTY supervisor. It launches the child under a PTY, acquires the serve lease (§5.4), polls the agent's OWN inbox each tick, advances the registration's `last_seen` via its lease-gated heartbeat (§9), and injects `[agentchute] check inbox` into the child's PTY when new mail appears (respecting an idle/injection window so it doesn't interrupt mid-turn). It uses per-vendor submit bytes (e.g. bracketed-paste + enhanced-enter for codex) so the cue actually submits. For a **remote lane** the runner's poll **is** the tick over the hub channel (§13); injection is unchanged and local — the child, the PTY, and the cue never leave the joining machine.

The leading bracket in the injected cue is machine metadata; the model-facing instruction is `check inbox`. `setup --wake` installs the runner path only; the former tmux/herdr wake adapters and the runner receive-socket were removed in the pull-only redesign.

PTY injection is a best-effort cue, not a compliance guarantee. The supervisor attempts to inject the text cue without disrupting active child input, but terminal size changes, SIGWINCH propagation, or platform-specific shell quirks (e.g., Windows native consoles, where WSL must be used instead) can delay or miss cue delivery. In all cases, the child's underlying poll remains the authoritative discovery mechanism.

## 9. Liveness & presence

Presence is **soft state, read directly from the registration row** (v2.5 plan B5) — there is no separate presence file and no publish/heartbeat machinery beyond the registration itself. A row's own `last_seen` (§5.1) IS the presence record.

**Freshness — three distinct horizons, deliberately not conflated:**
- The **serve lease** (10s, §5.4) governs who may currently act as an id — the tightest horizon, because a wrong answer here risks a dup-writer.
- **`gate`/`doctor`'s self-freshness check** (30 minutes, fixed) governs whether an agent's OWN registration is fresh enough to `commit`/`release`.
- **Sweep staleness** — the age past which a row becomes a candidate for removal (below) — is the pool's configurable `stale_after` (default 1 hour; `agentchute setup --stale-after <duration>`).

A row's age compares its `last_seen` against the reader's clock under the same NTP-loose assumption as §5.4: clock skew between reader and writer shifts perceived freshness in either direction. Stale or absent ⇒ **not-alive** — never an error; an unregistered or long-gone agent simply reads not-alive. This is the dead-mailbox detection: "came back days later, one agent never returned" surfaces as a stale row.

**Refresh — up to four writers, and the cadence differs by vendor:**
1. A live `agentchute serve` process advances its own agent's `last_seen` unconditionally on every poll tick (default interval 5s, configurable via `--interval`, floored at 5s), gated by its serve-lease fencing token (§5.4) — a fenced-out (reclaimed) holder's heartbeat writes nothing, so it cannot resurrect a row another lease now owns. Universal: applies under `serve` regardless of vendor or hook coverage.
2. Explicit `boot`/`register` sets `last_seen` at that moment, needing no lease. Also universal.
3. On vendors whose hooks include a turn-start entry (claude-code, codex — §15), `self-check` writes it again at the START of every turn, using the same registration self-repair logic `boot` uses, needing no lease either.
4. On vendors whose hooks include an end-of-turn handler (claude-code, codex — Stop; gemini — `BeforeAgent`, §15), `turn-end` writes it as step 0 of its ordered sequence, before evaluating the finish gate.

Writers 3 and 4 are **not** symmetric across every hook-covered vendor — this is the same per-vendor unevenness §15 already documents for the guard, restated here for presence rather than glossed over: claude-code and codex get two genuinely distinct hook-driven writes per turn boundary (self-check at the START of THIS turn, `turn-end` at the END of THIS turn). gemini has no separate self-check entry — its single `BeforeAgent` handler is itself a turn-start hook, and it runs `turn-end` there, so one call at the start of the NEXT turn covers both roles rather than two calls at two points in the cycle. grok has no hook system whatsoever (§15): neither writer 3 nor 4 ever fires for it; only 1 and 2 keep a grok row fresh.

The practical effect: a claude-code or codex session refreshes its own freshness on every turn boundary purely from hooks, whether or not it is ALSO running under `agentchute serve`. A gemini session gets comparable protection through one call rather than two, timed differently in the cycle. A grok session gets none of this — only `serve` (or explicit `boot`/`register`) keeps its row from aging. No other command — `check`, `send`, `status`, `gate`, `doctor` — ever writes `last_seen`; they only read it.

**Registration rows are pool-shared state, not exclusively self-owned.** Any agent's row may be removed by another process — in practice, whichever one is running `boot` or `serve`'s own slow tick — once it satisfies both sweep conditions below; this is the intended hygiene mechanism, not a violation of the named agent's authority over its own state.

**The lazy sweep.** A row is swept when:
1. its age exceeds the pool's `stale_after` threshold — using the row FILE's own mtime as a fallback age proxy when the row fails to parse at all, so a hand-corrupted row is not immortal merely because its `last_seen` can't be recovered — **AND**
2. its serve claim (§5.4) is absent or itself stale. A fresh lease immunizes an old-looking row even past `stale_after`: the row belongs to a process that just hasn't re-registered explicitly in a while, not a dead one.

The sweep never removes the sweeping agent's own row, caps itself at 5 removals per pass (a large backlog drains over several triggers, never all at once), and re-checks BOTH conditions under the target row's own lock immediately before deleting it — a heartbeat or fresh lease landing in the narrow window between the initial scan and the delete wins, and the sweep backs off for that row rather than deleting state that just became current again. Sweeping touches ONLY the `agents/<id>.md` registration file; it never touches an agent's inbox, mail, or any other state.

**Triggers, exactly two:** `boot`, once, immediately after the caller registers itself and before it peeks its own inbox; and `serve`'s poll tick, at most once every 10 minutes. `send`, `status`, `doctor`, and `gate` never trigger a sweep — they only report what they read.

There is **no watchdog and no cross-agent liveness push** — both were deleted as push machinery; pull-only coordination needs neither. A sender doesn't care whether the recipient is live (the message waits in the inbox regardless), and an asker learns of a dead recipient from its own expired `.owed` (§6.6) plus the recipient's own stale registration row.

## 10. (removed) Watchdog

The liveness-only watchdog and its cooperative-waking step are **removed**. Cross-agent liveness pushing was unreliable (stale caches, watchdog races, gates on phantom liveness); pull-only coordination + registration-row presence (§9) replaces it. This section is retained as a pointer so older references resolve.

## 11. Protocol correction (best effort)

Every agent participates in keeping the pool healthy.

### 11.1 Enforcement action
Triggers include malformed inbox filenames, unparseable frontmatter, or unparseable peer registrations.
1. **Quarantine**: atomic move to `.agentchute/loop/malformed/`.
2. **Continue**: do NOT block the sender or the turn.

A well-formed canonical filename (either grammar, §6.1) is never quarantined; only a genuinely-unrecognized name is enforced on.

Quarantine happens **pre-claim** (`check` validates before it claims, §6.3 step 2): a quarantined item is never claimed and never archived, so it never counts as **consumed**. It has no effect on the sender's monotonic floor (§6.1) either way — the floor is the sender's OWN durable per-sender state, not something a reader advances by claiming or quarantining a message, so a malformed item simply never touches it. It is never silently dropped: it persists as a file under `.agentchute/loop/malformed/` until an operator or agent inspects it. This is surfaced proactively, not just documented — `doctor`/`pending`/`boot` report the malformed count with a `check`-to-quarantine hint, and `gate` (including `--before finish`) blocks on any unquarantined malformed file until `check` runs.

## 12. Non-goals
- No non-filesystem **state substrate** in the reference CLI; the hub's SSH channel forwards *operations* to the one filesystem pool and is part of the reference implementation (§13). Sync/replication of loop state remains excluded.
- No sender-side wake / push presence / reachability cache.
- No durable/authenticated audit trail (archive is gitignored; default off).
- No capability-based routing.
- No protocol-level signing or auth.
- No coordinator/router agents and no cross-agent liveness tracking.
- No *dynamic capability* negotiation beyond the versioned hub handshake (§13.3) and the static registration `v:` field.

## 13. Hub wire & lifecycle (reference implementation)

The hub is today's pool: plain files under `.agentchute/loop/`, every CI-tested invariant (`flock`, `link()`-no-clobber, rename claim, serve lease + fencing token) executing on the hub's kernel exactly as it does now. The network moves the **operation** to where the state lives; it never moves, syncs, or replicates the state. After this section, when this text and `proposal/ssh-hub/DESIGN.md` disagree, this text wins.

### 13.1 Name and carriage

Protocol name: **`agentchute-hub`**, version **1** (integer). The name appears in the client's SSH exec request, the `hello` frame, and error text.

Carriage is a **forced-command pseudo-subsystem**, not an sshd `Subsystem` directive. The client requests exec of the literal string `agentchute-hub`; the hub's `authorized_keys` forced command ignores it and runs `agentchute hub session …`. The client's request is preserved in `SSH_ORIGINAL_COMMAND` for audit. One carriage path only.

### 13.2 Framing

Newline-delimited JSON control frames + raw body trailers, both directions.

- A frame is one UTF-8 JSON object on one line, LF-terminated. Max frame line: **64 KiB** (`E_TOO_LARGE` past it).
- A frame that carries a payload declares `"body_len": N` (bytes); exactly N raw bytes follow the frame's LF, then the next frame begins. Max body: **4 MiB**. Bodies are byte-exact message content — no base64, no re-encoding.
- Client requests carry a client-assigned monotonically increasing integer `"id"`; responses echo it as `"re"`. **One request in flight per session, strictly serial** — concurrency comes from opening more one-shot sessions, never from interleaving frames.
- Unknown JSON fields are ignored (mirror of §6.5). An unknown `"t"` gets `{"t":"error","code":"E_UNSUPPORTED",…}` and the session stays up. A *framing* violation (non-JSON line, `body_len` mismatch, oversize) gets an error frame and the session closes — there is no resynchronization. Violation-closes-session applies to **received** frames only: the hub never composes an oversized line, so a session can never be killed by the size of its own response — in particular never *after* a commit.
- Hub→client `note` frames may precede a response. **`level` is one of exactly two values, and the level IS the stream**: `warn` → the client's **stderr**, rendered `warning: <msg>`; `info` → the client's **stdout**, rendered `<msg>` with no prefix. `msg` never carries its own level prefix — the renderer adds it. A third level is a spec change, never an implementer's choice. Both arms are load-bearing: `warn` carries today's stderr warnings (quarantine); `info` carries `check`'s in-stream stdout status lines (empty-inbox, limit, CLAIMED-not-yet-archived) so they stay in production order rather than being re-derived from the terminal `check-ok` after later `owed-item` events.

### 13.3 Handshake

Mandatory first exchange on every session, both channel and one-shot:

```
C: {"t":"hello","id":1,"proto":"agentchute-hub","v":1,"min_v":1,"agent":"codex","bin":"1.7.0"}
H: {"t":"hello-ok","re":1,"v":1,"agent":"codex","pool":"/home/alex/code/agentchute",
    "pool12":"9c4e12ab77f0","writable":true,"hub_bin":"1.7.0",
    "hub_time":"2026-08-14T21:05:03.123456Z"}
```

- Version selection: hub computes `use = min(hub_max, client v)`; if `use < client min_v` or `use < hub_min` → `{"t":"error","code":"E_VERSION","msg":"hub speaks agentchute-hub v1; client requires ≥2"}` and close. v1 is the only version at ship; the negotiation exists so v2 can ship without a flag day. Fleet rule stated in the error text: **the hub upgrades first.**
- Identity: `hello.agent` is the client's resolved id; the hub compares it to the key's pinned `--agent`. Mismatch → `{"t":"error","code":"E_IDENTITY","msg":"key is authorized as \"codex\"; you are acting as \"grok\""}` and close. Past this point **no frame carries an actor field** — every op executes as the pinned id.
- Pool: `hello-ok` reports `pool` (normalized path, for display) and `pool12` (read from the pool's own `state/pool.id` at session start — never an argv echo). At join the client records both; on every later session it hard-fails unless `hello-ok.pool12` equals the recorded value — the **client-emitted** arm of `E_POOL_MISMATCH`. The hub emits the same code at session start when `state/pool.id` is absent or ≠ the forced command's `--pool-id`.
- `hub_time`: the client computes `offset = hub_time − local_now` once per session and applies it to *display-only* age math. Protocol state never uses the client clock.

### 13.4 Vocabulary

Complete v1. Client→hub: `hello`, `send`, `check`, `ack`, `register`, `status`, `gate`, `pending`, `clean-owed`, `lease-acquire`, `tick`, `lease-release`. Hub→client: `hello-ok`, `send-ok`, `msg`, `owed-item`, `check-ok`, `ack-item`, `ack-ok`, `register-ok`, `status-ok`, `gate-ok`, `pending-ok`, `clean-owed-ok`, `lease-ok`, `tick-ok`, `release-ok`, `note`, `error`.

Event-stream frames interleave in production order. Unbounded lists never ride inside one control frame: `check`/`pending` stream as `msg` (a pending `msg` omits `body_len` unless the request set `show_body`); `ack` results as `ack-item`; owed entries as `owed-item`; notes — both `warn` and `info` — as `note`. Terminal `*-ok` frames carry counts for those streams, never arrays of the streamed items; fixed-small lists may ride inline.

`tick-ok.warnings` is `[]string` and is **always present** — `[]` when the tick was clean, never omitted. A missing field is a malformed response, not a defaulted empty. The fenced case is the tick's only hard error; every other step failure rides in `warnings`.

`send` — normative shape; `committed` is present:

```
C: {"t":"send","id":2,"to":"claude-code","ask":true,"reply_by_s":3600,
    "serve_token":"9f2c…32hex","body_len":184}
   <184 raw bytes>
H: {"t":"send-ok","re":2,
    "filename":"20260814T210503123456Z_from-codex_r4b1d….md",
    "ref":"to-claude-code_from-codex_20260814T210503123456Z_r4b1d…",
    "committed":true,"durability_note":"","owed_note":""}
```

**`committed` is mandatory on every `send-ok`.** `committed:true` means the recipient-side `link()` succeeded, so the message IS delivered and must never be resent — including `committed:true` with a non-empty `durability_note` (linked, dir-sync failed) and/or a non-empty `owed_note` (delivered, asker-side reply-obligation record failed). A `send-ok` without `committed` is a **malformed** response, not a defaulted `false`.

**`durability_note` and `owed_note` are mandatory on every `send-ok`**, each a string, always present — `""` when that arm is clean; omission of either field is a **malformed** response, not a defaulted empty (the same rule `tick-ok.warnings` follows). They are independent: both may be non-empty on the same send, because a dir-sync failure and an owed-record failure are two distinct facts. A non-empty `owed_note` is not a delivery failure. Nothing may treat it as grounds to resend. **`claimed_held` on the terminal `error` frame deliberately departs from this always-present rule.** A forgotten `durability_note` or `owed_note` is a malformed `send-ok` and the client knows something is wrong; a forgotten `claimed_held` is indistinguishable from a legitimate "no residue" omission. That cost is accepted: presence means `true`, absence means no claimed residue is held.

A remote send terminates as `send-ok` or `error`. Those are two shapes: `error` means nothing was delivered; `send-ok` means delivery committed (`committed` is the discriminator). An owed-record failure cannot ride as `error` — that frame would lose the committed response and drive spool/retry on an already-delivered send.

### 13.5 Ambiguous send — fail closed, never replay

The ambiguity window opens when the first byte of the `send` frame is handed to the ssh child's stdin, and closes when `send-ok` or an `error` frame for that `id` is read.

- **Before the window** (connect, hello, preflight error frame): the send provably did not happen. The CLI spools the body and prints the retry command. Retrying is safe.
- **Inside the window** (channel drops, ssh exits, response deadline expires with no frame): the outcome is **unknown**. The CLI spools the body, exits 1 with `E_SEND_UNKNOWN`, and **never retries automatically**. There is no delivery-side dedup (§6.2: at-most-once, no idempotency key); a blind replay would be a duplicate message.
- A `send-ok` with non-empty `durability_note` and/or non-empty `owed_note` is still a committed delivery: report, do not resend. Neither field is a delivery failure.

### 13.6 Disconnect after claim

Claiming happens hub-side (rename into `.claimed/`) **before** the bytes stream back. If the connection dies mid-stream, the mail is claimed-but-undisplayed — the existing two-phase crash window: the next `check` lists `.claimed` residue and re-displays it with the REDELIVERED banner. At-least-once for the work, unchanged.

### 13.7 Remote turn-end order

The guard latch **never crosses the wire**. In remote mode it is a local file under the shadow loop dir. Remote `turn-end` order:

0. **best-effort** wire `register` — a failure is reported and does **not** abort steps 1–3;
1. wire `ack` commits hub-side;
2. local `ClearGuardLatch`;
3. wire `gate` for the finish verdict.

The latch clears only after the commit is confirmed. If the hub is unreachable at step 1, `turn-end` fails, the latch stays armed, and the claimed mail stays in the hub's `.claimed/`.

### 13.8 Timeouts, deadlines, cadence

| timer | value | enforced where |
|---|---|---|
| TCP connect | 5 s | client, `-o ConnectTimeout=5` |
| hello → hello-ok | 10 s | client (kills ssh child on expiry); hub kills session if no `hello` within 10 s of start |
| one-shot response deadline | 30 s per request | client (covers a 4 MiB body on a slow link) |
| channel tick interval | 5 s | client serve loop |
| tick response deadline | 10 s | client; expiry ⇒ kill ssh child ⇒ fence path |
| transport dead-peer kill | ~10–15 s | ssh itself, `ServerAliveInterval=5 ×2` (channel) |
| hub session read deadline | 20 s (channel, = 3 missed ticks + margin); 30 s idle (one-shot) | hub |
| hub session write deadline | 30 s per response | hub |
| one-shot session max lifetime | 10 min | hub |
| mux master linger | 60 s | `-o ControlPersist=60s` |
| serve lease timeout | **10 s — unchanged** | hub |
| heartbeat / registration refresh | every tick (5 s) — unchanged cadence | hub session |
| sweep throttle | 10 min — unchanged | hub session |
| lease reclaim protection | stale ≥10 s **and** (hub-pid dead **or** the claim's recorded `boot_ref` differs from this host's current one) | hub |

**SSH authorization lifetime.** Identity binding and authorization freshness are separate properties. A multiplexed master remains authenticated as exactly its pinned key and agent id — `--as`/`--from` enforcement and the no-actor-field wire remain unchanged — but sshd does not re-read `authorized_keys` for new session channels on an established connection. Revocation and re-authorization therefore take effect at the next authentication: a serve channel retains its authorization until the channel drops, and a persistent one-shot master retains it until the master closes. `ControlPersist=60s` is an idle timeout, not a wall-clock bound; an active lane can keep a master alive indefinitely. For an immediate cut, stop or relaunch the remote lane (or reap its master with `ssh -O exit` on the joining machine). A hub-side operator cannot reap a master held on another host or account. Repointing is especially sensitive: until the old master closes, its forced-command snapshot can continue targeting the old pool, so do not treat a successful hub-side edit as proof that an active remote lane moved.

One-shot masters are isolated by an opaque 12-hex digest over the hub id, agent id, and **resolved key version**. The hub id already hashes the canonical URL, including user, host, port, and pool path. Resolving the stable active-key symlink makes rotation self-invalidating: promotion changes the digest and the next operation authenticates with the new key. `%C` alone is insufficient because it omits `IdentityFile`. All invocations sharing a master MUST keep the same connection-affecting SSH options; an attached session inherits the master's forwarding, host-key, and route decisions. User `ssh_config` alias changes are deliberately not reimplemented in this digest, so a live master retains its previously resolved route until close.

### 13.9 Identity pinning

One authorized key = one agent id. The `authorized_keys` line **is** the mapping — no side database.

```
restrict,command="/usr/local/bin/agentchute hub session --agent <id> --pool <abs> --pool-id <pool12>" ssh-ed25519 … agentchute:<id>:<pool12>
```

- `restrict` + forced command: no shell, no PTY, no forwarding. The client's exec request is discarded. The wire carries **no actor field**. A `--as`/`--from` mismatch is rejected at hello (`E_IDENTITY`), never silently rewritten.
- `state/pool.id` is the pool's durable identity: one regular, non-symlink, 0600 file whose entire content matches `^[0-9a-f]{12}\n$`. `hello-ok.pool12` always carries the value **read from that file**, never an argv echo.
- A compromised remote key can act fully as that one id (send, claim/ack its inbox, hold its serve lease, read the roster) and poison peers with message content. It cannot get a shell, act as another id, read another agent's `state/` or mail, or tamper with pool state outside the protocol. Co-tenants on the hub itself remain under §15 cooperative trust; bodies remain untrusted data.

### 13.9a Writers under a migrated hub tree

A joined machine's shadow loop dir lives **inside** the hub directory, and a same-hub
migration (an alias/URL change for the same pool) renames that directory. A descriptor
follows the inode, not the name, so renaming does not detach a writer that already has one
open: a write issued after the migration has verified the copy lands in the moved-aside tree
and is destroyed with it.

**Any process that opens a descriptor under a hub tree MUST hold that hub's shared lock for
as long as the descriptor lives, and a migration MUST hold the same lock exclusively for both
the old and new hub ids.** The lock file lives beside the hub directories, never inside one,
so it survives the rename. A migration that cannot take the lock MUST refuse rather than
proceed; it MUST NOT wait indefinitely, and it MUST NOT delete a tree it has not verified.

Implementations that keep no state inside the hub directory are unaffected. This constrains
where state may live as much as how it is written: putting a writer's state inside the
migrated tree is what creates the requirement.

**What this does and does not enforce.** The requirement is enforced at acquisition: a process
that takes the lock cannot have a migration run underneath it, and a migration refuses while
any holder lives. It is **not** enforced at write time, and cannot be — a descriptor follows
the inode, so a writer that never took the lock is invisible to it. Two consequences follow
and both are accepted, not overlooked. A violator that re-resolves its path after the freeze
has its write **orphaned** in a recreated directory; a violator holding a descriptor from
before the freeze has its write **destroyed** with the frozen tree. Neither is reachable from
agentchute's own code paths, which acquire the lock at a single seam; both remain reachable
for a binary older than this contract and for any process outside agentchute. The structural
repair is to keep lane state out of the hub-id-keyed directory entirely, so that a URL change
renames nothing a writer can hold open.

### 13.10 Error-code registry

Wire frame: `{"t":"error","re":N,"code":"E_…","msg":"<human text>","retriable":false}`

**`claimed_held`** is an optional boolean, top-level on the `error` frame. It is encoded **only when `true`**; its absence means no claimed residue is held. Absence is `false` by definition, not by inference, and is not a malformed frame. When `true`, the operation failed **and** claimed mail is held on the hub for this actor, so the client must arm its guard latch even though it received no `msg` frames. A client that sees `claimed_held: true` arms its latch. The version handshake makes a mixed-version pair impossible, so this is a parsing rule, not a compatibility rule. With an always-present field, a hub that omits it produces a malformed frame and the client knows something is wrong; with presence-means-true, an omission is indistinguishable from a legitimate absence. **W6** covers that gap.

| code | emitter | meaning |
|---|---|---|
| `E_VERSION` | hub | protocol version mismatch (handshake) |
| `E_IDENTITY` | hub | `hello.agent` ≠ pinned key id (handshake) |
| `E_POOL_NOT_FOUND` | hub | forced command's `--pool` invalid hub-side (session start) |
| `E_NOT_REGISTERED` | hub | actor has no registration row — two exact texts, selected client-side by call site |
| `E_RECIPIENT_UNKNOWN` | hub | no row for `to` |
| `E_RECIPIENT_UNREADABLE` | hub | row exists, unparseable |
| `E_RECIPIENT_STALE` | hub | preflight stale |
| `E_RECIPIENT_RACING` | hub | fresh-then-stale under lock |
| `E_FENCED` | hub | serve token check failed |
| `E_LEASE_HELD` | hub | fresh lease owns the id |
| `E_HUB_IO` | hub | hub filesystem error |
| `E_MALFORMED_FRAME` | hub | framing violation (session closes) |
| `E_TOO_LARGE` | hub | frame >64 KiB or body >4 MiB |
| `E_UNSUPPORTED` | hub | unknown `t` (session survives) |
| `E_ORDER` | hub | request out of order (session survives) |
| `E_POOL_ID_INVALID` | hub | `state/pool.id` fails the regular-0600 / `[0-9a-f]{12}`+LF contract (session start) |
| `E_UNPINNED` | hub | the session was reached with no `authorized_keys` forced command, so the agent id and pool were chosen by the caller rather than pinned by sshd (session start; the hub refuses to serve) |
| `E_POOL_MISMATCH` | **both** | this key is not serving the pool it is supposed to serve |

**`E_POOL_MISMATCH` is emitted by both sides**, deliberately one code with two emitters and two exact texts — not client-only:

- **Hub-emitted, at session start:** `hub session` re-reads `state/pool.id` from the actual `--pool` and refuses when that value is absent or ≠ `--pool-id`. Exact text: `hub: this key is authorized for pool id 9c4e12ab77f0, but /home/alex/code/agentchute on the hub reports pool id 41d2c8ab0917 (or has no state/pool.id at all). The authorized_keys line's --pool was edited without its --pool-id, or the pool directory was replaced. On the hub, re-run: agentchute hub authorize --agent codex --replace-key --pool <the pool this key should serve> --key "<key>".`
- **Client-emitted, after `hello-ok`:** the client compares `hello-ok.pool12` against the value recorded in `config.json` at join and fails closed on inequality. Exact text: `hub: this key now serves pool /home/alex/other-pool (id 41d2…) on the hub, but this machine joined pool id 9c4e12ab77f0 (/home/alex/code/agentchute). The key line was re-pointed or the hub moved the pool. Re-join if the move is intended (agentchute hub join <url> --as codex), or re-authorize the key with the right --pool on the hub.`

The two arms are ordered and non-overlapping: the hub arm runs before `hello-ok` exists; the client arm only on a `hello-ok` the hub arm already let through.

Client-side only (never on the wire): `E_CONNECT`, `E_UNAUTHORIZED`, `E_HOSTKEY_CHANGED`, `E_CHANNEL_LOST`, `E_SEND_UNKNOWN`, `E_HELLO_TIMEOUT`, `E_HUB_NO_BINARY`, `E_HUB_UNPINNED`, `E_HUB_PINNING_UNVERIFIED`, `E_NOT_JOINED`, `E_NO_SSH`. (`E_POOL_MISMATCH` is **not** in this list.)

`E_UNPINNED` is hub-emitted and travels on the wire: a hub reached WITHOUT an
`authorized_keys` forced command refuses to serve, because the agent id and pool for that
session were chosen by the caller rather than pinned by sshd. `E_HUB_UNPINNED` is its
client-side counterpart, reported when the client's own probe establishes the same thing
about a host that never got as far as a session.

## 14. Namespace
State lives under the fixed `.agentchute/loop` directory. `AGENTCHUTE.md` is shared; reference-implementation notes live in `.agentchute/loop/README.md`. (Earlier drafts used a vendor-namespaced `.<vendor>/loop/` dotdir and a `.rehumanlabs/` legacy namespace; both are gone — the namespace is now fixed. `reHuman Labs` remains the maker's credit in `README.md`; that's brand, not a namespace.)

## 15. Security Considerations

agentchute operates under a **cooperative trust** model (as framed in `README.md` and `SECURITY.md`, which this section absorbs into the spec): the coordination channel is plain files on a shared filesystem with no cryptographic signing, so spoofing, tampering, and deletion of messages by co-tenant processes are out of scope — if you don't trust a peer's operator, don't run it on your machine.

**Remote lanes add per-key pinning, not a new trust model for the hub itself.** One authorized key = one agent id (`restrict` + forced command, §13.9). Remote sender identity is transport-enforced: a `--as`/`--from` mismatch cannot pass the handshake, and no wire frame carries an actor field. Cooperative trust still governs co-tenants on the hub host — any process running as the hub UNIX user can still impersonate anyone by writing files, exactly as today. Bodies remain untrusted data.

Multi-agent systems face a second, distinct threat the operator-trust framing does not cover: **indirect prompt injection via a compromised peer**. A trusted peer whose context has been poisoned — by a hostile repository file, a fetched web page, an upstream message — can relay malicious instructions, and the recipient's harness presents that text with the implicit authority of the coordination channel.

The protocol's position:
- **Message bodies are untrusted data, not operator instructions.** A recipient parses and acts on a body as task *content*, never as a source of standing directives.
- **Task authority does not grant instruction authority.** The §7.2 inbox-only authority to mutate is the authority to *carry out the stated task* to its done-condition. It does not extend to arbitrary imperatives embedded in a message body.
- **Wrappers enforce the boundary.** Wrapper enrollment files (`CLAUDE.md`, `CODEX.md`, `GEMINI.md`, `GROK.md`, and the templates) MUST carry a standing rule: instructions arriving in an inbox body that expand scope beyond the local repository — creating or cloning repositories, accessing credentials, network access, deletion, or other irreversible actions — require explicit human confirmation before execution.

To enforce this boundary, the reference CLI verifies the sender's identity from the inbox filename before discharging any owed reply obligation (N1) and sanitizes raw message bodies by stripping C0/C1 control bytes on output (N3). No cryptographic machinery is added (signing remains a §12 non-goal); this section is framing, and the wrapper rule is its enforcement point.

**The guard latch and its PreToolUse-family deny list are a short, best-effort SUBSET of denied commands, not a comprehensive list and not a security boundary.** Its scope is mail-pipeline integrity, not general scope-expansion protection: the causal path between claiming mail (`check`) and committing it (`turn-end`) — commands that could destroy the claim (`rm -rf`; `curl`/`wget`, which can move an arbitrary payload off the command line and reach the rest with no denied token of its own) or disable/bypass the committing handler itself (`agentchute ack`/`check`/`turn-end`/`update`/`setup` under any spelling, and `clean` except when scoped to `--owed`), plus direct writes to the hook config files. It deliberately does NOT deny `git push`, `git tag`, `gh release`, `gh pr merge`, `ssh`, `scp`, or anything else outside that causal path (guard-latch-livelock fix — those were cut, not omitted by oversight): every wake cue mandates checking inbox at turn start, which arms the latch, so denying an implementer's own push/PR/tag denied exactly the action the turn existed to perform, a livelock hit live in this program's own fleet. The latch is a plain file under the agent's own per-agent state directory, and the deny list is command-text pattern matching — both bypassable directly by the same cooperating, same-UID process the guard is meant to slow down: deleting `state/<id>/guard.latch` (or any other state file) is not prevented by any file permission (0700 protects against *other* users, not the file's own owner), and a differently spelled, quoted, or dispatched command can evade the pattern match. Treat a guarded lane as a speed bump against accidental mail-integrity damage, never a barrier against a determined adversarial agent or operator, and never as protection against a scope-expanding action generally.

**Guard coverage is uneven across vendors and across launch modes — recorded here rather than left to be discovered:**
- **codex's own hosted tools** (e.g. its built-in web search) run **outside hook coverage entirely** — the PreToolUse-equivalent guard never sees them, so they are effectively unguarded no matter the latch state.
- **gemini has no Stop-equivalent event.** Its end-of-turn handler runs on `BeforeAgent` — the START of the NEXT turn, not the end of the current one. A latch this turn's `check` set stays armed into whatever begins next; §9's turn-end ordering only ever archives what the CURRENT session itself claimed, so this is safe for the redelivery guarantee, but it is a genuinely different timing from the other two guarded vendors and should not be assumed away. Separately — and this predates the timing point above — gemini's guard event/decision JSON shape is UNVERIFIED against vendor docs as of this writing; treat a gemini lane as guarded-in-name-only until independently confirmed.
- **grok has no hook system at all** and is therefore unguarded, full stop: no latch is ever set for it, `ack` behaves exactly as it always has, and there is nothing to bypass because nothing is armed.
- **A session not launched under `agentchute serve` carries no serve token** (`AGENTCHUTE_SERVE_TOKEN` unset) and is therefore ALSO unguarded — identically to grok, regardless of vendor — because the guard requires both a live serve lease AND the vendor's own guard-enablement bit; a hand-run or otherwise unsupervised session has neither.
- **Sender routing rule:** never route irreversible or scope-expanding work (pushes, tags, releases, deletions, network access, credential access) on the theory that a lane's guard will catch a mistake — it structurally cannot, for ANY lane, guarded or not: the guard denies only the mail-claim/commit causal path (above), and never denied — or, since the guard-latch-livelock fix, no longer denies — scope-expanding actions as a class. Unguarded lanes (grok, any session not under `serve`, a codex hosted-tool call) additionally have zero mail-pipeline protection either. Routing judgment and the wrapper-enforced human-confirmation rule (this section, above) are the only controls on scope-expansion, for every lane alike.

**Mixed hook-trust recovery.** Some vendors independently gate hook definitions per-command and per-event: after a hook definition changes, one event (e.g. the PreToolUse-equivalent guard) may become active again before another (e.g. the Stop/end-of-turn hook that runs `turn-end`) is re-trusted, or the latter may simply fail at runtime. In that state a lane latches on its next claimed mail and stays latched — the guard denies a direct `turn-end` invocation too, by design, since allowing it would let a same-turn instruction clear its own latch and disarm the rest of the deny list. Remediation STARTS with repairing the end-of-turn hook path itself (re-trust or fix its definition) and confirming it actually runs; relaunching the lane (a new serve session mints a new token, so the old latch reads as foreign/inert and `check` redelivers the claimed residue under the fresh session) or removing `state/<id>/guard.latch` directly and immediately running `agentchute turn-end` to commit and gate are only durable AFTER that repair. Doing either WITHOUT first fixing the end-of-turn hook is a temporary unwedge, not remediation: the very next `check` claims new mail, writes a fresh latch, and the lane wedges again at the same boundary. (This spec does not assert, as a normative guarantee, that a vendor's own harness-invoked end-of-turn hook execution never itself re-enters the same PreToolUse-equivalent guard path. No per-vendor smoke test proving that non-reentrancy has been run or recorded as of this writing — it stays a REQUIRED, outstanding smoke-test item for each vendor's shipped hook template before this guard's coverage claim can be trusted, not a proven protocol invariant and not something already verified.)

**Shared GitHub credential (recorded, accepted risk).** Every agent in a fleet operating this protocol may authenticate to GitHub through the SAME underlying `gh` CLI login, with no per-agent GitHub identity — this is an operational choice the protocol itself has no way to prevent or detect. A visible consequence, observed in practice: `gh pr review --approve` self-blocks fleet-wide (GitHub treats repeat reviews from the same authenticated actor as one actor reviewing itself), which is why review verdicts in this program are delivered on the coordination bus and mirrored to `gh pr comment` — never `gh pr review` — when mirroring is authorized. More broadly, any agent with shell access can act on GitHub as fully as the human operator who owns that token can; nothing in the guard latch or the deny list is GitHub-specific protection. Operators who share one GitHub credential across a fleet are accepting this knowingly, not overlooking it — recorded here so it reads as a decision, not a surprise discovered later.

---

## Appendix B. Reference implementation hook templates

See `README.md` or `examples/hooks/` for current Claude Code, Codex, and Gemini CLI hook templates.

## Appendix C. Hand-protocol walkthrough

The hand-protocol is exclusively for environments without the reference CLI binary; an agent with the reference CLI available MUST use the CLI rather than driving files by hand. A hand-protocol agent SHOULD coordinate to keep a single writer per id and a monotonic per-`(from,to)` counter, exactly as walked through below. The hand-protocol carries no multi-writer protection or fencing mechanism; preventing write collisions is a best-effort coordination task for the operator.

**Divergence from the reference CLI (v2.5 plan B7):** this walkthrough's own counter-based grammar remains internally valid as a hand-driven mechanism — a hand operator who genuinely controls their own monotonic counter gets a real EEXIST-only-on-true-resend guarantee. But it is no longer what the reference CLI's own `send` does: `send` mints a timestamp+random-suffix identity per attempt (no counter, no idempotency key) and, on a collision, retries under a FRESH identity rather than treating `EEXIST` as "already landed." Reading this appendix as a description of the CLI's current wire behavior would be wrong; it describes a still-valid, but now CLI-divergent, alternative mechanism (see `EXTENSIONS.md`).

### C.1 Registration
Write `.agentchute/loop/agents/<id>.md`:
```markdown
---
agent_id: claude-code
vendor: anthropic
control_repo: /absolute/path/to/repo
host: macbook.local
last_seen: 2026-06-30T00:00:00Z
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
1. Rewrite your own `agents/<id>.md` registration with a fresh `last_seen` (§9 — presence is read from this row directly; there is no separate presence file to maintain).
2. List `inbox/<id>/` oldest-first by filename. Also re-handle any residue in `inbox/<id>/.claimed/` (uncommitted from a prior crash).
3. **Claim**: `mv inbox/<id>/<file> inbox/<id>/.claimed/<file>` (same canonical name).
4. **Act** on the claimed content. Make handlers idempotent (a crash before commit re-delivers).
5. **Commit**: `mv inbox/<id>/.claimed/<file> ../archive/<consumed-ts>_to-<id>_<file>`.
6. If the message replied to one of your `--ask` obligations, clear the matching entry in `state/<id>/owed.json`.

## Appendix D. Compatibility history

**Reconciliation in v0.11.8 — Covenants held despite prose growth.** While the stable covenants (§1, §6.1, §6.3, §6.4, §11.1, and conformance invariants) remained strictly frozen and unchanged across the v0.10.x and v0.11.x releases, the explanatory prose of this specification grew by approximately 20%. This growth represents the addition of security framing (§15), operational assumptions, and scope honesty boundaries (such as NFS/PTY limitations)—none of which altered the normative wire protocol.

**DONE in v0.11.0 — unspec'd priority frontmatter reader removed (clean delete).** The undocumented `priority` frontmatter field (previously dropped from the spec in v0.3.3) was removed from the CLI reader (`pending`/`boot` display and structs), eliminating obsolete dead weight and aligning the reader with the normative envelope schema (§6.4).

**DONE in v0.9.1 — dead shim-generation code removed (clean delete).** `renderShimScript` (the legacy per-wrapper shim generator) had zero production callers and moved to a test-only `legacyShimScript` fixture helper. `removeSetupAliasShimsForWrapper` was deleted outright (zero callers — the same-name alias cleanup is unreachable now that aliases are never installed). The misnamed `gitignoreBeginV1`/`gitignoreEndV1` constants (they held the current `v3` marker) were renamed `gitignoreBegin`/`gitignoreEnd`.

**DONE in v0.9.1 — deprecated no-op flags + `--wake` collapse removed (clean delete).** The `ac` dispatcher cutover is complete (live pool on v0.8.8+), so the accept-and-ignore `shims install --wrapper`/`--aliases` and `setup --aliases` flags were removed outright (passing them now errors), along with the persisted `aliases` field (setupPoolState/setupGlobalState) and update's `--aliases` re-pass. Separately, `--wake` collapsed to runner-only: the `tmux`/`herdr`/`both`/`all` aliases + set machinery (`wakeSetContains`, `normalizeSetupWake`'s multi-value parsing, `canonicalizePersistedWake`) are gone — any persisted legacy wake reads back as `runner`.

**DONE in v0.9.1 — `run` verb alias + redundant `default-id` command removed (clean delete).** Pulled forward from the v0.10.0 target: `serve` is now the only launch verb (the `"run": cmdServe` dispatch entry and its `COMPAT` marker are gone; `agentchute run` / `ac run <wrapper>` now error as unknown). `setupCommandMatchesRunnerPool` still ATTRIBUTES a live pre-v0.9.1 `agentchute run` supervisor for teardown only, so reset/wipe/update stop an orphaned old runner cleanly on upgrade (process attribution, not command support). `identity` is the single id-resolution command — the `"default-id"` alias was dropped (`identity` is what the enrollment docs use). Enrollment marker bumped v19 → v20.

**DONE in v0.9.0 — legacy-nonce inbox reader + writer removed (clean delete).** The one-release dual-read window is over: the live-bus gauge reported zero legacy `<ts>_from-<sender>_msg-<nonce>.md` files pool-wide, so both the reader (`inboxFilenameRE`/`inboxFilenameShapeRE`, the `InferSenderFromFilename` legacy branch, the `LegacyNonce` classifier + struct field, `ParseInboxFilename`, `CountLegacyNonce`) and the test-only writer (`WriteInboxMessage`/`generateNonce`/`formatInboxFilename`) were deleted. The canonical `from-<from>_seq-<020d>.md` is now the only inbox filename format; a stray legacy-named file is simply skipped as unrecognized (and quarantined by `check` like any other malformed name). The non-blocking `gate`/`doctor` drain gauges were removed with it.

**DONE in v0.9.0 — `message_id` frontmatter removed (clean delete).** The reference CLI's `send` no longer emits a `message_id` field, and the display-only readers (`boot`/`pending`/`sendResult`) were dropped — the canonical `(to,from,seq)` identity is surfaced via the message filename (`from-<from>_seq-<seq>`) and reply threading rides `in_reply_to` (the `(to,from,seq)` ref) + the asker-owned `.owed` ledger. A `message_id` on an older in-flight message is tolerated on read but never consulted for identity or reply discharge.

**DONE in v0.9.0 — the `.owed` redesign (clean delete).** The asker-owned `.owed` ledger is now the **sole** reply-obligation authority. The recipient-side `pending-replies.json` ledger AND the `defer` command were **removed outright** (recipients are never blocked at finish by a `reply_required` message; delivery is best-effort pull). Rationale: `RecordPendingReply` had zero production callers for two releases and the live gauge was zero, so keeping a recipient ledger/gauge was anti-subtractive; and with `defer` gone, any legacy recipient-ledger entry would have been permanently unclearable. Reply threading rides `in_reply_to` + `.owed` (see §6.6).
