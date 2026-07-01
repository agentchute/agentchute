# agentchute protocol — v2 deltas (RFC)

**Status:** Draft for maintainer decision
**Scope:** the **wire/spec only** — message identity, envelope, ordering, presence, registration. Not the runtime (that is the separate "simple again" redesign + spike).
**Relationship:** orthogonal to the redesign, with **one coupling** (presence ↔ §5 fork). Decide them in the order given there.
**Reviewed without baggage:** this critiques the redesigned protocol too, not only the shipped one.

**Verdict:** the protocol's real problems are not missing features — they are an **ordering guarantee it cannot keep** and an **envelope carrying application concerns**. Most of this is deletion. One robustness flip and one genuine fork are the only things that add.

---

## 0. The one decision (read first)

**N private inboxes vs. one shared append-only log + per-agent cursors** (§5). It is the difference between *patching* the inbox model's broken ordering and *adopting* a model where ordering and presence come free. Decide this before the refinements — several of them dissolve under the log.

---

## 1. Cut (lying or redundant)

- **The cross-sender total-order guarantee — remove it.** "Process oldest-first by sender timestamp" assumes a shared clock that does not exist across senders. The shipped spec already contradicts itself: §6.1 orders by filename timestamp; §10.1c orders by *arrival* mtime, "NOT the filename timestamp." Replace with the honest guarantee:
  - **Per-sender FIFO is guaranteed.**
  - **Cross-sender order is arrival order** — substrate-defined, **advisory**, not a wire guarantee.
  - Sender timestamp becomes a **label**, not truth.
- **`to` (in the envelope) — delete.** The message is already *in* `inbox/<to>/`. Duplicate state that can disagree with location.
- **`message_id` — delete; collapse into substrate-confirmed identity.** A message's identity is what the substrate accepted on delivery (the committed filename / offset), not a second sender-asserted handle. One identity concept, not two.
- **`task` and `status` (`request|findings|signoff|request-changes|info`) — move to the body as convention.** A coordination bus must not enumerate a review-workflow vocabulary on the wire. Application state lives in the body.

**Result — the entire normative envelope:** `from`, optional `reply_required`, optional `in_reply_to`. (Down from ~7 fields + `message_id`.)

---

## 2. Fix (robustness the spec currently leaves to luck)

- **Flip obligation ownership to the asker, with a timeout.** Today `reply_required` obligates the *recipient* and the gate enforces the recipient side — so if the recipient dies, the asker waits forever and nothing notices. Make the authoritative record the asker's: *"I am owed a reply to `<key>` from `<id>` by `<T>`."* A dead recipient then surfaces as the **asker's expired obligation**, not silence. Recipient-side `reply_required` becomes an advisory hint; correctness no longer depends on the recipient being alive.
  - Principle: **the party harmed by non-fulfillment owns the obligation.**
- **Make two invariants normative (currently reference recipes):**
  1. **Atomic visibility.** A message becomes visible **only when fully written**, via an atomic commit (tmp-then-rename, ref CAS, ETag PUT). No reader ever sees a torn message. *(Same rule applies to presence writes.)*
  2. **At-least-once + idempotent consume.** At-most-once silently drops coordination messages — the worst failure for this bus. Consume MUST be act-then-commit (re-deliverable on crash); handlers MUST tolerate a repeat.

---

## 3. Add (only these — each tied to a failure)

- **Optional `msg_key` (idempotency key), distinct from the nonce.** No-overwrite stops *delivery* duplicates; it does nothing for *crash-retry* duplicates (sender resends after a crash, unsure the first landed → two messages, two nonces, one logical event). `msg_key` lets the receiver dedup logically. Optional, small, real.
- **A one-token `v:` version field on registration.** Ignore-unknown-fields (good, keep) makes a future breaking change parse *silently wrong*; a version tag makes it **detectable** for mixed implementations. Cheap insurance.

That is the whole addition list. Resist more.

---

## 4. Reframe (for universality)

State the protocol as **abstract operations + invariants, with the filesystem as one binding** — not as filesystem mechanics with other transports demoted to "sketches." Inverting this makes git / HTTP+ETag / Redis Streams / queue **equal bindings**, and is *simpler* because the rules are stated once.

Core (see Appendix A for the testable form):
```
register(id)                 publish existence
deliver(to, msg)             atomic, no-overwrite
poll(id) -> ordered          per-sender FIFO; cross-sender = arrival order
consume(id, msg)             at-least-once, idempotent
presence(id) -> {alive, last_seen}
```
**Presence is an abstract "recipient-published liveness fact with a freshness timestamp."** `.live` is its filesystem binding; a TTL key or a heartbeat endpoint is another. Do not bake the file into the protocol.

---

## 5. The fork (the headline call)

**N private inboxes** vs **one shared append-only log + per-agent cursors.**

| Dimension | N inboxes (today) | Shared log + cursors |
|---|---|---|
| Cross-agent order | clock fiction (§1) | **real** — one stream, one order |
| Presence | needs `.live` | **free** — last cursor advance = `last_seen` |
| Torn-read class | exists (mitigated by atomic write) | **gone** — append-only |
| Audit / replay | none (archive gitignored) | **free** |
| Substrate fit | FS dir; git/HTTP need mapping | append file / git branch / Redis Stream / Kafka — **clean** |
| **Recipient-owned bodies** | **yes** | **no** — everyone can read every message |
| Per-agent state | none | a read cursor each |

- **Recommendation, conditioned:** for a **single-owner, co-located pool of trusted agents**, inter-agent privacy is usually a non-requirement — so the **log is simpler, more universal, and more robust**, and it deletes both the ordering lie and the separate presence primitive in one move. Keep N inboxes only if **privacy between agents** is a real requirement (multi-tenant, bridge/proxy across trust boundaries).
- **Coupling to the redesign:** if you adopt the log, **presence falls out of cursors → `.live` and `serve`'s presence-writing go away.** This is the one place the two RFCs interact; decide the fork first.

---

## 6. Where the panel splits (honest, unresolved)

- **Keep vs cut `reply_required`.** The minimalist calls it application and cuts it. The coordination view keeps it (asker-owned) — it is the *one* cross-agent state that justifies a coordination bus over a plain mailbox. **Sided with keep.**
- **Privacy purist vs log pragmatist.** The purist will not trade recipient-owned bodies for the log's wins; the pragmatist says privacy was never a requirement for single-owner pools. **Genuinely unresolved without your target deployment** — hence §0.

---

## 7. Migration / compatibility

Additive-and-subtractive, safe under ignore-unknown:

1. Stop *emitting* `to`, `message_id`, `task`, `status`; keep *accepting* them for a release.
2. Restate the ordering guarantee (per-sender FIFO + advisory arrival order); align the watchdog to the same clock.
3. Declare the two invariants normative; confirm the reference CLI already meets them.
4. Flip obligation ownership to the asker; keep recipient `reply_required` as advisory.
5. Add optional `msg_key`; add `v:` to registration.
6. **Then** decide §5. If log: introduce it as a new binding alongside inboxes, migrate, retire `.live`.

---

## Appendix A — Normative core (conformance checklist)

A conforming binding (FS, git, HTTP, Redis, …) MUST satisfy:

- **R1 register/presence:** an agent publishes its existence and a monotonically refreshed `last_seen`; absence or stale `last_seen` ⇒ not-present. Readers MUST NOT treat *absent* freshness as a hard "dead" without a liveness fallback.
- **D1 deliver atomic:** a delivered message is visible only when fully committed; partial writes are never observable.
- **D2 no-overwrite:** delivery never replaces an existing message; identity collision ⇒ sender retries.
- **O1 order:** per-sender FIFO MUST hold. Cross-sender order is arrival order and is advisory.
- **C1 consume once-plus:** consume is at-least-once; a crash mid-consume re-delivers. Handlers MUST be idempotent (aided by `msg_key` when present).
- **E1 envelope:** receivers MUST ignore unknown fields; senders MUST NOT redefine the three normative fields (`from`, `reply_required`, `in_reply_to`).
- **B1 bodies:** under the inbox model, only the recipient reads its own message bodies. (Void under the shared-log model — state which model the binding implements.)

These are testable; a conformance suite over them lets any substrate claim agentchute compatibility on equal footing.

## Appendix B — before / after

**Message envelope**
```
before: message_id, from, to, in_reply_to, reply_required, task, status   (+ identity tuple)
after:  from, reply_required?, in_reply_to?, msg_key?                       (identity = substrate-confirmed)
```

**Registration**
```
before: ~18 fields (id, vendor, host, wake_method/target, last_seen, status,
        restart_at, last_active, reachable_at, reachability_*, launched_by,
        shim_name, hook_event, reserved wake_endpoints, control_repo, working_repos)
after:  id, last_seen, v   (+ optional advisory: vendor, host)
        presence/liveness fact is the abstract primitive; wake state is gone (recipient-pull)
```
