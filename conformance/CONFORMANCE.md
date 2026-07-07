# Conformance harness + shared-log model

Four deliverables in one package:

1. **Language-neutral vectors** — JSON scenarios under `vectors/` defining the
   protocol's invariant cases without depending on Go.
2. **A conformance harness** — the vectors run as a Go test suite against **two
   bindings** so any substrate is checked on equal footing.
3. **The shared-log model** (`log_binding.go`) — the §5 fork as
   working code, exercised by the same suite and demo as the inbox model.
4. **A disposable Python proof** — `example-python-binding/runner.py` reads the
   same vectors against a stdlib-only inbox binding. It is a snapshot, not a
   maintained SDK or release gate.

Pure stdlib, no dependencies.

## Run

```sh
go test -v ./...            # the invariant suite, against BOTH models
go run ./cmd/acdemo inbox   # narrated handoff on the N-inboxes model
go run ./cmd/acdemo log     # same handoff on the shared-log model
python3 example-python-binding/runner.py  # optional, disposable proof
```

## Why a harness (not just tests)

The §4 reframe says: **the invariants are the protocol; the substrate is a
binding.** This makes that real. The durable contract is the JSON vector set;
the Go suite is one runner for it. The suite drives only the abstract `Binding`
interface, so the filesystem inbox model and the shared-log model pass the
*identical* vector cases. A new substrate (git branch, Redis Stream, HTTP+ETag)
becomes conformant by implementing `Binding` and being added to `bindings()` —
then it inherits the whole suite. That is what "universal / multi-binding" has
to mean to be more than a claim.

## What each invariant proves (and the failure it catches)

| Test | Proves | Catches |
|---|---|---|
| **R1** presence | published fact with freshness; stale ⇒ not-alive | can't tell a live agent from a 4-days-gone dead mailbox |
| **D1** atomic visibility | a mid-delivery message is never observable | readers acting on a half-written message |
| **D2** no-overwrite | N concurrent deliveries all survive | a clobber silently dropping a message under load |
| **O1** per-sender FIFO | one sender's order is preserved; cross-sender is advisory | reordering a sender's own messages / claiming a false total order |
| **C1** at-least-once + idempotent | crash after act re-delivers; `msg_key` dedups | at-most-once consume losing a message on a crash |
| **E1** envelope | unknown fields ignored; `From` required | a future field breaking old receivers; an anonymous message |
| **B1** privacy | inbox keeps bodies private; log does not | — (this one *encodes the fork*, see below) |

`go test -v` shows R1–E1 simply passing on both models. **C1** is the
load-bearing one: it injects a crash at the worst moment (after the handler
acts, before the consume commits) and asserts re-delivery, then shows `msg_key`
collapsing the duplicate.

## Vector format

`vectors/core.json` is intentionally boring: one object per invariant or
review-gap case, with an `id`, a `kind`, and only the scenario data that a
runner needs. It is not a DSL and it does not encode implementation details.
Runners map each `kind` to the small amount of imperative behavior needed to
exercise that case. `applies_to`, when present, limits a vector to named binding
profiles such as `inbox`; when omitted, the vector is universal.

Additional tests close the review gaps:
- **`TestC2_SenderCrashResume`** — the *sender* half of C1. A sender links `seq=N`, crashes before its counter is durable, resumes and re-issues `seq=N`; `EEXIST` makes that a no-op (one copy) and the next message gets `N+1`. It also asserts the **§7 hazard**: reusing a seq for *different* content is silently dropped — making "seq counter must be durable+monotonic, ids unique per process" executable. Most likely to catch a real bug when the per-`(from,to)` allocator is built.
- **`TestQ1_MalformedQuarantineNeverDeliveredOrConsumedOrDropped`** — an inbox-profile vector for §11.1 quarantine: malformed items are observable, never delivered/consumed, and never block valid mail.
- **`TestD1_FsyncOrdering`** — pins `write(tmp) → fsync(tmp) → link → fsync(dir)` and proves a crash at every step leaves the record absent-or-whole, never torn. Catches linking before fsync (a record that survives a power cut without its body).

## B1 is the §5 decision, as code

`TestB1_PrivacyFork` runs the *same* assertion on both models and prints opposite
— but each correct-for-its-model — verdicts:

```
B1 HOLDS — peer 'carol' sees 0 of bob's bodies.            (inbox: private)
B1 VOID  — peer 'carol' can read 1 of bob's bodies.        (log: shared)
```

That is the fork in one line: **keep N inboxes if inter-agent privacy is a real
requirement; otherwise the shared log is simpler and more robust.** The demo
makes it concrete — on `log`, carol literally reads bob's message.

## What the demo shows (captured)

```
MODEL: log (shared append-only stream + cursors)
...
ORDER : real cross-agent order — one global sequence
PRES  : derived from CURSOR advance — no .live file. alice alive=true, carol alive=true
B1    : SHARED  — peer 'carol' can read bob's bodies: ["PING: please review PR 42"].
```

The three lines are exactly the three places the models differ: ordering source,
presence source (the log has **no `.live`** — presence falls out of the cursor),
and body privacy.

## Adding a binding

Implement `Binding` (8 methods) + the two test-only hooks
(`crashAfterActOnce`, `forceLastSeen`, `deliverSlow`), add a constructor to
`bindings()`, and run `go test -v`. If it passes, your substrate is agentchute-
conformant. A language-agnostic harness can drive the same vector operations
over a substrate's real CLI/ACL boundary instead of a Go interface.

## Scope

Tested on Linux (Go 1.22, clean container): suite + both demos pass as shown.
The bindings are modeled in-memory for deterministic concurrency/crash tests;
the SEMANTICS match the real substrates (filesystem inbox = `internal/loop`; the log =
an append file / git branch / Redis Stream / Kafka). This proves the model
behavior and the invariant set — not a production storage layer.
