# EXTENSIONS.md

*How to implement agentchute over a substrate or transport other than the one the reference CLI ships.*

The protocol ([`AGENTCHUTE.md`](AGENTCHUTE.md) §1) is medium-agnostic: it fixes a
small set of primitives and stays silent on *how* you realize them. The
reference CLI just picks the simplest concrete substrate — Markdown files on a
shared filesystem, delivered with a unique-temp + atomic `link()`-no-clobber.
Anything that preserves the protocol's semantics is a valid agentchute
implementation.

For a copy-pasteable filesystem walkthrough, see
[`AGENTCHUTE.md` Appendix C](AGENTCHUTE.md#appendix-c-hand-protocol-walkthrough).

The conformance vectors — seven core invariants (`R1`/`D1`/`D2`/`O1`/`C1`/`E1`/`B1`) plus the crash-safety vectors (`C2`, `Q1`) — *are* those semantics,
and [`conformance/`](conformance/) is the executable spec — the invariants as a
Go test suite driven against multiple substrate bindings. An implementation that
passes the suite is conformant; when this prose and the suite disagree, the suite
wins. The spec is the contract.

---

## What is no longer an extension surface

Earlier drafts of this document described pluggable *wake* mechanisms — tmux/herdr
adapters, a watchdog, sender-side pokes. They are gone. The 0.8 redesign made
coordination **pull-only**, and pull-only has no wake surface to extend:

- A sender's whole job is durable delivery — write the message into the
  recipient's inbox. It never pokes or wakes anyone.
- A recipient discovers its own mail by polling its own inbox. A loopless
  wrapper is polled by the reference CLI's runner, which is local to the agent it
  supervises, not a sender-reachable endpoint.
- Presence is a published `.live` freshness fact, not a wake target: fresh ⇒
  alive, stale/absent ⇒ not-alive.

So "plug in a new wake mechanism" is no longer a thing. The one genuinely
pluggable surface is the **inbox substrate/transport** and the **message
medium** — how a message is carried into a recipient's inbox and encoded there.

---

## The real extension surface — alternate transports and substrates

The inbox does not have to be a directory on a shared filesystem. Any substrate
that can give each recipient an ordered, private, no-overwrite inbox that the
recipient reads on its own can carry agentchute messages. Whatever the substrate,
an implementation maps the same primitives (§1):

- a **per-recipient inbox** the recipient alone consumes;
- message **identity = `(to, from, seq)`** — the sort key and the dedup key, with
  a durable, monotonic per-`(from, to)` `seq`;
- **no-overwrite delivery** — committing the same `(to, from, seq)` twice is a
  benign no-op;
- **pull** — the recipient reads its own inbox; senders only write;
- **presence** as a published freshness fact;
- **self-registration** — each agent publishes a small record naming itself.

The sketches below are brief on purpose. None ship in the reference CLI, and the
spec, not this list, is the contract.

### Message queue (e.g. SQS, NATS)

One queue or subject per recipient. Deliver by publishing to the recipient's
queue; get no-overwrite from message deduplication keyed on `(to, from, seq)`
(SQS FIFO deduplication id, NATS message id). The recipient consumes only its own
queue. `seq` gives per-sender FIFO; presence is a separate published record.

### Object store (e.g. S3, GCS)

An inbox is a key prefix per recipient (`inbox/<id>/`). Deliver with a
conditional put — `If-None-Match: *` / put-if-absent — under the `(to, from, seq)`
key, so a re-put of the same key is the no-overwrite no-op. The recipient lists
its own prefix to read; archive is another prefix.

### HTTP endpoint

Each recipient has an inbox endpoint. A sender POSTs a message; the server
enforces `(to, from, seq)` idempotency (a repeat is a benign 200/204, not a
second message). The recipient GETs its own inbox (poll or long-poll) and marks
each message consumed. ETag / `If-None-Match` is the natural no-overwrite
primitive.

### Git-backed

Each recipient has an inbox path or ref (e.g. a branch `agentchute/inbox/<id>`).
A sender writes the message file and pushes; the ref update is the atomic commit
point, and a rejected push (someone landed first) is the no-overwrite primitive
at commit granularity. The recipient pulls its own ref and reads oldest-first by
the `(to, from, seq)` filename. Cross-machine reach and an audit trail come for
free; you trade latency and finer-grained atomicity for it.

### Interoperability

A filesystem implementation interoperates with the reference CLI directly — it
reads and writes the same files under the same `.agentchute/loop` layout. Any
other transport is protocol-compatible but shares no bytes with the reference
CLI; it interoperates only through a shared-filesystem loop that both sides
mount, or through a bridge process that speaks both.

---

## Cross-pool bridges

Identity is pool-scoped: `(pool_locator, agent_id)` (§7.3). A single process may
participate in several pools at once — a **bridge**. The protocol needs no new
fields for this: per-pool registrations, and the reference CLI selects a pool
with `--control-repo` / `AGENTCHUTE_CONTROL_REPO` / the `.agentchute-control-repo`
pointer file (§4.1). The hazards are what matter:

- **Authority is not transitive.** A message addressed to the bridge in pool A
  authorizes the bridge to apply *its own* policy; it grants pool A's peers no
  authority in pool B. Acting in pool B is a NEW action under the bridge's pool-B
  identity, not protocol-level forwarding.
- **Authorization laundering (the inverse-firewall hazard).** The central risk: a
  low-trust pool A peer gets the bridge to do something in high-trust pool B that
  B's peers would not have accepted directly. Treat every cross-pool forward as an
  explicit policy decision — reject or transform, don't translate blindly.
- **Information leakage.** A bridge has full access to its own inboxes in each
  pool but no license to redistribute one pool's content, metadata, peer
  identities, or topology into another.
- **Loop amplification.** A → bridge-AB → B → bridge-BA → A can recirculate
  forever; use `in_reply_to` or correlation ids to recognize and stop a bridge's
  own forwarded requests.

---

## Not extension space

Some things are excluded by design or reserved for a future protocol version.
Don't ship a fork that adds these under the agentchute name:

- **Routing / role assignment / wildcard inboxes** (§7, §12) — agents are peers;
  senders address recipients explicitly.
- **Protocol-level signing or auth** (§12) — agentchute is cooperative-trust;
  layer a signed-envelope protocol above it if you need one.
- **Durable / authenticated audit trail** (§12) — archive is gitignored; use a
  layered protocol for durable transcripts. (The opt-in shared-`log` binding in
  [`conformance/`](conformance/) is the first-class audit profile — a v2 item.)
- **Coordinator / router agents** (§13) — reserved for v2.

Shipping the smallest thing that works is the point. Every adapter the reference
CLI doesn't carry is one less dependency and one less interpretation of "what
agentchute is." The protocol is portable on purpose; if you build something from
it, ship your fork or open a PR.
