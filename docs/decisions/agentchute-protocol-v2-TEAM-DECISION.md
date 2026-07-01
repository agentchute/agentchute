# Team Decision Record — agentchute wire protocol v2

**Status:** DECIDED. 4-way bus consensus 2026-06-30 (claude · codex · grok · gemini), two rounds. Input for the **RFC author** — records the decision, the evidence, and the implementation constraints. Not itself the RFC.

**Scope:** the **wire/spec** (message identity, envelope, ordering, presence, registration). Orthogonal to the runtime "simple again" decision (`agentchute-simple-again-TEAM-DECISION.md`), with one coupling — the §5 presence fork — **now resolved here** (see §0).

**Source RFC:** `proposal/agentchute-protocol-v2-deltas.md`. **Proof:** the conformance harness (`proposal/spike-conformance_test.go` + `spike-log_binding.go`), run 14/14 on both bindings. ⚠️ The shipped harness was **incomplete** (only the test + log binding; missing the inbox binding, the `Binding`/`Msg`/`Deduper` types, and `cmd/acdemo`, and `agentchute-spike.tar.gz` is the *old runtime spike*). The team reconstructed the missing pieces from the test contract to run it; **get the complete harness from the author or treat the reconstruction as canonical.**

---

## 0. The headline result: the §5 fork is a FALSE BINARY

The RFC framed §5 as "N private inboxes **vs** one shared append-only log." The team's decision is that this is **one substrate under two orthogonal config knobs**, not two models:

- **Substrate (one):** a directory of immutable per-record files, committed by **atomic `link()`-no-clobber**. This is the NFS-safe primitive the shipped code already uses (`internal/loop/inbox.go:222-228`, `:470-478` → `os.Link`, `EEXIST`; Maildir/dotlock-proven on network mounts).
- **Knob 1 — read-domain:** `private` (per-recipient dir) | `shared` (one dir all read).
- **Knob 2 — seq-source:** `per-writer` | `global-CAS`.
- A **capability declaration** (`ordering`, `body_visibility`, `presence_mode`) names the corner. The **7-invariant conformance suite (R1/D1/D2/O1/C1/E1/B1) IS the spec** — any substrate that passes it is conformant.

**Coupling to the runtime, now resolved:** presence stays **`.live` + serve pid**, *not* cursor-derived. So the runtime decision's `.live` is **no longer provisional — it is final.** (The runtime record's coupling caveat is discharged.)

---

## 1. The decision

| # | Decision |
|---|---|
| **A** | **One substrate:** directory of immutable per-record files via atomic `link()`-no-clobber. NFS-safe; already shipped. |
| **B** | **Two orthogonal knobs + capability declaration:** read-domain {private\|shared} × seq-source {per-writer\|global-CAS}, body_visibility/presence_mode. Conformance suite = spec. |
| **C** | **Default reference profile (ships):** private read-domain + per-(sender,recipient) seq + `.live`/serve presence. NFS-safe, runtime-compatible, no false-dead. |
| **D** | **`shared-log` = first-class profile** (shared read-domain ± global-CAS, or git-branch/Redis) — recommended for trusted pools wanting audit/replay, **only on a substrate with real sequencing/CAS**; never the forced default over a plain mount. |
| **E** | Identity, ordering, obligation, envelope, invariants — see §2–§5. |

This **subsumes** all three round-1 positions: codex's binding-abstract/profile direction (sharpened into orthogonal knobs), gemini/grok's log (preserved as the `shared`+`global-CAS` corner), and resolves the two real defects the bus surfaced (NFS-safety; false-dead presence).

---

## 2. Identity & ordering — the load-bearing change

- **Identity = the full committed delivery key `(to, from, seq)` / canonical filename** (codex amendment, unanimous). **Not** a bare seq; **not** a sender-provided `message_id`. The code already treats `message_id` as non-delivery-unique and trusts `OriginalFilename` as the obligation key (`internal/loop/ledger.go:168-183`).
- **Per-(sender,recipient) `seq` REPLACES the `crypto/rand` nonce** as both sort key and identity. Today the nonce is random (`internal/loop/inbox.go:197-201`) and doubles as the lexicographic tiebreaker (`:333-334`) → two same-microsecond same-sender messages sort **randomly = a live O1 violation in shipped code.** A durable per-(sender,recipient) seq fixes this exactly.
- **One move collapses four concerns:** (a) seq IS the substrate-confirmed identity → `message_id` deletion by construction; (b) seq is the sort key → exact per-sender FIFO with **no clock**; (c) `link()` `EEXIST` now means "this exact message already landed" → the delivery-duplicate half of C1 folds into the substrate (a crash-uncertain sender re-sends the same seq → guaranteed no-op), and the **NFS lost-reply hazard** (link applied, ack dropped, client retries → spurious `EEXIST`) becomes a **non-event** (`EEXIST` = success).
- **Clock-free ordering:** seq is the *only* order key, so no ordering path touches wall-clock. The two-clock contradiction — spec orders by filename timestamp (`AGENTCHUTE.md:16`, `:120-130`) while the watchdog uses mtime (`:207`) — **collapses.** Cross-sender order is **advisory arrival order** (per O1, non-normative); the only residual cross-host clock comparison is presence-freshness + obligation-timeout (NTP-loose).
- **`seq` scope MUST be per-(sender,recipient)**, not global per-sender — else `(bob,6)` aliases across two recipients and breaks `EEXIST` idempotency.

## 3. Obligation flip (§2 of the RFC) — ADOPT

- Flip reply obligations to **asker-owned with a timeout**: "I am owed a reply to `(to,from,seq)` from `<id>` by `<T>`," held as an **asker-local `.owed` ledger** (single-writer, atomic rename). The gate reads only its own ledger; it never scans peers.
- A dead recipient surfaces **twice over** — the asker's expired obligation **and** the recipient's stale `.live` in the serve roster — so the gate never deadlocks on a corpse.
- Recipient-side `reply_required` becomes an **advisory hint**; **keep it on the wire** (the one cross-agent state that justifies a coordination bus over a plain mailbox). This is the **only load-bearing ADD**.

## 4. Envelope & registration (§1) — ADOPT the cuts

- **Delete:** `to` (location encodes it), `message_id` (= substrate `(to,from,seq)`), `task`/`status` enum (→ body convention), the `crypto/rand` nonce as identity/sort key (replaced by seq; an optional hex tail may survive only as a pre-link temp disambiguator, never consulted).
- **Normative envelope = `from`, `reply_required?`, `in_reply_to?`** (in_reply_to references `(to,from,seq)`) + optional `idempotency_key` (logical dedup only) + `v:` on registration.
- **Amendment:** do **not** make `from` structural via a stream path — that bakes FS layout into wire identity and breaks the §4 universality goal. `from` stays an explicit field (parsed from the filename in the FS binding, abstract in the protocol).
- **Registration** drops to `id, last_seen, v` (+ optional advisory `vendor`, `host`); the ~18-field wake cluster is gone (recipient-pull).

## 5. Invariants — all 7 normative

`D1` atomic visibility (tmp → fsync → link → fsync-dir; the fsync-before-link is load-bearing). `D2` no-overwrite (intrinsic to `link`; `EEXIST` = already-delivered). `O1` per-sender FIFO **exact** via seq, cross-sender advisory — the team **rejects** upgrading O1 to a "real total order" (falsifies on NFS — a global next-seq needs a freshness CAS the mount can't cheaply give; and it's unneeded). `C1` at-least-once + idempotent — **flip `check.go` from archive-at-display (`check.go:197-223`, at-most-once = the worst bug for this bus) to act-then-archive** + `.consumed` high-water + `EEXIST` delivery-dedup. `R1` presence = `.live` + serve pid fallback, **not cursor-only** (cursor advance proves read progress, not a live wrapper; a busy native-loop agent in a long turn would read **false-dead**, the dangerous direction; the poller already declines to refresh `last_seen` when health is unproven — `poller.go:195-208`). `E1` ignore unknown fields, `from` required, `v:` for breaking-change detection. `B1` privacy **HOLDS** under the default private read-domain — kept because it is **free** and buys blast-radius containment (trusted ≠ uncompromised: a context-poisoned or looping agent must not ingest the whole bus), not because privacy was demanded.

---

## 6. Implementation notes (for the RFC/impl — not decisions to relitigate)

- **Seq allocator is real work:** per-`(from,to)` lock/state + `link`-`EEXIST` retry so concurrent same-id sends cannot duplicate or skip into ambiguity.
- **C1 flip is a real behavior change** (`check.go`), not wording: spec currently says archive/consume before act (`AGENTCHUTE.md:133-140`, `:282-286`); code records the obligation then archives during display (`check.go:197-223`).
- **O1 fix** replaces the random-nonce lexicographic sort (`inbox.go:333-334`) with the seq key.

## 6b. id-uniqueness enforcement — the dup-writer fix (decided 2026-06-30, claude+codex)

The per-`(sender,recipient)` seq design is only sound if **one live process owns an id at a time** (a second live writer under the same id breaks per-writer-seq). The team decided **how** that guarantee is produced — a gating question for the seq allocator:

- **`serve` enforces id-uniqueness at launch via a decentralized shared-FS lease — NOT a pool-level allocator.** A pool allocator would reintroduce the central broker the runtime decision explicitly rejects (`proposal-2 §7`: "no central runtime/shared authority ... must not become a broker"). `serve` is the right ownership point — it launches the child under PTY and injects `AGENTCHUTE_AGENT_ID` (`run.go:242-245`); its current collision guard (`run.go:238`, `:402-416`) is same-host only (`processAlive` + socket), insufficient for shared mounts.
- **It must be a lease/fence protocol, not a best-effort `.live` freshness check.** The claim is a `state/<id>/claim` file (or a claim section in `.live`) carrying `{id, host, pid, serve_token, started_at, last_seen}`, acquired atomically via `link`-no-clobber.
  - A **fresh** valid claim ⇒ a second `serve --id X` **fails closed**.
  - A **stale** claim is reclaimable only via the R1 liveness rule: stale `.live` **plus** failed same-host pid/socket proof when same-host; cross-host can use only freshness/timeout (PID proof is local).
- **Fencing token (the load-bearing addition, codex):** the seq allocator records the active `serve_token`/epoch; **every heartbeat and every per-`(from,to)` seq write verifies it.** A stale holder that resumes after its lease was reclaimed fails on token mismatch — so a zombie/paused process cannot create a dup-writer *even though launch was guarded*. Without the fence, launch-time guarding alone does not close the hole.
- **Acceptance:** (1) launching `serve --id X` while a fresh valid claim exists fails closed; (2) reclaim succeeds only after stale-by-threshold + same-host liveness failure (where applicable); (3) every heartbeat and per-`(from,to)` seq allocation verifies the claim token/epoch; (4) tests cover simultaneous same-id launch, stale reclaim, stale-holder token mismatch, and normal distinct ids.

This upgrades CLAUDE.md's "give EACH process its own id" from convention to an **enforced, fenced** invariant — now load-bearing for the protocol, not just operability.

## 7. Stated assumptions (operational, not protocol guarantees)

- **dup-writer** (one id, two live processes on a multi-host mount via partition/failover/zombie) is **no longer a bare assumption — it is enforced** by the serve lease + fencing token in §6b (id-uniqueness confirmed 4/4: claude+codex+grok+gemini). What remains an operational assumption: the lease state directory lives on the same shared mount as the inboxes, and **NTP-loose clocks** for the cross-host staleness/timeout comparison. Concretely (gemini): size **lease-timeout ≫ heartbeat-interval + max-expected-skew** (e.g. 10s / 1s / 2s). Severe skew degrades to premature reclaim (stealing a still-valid lease) or delayed reclaim (a brief dup window) — but the **fencing token still prevents an actual dup-WRITE** in both cases; same-host pid/socket proof tightens it locally. Operational mitigation (tune thresholds, monitor), not a protocol-level change.
- **NFS attribute/dir caching** = delivery-visibility + `.live` latency up to `acregmax` — **latency, not correctness.** Tune `actimeo` low on the loop dir; set presence stale-threshold > `acregmax`; accept seconds-scale handoff latency. Ordering needs no synchronized clock; presence-freshness + obligation-timeout need only NTP-loose sync.
- **Archive growth** if audit/replay is enabled — default OFF (bounded by act-then-archive consume); if on, ship a documented prune/rotation policy. Do **not** adopt a min-cursor compaction janitor (its watermark is pinned forever by any dead agent's frozen cursor — re-breaking the R1 dead-mailbox case).

## 8. Migration (additive-and-subtractive, safe under ignore-unknown)

1. Stop *emitting* `to`, `message_id`, `task`, `status`; keep *accepting* them for a release.
2. Introduce the per-(sender,recipient) seq allocator; make `(to,from,seq)`/filename the identity; restate ordering as per-sender FIFO + advisory arrival; align the watchdog to the same key.
3. Declare the 7 invariants normative; flip `check.go` to act-then-archive (idempotent handlers).
4. Flip obligation ownership to the asker (`.owed` ledger); keep recipient `reply_required` as advisory.
5. Add `idempotency_key` (logical dedup only) and `v:` on registration.
6. Land the profile/capability declaration; default = private + per-writer. The `shared-log` profile ships when a real sequencer/CAS substrate is configured.

## 9. Out of scope / verdict trail

Out of scope (non-goals, not relitigated): auth/signing/encryption, routing/coordinator/wildcards. Per-lane round-1 and round-2 verdicts are in `.agentchute/loop/archive/` (protocol messages dated 2026-06-30 05:12–05:31Z). The claude (rater-A) position came from a 9-agent generate→stress→synthesize workflow over the real tree + the proof. **No code changes executed — decision/design only.**
