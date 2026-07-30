# Proposal: agentchute v2.5 — soft-state registration, timestamp messages, and the 2026-07-30 rulings

Status: PROPOSAL, REVISION 2.3 — after round-1 review by codex, sonnet, and grok (all three: needs-changes, direction sound) and three delta passes: grok (§7 lease-fenced restart, §9 crash recovery — folded, grok ship-shape), codex (§5 lock ordering, §2 spool commit boundary — folded, confirmation pending), sonnet (round 1 ship-shape; second pass caught the §9 carve-out bypass — folded here as the per-session latch, confirmation pending). §13 lists what each reviewer caught and what was disputed. Nothing is implemented. Alex has ruled on direction; the team loop is the gate before any code.

Origin: two expert-panel review rounds (18 agents, then 8 with red-team) over the full protocol, implementation, and 625-message live archive, followed by a long owner discussion that accepted, rejected, or reshaped the findings. This record captures the decisions **with their reasoning**, plus everything rejected and why.

Trigger incident: the whole fleet was paused 2026-07-07 → 2026-07-30. Mail sat unread for 23 days; reply-obligation warnings nagged about questions nobody cared about anymore; on comeback nothing told the reader the mail was three weeks old. The incident showed the presence machinery measures the wrong things and that the protocol has no notion of message age or registration hygiene.

---

## 1. Registration becomes soft state, cleaned by a lazy sweep

**Decision.** A registration row means "reachable now-ish," not "exists forever."

- The serve supervisor heartbeats the row every few seconds for as long as the session/pane exists. The heartbeat is an **unconditional write** (creates the row if missing) — self-healing if the row ever vanishes.
- Staleness threshold: **configurable, default 1 hour**. Config lives as one key in the existing per-pool settings file, settable via a setup flag. No new config mechanism.
- **One lock rules the row** (rev 2, codex+sonnet): heartbeat create/refresh, the sweeper's recheck-and-delete, and send's freshness check plus delivery all serialize on the same per-recipient registration lock. The sweeper re-checks freshness **under that lock** immediately before deleting.
- **Sweep is disk hygiene, not enforcement** (rev 2, sonnet): reachability is decided by send reading the row's age at send time (§2), never by whether a sweep tick has already fired.
- Sweep triggers: **boot and a slow idle tick of serve only** — never send, status, doctor, or gate. Diagnostics stay strictly read-only; status/doctor may *report* "stale, would be swept," never act.
- **Sweep hardening** (rev 2, grok): sweep only rows whose registration is stale AND whose serve lease is absent or stale; never sweep self while own lease is fresh; bounded deletions per tick; threshold read from config only.
- Sweep removes **only the registration row — never inboxes, never mail**.
- A returning agent re-registers automatically at boot; **boot writes the registration before doing anything else** (rev 2, grok), so a lane is addressable the moment it exists. A quitting agent does nothing (no deregister-on-exit, ever); its row ages out.
- The spec adds one sentence: registration rows are **pool-shared state**; sweeping them under the rules above is legal. (Today's "never touch another agent's state" wording would otherwise forbid it.)

**Why.** The old design had six overlapping liveness files that disagreed with each other on the real bus, and `.live` was refreshed by *any* CLI touch — "alive" never meant "reachable." Soft state with lazy expiry (the DHCP-lease pattern) is self-sufficient: no daemon, no cron, no human. The worst failure — sweeping an agent about to wake — costs one automatic re-register.

**Threshold rationale.** The heartbeat ticks every few seconds while any session exists, so staleness means "no session at all." One hour ≈ a thousand missed heartbeats: far beyond any laptop-sleep or restart hiccup, well below "actually gone." Shorter thresholds sweep agents that are mid-restart, and each false sweep now also refuses sends to them.

## 2. Send requires a currently-fresh registration and fails loudly otherwise

**Decision.** Send **re-derives freshness itself**: under the recipient's registration lock, the row must exist AND its last heartbeat must be within the threshold; delivery (the link into the inbox) happens under the same lock (rev 2, codex+sonnet — this closes the check-then-deliver race and the "stale row physically present because no sweep fired yet" hole; without it, black-hole mail survives exactly as today). Strictly **one-to-one** messages; no broadcast primitive, ever.

- **Error text distinguishes three cases** (rev 2, grok): never registered ("unknown agent"), stale past threshold ("was here, gone since <time>"), and fresh-but-racing ("was here seconds ago — retry once"; covers fleet-wake storms while a peer's serve is mid-start).
- **The failure-preserves-body promise is narrowed, and respects the commit line** (rev 2, codex; boundary corrected in rev 2.2, codex): all preflight checks run before stdin is read, so the common failures never consume the composed body. A failure after stdin is drained but **before the message file lands** spools the body to a temp file and prints its path for retry. A failure **after the file has landed** (e.g. the reply-obligation bookkeeping write fails) is reported as partial success with the committed filename plus the specific bookkeeping error — the sender is never told to resend, because the message is already delivered and a resend would duplicate it under the at-most-once contract.

**Why.** Mail can never rot in a box nobody will read, because you can only mail the reachable — and the sender learns at the only useful moment, while it can react. The trade, made explicitly: the bus stops being storage across long pauses and becomes **messaging for the currently-active team**. Cross-pause continuity lives in repo files, which is how the fleet already works.

**Duplicate-brief cost.** A quarter of archived bytes were byte-identical hand-copied briefs. Multi-recipient send was considered and **rejected**. The answer is the already-ratified **briefs-by-reference** convention: brief once in a repo file, short pointer note per peer.

## 3. Liveness philosophy: best effort, nobody babysits

**Decision.** Whole-fleet pauses are normal; silence during a pause is *correct* and must not alarm. No monitoring processes, no operator duties, no peer-watching. An agent that is hung, out of credits, or mid-long-job is **outside protocol scope**: its supervisor keeps heartbeating, it stays send-valid, mail waits. Best effort, period.

**Why.** Rejected alternatives and reasons in §11. Accepted consequence, knowingly: a frozen-but-heartbeating agent looks reachable while its mail waits. Fixing that would mean the tool judging agent health — the watchdog machinery this project already built, watched fail, and deleted.

## 4. Old mail: age is shown, relevance is the reader's call

**Decision.**
- `check` prints the message age prominently when mail is old ("this message is 23 days old").
- Convention: skim, archive, and **confirm with the sender before acting** on an old task brief — the repo may have moved on.
- Stale reply-obligation entries are **offered for pruning** when their owner runs check — never removed automatically.
- Mailboxes are never auto-deleted. A **manual clean command** handles abandoned mailboxes and stale obligation entries. Data destruction stays explicit and human-triggered; everything self-sufficient is metadata-only.

**Why.** No expiry dates, no schema: sometimes a three-week-old handoff note is exactly what the returning agent needs. The tool surfaces age loudly; the reader judges relevance.

## 5. Message identity: timestamps replace counters (the wire break)

**Decision — filename grammar** (rev 2, rewritten per codex+sonnet+grok):

```
<UTC-basic-timestamp>_from-<id>_r<32hex>.md
e.g. 20260730T182415123456Z_from-codex-agentchute_r9f2c...{32 hex}.md
```

- **Timestamp first**, so plain filename sort = chronological order — within an inbox and across the whole bus. (Rev 1 had sender-first, which sorts by sender, not time; codex caught the chronology claim being false.)
- **Basic format, fixed width, no colons** — colon-bearing timestamps break on Windows filesystems, and Windows is a real target (sonnet).
- **128-bit random suffix** (sonnet/grok: a short suffix plus the existing "already-exists means success" shortcut would silently drop a colliding different message). On a genuine name collision, **retry with a fresh suffix — never treat exists-as-success** under this grammar.
- **Monotonic floor, honestly stated** (all three reviewers): a sender never issues a timestamp ≤ its previous one. This requires a small durable per-sender last-issued file under the sender's own state directory, updated check-and-set under its lock. Rev 1's "nothing to lock or recover" oversold it: this is tiny *local* state replacing a shared allocator plus fencing tower — much smaller, not zero.
- **Lock ordering: never hold both** (rev 2.2, codex blocking + sonnet advisory, independently): send mints and durably saves its timestamp under its own floor lock, **releases it**, then acquires the recipient's registration lock for the freshness check plus link, releasing immediately after the link. Nesting the two locks deadlocks two peers sending to each other simultaneously, and self-send instantly. There is no correctness reason to make minting and delivery atomic with each other — send is at-most-once.
- **Reply references keep the full triple** (rev 2, sonnet — highest-severity round-1 catch): a reference is `(to, from, timestamp+suffix)`, the current reference grammar with the counter replaced — **not** a bare filename. The recipient field is the anti-spoofing check: an obligation "B owes A a reply" is cleared only by mail whose actual sender matches B. A bare-filename reference (rev 1's wording) would let any third agent clear someone else's obligation with a guessable name.

**Kept, explicitly** (rev 2, codex+grok converged; my rev-1 position withdrawn): the **serve token and per-send fence verification stay**. They enforce one live process per agent id at the moment of sending — a paused supervisor that resumes after its identity was reclaimed must not get a send through. Only the counter *allocation* machinery is deleted.

**Deleted**: the ~413-line sequence allocator, the opt-in idempotency key and its dedup window, the counter-recovery spec appendix, the key-choice footgun docs. Sonnet verified the key's only two callers are both themselves being deleted — no orphaned dependency.

**Delivery contract, stated loudly** (rev 2, grok): with the key gone, send is **at-most-once**; the covenant backstop is and remains handler idempotency. This is a contract change and lands as explicit edits to the spec's delivery section, the extensions doc, the changelog, and the conformance suite — not a silent deletion. Immediate step (no wire break): remove the three code/conformance comments claiming a "receiver-side dedup backstop" exists; it never did.

**Why.** A counter is shared mutable state, forcing a single-writer guarantee, which dragged in the most complex, footgun-dense machinery in the codebase — to guarantee exact ordering nothing consumes (the reader is a model taking the whole batch; every live counter file shows the idempotency key was never used in production). Timestamps need only the tiny local floor file; ordering is near-perfect on one host, millisecond-drift across NTP hosts; true conversation order rides the reply chain regardless of clocks.

## 6. Identity: explicit only, guessing deleted

**Decision.** The contextual "-N suffix" derivation is removed. An agent's id comes from an explicit flag or environment variable, or the command **fails with a fix hint**. Hook templates error loudly when unpinned instead of re-deriving. Serve keeps pinning the id into child processes. Adjacent rulings: a second unnamed session of the same wrapper on one bus fails and asks for a name; boot warns when a re-registered id inherits mail predating its registration.

**Why.** Guessing caused wrong-inbox reads (two claude-code lanes coexist in the live pool as evidence). Three documents of warnings compensate; deleting the guess deletes the warning corpus. The self-cleaning roster (§1) also removes the stale entries that made guessing misfire.

## 7. Version: 2.5, announced honestly — with a migration plan

**Decision.** The wire break ships as **version 2.5**, announced on the project page with the reasoning. The "v2 is final" statement is walked back openly.

**Migration** (rev 2 — all three reviewers independently blocked on its absence; codex counted 629 counter-named files and 5 counter-keyed obligations live on this very bus):

- **Read both, write new**: v2.5 binaries parse *both* filename/reference/obligation formats for at least one release, and write only the new format. Old unread mail, crash-orphaned claimed files, and outstanding reply obligations all stay reachable and dischargeable. Strict parsers must not make old files silently invisible (sonnet: today's strict-regex listing would skip them without even a quarantine warning).
- **Quarantine only truly unknown shapes** — never either known format.
- **The upgrade still forces restarts — via lease fencing, not row-clearing alone** (rev 2, sonnet; mechanism corrected in rev 2.1, grok): row-clearing by itself is undone within seconds, because §1's unconditional heartbeat would simply recreate the row from the still-running old binary. So: **the heartbeat's unconditional write is valid only while holding a fresh serve lease, and update invalidates all serve leases**. A fenced-out old-binary supervisor cannot recreate its row or send; it exits with a restart notice, and the relaunch goes through the new binary. That is the actual forcing function. Only genuinely redundant stale-hygiene paths in setup/update go.
- Registrations carry the protocol version (they already do); doctor reports mixed-version pools.

## 8. Cue delivery: keep typing, make it self-correcting

**Decision.** The runner keeps waking agents by typing the fixed cue string into their terminal. The one-shot "seen" set is deleted; the runner **re-cues on an interval for as long as unread mail sits in the inbox**, and cues mail already present at serve startup. **The idle-window discipline is kept and is mandatory** (rev 2, grok): re-cue fires only when the injection window says the terminal is safe to type into — an interval cue into a mid-paste or mid-output terminal would corrupt input. Self-correcting means retried-when-safe, not typed-blindly-on-a-timer.

**Why typing at all.** An idle interactive CLI listens to exactly one thing: typed input. Hooks and tool plugins only fire during a turn — they cannot start one. Typing *is* the poke; there is no other universal door. It stays fragile (no receipt, vendor quirks), so the fix is retrying until the mail is read.

**Parked.** Agent Client Protocol adoption (acknowledged wakes over a real interface) — the vendors that matter here lack native support today.

**Rejected.** Ambient delivery (mail bodies auto-appearing in agent context): the agent reads its mailbox when it can; bodies never enter the input channel.

## 9. Security mechanisms

**Decision.**
- **Pre-tool-use guard, honestly framed** (rev 2, all three reviewers): while the on-disk claimed-mail marker is non-empty, scope-expanding tool use is denied — pushes, tags, releases, network access, deletions, hook-config writes, **and the agentchute ack/check commands themselves** (sonnet: without that last item the guard self-destructs — a malicious message says "run ack first," the marker clears mid-turn, done). The marker is cleared only in the automatic end-of-turn path, as **one ordered handler: archive mail, then gate** (codex: separate concurrent handlers race). **The guard is a per-session latch — never a live read of the claimed directory** (rev 2.1 grok; mechanism corrected in rev 2.3, sonnet): check is allowed while the session's latch is unset, and the latch sets the moment the session claims or is shown any claimed mail — fresh or crash-residue (redelivery banner as today). From that moment, ack, further check, and all scope-expanding tools are denied until this session's own ordered end-of-turn handler archives the mail and clears the latch. Draining the claimed directory mid-turn must NOT unlock anything — sonnet's catch: if the guard re-derived its state from directory emptiness, an attacker could deliberately claim-then-abandon a message, and the next session would check it, ack it (directory now empty), and run the payload unrestricted in the same turn. A dead session's latch dies with that session, so grok's permanent-lockout scenario stays closed: the next session's first check recovers the residue and sets its own latch.
  - **Coverage is best-effort defense-in-depth, not a hard boundary** (codex+sonnet): on Claude-family CLIs the denial is text pattern-matching over a generic shell tool (a determined injection can alias or encode around it); on codex, hosted tools like its web search are outside hook coverage, and repo-level hook changes are inert until trusted; on grok there are **no hooks at all** — the lane is unguarded, full stop.
  - **Consequence, recorded as a rule** (grok): senders MUST NOT route irreversible or scope-expanding work to hookless lanes; for grok the protection remains prose plus human confirmation. The spec's security section says this in those words instead of claiming a fleet-wide mechanism.
- **Delete the corrective auto-send**, keep quarantine. It laundered attacker-influenced filename text into authentic sequenced mail from a trusted peer, enabled mutual finish-gate flooding, and its only real-world output was machine mail complaining about malformed machine mail.
- **Send fixes**: failure text for an unknown recipient must not coach the sender into registering the recipient (impersonation door); body-consumption promise per §2.
- **Shared GitHub token**: raised by the panel as the highest-consequence gap. Owner ruling: **accepted as-is**; recorded so the acceptance is explicit.

## 10. Docs, spec, and code diet

**Decision.**
- The task-envelope rulebook is demoted to a **one-page template plus three rules**, each backed by an observed incident: stable pointers only; every task states a done-when the recipient can verify; no irreversible action without explicit authorization. Plus a ~5-line warn-only check when an ask-send lacks a done-when. Evidence: 20 of 625 archived messages followed the full format, yet 78 PRs merged fine; the only well-adopted element (the ask heading) is the one the tool injects — adoption follows tooling, not covenants.
- **One frontmatter parser** with the grammar written into the spec; characterization fixtures promoted to conformance vectors.
- **Gate keeps a self-freshness check at commit/release** (rev 2, grok+sonnet — load-bearing catch): today that gate reads `.live`; rev 1 deleted the branch without a replacement, which would let a dead or wedged lane pass a release gate. The check is **re-pointed at the agent's own registration heartbeat age** (its own row, read-only, possibly a stricter threshold than the sweep's). Finish/continue gates stay presence-free as today.
- **Deletions** (~6,000+ lines): all presence machinery — detached poller, .live, session-ancestry checks, presence scan (~3,500); registration status extras and their doctor checks (~300); the *stale-hygiene* registration paths of setup/update (~part of 700 — the upgrade-forces-restart step is kept, §7); roughly a quarter of the spec (presence section, corrective auto-send, idempotency/counter text, and the normative multi-host/NFS text — the substrate is declared single-host, matching what CI tests).
- **Installer stays** in the single binary. Self-installation from one binary is a feature; the lesson is reviewing installer changes with protocol-change rigor, not splitting the project.

## 11. Product identity, and everything rejected with reasons

**Ruling.** agentchute is a **team of long-lived peers** — each with its own context and memory, talking one-to-one, deciding for themselves whether to recruit teammates or work alone. No orchestration layer above them. The team is **whoever is active on the bus right now**, not a fixed roster.

Rejected along the way, each after real consideration:

- **Mailroom daemon / pool bus daemon (SQLite)** — breaks "no daemons, just files"; core justification factually weak for this deployment; resident watchers were already built and deleted once.
- **Sender-side delivery duties** (advisory stale warnings as the mechanism, sender-triggered cleanup-and-stop) — superseded by §1+§2, where registration carries reachability and send simply fails under the lock.
- **Peer liveness monitoring / operator status tables as the liveness answer** — the fleet pauses whole; silence is normal; self-sufficiency rule (§3).
- **Multi-recipient send** — stays 1-to-1; briefs-by-reference covers the real cost (§2).
- **Ambient body delivery** — bodies never enter the input channel (§8, §9).
- **Auto-expiry or auto-deletion of mail** — age is shown, relevance is judged by the reader (§4).
- **Heartbeat stops when own inbox is stale** (panel's "frozen agent" fix) — agents legitimately run long jobs without reading mail; hung/broke agents are out of scope (§3).
- **Orchestrated-runtime pivot / spawned-worker dispatch as the product direction** — peers with own contexts; agents choose their own collaboration shape.
- **Splitting the installer out** — rejected (§10).
- **Agent Client Protocol adoption now** — parked, not rejected (§8).

## 12. Implementation fix list (rev 2 — all review catches folded)

1. Runner: delete one-shot seen set; interval re-cue while inbox non-empty **and idle-window-safe**; cue startup mail.
2. Heartbeat: unconditional row write, under the per-id registration lock.
3. Sweep: boot + slow serve tick only; under-lock freshness re-check; stale-registration AND stale-lease required; never self; bounded per tick.
4. Send: freshness check + delivery under the recipient's registration lock; three-way error text; no register-the-recipient coaching; preflight before stdin; spool-for-retry only pre-link, partial-success report post-link (never a resend hint); floor lock released before registration lock is taken (no nesting, ever); **fence verification kept**.
5. Boot: registration written first, before any other work.
6. Filenames: timestamp-first basic-format UTC + sender + 128-bit random; retry on collision, never exists-as-success; durable per-sender monotonic floor file.
7. References and obligations: (to, from, timestamp+suffix) triple; dual-format read for one release; obligations dual-matched.
8. Update: invalidate all serve leases (heartbeat row-write valid only under a fresh lease; fenced-out supervisor exits with restart notice); delete only stale-hygiene paths.
9. Gate: commit/release self-freshness re-pointed to own registration age.
10. Guard: per-session latch — set on first claim/display of any claimed mail (incl. redelivered residue), cleared only by this session's own ordered end-of-turn handler (archive, then gate), never re-derived from directory emptiness mid-turn; while set, denies ack/check and scope-expanding tools; a dead session's latch dies with it; documented as defense-in-depth; grok documented unguarded + sender rule.
11. Truth-fix: remove false receiver-dedup claims; declare at-most-once in spec/extensions/changelog/conformance.
12. check: age banner on old mail; offer stale-owed pruning.
13. Manual clean command for mailboxes/owed.
14. Threshold config key + setup flag.
15. Parser consolidation + grammar in spec + conformance vectors.
16. Delete: corrective auto-send, presence machinery, registration extras, stale-hygiene setup/update paths, idempotency key + allocator + recovery appendix, spec sections per §10.

## 13. Round-1 review ledger (who caught what)

- **codex**: shared-lock discipline across heartbeat/sweep/send; sender-first filenames don't sort chronologically; migration evidence (629 old-format files, 5 old-keyed obligations live); fence-on-send load-bearing (with grok); stdin promise physically impossible as written; codex hosted-tool hook gap; ordered end-of-turn handler.
- **sonnet**: send must re-derive freshness itself (sweep is hygiene, not enforcement); bare-filename references break the reply anti-spoofing check (highest severity); update's registration-clearing is the upgrade forcing function, not hygiene; guard must deny ack/check itself; Windows-safe timestamp format; strict parsers would make old mail silently invisible; verified idempotency-key removal is orphan-free.
- **grok**: send/sweep race (with codex); fleet-wake storm + three-way error text; boot registers first; re-cue must keep idle window; at-most-once must be declared loudly; sweep hardening (lease cross-check, rate limit, never self); commit/release gate needs a named freshness source (with sonnet); hookless-lane routing rule.
- **grok, delta pass (rev 2.1)**: unconditional heartbeat would undo update's row-clearing, so forced restart needs lease invalidation (§7); guard denying ack/check needed a session-scoped crash-recovery path or a crash locks the lane (§9).
- **codex, delta pass (rev 2.2)**: sender floor lock and recipient registration lock must never nest (mutual send deadlock; self-send instant deadlock) — mint, release, then deliver (§5; sonnet flagged the same independently as advisory); spooling after a successful link invites a duplicate resend — spool only pre-link, partial-success report post-link (§2).
- **sonnet, second delta pass (rev 2.3)**: the rev-2.1 crash-residue carve-out reopened the same-turn self-clear bypass when the guard is defined by directory emptiness (claim-then-abandon manufactures a fake crash; recovering session checks, acks — directory empty — then runs the payload unrestricted); fixed by defining the guard as a per-session latch cleared only by the owning session's end-of-turn handler (§9).
- **Disputed and resolved**: claude's rev-1 position that per-send fencing could go — withdrawn (codex+grok scenario stands). Grok's local idempotency re-issue table — not adopted; at-most-once declared honestly instead (sonnet's orphan check makes the deletion clean).

## For the delta re-review

Rev 2 changes only: §1 (lock, hardening, hygiene-vs-enforcement), §2 (freshness under lock, error text, stdin), §5 (grammar rewrite, references, fence kept, at-most-once), §7 (migration plan, update restart kept), §8 (idle window), §9 (guard: ack/check denial, ordered handler, honest coverage, grok rule), §10 (gate re-point, deletion list adjusted), §12 (fix list), §13 (ledger). Confirm your round-1 findings are correctly resolved; flag anything mis-folded or newly broken. Delta only — no full re-read expected.
