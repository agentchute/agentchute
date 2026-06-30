# feat: "simple again" + protocol-v2 — pull-only coordination

## Summary

Replaces agentchute's push apparatus with **pull-only** coordination. Senders only ever write files; nothing pokes a recipient. A loopless wrapper is supervised by the runner (`agentchute run`), a per-agent PTY supervisor that polls the agent's own inbox and injects a `check inbox` cue. This is a **correctness** argument, not a simplicity one: push presence is unreliable (stale caches, watchdog races, gates on phantom liveness); parent-child supervision is ground truth. Simplicity is the byproduct — **~4,800 net lines of code removed** (+6,640 / −11,422).

The change implements two ratified team decisions:
- `docs/design/agentchute-simple-again-TEAM-DECISION.md` (the runtime: launch, wake, presence, liveness).
- `docs/design/agentchute-protocol-v2-TEAM-DECISION.md` (the wire: identity, ordering, obligation, envelope, invariants).

It is staged across eight compile-green gates so each step is independently reviewable and bisectable.

## Commits (per gate)

| Commit | Gate | What |
|---|---|---|
| `e20a8a7` | g0 | In-tree conformance suite + design records; relocate `underAgentchuteRunner` |
| `201ed8a` | g1 | Delete watchdog + recipient_liveness (cross-agent liveness push) |
| `87677f7` | g2 | Protocol-v2 core (additive): seq allocator, serve lease+fence, `.live`, asker `.owed` |
| `a21d0f6` | g3 | `.live` as the presence source (writer in `UpdateLastSeen`; readers switched) |
| `34d8284` | g4 | seq identity on the wire + inbox dual-read (nonce→seq) — highest-risk gate |
| `2c08876` | g5 | Act-then-archive consume (claim+ack) + asker-owned `.owed` obligation flip |
| `1566938` | g6a | Pull-only: delete wake dispatcher/adapters/reachability; senders stop poking |
| `41b52fd` | g6b | Strip runner receive-socket; id-uniqueness + seq fence ride the serve lease |
| `c751b4b` | g6c | Delete wake fields + register wake-autodetect + tmux/herdr state (wake teardown complete) |
| `597e4de` | g7 | Final trims: delete `migrate`, fixed `.agentchute/loop` namespace, `setup --wake` runner-only, envelope cuts |

(`bb8c1d5` is the co-authored plan revision.)

## What changed (the model)

- **Pull-only.** Senders write `inbox/<to>/` and never wake recipients. Deleted: the watchdog, cooperative/sender-side wake, the tmux/herdr wake adapters, the runner receive-socket, the reachability cache, cross-agent liveness tracking, and the `wake_method`/`wake_target`/`reachable_at`/`reachability_*`/`wake_endpoints` registration fields. The runner polls its OWN inbox and injects a recv trigger to its child; loopless agents need the runner.
- **Identity / ordering.** A message's filename is the canonical identity `from-<from>_seq-<020d>.md`, where `seq` is a durable, monotonic, per-`(sender,recipient)` sequence (write-ahead, so a crash gaps but never reuses). Per-sender FIFO is exact via lexicographic sort; cross-sender order is advisory arrival order. The committed identity is `(to, from, seq)` — not a sender-asserted `message_id`. `in_reply_to` references `to-<to>_from-<from>_seq-<020d>`.
- **Delivery.** Atomic `link()`-no-clobber; `EEXIST` = "this exact message already landed" (crash-resend no-op, NFS-safe).
- **Consume (act-then-archive, two-phase, at-least-once).** `check` CLAIMS (moves `inbox/<id>/<name>` → `inbox/<id>/.claimed/<name>`) and displays, re-displaying any uncommitted `.claimed` residue with a `REDELIVERED` banner; `ack` COMMITS (archives). A crash between `check` and `ack` re-delivers; handlers must be idempotent. The Stop hook runs `ack` then the read-only finish gate.
- **Presence.** A `.live` file per agent at `<loop>/live/<id>.live` (`last_seen` + advisory `busy`), written by `UpdateLastSeen` every heartbeat (atomic tmp+rename). Fresh ⇒ alive; stale/absent ⇒ not-alive. `busy` never affects aliveness (avoids false-dead). No registration wake state.
- **Obligations (asker-owned).** `send --ask` records the asker's `.owed` (owed a reply to `(to,from,seq)` by a deadline); a reply with the matching `in_reply_to` clears it; the gate surfaces expired `.owed` as a NON-BLOCKING warning (dead-recipient detection). Wire `reply_required` is an advisory hint.
- **Id-uniqueness.** The runner acquires a serve LEASE (`state/<id>/serve.claim`, fails closed if a fresh claim is held) with a FENCING TOKEN verified on every heartbeat and every seq write; a reclaimed/zombie holder is fenced and cannot become a dup-writer.
- **Registration.** Just the inbox dir + `.live` plus a small record (id, last_seen, vendor, host, status, advisory provenance). Fixed `.agentchute/loop` namespace (no vendor-namespacing). `setup --wake` is runner-only.

## End-to-end handoff proof

The full alice→bob reply-required handoff exercises every new path:

1. `boot` — both agents register; the runner writes each `.live`.
2. `alice: send --ask --to bob` — lands `inbox/bob/from-alice_seq-00…01.md`; alice records her own `.owed` keyed `(to=bob, from=alice, seq)`. No poke.
3. `bob: check` — CLAIMS the message into `inbox/bob/.claimed/` and displays it with the copyable `--reply-to to-bob_from-alice_seq-…` reference. A second `check` before `ack` re-displays it with the **REDELIVERED** banner (at-least-once verified).
4. `bob: send --reply-to to-bob_from-alice_seq-… --to alice` — the reply carries the canonical `in_reply_to`.
5. `bob: ack` — archives the `.claimed` residue (single commit point).
6. `alice: check` — consumes bob's reply; the matching `in_reply_to` clears alice's `.owed`.
7. **Gates clear** — bob's finish gate passes (nothing claimed/owed), alice's finish gate passes (no outstanding `.owed`).

## Test posture

- `go build ./...`, `go vet ./...`, `go test ./...` green; full suite re-run under `go test -race ./...` green.
- **Conformance suite 25/25** (`conformance/`): the seven invariants (`R1`/`D1`/`D2`/`O1`/`C1`/`E1`/`B1`) plus the sender-crash-resume and fsync-ordering tests, driven against both bindings (private inbox dir + shared log).
- **The 3 exact-byte submit tests are preserved verbatim** — `TestPromptInjectionBytesDefaultUsesCarriageReturn`, `TestPromptInjectionBytesCodexUsesBracketedPasteAndEnhancedEnter`, `TestPromptInjectionBytesCodexWrapperUsesEnhancedEnter` — so the **PTY submit bytes are byte-unchanged** (the spike's bare-`\n` regression is avoided; codex still gets bracketed-paste + enhanced-enter).

## Deferred follow-ups (intentionally NOT in this PR)

- Rename the `run` verb to `serve`.
- Cut `message_id` emission (still emitted as a one-release compat field).
- Remove the legacy nonce reader once every inbox reports zero (the dual-read drain gauge tracks this).
- Make `.owed` the sole reply-obligation authority and drop the recipient-side pending-reply ledger block (the recipient ledger still blocks the finish gate for one release).
- Finish residual setup/socket-helper cleanup (a few `agentchute-run` socket helpers and example scripts predate the redesign).

## STOP before merge

Release is the owner's call. Do not merge, tag, or publish without explicit owner authorization.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
