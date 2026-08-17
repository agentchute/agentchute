# SSH hub — design-round brief (2026-08-14, Alex-directed)

Deliverable: `proposal/ssh-hub/DESIGN.md` — a complete, implementation-ready
design for the SSH hub reference implementation. Design only; no code changes.

## Binding inputs (consensus from the 2026-08-14 research round)

Verdict + vetoes: `.agentchute/loop/scratch-synthesis-2026-08-14-next-ref-impl.md`
("FINAL CONSENSUS" section). Non-negotiables:

1. One authoritative pool host (the hub); state stays plain files on its disk;
   every existing invariant (flock, link-no-clobber, rename claim, lease
   fencing) keeps executing on the hub's kernel.
2. Remote lanes use a LONG-LIVED SSH subsystem helper that owns lease
   acquire/renew/poll — never one-shot interpolated `ssh host agentchute <argv>`.
3. Forced-command per-key identity pinning ships day one: one authorized key →
   one agent id; server side rejects any `--as`/`--from` mismatch.
4. Release gates: hard connect/read/heartbeat timeouts; dropped session
   stops/fences the local child; explicit protocol/version handshake; NEVER
   auto-replay `send` after an ambiguous disconnect (unknown outcome → fail
   closed and report).
5. First implementation PR carves the operation seam (extract structured
   operations from `cmdSend`/`cmdCheck`/`ack`/lease lifecycle); filesystem
   remains the only backend; no generic low-level Store rewrite.
6. Zero new Go dependencies. Client side shells out to the system `ssh`
   binary (justify and specify exactly how) or justifies an alternative that
   adds no dep.
7. Structured framing over the wire (stdin/stdout of the subsystem), never
   argv re-parsing or shell text; `--body-file` bodies ship over the channel.
8. Vetoed: federation, SFTP-as-filesystem, loop-dir sync, unrestricted keys,
   unbounded SSH waits.

## Alex's priority for THIS round (weight it above everything)

Deployment and end-user seamlessness. "As simple as possible to deploy and
use, zero issues, very seamless for end user." Concretely:
- Hub setup and joining a new machine must each be a handful of commands at
  most; design the exact UX (`agentchute hub ...` / join flow), the exact
  quickstart text, and the key-provisioning story (who runs what, where keys
  land, how the authorized_keys line gets installed — can the hub generate it?).
- Every failure an end user can plausibly hit gets a named, actionable error
  message (catalog them).
- Existing single-host pools keep working untouched; a hub pool must be
  adoptable incrementally (hub is just today's pool; remotes join it).
- Tailscale documented as the blessed deployment recipe (incl. `tailscale ssh`
  where it removes key management) — recipe, never a dependency.

## Required DESIGN.md table of contents (all sections mandatory)

1. Goals / non-goals (incl. the vetoes above).
2. Architecture overview + one ASCII diagram (hub, remote lane, helper,
   runner, wrapper).
3. Operation seam: the extracted operation set — for each operation its
   inputs, outputs, error cases, and which existing internal functions it
   wraps (cite file:line at HEAD). This is the API both FS-local and SSH
   transports call.
4. Wire protocol: subsystem name, framing (choose and specify exactly — e.g.
   newline-delimited JSON with length-prefixed bodies; version handshake
   message; every message type with fields and an example); timeout values
   and where enforced; heartbeat cadence; how `check`-claimed bytes flow back;
   how send bodies flow in; ambiguous-outcome reporting for `send`.
5. Identity & security: authorized_keys template (exact line), the hub-side
   entry command, key→id mapping storage, `--as`/`--from` enforcement point,
   what a compromised remote key can and cannot do, host-key verification
   story (first-connect UX), threat table.
6. Lease & liveness: how the helper acquires/renews the serve lease over the
   channel, fencing token flow, what happens on channel drop (both sides),
   reconnect/backoff, interaction with the local runner + wake cues + guard
   latch (guard-latch reads must stay local-cached — specify).
7. End-user experience: exact quickstarts (hub operator; joining agent), all
   new commands/flags (keep them minimal — subtract-default guardrail),
   config/pointer-file format, the error-message catalog, `doctor`
   integration, Tailscale recipe.
8. Failure-mode matrix: every fault (hub down, TCP half-open, helper crash,
   wrapper crash, version skew, clock skew, key revoked, disk full on hub,
   duplicate id join) × detection × behavior × user-visible message.
9. Compatibility & spec delta: exact AGENTCHUTE.md sections to amend (spec PR
   comes first per Working rule 1), conformance-suite additions (serve
   lease/fencing coverage gap is known), what EXTENSIONS.md keeps.
10. Test plan: in-process operation-seam tests, framing tests with a fake
    transport, real-sshd integration matrix (disconnect-after-claim,
    disconnect-after-send, reclaim-during-send, identity mismatch, version
    mismatch), CI wiring on ubuntu+macos.
11. Implementation plan: ordered PR list with scope, rough LOC, and its
    reviewer gate; which PRs touch spec vs code; what ships in which release.
12. Open questions (should be near-empty — resolve, don't punt).

## Review loop (after the draft)

Internal critics first (seamlessness critic + adversarial correctness critic),
then codex and grok review the pinned doc; loop FIX→revise→delta-re-review
until both return SHIP. Reviews are prose; no repo mutation by reviewers.
