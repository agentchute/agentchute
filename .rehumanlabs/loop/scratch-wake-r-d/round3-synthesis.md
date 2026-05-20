# Wake-method R&D synthesis (round 3) — proposal for Alex

Across three teams, nine candidates, two rounds of independent design + cross-review.
Convergent answer below. Sign-off in progress from codex + gemini.

---

## Framing (codex's anchor, accepted by all)

**Sender-side wake is physically impossible if the only shared thing is passive
mailbox storage AND no recipient-side process is alive.** Something must be running
on or near the recipient — either polling the mailbox or listening on a network
endpoint. Wake is best-effort; the inbox entry is the durable event.

This framing makes the design space orthogonal:

1. **What runs on the recipient side?** (watcher daemon, HTTP listener, SSH server, push subscriber)
2. **How does the sender trigger it?** (no-op + recipient polls, HTTPS POST, SSH command, pub/sub publish)

---

## Recommended shortlist

Three first-class options that span the friction/power spectrum. All share the
same wake-event metadata contract (defined below) so adapters interoperate.

### 1. Bounded recipient-owned watcher  *(baseline, mailbox-only)*

The honest answer when nothing remote can reach the recipient: a local daemon
polls the recipient's own inbox and triggers the configured action (notify
human / print / exec wrapper-relaunch) on each new arrival.

- **Recipient runs**: `agentchute watch --as <id> --notify|--print|--exec ...`
- **Registration**: `wake_method: ""` is the recommended baseline (senders correctly
  skip the wake adapter; delivery alone is the protocol move). A future advisory
  `wake_method: self-poll-v1` could surface "this agent is bounded-self-polling
  but expects no external wake" for diagnostic visibility — strictly advisory,
  not a normative wake adapter, since senders intentionally skip it.
- **Setup friction**: low. One long-lived process per recipient. Doctor can
  generate launchd/systemd unit files (gemini's `--generate-service` idea).
- **Cross-host**: works as long as the mailbox substrate is visible to the recipient's machine.
- **Optimization (v0.2 follow-up)**: fs-events / kqueue / inotify on the inbox
  dir for sub-second responsiveness on single-host or local-FS setups. Falls back
  to polling on substrates without kernel events. Implementable via direct syscalls
  to keep agentchute deps-free.
- **Failure mode**: recipient daemon stopped → mail waits in inbox until restart.
  SessionStart hook on wrapper relaunch re-syncs via `boot`.

### 2. Direct HTTPS wake endpoint  *(cross-host primary, `http-post-v1`)*

The recipient runs a tiny HTTPS listener; senders POST a minimal metadata-only
wake event. All three teams independently proposed this; strongest consensus.

- **Recipient runs**: `agentchute serve --as <id> --listen <host:port>`
- **Registration**:
  ```yaml
  wake_method: http-post-v1
  wake_target: https://recipient.host/.well-known/agentchute/wake/<id>?kid=<key-id>
  ```
  `kid` is a key identifier, never the secret itself. The actual HMAC secret
  lives in local config (file, env var, credential store) keyed by `kid`.
- **Wake event payload** (the wire contract — see §"Generic WakeEvent" below):
  ```json
  {
    "type": "agentchute.wake.v1",
    "pool_id": "<canonical pool identifier>",
    "to": "claude-code",
    "from": "codex",
    "delivery_id": "<filename or substrate-native delivery key>",
    "message_id": "<frontmatter message_id, optional, for logs only>",
    "sent_at": "2026-05-20T14:41:00.123456Z",
    "event_id": "<derivable: sha256(pool_id|to|delivery_id)>"
  }
  ```
- **Auth**: HMAC over (method + path + query + sent_at + body bytes) using the
  secret named by `kid`, OR mTLS. Recipient rejects stale or materially-future
  `sent_at`; sender signs the timestamp as part of the HMAC input. Replay is
  harmless (extra scan); recipient keeps an LRU dedup cache keyed on
  `delivery_id`.
- **Setup friction**: medium. Open a port, manage cert/tunnel, provision the HMAC
  secret. Tailscale / WireGuard / Cloudflare Tunnel collapse the setup friction
  significantly; the README's "Reachability Guide" recommends them.
- **Cross-host**: real cross-host without a third party. NAT/firewalls solved via
  overlay network or tunnel; the protocol doesn't care.
- **Failure modes**: endpoint unreachable / 5xx / auth mismatch → sender logs
  `wake_result: failed (<reason>)`, message still in inbox.

### 3. SSH nudge  *(sibling cross-host adapter, leverages existing infra)*

When the operator already has SSH keys between machines (very common in
ops/personal-cluster setups), no new daemon protocol is needed.

- **Sender invokes**: `ssh <safe-fixed-args> <user@host> agentchute wake --event-json -`
  with the WakeEvent JSON piped on stdin. SSH adapter still uses the shared
  WakeEvent contract — the wire just happens to be stdin instead of HTTP body.
- **Recipient setup**: SSH key in `authorized_keys` with a **forced command**
  pinned to `agentchute wake --event-json -` (or a local wrapper that reads
  stdin and validates `pool_id` / `to` before triggering). Forced command
  prevents sender-selected arbitrary commands. Host-key verification mandatory;
  no `StrictHostKeyChecking=no` in docs.
- **Registration**:
  ```yaml
  wake_method: ssh
  wake_target: user@host[:port]
  ```
  No secrets in registration; relies on SSH agent / on-disk keys.
- **Setup friction**: medium-low if SSH is already wired between machines.
  Higher than HTTP if keys are not.
- **Cross-host**: excellent. SSH solves NAT in many setups via reverse tunnels;
  works across cloud / local / personal mix.
- **Failure modes**: connection refused / auth failure / remote binary missing
  → sender's wake_result captures the SSH stderr.

---

## EXTENSIONS.md recipes (not first-class adapters)

These are good recipes for specific contexts but don't earn protocol-text slots
because they don't introduce a new wake-event shape beyond what `http-post-v1`
covers, or because they're operationally niche.

- **ntfy.sh / Pushover / FCM** — pub/sub recipes that publish a wake event via
  `http-post-v1`-shape to a public push service. Recipient subscribes; service
  triggers a local shell command. Trade-off: no agentchute daemon to run, but
  metadata leaves the pool and the service is a third-party dep.
- **Relay / long-poll** (codex's `relay-v1`) — outbound long-poll from recipient
  daemon to a relay; sender POSTs wake events to relay. Solves NAT without
  inbound port. Useful when neither HTTPS nor SSH works (e.g., recipient on
  carrier-grade NAT with no overlay). Keep relay contentless and non-routing.
- **fs-poke** (gemini's original Cand 1) — sender writes empty
  `.pokes/<delivery_id>.poke` into the shared substrate; recipient's watcher
  monitors `.pokes/`. Functionally redundant with recipient just watching the
  inbox itself; useful only if `.pokes/` polling cadence can be very tight while
  inbox polling stays relaxed. Document as a recipe, not a normative adapter.
- **WoL / cloud-init** — wake a powered-off host. Out of scope for v0.2; mention
  as a future direction.

---

## Generic WakeEvent contract  *(proposed AGENTCHUTE.md §8 addition)*

A new normative subsection that defines what any network-based wake adapter
sends, so adapters can be added without re-litigating the wire shape every
time.

> **§8.x WakeEvent metadata contract.**
>
> When a wake adapter dispatches a network or RPC notification to the
> recipient's declared `wake_target`, the payload SHOULD be a `WakeEvent`:
>
> ```
> type:        "agentchute.wake.v1"
> pool_id:     <canonical pool identifier>
> to:          <recipient agent_id>
> from:        <sender agent_id>
> delivery_id: <filename or substrate-native delivery key — the §6.1.1 identity tuple>
> message_id:  <optional frontmatter message_id, for logs only>
> sent_at:     <RFC3339 UTC, microsecond precision>
> event_id:    <derivable; sha256(pool_id|to|delivery_id) recommended>
> ```
>
> A WakeEvent carries metadata only. Message bodies MUST NOT travel in the wake
> event. Wake delivery is best-effort and non-authoritative; the durable
> protocol move is the inbox entry per §6.1.
>
> Recipients MUST NOT dedupe by `message_id` (which per §6.4.1 is for reply
> chains and is not delivery-unique). Repeated WakeEvents for the same
> `delivery_id` MUST be idempotent. Implementations SHOULD dedupe local wake
> actions by `delivery_id` and MAY still scan the inbox, which remains
> authoritative. Receivers MUST scan their own inbox oldest-first; the
> WakeEvent is a hint, not an ordering signal.
>
> Wake failures are local operational events, never §11 protocol-correction
> triggers.

The tmux adapter remains the degenerate "type 'check' into the recipient's
pane" case and doesn't carry a WakeEvent — it doesn't need one because the
recipient's wrapper sees keystroke input. Other adapters (HTTP, SSH, relay,
pubsub) all use the WakeEvent contract.

---

## What ships in v0.2

If we commit to building this:

- **Reference CLI gets**:
  - `agentchute serve` — the HTTPS listener daemon for `http-post-v1`.
  - HTTP wake adapter in `loop.PokeWakeTarget` (recognizes `wake_method: http-post-v1`).
  - SSH wake adapter (`wake_method: ssh`) — argv-only, no shell-eval.
  - `doctor` gains `--generate-service` to emit launchd/systemd units for the watcher.
  - Optional: fs-events optimization in `watch` if we can pull it off deps-free.
- **AGENTCHUTE.md gets**:
  - §8.x WakeEvent metadata contract.
  - §8.y http-post-v1 adapter normative shape.
  - §8.z SSH adapter normative shape.
- **EXTENSIONS.md gets**:
  - ntfy.sh / Pushover / pub/sub recipes.
  - Relay/long-poll recipe.
  - fs-poke recipe.

Three new adapters total (HTTP, SSH, optional FS-events recipient-side speed
boost). The protocol grows by one structured wake-event contract that all
future adapters share. The reference CLI stays small.

---

## Open questions for Alex

1. **fs-events dep**: should the FS-events optimization use `fsnotify` (commonly
   accepted single-dep) or stay deps-free with per-OS syscalls (more code, no
   dep)? The repo's current norm is deps-free.
2. **`agentchute serve` lifecycle**: does it ship as a separate command, or fold
   into `agentchute watch --listen <addr>`? Codex leans separate; gemini leans
   folded. My read: separate is cleaner for the spec, folded is friendlier UX.
3. **Pool identity (`pool_id`)**: today the loop dir defines a pool implicitly.
   For `http-post-v1` to authenticate cross-pool, we need a canonical pool
   identifier in registrations. **Recommendation (consensus from codex)**: an
   opaque UUID generated at `agentchute init` time and written to
   `.agentchute-pool` at the loop dir root. Path-hash options leak local
   filesystem shape and would differ across machines for the same logical
   pool. Operator-supplied IDs remain available for migration/import.
4. **Scope for v0.2**: ship all three adapters (HTTP + SSH + watch optimization),
   or just HTTP + watch optimization, with SSH following in v0.2.x?
