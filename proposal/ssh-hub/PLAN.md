# SSH Hub — Implementation Plan

Source of truth: [`proposal/ssh-hub/DESIGN.md`](DESIGN.md), designed against HEAD
`1244ae4`. **Every `file:line` anchor below is `@ 1244ae4` and was re-opened and
verified for this revision** — where a citation in round 1 was wrong, the
corrected anchor is used and the correction is called out inline.

Bar for this document: an agent assigned any single work item (WI) can finish it
from this plan plus the named DESIGN.md sections, **without asking a question**.
Where DESIGN.md deliberately left an implementation detail open (package/file
names, test placement, contingencies), this plan pins it and says so (§7 lists
every such pin).

Round 2 of this plan applied the four review lanes' findings; dispositions are
logged in [`plan-reviews-r1.md`](plan-reviews-r1.md) §"Revision log (plan round
2)". **Round 3 applies R1–R14 of
[`plan-reviews-r2.md`](plan-reviews-r2.md)**; dispositions are logged there
under §"Revision log (plan round 3)".

---

## 1. Overview — six merges, one release

Per DESIGN.md §11 (M1–M6) and §9 (ordering, C6):

```
M1 seam ──▶ M2 spec ──▶ M3 codec+session+vectors ──▶ M4 client ──▶ M5 channel+join ──▶ M6 sshd matrix+CI+docs ──tag v1.6.0──▶ fleet rollout
```

(M1 is merged at `7d08654`. M2 is merged at `1431657`. #154 (published spec
off `main`) closed at `69f4e3c`; the published spec now points at the
latest release.)

- **M1** (operation seam, **2,000–2,500 LOC** — re-priced from 1,500 because
  every wrapped helper MOVES into `internal/op` rather than being called
  in place, B1/X2) is an invisible refactor. **No standalone tag.** It is
  already on `main`.
- **M2** (spec, ~380 LOC prose) lands before any hub code (Working rule 1).
  After M2, on any disagreement the spec wins over DESIGN.md. This erratum
  aligns DESIGN with the authorized `owed_note` shape, so that field no
  longer needs a carve-out.
- **M3 → M4 → M5** are strictly sequential (each consumes the previous
  layer). **No tag anywhere in M1–M5.**
- **M6** gates **v1.6.0** (the single release; semver minor over v1.5.7):
  the real-sshd integration matrix green on ubuntu-latest AND macos-latest,
  and every conformance L/W vector green (§9.3: L + the hub-side W halves
  land in M3, the client-side W halves in M4, the sshd-backed W runs in
  M6). No tag while any of those is red.
- **Work items, ≈ 10,700 LOC incl. tests.** (WI-1.9 is spent; WI-3.6 is
  added; WI-6.6 is the single release-notes item.)

Merge mechanics: every merge is one PR from an isolated worktree branch,
gated per §3. claude-code (integrator) performs the merge/tag/release
steps and owns WI-6.6 only (WI-1.9 is spent).

---

## 2. Work items per merge

Format per item: **(a)** goal / **(b)** DESIGN.md pointers / **(c)** files /
**(d)** fixed shapes / **(e)** tests / **(f)** done-when / **(g)** LOC.
"Rows" = the named test rows in DESIGN.md §10.3 (or §10.1, §10.2 where stated).

**Execution rules for every merge (stated once, binding everywhere):**

1. **One implementer per merge.** One lane owns the merge and its PR. Where the
   lane table (§4) names a specialist for a *specific* item (WI-3.4,
   WI-5.3b, WI-5.3c), that item is handed over **inside** the same merge,
   sequentially, and the merge owner integrates it. A merge is never split
   across two concurrently-working lanes.
2. **Items run in numeric order within a merge.** Where the ordering is
   load-bearing it is restated on the item itself: WI-1.4 before WI-1.5 (gate
   core before `op.Ack` — F7/X2), WI-1.6 before WI-1.7 (register before the
   tick template), WI-4.1 before WI-4.2 (config format before discovery),
   WI-4.2 and WI-4.3 in the SAME merge (§6.8 rule 5), WI-5.4 and WI-5.5 in the
   SAME merge (pool.id writer + wipe preservation).
3. **No two items run in parallel if they touch the same file.** Each item's
   **(c)** file list is the authority; if two items you were about to start
   share a file, run them in numeric order instead.

### M1 — operation seam (`internal/op`) (merged; no standalone tag)

Ground rules for every M1 item:

- Zero behavior change. The existing CLI test files must pass **unmodified
  EXCEPT the named list below** (§10.1) — and the list is closed: **three
  files, three hunks, nothing else.**

  | file | hunk | item | why no alias is possible |
  |---|---|---|---|
  | `internal/cli/send_a5_test.go` | `229-237` | WI-1.2 | the seam var moves package; a test that REASSIGNS it must name the new one |
  | `internal/cli/send_b3_test.go` | `137-146` | WI-1.2 | same |
  | `internal/cli/run_test.go` | `495-507` (`newPollTestRuntime`) | WI-1.7 | three `runnerRuntime` fields collapse into one `*op.Channel`; a struct literal cannot be aliased |

  Everything else the move set removes from package `cli` keeps a thin
  **alias or adapter**, pinned on its own item: `gateStatus` (type alias,
  WI-1.4), `performRegister`/`registerOpts`/`registerResult` (adapter,
  WI-1.6), `printStatus` (signature preserved, WI-1.4),
  `refuseLiveRunnerCollision` (signature preserved, WI-1.7).
- **Move-set rule (R1, widened by T2, field population added by #150) —
  binding on every item in EVERY merge, and the reason the list above is
  trustworthy.** The rule covers three populations:
  1. every identifier an item **removes** from package `cli` — a type, a
     function, a struct field;
  2. every identifier an item **re-signatures** in package `cli` — same name,
     same package, different parameters or results. A test that calls it
     stops compiling just as surely as if it had moved, and no alias can
     bridge a changed signature.
  3. **every struct field** an item **collapses, removes, or re-signatures**.
     A field reached through a receiver (`rt.lease`) is neither a function
     nor a type, so populations 1–2 miss it. For each such field, grep every
     `internal/cli/*_test.go` for **two spellings** before the item may be
     called done: receiver-qualified uses (`rt.<field>`, `runtime.<field>`,
     …) **and** keyed struct-literal fields (`<field>:`). A test that
     builds the struct by key is invisible to a receiver-qualified grep —
     `newPollTestRuntime` is the example. (The production-side omission
     that caused M1's leak is a different class, covered by the
     constructor guard, not by this grep.) A comment-only
     hit needs nothing; a **code** hit needs an alias, an adapter, a
     preserved field, or a named exception.

  For each identifier in populations 1–2 the item does ONE of two things
  before it may be called done: leave a thin alias/adapter in `cli` (or
  preserve the signature), or name the test file and its hunks — in the M1
  table above for an M1 item, or in the item's own (c) file list and (f)
  acceptance for a later merge. The check is mechanical and is part of the
  item's done-when: run `grep -n '\b<identifier>\b' internal/cli/*_test.go`
  for each moved OR re-signatured identifier; a comment-only hit needs
  nothing, a **code** hit needs an alias, an adapter, a preserved
  signature, or a named exception.

  **M1 is already clean under the field population.** Its three touched
  fields are `regTemplate` (1 hit, `run_test.go:505`, named exception row
  3), `lastSweep` (2 hits, both comments), and `lease` (1 hit,
  `b1_convergence_test.go:67`, the adapter). There is no fourth. **Re-run
  the field check for M3–M6 only**; do not re-open M1.

  Run over the three populations at `1244ae4`, the check yields exactly: the three
  M1 exceptions in the table above, the four M1 aliases/adapters, and — from
  population 2, which is where round 3's version stopped — **`resolveAgentID`
  in M4**, whose five direct calls in `internal/cli/identity_test.go` are
  named in WI-4.8(c). Nothing else in the plan re-signatures a `cli`
  identifier that any `*_test.go` calls; `printStatus`
  (`status_test.go:35,77,126,181`) and `refuseLiveRunnerCollision`
  (`run_test.go:130,140,159,170`) are the two the plan keeps byte-identical
  precisely so they stay off this list. **M1's own exception table is still
  closed at three files and three hunks** — the `resolveAgentID` edit is
  M4's, in a merge with no zero-test-edit criterion. An item that adds a new
  move or a new signature change must re-run the check.
- `internal/loop` signatures unchanged; `git diff --stat` shows no
  `internal/loop` change anywhere in M1.
- No wire code, no transport code.
- **Every wrapped helper MOVES into `internal/op`** (B1/X2). There is no
  "stays in cli as the loop-level worker the op wraps" option anywhere in M1 —
  it does not compile: `internal/op` MUST NOT import `internal/cli` (§7.4
  dependency direction, B2), so an op that called a CLI-resident helper would
  close a cycle. The moving set, in full: `evaluateGate` (`gate.go:129-256`),
  `finishGateClear` (`gate.go:262-268`), `gateStatus` (`gate.go:271-286`),
  `archiveAllClaimed` (`ack.go:164-179`), `performRegister`
  (`register.go:72-98`) with `publishRegistrationOnce` (`register.go:107-187`)
  and `registrationLiveElsewhere` (`register.go:189-195`),
  `hasPendingInboxMail` (`serve.go:757-763`), and the delivery seam var
  `sendTsMessageWithCommit` (`send.go:21`).

  What moves is the **implementation**; the move-set rule above still applies
  on top of it, and neither survivor weakens B2: `gateStatus`'s declaration
  moves and `cli` keeps a one-line **type alias** (an alias is not a second
  type and not an import cycle), and `performRegister`'s **body** moves while
  `cli` keeps a thin **adapter** of the same name (a caller of `internal/op`,
  never a callee).
- `internal/op` imports only the standard library and `internal/loop`. A
  dependency-direction test lands in WI-1.8.

**WI-1.1 — seam skeleton: context, events, errors.**

(a) Create `internal/op` with the actor context, the typed event union, the
typed error set, and the error→code function.

(b) §3 conventions (all three bullets); §4.4.2 (the amended error-code registry
— codes only, no wire yet).

(c) NEW `internal/op/op.go`, `internal/op/event.go`, `internal/op/errors.go`.

(d) Fixed shapes:

- `type Context struct { ActorID string }`
- `type Event struct { Message *MessageEvent; Note *NoteEvent; Owed *OwedEvent; Ack *AckItemEvent }`
  — exactly one arm non-nil, emitted in production order.
- `MessageEvent{Filename, Sender, Stamp string; Redelivered, ReplyRequired bool; ReplyRef string; Body []byte}`
- `NoteEvent{Level, Msg string}` — **`Level` is one of exactly `"warn"` or
  `"info"`** (F4). Routing, pinned: `warn` → **stderr**, rendered
  `warning: <Msg>`; `info` → **stdout**, rendered `<Msg>`. `Msg` NEVER carries
  its own level prefix — the renderer adds it, so the wire and the local path
  cannot drift. The wire `note.level` field (§4.3) uses this same two-value
  vocabulary. A third level is a spec change (M2), never an implementer's
  choice. **DESIGN §4.3's "Hub→client `note` frames" bullet now carries BOTH
  arms** — two example frames, the "the level IS the stream" rule, and the
  three `check` `info` lines it is written against (a round-3 note said the
  bullet showed only `warn`; DESIGN has since been amended and that note is
  stale). The `info`→stdout arm is load-bearing (WI-1.3), so WI-2.1's spec
  copy MUST reproduce both arms and their streams.
- `OwedEvent{To, From string; Seq uint64; Stamp, Suffix string; By, RecordedAt time.Time; Ref string}`
  — the full `loop.OwedEntry` fields per E4 (`internal/loop/owed.go:114-122`).
- `AckItemEvent{Filename, ArchivePath string}`
- **The M1 op-layer sentinel set — exactly eight, no more** (F6). Four are
  **re-exports** (`var X = loop.X` aliases, NOT redeclared sentinels, so
  `errors.Is` matches across both packages and a `loop` error travelling
  through `op` still satisfies the CLI's existing checks); four are new:

  | sentinel | origin | code |
  |---|---|---|
  | `op.ErrNotRegistered` | NEW | `E_NOT_REGISTERED` |
  | `op.ErrRecipientUnknown` | re-export of `loop.ErrRecipientUnknown` (`seq.go:191`) | `E_RECIPIENT_UNKNOWN` |
  | `op.ErrRecipientUnreadable` | re-export of `loop.ErrRecipientUnreadable` (`seq.go:206`) | `E_RECIPIENT_UNREADABLE` |
  | `op.ErrRecipientStale` | NEW — the **preflight** arm (C29b, `seq.go:240`) | `E_RECIPIENT_STALE` |
  | `op.ErrRecipientRacing` | NEW — the **under-lock** arm (C29c, `seq.go:253`) | `E_RECIPIENT_RACING` |
  | `op.ErrFenced` | re-export of `loop.ErrFenced` (`lease.go:51`) | `E_FENCED` |
  | `op.ErrLeaseHeld` | re-export of `loop.ErrLeaseHeld` (`lease.go:46`) | `E_LEASE_HELD` |
  | `op.ErrOrder` | NEW — `Channel.Tick` before `Channel.Register` (§3.4/§6.1) | `E_ORDER` |

  `ErrRecipientStale` and `ErrRecipientRacing` both **wrap** the underlying
  `*loop.ErrRecipientStale` value, reachable via `errors.As`, because the CLI's
  C29 renderer (`send.go:347-416`) needs its fields. The op sentinel records
  only WHICH raise site produced it: today the CLI classifies the two by
  position in `cmdSend`, but the hub must emit two distinct codes for them
  (§4.4.2), so the distinction becomes explicit at the seam.
- **`func CodeFor(err error) string`** — a FUNCTION, not a table (F6): an
  `errors.Is` switch over the eight sentinels with a **default arm returning
  `"E_HUB_IO"`**, and `CodeFor(nil) == ""`. `loop.ErrInboxMissing`
  (`inbox.go:111`) has no code of its own and takes the default arm
  deliberately.
- **The three code sets, with the arithmetic pinned (R3).** §4.4.2's hub-side
  registry is **17 rows**, partitioned with no gap and no duplicate:
  - **op-owned outputs: 9** — the eight sentinels above **plus** the default
    `E_HUB_IO`. `E_ORDER` is op-owned (it is a sentinel), so M3 must not
    re-add it.
  - **M3 session/codec: 8** — `E_VERSION`, `E_IDENTITY`, `E_POOL_NOT_FOUND`,
    `E_POOL_ID_INVALID`, `E_POOL_MISMATCH` **hub arm**, `E_MALFORMED_FRAME`,
    `E_TOO_LARGE`, `E_UNSUPPORTED`. **9 + 8 = 17.**
  - **client-only: 9**, landing in M4 — `E_CONNECT`, `E_UNAUTHORIZED`,
    `E_HOSTKEY_CHANGED`, `E_CHANNEL_LOST`, `E_SEND_UNKNOWN`,
    `E_HELLO_TIMEOUT`, `E_HUB_NO_BINARY`, `E_NOT_JOINED`, `E_NO_SSH`. This
    list is disjoint from the 17.
  - **`E_POOL_MISMATCH` is the one code with TWO emitters** (§4.4.2 as
    amended, F9/X1) and is **not** a client-only-list member: it is modelled
    as an explicit **both-sides classification** — hub arm at session start
    (M3, WI-3.3a), client arm after `hello-ok` (M4). Neither arm is an
    op-layer sentinel. Requiring the client list to be disjoint from the hub
    registry is only sound because this code is classified rather than
    listed twice (WI-3.1a's completeness test asserts exactly that).
- **Not-registered rendered text — CORRECTION to the round-1 finding.** The two
  CLI sites are **not** byte-identical today: `send.go:145` reads
  `sender %q is not registered. Run ...` and `check.go:120` reads
  `agent %q is not registered. Run ...` (and `status.go:62` matches
  `check.go`) — identical after the first word. So the rule is ONE sentinel,
  TWO renderers: each CLI site keeps its own wording verbatim, formatted from
  `Context.ActorID`, and neither string changes by a byte. DESIGN carries
  both texts and this rule (§4.4.2's `E_NOT_REGISTERED` registry row — the one
  naming `send.go:145`, `check.go:120` and `status.go:62` — plus its §7.5
  catalog entry), so the remote `send` path renders "sender" too — there is no
  local/remote divergence to flag.

(e) Unit tests: event-union invariant (exactly one arm non-nil) for every
constructor; **`CodeFor` completeness** — a table over all eight sentinels, a
`fmt.Errorf("%w", …)`-wrapped sentinel, a bare I/O error (⇒ `E_HUB_IO`), and
`nil` (⇒ `""`); plus a list test asserting the exported sentinel names equal a
literal expected list, so adding a ninth sentinel without a code arm FAILS.

(f) Done-when: the package compiles standalone against only stdlib +
`internal/loop`; `CodeFor` has no reachable path returning `""` for a non-nil
error; the completeness test enumerates all eight plus the default.

(g) ~190 LOC.

**WI-1.2 — `op.Send` (and the delivery test seam).**

(a) Extract send's validate-and-mutate into
`op.Send(cfg *loop.Config, ctx Context, req SendReq) (SendResp, error)`.

(b) §3.1; §4.5.3 (the spool stays CLI-side).

(c) NEW `internal/op/send.go`; MODIFY `internal/cli/send.go`.

- **Moves out of `cli`**: the sender-enrollment stat (`send.go:141-148`), the
  recipient preflight (`send.go:158-171`), the delivery call (`send.go:242`),
  and the package-level seam var `sendTsMessageWithCommit` (`send.go:21`,
  today's sole production call site is `send.go:242`).
- **Stays in `cli`**: compose (`send.go:174-224`), the spool
  (`send.go:418-477`), the C29 renderer (`send.go:347-416`),
  `rejectLoopStateBodyFile` (`send.go:600-633`), `sendStdin` (`send.go:23`),
  `afterSendPreflightHook` (`send.go:19`).

(d) `SendReq{To string; Content []byte; Ask bool; ReplyBy time.Duration; ServeToken string}`
— **no `From` field**; wraps `loop.CheckRecipientReachability`
(`internal/loop/seq.go:226`), the delivery seam, and `loop.RecordOwed`
(`internal/loop/owed.go:202`).
`SendResp{Filename, Ref string; Committed bool; DurabilityNote string}`.

**The test seam (F1), pinned.** `internal/op` declares an **exported**
package-level var:

```go
var SendTsMessageWithCommit = loop.SendTsMessageWithCommit
```

(same signature, same single production call site), and
`internal/cli/send.go:21`'s unexported var is deleted in the same commit. It is
**exported** deliberately: two existing tests in package `cli` reassign the
current var, and an unexported one in `op` would leave them nothing to patch.
An exported var needs no test-only setter function and no test relocation.

**Named exception to the zero-test-edit rule** (rows 1–2 of the M1 preamble's
closed table). Exactly two existing test files may be edited **by this item**,
and only to retarget that variable:

- `internal/cli/send_a5_test.go:229-237` —
  `TestSendPostLinkSyncFailureIsPartialSuccess` (decl `:227`): `:229` saves the
  original, `:230-236` installs a wrapper that DELEGATES to the real
  implementation and on success returns `id, true, errors.New("forced post-link
  sync failure")`, `:237` restores by defer.
- `internal/cli/send_b3_test.go:137-146` — `TestSendFreshButRacingText`
  (decl `:136`): `:137` saves, `:138-145` installs a NON-delegating stub
  returning `loop.TsID{}, false, &loop.ErrRecipientStale{…}`, `:146` restores.

Both stay where they are, so their surrounding assertions keep exercising the
real `cmdSend` rendering. **No other existing `*_test.go` may be touched in
M1.**

(e) §10.1: A5 preflight ordering, C29 classification, C4 collision retry,
partial-success (linked-but-sync-failed) — ported to seam level, driven through
BOTH `op.Context` constructors.

(f) Done-when: `tools/test.sh` green; `git diff` on `send_a5_test.go` and
`send_b3_test.go` shows ONLY the seam-retarget hunks named above (four lines
each, no other hunk); every other `*_test.go` unmodified.

(g) ~260 LOC.

**WI-1.3 — `op.Claim` (streaming).**

(a) Extract check's state half into
`op.Claim(cfg, ctx, ClaimReq{Limit int; NoArchive bool}, emit func(Event) error) (ClaimSummary, error)`.

(b) §3.2 (exact order: residue → quarantine → per-message validate/claim/emit →
owed discharge → expired-owed events); §4.5.2.

(c) NEW `internal/op/claim.go`; MODIFY `internal/cli/check.go`.

- The loop body `check.go:125-283` becomes the local emit renderer.
- **`displayConsumed` (`check.go:312-334`) SPLITS** (F5 — the round-1 item
  contradicted itself here): the **ClearOwed half** (`check.go:321-331`, plus
  its two `warning: failed to clear owed obligation` sites at `:324` and `:328`)
  moves into `op.Claim` and is **suppressed under `NoArchive`**, matching
  today's read-only variant `displayConsumedReadOnly` (`check.go:338-341`),
  which never discharges; the **print half** (`printConsumedBody` call at
  `:313`, `printReplyRefIfRequired` at `:333`) stays as the renderer.
- **CORRECTION to the round-1 "stays" range**: it is not `335-408` (`:335` is
  blank). What stays CLI-side unchanged is `displayConsumedReadOnly`
  (`338-341`), `printConsumedBody` (`348-364`), `sanitizeControlBytes`
  (`374-388`), and `printReplyRefIfRequired` (`393-408`).
- Latch arming stays CLI-side and remains **per-first-message before display**
  (E1): `latchArmed` (`check.go:168`), the `setLatch` closure (`169-175`), its
  three call sites (`:185` redelivered residue, `:243` `--no-archive` path,
  `:256` post-claim), and `maybeSetGuardLatch` (`292-300`).

(d) `ClaimSummary{Claimed, Redelivered, Quarantined, OwedExpired int}` — counts
only (D2), exactly DESIGN §3.2's four fields, **no additions**.

**The three load-bearing stdout lines are `Level:"info"` NoteEvents emitted at
their production points (R2 — this replaces round 2's `Listed`/`Remaining`
summary fields).** A summary field is read *after* the stream ends, so a
renderer driven from it prints these lines AFTER the expired-owed lines that
`op.Claim` emits inside the op — a silent reordering inside the merge whose
whole claim is zero behavior change, and one `check_age_owed_test.go:196-224`
would not catch (it matches with `strings.Contains`). Emitted as notes, the
order survives the local path and the wire alike. `Msg` carries the line
verbatim, with no level prefix (the renderer adds none for `info`):

| production point | guard, verbatim | `Msg` |
|---|---|---|
| after the redelivered-residue loop, **before** the claim loop (`check.go:200-202`) | `len(msgs) == 0 && len(redelivered) == 0` | `(inbox empty)` |
| at the limit break **inside** the claim loop (`check.go:206-208`) | `limit > 0 && claimed >= limit` | `(reached limit of %d; %d more pending)` with `limit`, `len(msgs)-claimed` |
| after the claim loop, **before** the owed/expired section (`check.go:261-263`) | `!noArchive && claimed > 0` | `check.go:262`'s string verbatim (`note: messages CLAIMED …`) |

Note the second row is why a count cannot substitute: the line is produced
mid-stream, between two `MessageEvent`s. The third row's text already begins
with `note: ` — that is its own wording (`check.go:262`), not a level prefix,
and it is emitted byte-for-byte.

`NoteEvent`s: `op.Claim` emits `Level:"warn"` for every stderr warning site it
now owns — `check.go:141`, `144-145`, `148`+`150`, `221-222`, `228`,
`232-233`, `276`, and the two ClearOwed sites `324`/`328` — with `Msg` carrying
the text WITHOUT the `warning: ` prefix. Its `Level:"info"` notes are **exactly
the three rows above and nothing else**. `check.go:298` (`failed to set guard
latch`) stays CLI-side: the latch is local (§6.6). The stale-obligation stdout
print (`check.go:278-279`) and the `clean --owed` hint (`check.go:279-281`) are
rendered from `OwedEvent`s + `OwedExpired`, not from notes.

Wraps `inbox.go:121,388,58,328`, `message.go:105`, `owed.go:241,149,273`,
`registration.go:34` (+ the 4 MiB cap `registration.go:19`).

(e) §10.1: claim idempotence, redelivery, quarantine, owed discharge; the
emitter memory test (≤1 body held); event ORDER test (a note lands between the
messages it occurred between); **a byte-for-byte stdout/stderr golden test for
the three `info` lines above**, in the empty, limited, and all-quarantined
shapes — asserting **position, not just presence**: the limit line lands
between two message renders, and the CLAIMED line lands BEFORE the
stale-obligation lines (the ordering a summary-derived renderer inverts).

(f) Done-when: `check_a6_test.go`, `check_age_owed_test.go`,
`consume_boundary_test.go`, `n3_sanitize_test.go` green unmodified.

(g) ~320 LOC.

**WI-1.4 — `op.Status` / `op.Gate` / `op.Pending` / `op.CleanOwed` (the read
side + the gate core).**

> **Ordering (F7/X2): this item is a PREREQUISITE of WI-1.5.** `op.Ack` calls
> `finishGateClear`; leaving `evaluateGate` in `internal/cli` while `op.Ack`
> calls it is an import cycle, because `internal/op` must not import
> `internal/cli`. The gate core moves here, first.

(a) The read-side ops + the owed prune, with the gate evaluation core moved
into `internal/op`.

(b) §3.6 (Pending: `MessageEvent`s with `Body` only under `ShowBody`;
`OwedEvent`s full-entry; `NeedsBoot` hub-derived, E4, `pending.go:77-97`);
§4.4.3 (the `status-ok` framing cap — applied by the WIRE producer, never by
this op; see (d)).

(c) NEW `internal/op/status.go`, `gate.go`, `pending.go`, `cleanowed.go`;
MODIFY `internal/cli/status.go`, `gate.go`, `pending.go`, `clean.go`.

`internal/cli/gate.go` keeps the `gateStatus` **type alias** (see (d)) and only
renderers over `op.GateResp`: `cmdGate` (`:42`), `evaluateGatePhase` (`:307`),
`emitGateText` (`:346`), `emitGateJSON` (`:363`), and the
codex-Stop/blocked-stderr emitters. `internal/cli/status.go` keeps
`printStatus`, `registrationStatusLabel`, `countInbox` and the formatters
unchanged; only `readRegistrations` (`status.go:84-95`) moves, since its
`ReadRegistrationsLenient` wrap is the op's read half (the move-set grep is
clean for it — no `*_test.go` names it). `internal/cli/clean.go` keeps
`cleanOwedResult`,
`emitCleanOwedText`, `emitCleanJSON` and the whole `--mailbox` half.
`internal/cli/pending.go` keeps the boot hint (`pending.go:182`) and the
`--fail-if-any` exit-2 rule (`pending.go:167-169`).

(d) **Fixed shapes (F8 — the round-1 item had no fixed-shapes clause at all;
M3 serializes these verbatim):**

- `GateReq{Phase string; RequireConfirm, AckStaleReg bool}` — exactly
  `evaluateGate`'s parameters (`gate.go:129`) minus `cfg`/`agentID`/`now`:
  `cfg` is the op's first argument, `agentID` is `Context.ActorID`, and `now`
  is HUB-minted (§2 hub clock) and is **never** a wire field.
- `GateResp` = the `gateStatus` struct's fields verbatim (`gate.go:271-286`),
  same field names, same JSON tags. `internal/cli` renders `op.GateResp`, and
  `turnEndJSON` (`turn_end.go:197-200`) embeds it — so `gate --json` and
  `turn-end --json` stay byte-identical.
  - **Alias, not deletion (R1a).** `internal/cli` keeps a one-line
    `type gateStatus = op.GateResp` — an **alias**, never a defined type, so
    `var got gateStatus` + `json.Unmarshal` still compiles and still names the
    same struct. Eleven existing code sites depend on it:
    `consume_boundary_test.go:257`, `gate_test.go:207,221,372`,
    `enforced_enrollment_test.go:106,128`,
    `turn_end_test.go:38,87,126,236,494` (plus a comment at
    `turn_end_test.go:249`, which needs nothing). Every field is exported, so
    an alias bridges all eleven with zero test edits.
- `StatusReq{}` — no fields; `status` is pool-wide and read-only.
- **`op.Status` STREAMS its warnings**, so its signature matches `op.Claim`'s
  and `op.Pending`'s:
  `op.Status(cfg, ctx, StatusReq{}, emit func(Event) error) (StatusResp, error)`.
- `StatusResp{Agents []StatusAgent; Truncated bool; Now time.Time}`:
  - `Agents` carries **every** row the pool has, sorted exactly as today by
    `loop.RegistrationsByAgentID` (`status.go:111`). **The op never truncates
    and always returns `Truncated:false`.** BOTH framing budgets — the 64 KiB
    encoded-line budget and the 64-row cap — are wire-FRAMING concerns and
    live in the wire producer: it appends rows in sort order while both hold
    and sets `truncated:true` when either excluded a row, as it writes
    `status-ok` (dispatch in WI-3.3a, shape and full rule in WI-3.1a, source
    of truth §4.4.3). The sort stays in the op, so truncation is
    deterministic wherever it is applied.
    **Why the cap moved out (T1a).** Round 3 put it inside the op "so local
    and remote agree by construction". That reasoning inverts: §4.4.3 is a
    FRAMING rule downstream of the 64 KiB line cap, and in local mode there is
    no line — so the only thing capping in-op buys is a silent row drop in the
    one mode that never needed one. `printStatus` renders no truncation notice
    of any kind (`status.go:97-139`), so a >64-row pool would lose rows with
    nothing on stdout or stderr saying so — a behavior change inside the merge
    whose sole acceptance criterion is that it has none, and whose release
    notes say "no behavior change" (WI-1.9(d)). Local and remote diverging
    here is not a defect to prevent; it is the fact `truncated:true` exists to
    report.
  - **`Now time.Time`** — the hub's evaluation instant. The AGE column and the
    STATUS label are both derived from one clock; on a remote lane that clock
    must be the HUB's, or a skewed client renders a different age than the
    pool's own truth. (Same reason `hello-ok` carries `hub_time`, §4.3.)
  - **There is NO `Warnings []string` field (T1c).** R4 was right that
    `ReadRegistrationsLenient`'s warnings need a channel (`cmdStatus` prints
    each to stderr as `warning: %s` **before** the table, `status.go:67-70`,
    each entry an `e.Error()` string, `status.go:91-92`); it picked the wrong
    one. That list is **unbounded and the 64-row cap does not bound it**: the
    lenient read appends one error per malformed `*.md` under the agents dir
    (`registration.go:522-549`), and a malformed file is not a registration,
    so the 2–~10-agent pool-scale assumption says nothing about how many there
    can be — a directory holding a thousand junk `.md` files yields a thousand
    strings, each carrying a path. That is exactly §4.4.3's "unbounded lists
    never ride inside one control frame".
    **So they stream as `note` frames**, which is where the plan already
    routes notes (WI-1.3, §4.4.3): the op emits one
    `NoteEvent{Level:"warn", Msg: e.Error()}` per read error, in
    `ReadRegistrationsLenient`'s own order, **before** it returns. The pinned
    `warn` routing (WI-1.1(d)) renders each as `warning: %s` on **stderr**,
    and a note necessarily precedes the terminal response — so both the bytes
    and their position (before the table) are today's, by construction, in
    local and remote mode alike. A cap-and-truncate contract was the
    alternative and is rejected: it would need its own second truncation flag,
    and its failure mode is a corrupt row going silent, which is the precise
    defect R4 opened.
- `StatusAgent{AgentID string; LastSeen time.Time; Host string; ProtocolVersion int; InboxDepth int; Status string}`
  — one entry per registration row, and a **status-specific view: the row IS
  the view; there is no nested `Reg RegistrationView` (T1b)**. Round 3 put the
  full eight-field `RegistrationView` here, which puts `Body` on a 64 KiB
  control line — a bio bounded only by `loop.MaxRegistrationBytes` (1 MiB,
  `registration.go:18`), so one long bio makes `status-ok` unframable — and
  contradicts DESIGN §4.4.3, which already says status rows carry no body.
  Dropping `Body` removes the 1 MiB field; it does **not** make the row a
  bounded shape — `Host` is free-form and length-unbounded
  (`registration.go:217-241`, `register.go:206`), which is why the wire
  producer budgets ENCODED BYTES and not just rows (WI-3.1a(d), §4.4.3). The
  field set is exactly what the `status` command renders, checked against
  `printStatus` line by line:

  | field | JSON | what renders it |
  |---|---|---|
  | `AgentID` | `agent_id` | the AGENT column (`status.go:115`); also the sort key (`:111`) and the protocol-warning subject (`:242-247`) |
  | `LastSeen` | `last_seen` | LAST_SEEN via `formatMaybeTime` (`:118`) **and** AGE via `formatAge(now, …)` (`:119`) |
  | `Host` | `host` | HOST via `formatDash` (`:120`) |
  | `ProtocolVersion` | `v` | PROTO via `formatProtocolVersion` (`:121`) and the `PROTOCOL WARNINGS:` block (`:126-138`, `:242-247`) |
  | `InboxDepth` | `inbox_depth` | INBOX = `countInbox(cfg.AgentInboxDir(id))` (`:113,165`) — hub-derived |
  | `Status` | `status` | STATUS = `registrationStatusLabel`'s verdict (`:151-163`), `lease-held` / `stale-would-sweep` / `fresh` — hub-derived, because it reads the serve claim and the pool's `StaleAfter` |

  The four registration-sourced fields keep the frontmatter's own key names
  (`agent_id`, `last_seen`, `host`, `v`) — the same tags `RegistrationView`
  uses for them (see WI-1.6(d)), so one field is never spelled two ways
  across the two views.
  **Nothing else is on the row.** `Vendor`, `ControlRepo`, `WorkingRepos` and
  `Body` are never rendered by `status`: the header's `vendor:` line is
  `cfg.Vendor`, the local config's, not any row's (`status.go:100`). Neither
  is the claim: round 3 carried a
  `Claim *StatusClaim{Host, PID, StartedAt, LastSeen, Stale}` and **no
  renderer reads it** — `registrationStatusLabel` consumes the claim hub-side
  (`status.go:152-154`) and the only fact that survives into the output is the
  `Status` label above. It is cut for the same reason `ServeClaim.ServeToken`
  (`lease.go:59`) is excluded: `status` is pool-visible to every member, so
  the row carries what `status` prints and nothing more. Re-add it in the
  merge that ships a renderer for it, never before.
  - **`printStatus` keeps its exact signature**
    (`printStatus(w io.Writer, cfg *loop.Config, regs map[string]*loop.Registration, now time.Time)`)
    — four existing call sites depend on it (`status_test.go:35,77,126,181`).
    M1 wires `cmdStatus` as: `op.Status` with an emitter that prints each
    `warn` note as `warning: %s` to stderr → rebuild the
    `map[string]*loop.Registration` from `Agents` → `printStatus` unchanged,
    which re-derives the label and the depth locally exactly as today. The
    rebuilt registrations are **partial by design** — only `AgentID`,
    `LastSeen`, `Host`, `ProtocolVersion` are set — and that is sufficient by
    inspection: those are the only fields `printStatus`,
    `registrationStatusLabel` and `protocolVersionWarning` read
    (`status.go:111-138,151-163,242-247`). A partially populated
    `loop.Registration` never escapes `cmdStatus`.
    `InboxDepth`/`Status`/`Now` exist for the WIRE consumer and are **not**
    read by the local renderer in M1; **M4 (WI-4.5) is where the renderer
    switches to them**, because a remote client can neither stat the hub's
    inboxes nor read its claims. An M1 implementer who "helpfully" retargets
    the renderer breaks four tests for no gain.
- `PendingReq{ShowBody bool}` /
  `PendingSummary{Unread, Owed, Malformed int; NeedsBoot bool}`.
- **`CleanOwedReq{Apply bool}` / `CleanOwedResp{Agent string; Pruned []string; Applied bool}`
  (R4)** — `cleanOwedResult` verbatim (`clean.go:99-103`), with its shipped
  JSON tags `agent` / `pruned` / `applied`:
  - The request field is `Apply`, mirroring the shipped `--yes` flag's polarity
    exactly (`clean.go:41,87`). An inverted `DryRun` is a defect waiting to
    happen. `--json` is a render flag and stays CLI-side; `--as`,
    `--control-repo`, `--loop-dir` are resolution flags, not op inputs;
    `--vendor` is accepted by `cmdClean` but unused on the `--owed` path.
  - **Counts alone lose behavior**: the text output prints one line per ref
    (`clean.go:189-192`) and `--json` emits the ref list
    (`clean_test.go:391-412` decodes into `cleanOwedResult` and asserts
    `Agent`, `len(Pruned)`, `Applied`). Returning `{Pruned, Remaining int}`
    fails that test.
  - `Pruned` is initialized to `[]string{}`, never nil (`clean.go:121`), so
    `--json` renders `[]` and not `null` — load-bearing for byte-identical
    JSON.
  - `Applied` follows `clean.go:171`: `apply && len(Pruned) > 0`.
  - `cleanOwedResult` itself **stays in `internal/cli`** as the render/JSON
    struct (`clean_test.go:407` names it), populated from the response.
    Nothing prints a "remaining" count today; the field is dropped.

(e) §10.1 ports; Pending event-stream tests (many owed entries; `ShowBody` body
present/absent); a golden test that `gate --json` and `turn-end --json` are
byte-identical before and after the move; **a corrupt-row test asserting
`status` still prints its `warning: …` line to stderr before the table** (now
via the `warn` note stream added in (d) — assert the STREAM ORDER too: the
note is emitted before `op.Status` returns, so the warning cannot land after
the table); **a >64-row pool asserting the op returns every row with
`Truncated:false` and that local `status` prints all of them** (the
regression T1a exists to prevent — it must FAIL against an in-op cap);
**a pool holding one valid registration whose `host` alone exceeds 64 KiB,
asserting the op returns it intact with `Truncated:false` and that local
`status` prints it** (the local mirror of the same rule: the byte budget is
framing-only, so it must not leak into the op — the row that the wire
producer will drop is the row local mode must still render); and
**a `clean --owed --json` golden with one
expired obligation and with none**, proving the ref list and the `[]`-not-`null`
empty shape survive.

(f) Done-when: `gate_test.go`, `status_test.go`, `pending_test.go`,
`pending_stale_age_test.go`, `clean_test.go`, `turn_end_test.go` green
unmodified.

(g) ~280 LOC.

**WI-1.5 — `op.Ack`.** *(Prerequisite: WI-1.4.)*

(a) `op.Ack(cfg, ctx, AckReq{}, emit func(Event) error) (AckSummary, error)`.

(b) §3.3.

(c) NEW `internal/op/ack.go`; MODIFY `internal/cli/ack.go` (the
`archiveAllClaimed` body MOVES from `ack.go:164-179`), `internal/cli/turn_end.go`
(its call at `turn_end.go:154` routes through the op).

Stays CLI-side: `--quiet`/exit-2 (`ack.go:20-55`), the guarded-session denial
(`ack.go:105-109` — it reads the **local** latch), the `ackItem`/`ackResult`
render structs (`ack.go:199-210`), `emitAckText` (`183-196`), `emitAckJSON`
(`:212`).

(d) `AckSummary{Acked int; GateClear bool; BlockReasons []string}`; wraps
`loop.ArchiveMessage` (`inbox.go:257`, idempotent EEXIST/SameFile handling
included) and `finishGateClear` — now `internal/op`'s, from WI-1.4.

(e) §10.1: ack idempotence (re-ack benign, `inbox.go:269-292`); emit-error
mid-stream leaves committed items committed.

(f) Done-when: `ack_test.go`, `turn_end_test.go`, `b1_convergence_test.go`
green unmodified.

(g) ~180 LOC.

**WI-1.6 — `op.Register`.**

(a) Registration (+announce) behind the seam with the D1 field semantics.

(b) §3.5 (the full three-shape contract).

(c) NEW `internal/op/register.go`; MODIFY `internal/cli/register.go`,
`internal/cli/boot.go`, `internal/cli/self_check.go`,
`internal/cli/turn_end.go`.

**The write path moves into `internal/op`** (B1 — no "implementer's choice"
here): the body of `performRegister` (`register.go:72-98` — CORRECTION: the
round-1 citation `35-189` conflated the struct with two other functions),
`publishRegistrationOnce` (`register.go:107-187`), and
`registrationLiveElsewhere` (`register.go:189-195`).

**`performRegister` stays in `cli` as a thin ADAPTER (R1b), and so do
`registerOpts`/`registerResult`.** Thirteen existing calls take the current
signature — `register_test.go:34,58,100,131,165,234,243,270,279,306,336,365,369`
— plus two bare `registerOpts` literals (`:33`, `:57`); and `:365` passes
`Bio:"old", BioProvided:true`, fields the reshaped request does not have, so
**no type alias can bridge it**. The adapter is:

```go
func performRegister(cfg *loop.Config, opts registerOpts, now time.Time) (*registerResult, error)
```

— unchanged signature, unchanged struct definitions at `register.go:35-44` and
`54-61` — which (1) resolves the host when `!opts.HostProvided`
(`register.go:80-87`'s `os.Hostname()` substitution and its warning text move
HERE, client-side, verbatim), (2) maps `Bio`/`BioProvided` to the `*string`
presence pointer, (3) calls `op.Register`, and (4) maps the response back into
`*registerResult`. It is a **caller** of `internal/op`, never a callee, so no
cycle. The four production call sites (`boot.go:89`, `register.go:262`,
`self_check.go:128`, `serve.go:536`) keep calling it and are unchanged. Tests
that only mention the name in a comment need nothing
(`hooks_sessionstart_test.go:11`, `turn_end_test.go:512`, `clean_test.go:476`,
`register_test.go:32,52`).

`internal/cli/register.go` keeps `cmdRegister` (`197-296`) as flag parsing +
rendering, including the `fs.Visit` presence capture (`226-233`).

**S1 — `internal/cli/self_check.go` is in scope.** `selfRepairRegistration`
(`self_check.go:117-133`) is the step-0 repair that `turn-end` drives
(`turn_end.go:120`) and that `self-check` is (`self_check.go:71`). It routes
through the op, and its "return a non-empty id even when the registration WRITE
failed" contract (`self_check.go:129-131`) must survive verbatim, because
`turn_end.go:121-123` aborts only on an EMPTY id and `turn_end.go:126` warns
and continues otherwise.

(d) `RegisterReq{Vendor *string; Host string; Bio *string; WorkingRepos []string; Announce bool; Sweep bool; ServeToken string}`
— **exactly DESIGN §3.5's field list; there is no `HostProvided` (R5)**:

- `Host` CLIENT-resolved (D1a), a plain string with **no presence flag**.
  `register.go:80-87`'s `os.Hostname()` substitution must never run for a
  remote actor — a nil/absent host resolved hub-side would record the HUB's
  hostname for every remote self-check — so the resolution happens in the
  caller (the `performRegister` adapter above; on the wire, the client) and
  the hub maps what it receives to `HostProvided:true` unconditionally. This
  is behavior-preserving in every arm: `--host` given ⇒ that value, explicit
  empty stays empty; `--host` absent ⇒ the caller's own `os.Hostname()`, i.e.
  today's value, and today's warning text on failure. Round 2 added a
  `HostProvided` field, which DESIGN deliberately does not have; it is
  removed, because carrying `false` over the wire is precisely the "hub
  substitutes its own hostname" bug the flag was meant to prevent.
- `Vendor` nil ⇒ hub-resolved (D1b). **`resolveAgentVendor` itself is
  UNCHANGED in M1** — it lives at `internal/cli/identity.go:72-83`
  (CORRECTION: the round-1 citation `57-82` was `vendorForAgentID`, which is
  `identity.go:57-70`). Only its four call sites branch, and that branch lands
  in **M4** (S2), not here.
- `Bio` pointer presence (`register.go:144` merge-preserve, `147-149` override,
  `226-233` capture).
- **`Sweep` — the boot-only discriminator (T3).** Round 3 had the op wrap
  boot's sweep with no request field to say when, which is not expressible:
  the shipped sweep runs in exactly two places, `cmdBoot` right after its own
  registration (`boot.go:99-101`, C11) and the runner's throttled tick
  (`serve.go:640`, which is `op.Tick`'s, WI-1.7 — not this op's). Always
  sweeping would add a pool-wide sweep to `register`, `self-check`,
  `turn-end`'s step-0 repair and `serve`'s startup registration; never
  sweeping would leave a remote `boot` with no hub-side trigger, because a
  remote lane's local sweep walks the mail-free shadow. So:
  - `Sweep:true` ⇒ the op runs `loop.SweepStaleRegistrations(cfg,
    ctx.ActorID, now)` (`sweep.go:60`) **after** the registration write, in
    boot's order, and on failure appends
    `fmt.Sprintf("sweep stale registrations: %v", err)` to `RegisterResp.Warnings`
    — `boot.go:100`'s string verbatim, with no `agentchute serve: ` prefix
    (that prefix belongs to the tick's warning, WI-1.7(d)). Never an error.
  - **`Sweep:false` from every other caller**, including the channel's
    mandatory startup `register` (§6.1): that lane's sweeping is the tick's,
    whose first pass is due immediately (`lastSweep`'s zero value,
    `serve.go:635-644`), so sweeping here too would double it.
  - **M1 has no `true` caller.** The `performRegister` adapter passes
    `Sweep:false` — `registerOpts` has no such field and its signature is
    fixed (R1b) — and `cmdBoot` keeps its own `loop.SweepStaleRegistrations`
    call at `boot.go:99-101` untouched, so local behavior is unchanged by
    construction. Same treatment as `Announce`: the field exists for M4/M5,
    where it must already be on the wire. The remote arm (and the deletion of
    the local call on a remote lane, so no arm sweeps twice) lands in
    **WI-4.5**.
- `ServeToken` vs `registrationLiveElsewhere` (`register.go:126-128,189-195`).

`RegisterResp{Announce *AnnounceView; Pending int; Reg RegistrationView; InboxDir string; Refreshed, ExistingFound bool; ResolvedHost string; Warnings []string}`,
with `AnnounceView{Sent, Total int; Warnings []string}` — the tagged mirror of
`loop.AnnounceResult` (`internal/loop/message.go:37-41`), for the same reason
`RegistrationView` is one (no JSON tags; `internal/loop` frozen in M1).

**The six render fields are forced, not optional** (knock-on of R1b, same
class as R4). Beyond the op's own two facts every consumer of today's
`registerResult` reads more, and M1 must be invisible:
`cmdRegister` prints `ResolvedHost` and `InboxDir` (`register.go:270,274`) and
`Warnings` (`:276-278`); `cmdBoot` reads `InboxDir`, `Reg.LastSeen`,
`Refreshed`, `ExistingFound`, `ResolvedHost` (`boot.go:104,111,137-142,200`);
`selfRepairRegistration` reads `ResolvedHost` and `Reg.LastSeen`
(`self_check.go:79-80`); `register_test.go` asserts `res.InboxDir` (`:144-145`),
`result.ExistingFound` (`:344,373`) and `result.Reg.{AgentID,LastSeen,Body}`
(`:347,376,379`).

**`RegistrationView` is defined HERE** — `RegisterResp.Reg` is its only user
(round 3 defined it under WI-1.4, whose status rows no longer carry it, T1b).
It is an `internal/op` struct mirroring `loop.Registration`'s eight fields
(`registration.go:58-68`) — `AgentID`, `ProtocolVersion`, `Vendor`,
`ControlRepo`, `WorkingRepos`, `Host`, `LastSeen`, `Body` — with the
frontmatter's own key names as JSON tags (`agent_id`, `v`, `vendor`,
`control_repo`, `working_repos`, `host`, `last_seen`, `body`). A mirror is
required, not a preference: `loop.Registration` carries **no JSON tags** (it is
frontmatter-serialized) and `internal/loop` is frozen in M1. Two conversion
funcs in `internal/op` (to/from `*loop.Registration`) are the single place that
mapping lives, and `StatusAgent` (WI-1.4(d)) reuses four of these tag names
verbatim so one field is never spelled two ways.
`Refreshed` is `true` on every successful write (`register.go:183`);
`ExistingFound` is the pre-write existence fact (`:184`); `Warnings` is empty
today and stays a field so the hub can populate it.

**`Announce` is a view, not a count** (same class, one layer out). `cmdRegister`
renders three facts from `loop.AnnounceEnrollment`'s result — each per-peer
warning (`register.go:285-287`), then `no peers to announce to` when `Total ==
0` or `sent to %d of %d peer(s)` from `Sent` and `Total` (`:288-292`) — so an
`AnnouncedTo int` carries one of the three and a remote lane, whose fan-out
runs hub-side, could not reproduce the other two. Nil unless the request set
`Announce`; a hub-side announce that failed outright leaves it nil and appends
`announce failed: <err>` to `Warnings`, which the client prints through the
same `warning: %s` loop (`:276-278`) — the same bytes in the same stderr
position, since `performRegister` returns no warnings of its own
(`:180-186`). In M1 the adapter passes `Announce:false` and `cmdRegister` keeps
its own `loop.AnnounceEnrollment` call (`:280-294`), so nothing local changes;
the field exists for M4/M5, where it must already be on the wire.

**There is no `Created bool`.** An earlier draft carried one beside
`ExistingFound`; it is exactly `!ExistingFound` in every returned response
(`existingFound` comes from the pre-write read, `register.go:112,120-125`, and
the exclusive-create arm is taken on precisely `!existingFound`, `:159-171`;
the EEXIST re-read re-enters that same arm, `:90-97`), and `emitBootText`
already derives the "Registered" verb that way (`boot.go:200`). Do not
re-add it.

**Both DESIGN gaps this item flagged are closed in DESIGN as it stands**:
§3.5's Output line carries the full set (with `Announce`), and §4.4.3 puts
`reg.body` (a bio bounded by `registration.go:18` — CORRECTION: the earlier
`:19` citation was `MaxInboxMessageBytes`) in a body trailer, never the control
line; §4.4.1 shows the `register-ok` frame. The two documents state one
`RegisterResp`; a revision that moves one moves both.

(e) §10.1 ports of the register/boot/self-check tests; local-mode equivalence
(a nil `Vendor` behaves exactly as today's `resolveAgentVendor` result);
**the adapter's host arm**: `--host` given / explicit `--host ""` / absent ⇒
the value written matches today byte-for-byte, with the `os.Hostname()`-failed
warning still emitted from the CLI side; and one direct `op.Register` call with
`Announce:true` against a seeded multi-peer pool holding one undeliverable peer
⇒ `Announce.Sent`/`.Total`/`.Warnings` match a direct `loop.AnnounceEnrollment`
on the same pool — the only M1 coverage of a field no local caller reads yet.
Same for the other such field: one direct `op.Register` with **`Sweep:true`**
against a pool holding one stale, lease-dead peer row ⇒ that row is gone
afterwards and the sweep ran AFTER the registration write (the caller's own row
is present and fresh, never swept); the identical call with `Sweep:false` ⇒
the stale row is still there — and a `Sweep:true` whose sweep fails ⇒ exactly
one `Warnings` entry, `sweep stale registrations: <err>`, with the register
itself still succeeding.

(f) Done-when: `register_test.go`, `boot_test.go`, `self_check_test.go`,
`enforced_enrollment_test.go` green unmodified — `register_test.go` in
particular, since it is the file the adapter exists to keep untouched.

(g) ~250 LOC.

**WI-1.7 — the lease/tick handle: `op.NewChannel`.** *(Prerequisite: WI-1.6.)*

(a) Lease lifecycle + the tick composite behind a **stateful handle**.

(b) §3.4 (no `poll` op — cut); §6.1; §6.2.

**Why a handle and not free functions (F2).** `op.Tick` cannot be stateless: it
needs the registration heartbeat template (today `regTemplate`, `serve.go:237`,
filled from `heartbeatTemplate(cfg, opts)`, `serve.go:554-563`, assigned at
`serve.go:430`), the 10-minute sweep throttle (`lastSweep`, `serve.go:242`,
`sweepInterval`, `serve.go:51`), and the lease token across calls. This shape is
inherited verbatim by M3's hub session and M5's remote serve, so it is pinned
once, here.

(d) **Fixed shape:**

```go
type ChannelOpts struct {
    HeartbeatTemplate *loop.Registration
    Lease             *loop.ServeLease // local adoption arm; nil ⇒ the channel acquires
}

func NewChannel(cfg *loop.Config, ctx Context, opts ChannelOpts) *Channel

func (c *Channel) AcquireLease(LeaseReq) (LeaseResp, error) // records the token
func (c *Channel) Token() string                            // for AGENTCHUTE_SERVE_TOKEN
func (c *Channel) Register(RegisterReq) (RegisterResp, error)
func (c *Channel) Tick(TickReq) (TickResp, error)
func (c *Channel) ReleaseLease() error
```

`ChannelOpts` is a **constructor input, not a wire struct** — the serialized
lease shapes are `LeaseReq{}` / `LeaseResp{Token string}` (§3.4), unchanged.

- `Lease` **non-nil** ⇒ the channel ADOPTS an already-held lease and
  `Token()` returns `Lease.Token`. This is the LOCAL runner's arm, and it is
  what keeps C10's pinned startup order literal: the runner still acquires
  through `refuseLiveRunnerCollision` (`serve.go:571-580`), which stays
  **byte-unchanged**, signature included. Without this arm the helper's return
  type would change and four existing call sites break
  (`run_test.go:130,140,159,170`, whose returned leases are passed straight to
  `loop.ReleaseLease` at `:147`, `loop.RenewLease` at `:163`, and
  `loop.ReleaseLease` again at `:166` and `:174` — CORRECTION, T5: round 3
  wrote `:147,164,167,172`. The fifth lease call in that file, `:137`,
  releases a lease this helper never returned — `loop.AcquireServeLease`'s own
  from `:126`) — an exception nobody needs.
- `Lease` **nil** ⇒ `AcquireLease` acquires and records the token. That is the
  HUB session's arm (WI-3.3a) and the remote channel's (WI-5.1). M1 exercises
  only the adoption arm; the two-arm shape is pinned NOW so M3 does not have
  to reshape the seam — exactly as with `HeartbeatTemplate` below.
- `HeartbeatTemplate` **non-nil** ⇒ used verbatim. The LOCAL runner passes
  today's `heartbeatTemplate(cfg, opts)` value, so M1 changes no heartbeat
  byte.
- `HeartbeatTemplate` **nil** ⇒ `Register` derives and caches the template from
  the `RegisterReq`, and a `Tick` before `Register` returns `op.ErrOrder`
  (`E_ORDER`, §6.1 step 3). M1 exercises only the non-nil arm; the nil arm's
  derivation lands with the hub session (WI-3.3a). The two-arm shape is pinned
  NOW so M3 does not have to reshape the seam.
- `Register` injects `c.Token()` into the request (§3.5 `ServeToken`).
- The local runner (`runnerRuntime`) stores ONE `*op.Channel` **in place of
  three fields**: `lease` (`serve.go:229`), `regTemplate` (`serve.go:237`),
  and `lastSweep` (`serve.go:242`).

  **Named exception, row 3 of the M1 preamble table (R1c).** One test helper
  builds that literal: `newPollTestRuntime` (`run_test.go:481-509`), whose
  `:495-507` acquires the lease and sets `lease:` (`:504`) and `regTemplate:`
  (`:505`). It becomes `op.NewChannel(cfg, ctx, ChannelOpts{Lease: lease,
  HeartbeatTemplate: &tmpl})` assigned to the single field. **One helper, one
  hunk, one file**: its callers are unchanged (`b1_convergence_test.go:36`
  and `run_test.go`'s own poll tests all go through the helper), and
  `run_resize_unix_test.go:44` builds `&runnerRuntime{ptmx:…, stopCh:…}` —
  fields this item does not touch, so that file is untouched too.
- `LeaseResp{Token string}`.
- **`TickResp{Pending, Skipped int; Swept []string; Warnings []string}`** (F3).
  `Tick` returns a non-nil ERROR **only for the fenced case** (`op.ErrFenced` —
  today's fatal branch, `serve.go:617-620`, which buffers the fatal message and
  SIGTERMs the child). Every other step failure becomes ONE `Warnings` entry,
  in production order, carrying the EXACT string today's `r.logf` receives
  minus its trailing newline:

  | source | anchor | warning text |
  |---|---|---|
  | renew, non-fenced | `serve.go:622` | `agentchute serve: renew serve lease: <err>` |
  | heartbeat | `serve.go:631` | `agentchute serve: heartbeat registration: <err>` |
  | sweep | `serve.go:641` | `agentchute serve: sweep stale registrations: <err>` |

  The CLI renders each as `r.logf("%s\n", w)`, so the runner log is
  byte-identical. `lastSweep` is advanced even when the sweep failed
  (`serve.go:643`) — the throttle keeps that behavior. The runner-state write
  failure (`serve.go:675`) is NOT a tick concern and stays in `pollOnce`.
  **Wire note:** DESIGN §4.4.1's `tick-ok` example carries `warnings` (both in
  the clean and the step-failure shape), and the paragraph under it — "`warnings`
  is `[]string` and, like `durability_note` on `send-ok`, is **always
  present**" — states the always-present rule; the M2 spec copy MUST reproduce
  both (WI-2.1) and the codec's fixed shape lists it (WI-3.1a).
- Tick composition, in `pollOnce`'s order: `loop.RenewLease` (`lease.go:317`) +
  `loop.HeartbeatRegistration` (`registration.go:192`) with the cached template
  + the 10-min-throttled `loop.SweepStaleRegistrations` (`sweep.go:60`) +
  pending counts (the seam version of `hasPendingInboxMail`,
  `serve.go:757-763`, which **fails OPEN** on a non-`ErrInboxMissing` error —
  preserve that).

(c) NEW `internal/op/channel.go`; MODIFY `internal/cli/serve.go` (`pollOnce`
`609-677` calls `Tick`; `runWrapper` constructs the channel with the lease
`refuseLiveRunnerCollision` already returned). **`refuseLiveRunnerCollision`
(`571-580`) is byte-unchanged** — see the `Lease` adoption arm in (d).

(e) §10.1: lease acquire/renew/release/fence, heartbeat self-heal, sweep
back-off; **the local serve startup order is PINNED and UNCHANGED** (C10):
lease (`serve.go:374`) → child (`serve.go:379-385`) → register
(`serve.go:393`), including the rollback at `serve.go:394-399`.

(f) Done-when: `m4_inject_recheck_test.go`, `b1_convergence_test.go`,
`run_resize_unix_test.go` and every lease/sweep test green **unmodified**, and
`run_test.go` green with its diff limited to the `newPollTestRuntime` hunk
named in (d) — no other hunk in that file; a new order-pin test asserts the
local sequence.

(g) ~240 LOC.

**WI-1.8 — M1 acceptance sweep + dependency-direction test.**

(a) Prove the refactor invisible, the seam dual-callable, and the dependency
direction enforced.

(b) §10.1 in full; §7.4 (dependency direction, B2).

(c) NEW `internal/op/*_test.go` additions only.

(d) Both-Context-constructor matrix: every actor-scoped op executed with a
CLI-resolved ctx AND a directly-constructed pinned ctx, asserting identical
state effects. PLUS the **dependency-direction test** (B1/B2): a test that
fails if `internal/op` imports `internal/cli`, `internal/hubwire`, or
`internal/hubclient` — implemented with `go/build`'s import scan over the
package's own files (stdlib only, no new dependency), asserting the import set
is a subset of {stdlib, `github.com/agentchute/agentchute/internal/loop`}. The
same file asserts `internal/loop` imports no `internal/op`.

(e) = (d) plus the §10.1 list.

(f) Done-when: `tools/test.sh` fully green with **zero** edits under any
existing `*_test.go` EXCEPT the closed table in the M1 preamble — three files
(`send_a5_test.go`, `send_b3_test.go`, `run_test.go`), three hunks, and each
diff limited to the hunk named there; `git diff --stat` shows no
`internal/loop` change. This item also **re-runs the move-set grep** over
every identifier M1 removed from package `cli` and fails if any code-level hit
outside that table survives — the check that keeps the criterion honest rather
than aspirational.

(g) ~300 LOC (tests).

**WI-1.9 — v1.6.0 release notes + CHANGELOG. (SPENT — merged with M1.
`docs/releases/v1.6.0.md` and the CHANGELOG Unreleased section are on
`main` and FROZEN until WI-6.6. Do not re-open.)**

(a) The tag literally cannot release without this file: `release.yaml:110` runs
`test -s "docs/releases/${tag}.md"` and `release.yaml:168` passes it as
`--notes-file`. A missing file fails the release job **after** the tag is
already pushed.

(c) NEW `docs/releases/v1.6.0.md`; MODIFY `CHANGELOG.md` (a new
`## v1.6.0 (<date>) — <headline>` section in the same shape as v1.5.7).

(d) The notes state explicitly: **internal refactor only; no behavior change;
no hub capability is claimed or shipped; nothing to do on any lane.** No fleet
cutover, no forced update.

(f) Done-when: `docs/releases/v1.6.0.md` exists and is non-empty at the M1 PR
head; `tools/fact-sweep.sh` PASS (CHANGELOG.md is check 3's surface).

(g) ~40 LOC prose.

→ **Spent.** No tag after M1. See WI-6.6 / checklist §5.

### M2 — spec merge (prose only; no Go changes)

**WI-2.1 — normative "Hub wire & lifecycle" spec section.**

(a) Add the binding hub section to `AGENTCHUTE.md`.

(b) §9.1 first bullet (C7).

(c) MODIFY `AGENTCHUTE.md`. **Pin: it lands as §13**, which is a genuinely
VACANT number today — the file runs §12 → §14 → §15, so nothing renumbers. The
`HUB.md`-by-reference option DESIGN §9.1 allows remains available if length
forces it; codex gates either way.

(d) Content, all literal values byte-for-byte from DESIGN §4:

- frame grammar + the 64 KiB / 4 MiB limits (§4.3);
- vocabulary + per-session serial order (§4.4), including
  **`tick-ok.warnings`** (F3) and **`note.level`'s two-value vocabulary with
  its routing** — `warn` → stderr, `info` → stdout (F4/R2; DESIGN §4.3's
  `note` bullet states both arms, and the spec copy must too);
- version handshake + hub-upgrades-first (§4.3);
- never-auto-replay (§4.5.3) **and the mandatory `send-ok.committed` flag it is
  written against** (§4.4.1, F11) — the spec's copy of that example carries
  `"committed":true`, and a `send-ok` without the field is **malformed**, not a
  defaulted `false`;
- disconnect-after-claim redelivery (§4.5.2);
- remote turn-end order (§6.6);
- the timeout table (§4.6);
- the pinning model (§5);
- **the error-code registry WITH each code's emitter side** (§4.4.2 as amended,
  F9/X1): hub-emitted, client-emitted, or **both** — `E_POOL_MISMATCH` is the
  one code with two emitters and two exact §7.5 texts, and the spec must say so
  rather than classifying it client-only.

(f) Done-when: every literal value (timeouts, caps, frame names, codes) matches
DESIGN.md §4 byte-for-byte; codex spec-gate SHIP.

(g) ~190 LOC prose.

**WI-2.2 — section amendments.**

(a) The §9.1 amendment list: §1 (reference transport), §2 (hub as a supported,
CI-tested config; network-mount demoted), §4 (`ssh://` locator), §5.4 (hub-side
session pid; token-checked release; **§6.9 boot-REFERENCE corroboration**), §8
(tick as the remote poll), §12 (non-goals rewording), §15 (pinning), plus the
`setup --wipe-state` surface: `state/pool.id` as preserved non-runtime scaffold
(H1).

(b) §9.1.

(c) MODIFY `AGENTCHUTE.md`.

(d) The §5.4 amendment is the ONE behavioral change to existing lease semantics
and must state the three properties that bound it: the comparison is
**equality only, never ordering**; the reference has **no wall-clock
component**, so a clock step cannot forge a difference; and the reference is
**HOST-scoped** (R12) — `/proc/sys/kernel/random/boot_id` and
`kern.bootsessionuuid` change on a host **reboot** and on nothing else, so a
container/VM restart, a service restart, or a namespace re-create on a host
that did NOT reboot leaves the ref identical and the claim behaves exactly as
today. The spec text must carry that clause rather than let a reader conclude
pid reuse is solved generally: the fix is scoped to reboots, and every other
pid-reuse wedge keeps the §7.5 manual remediation. An **absent ref means
unchanged, pre-existing behavior**. The withdrawn
`StartedAt`-predates-boot-*time* rule must **not** reach the spec (§6.9, B6).
For the wipe surface, name the three places behavior changes (wipe plan,
post-wipe rescan, dry-run/preserved output) so the M5 implementer and the spec
agree.

(f) Done-when: §12 no longer forbids what M3+ ships; the boot-ref wording says
"equality", "no wall-clock component" **and** "host-scoped (a restart without a
reboot is unchanged from today)" explicitly; wipe/pool.id wording present;
codex SHIP.

(g) ~130 LOC prose.

**WI-2.3 — EXTENSIONS.md alignment.** (b) §9.2. (c) MODIFY `EXTENSIONS.md`
(the HTTP sketch gains the phase-2-behind-`internal/op` line; any wording
implying every non-filesystem *transport* is out of the reference CLI is
aligned with the amended §12). (f) codex SHIP. (g) ~20 LOC.

**WI-2.4 — enrollment surface.**

(a) **FOUR** lane-facing rules (G-M4 as amended — the round-1 item said three
and predates P8): remoteness is *discovered* (never a `--hub`/`--remote`
spelling); NEVER re-send after `E_SEND_UNKNOWN` without confirming
non-delivery; run `doctor` immediately after a join and after any hub move; and
**on `E_VERSION`, WAIT for the hub to upgrade — never "fix" it by re-running
`hub join`** (P8: the hub upgrades first; re-joining rotates a key for no
reason).

(b) §9.1 second bullet; §7.5's `E_VERSION` text.

(c) MODIFY `AGENTS.md`, `CLAUDE.md`, `CODEX.md`, `GEMINI.md`, `GROK.md`,
`examples/hooks/` templates — the enrollment prose blocks only.

(d) **The enrollment prose blocks are MARKER-VERSIONED** (CHANGELOG records
"marker v30" for v1.5.7). This edit bumps the marker, and the embedded copies
must be regenerated in the same commit or `templates_drift_test.go` /
`assets_test.go` fail — the `//go:embed` coupling rule: a zero-`.go`-diff is
not a zero-behavior-change here.

(f) Done-when: grep finds all four rules in every wrapper file; the marker is
bumped; `templates_drift_test.go` and `assets_test.go` green; codex SHIP.

(g) ~40 LOC prose.

### M3 — wire codec + `hub session` + conformance vectors

**WI-3.1a — codec: frames, codes, round-trip.**

(a) NDJSON control frames + raw body trailers, both directions.

(b) §4.3 (framing bullets), §4.4 (vocabulary), §4.4.2 (registry), §4.4.3
(producer rules).

(c) NEW `internal/hubwire/frame.go`, `codec.go`, `codes.go`.

(d) Fixed: LF-terminated UTF-8 JSON objects; 64 KiB line cap; `body_len`
trailers ≤ 4 MiB (absent = no trailer); client `id` / response `re`; one
request in flight; unknown fields ignored; unknown `t` → `E_UNSUPPORTED`
(session survives); a framing violation → error frame + close, **received
frames only**; the hub MUST NOT compose a >64 KiB line (streamed aggregates:
`msg`/`note`/`owed-item`/`ack-item`; `status-ok` two budgets + `truncated`).
Full vocabulary exactly as §4.4 (no `ping`/`poll`/`pending-item`). Plus:

- **The `status-ok` budgets live HERE, in the wire producer — never in
  `op.Status` (T1a).** This package owns the `status-ok` encoder. It takes
  the op's fully-sorted `Agents` slice and appends rows **in order**, keeping
  a row only while BOTH hold:
  1. **wire budget** — the complete encoded control line, counting every
     non-row field (`t`, `re`, `now`, `truncated`), the `agents` array's own
     punctuation and the terminating LF, stays ≤ 64 KiB;
  2. **row cap** — the kept-row count stays ≤ 64.

  The first row that fails either check ends the append (prefix semantics —
  never skip-and-continue, which would omit a middle row while showing later
  ones), and `"truncated":true` is written whenever **either** budget
  excluded a row. `StatusResp.Truncated` from the op is always `false`
  (WI-1.4(d)); the producer is the only writer of the `true` value. Budgets
  applied one layer in would silently drop rows in local mode, where there is
  no line to overflow.
  - **A row count alone does not bound the line.** `Host` is free-form:
    `ReadRegistration` copies frontmatter `host` through verbatim under only
    the 1 MiB whole-file cap (`registration.go:72,88`),
    `Registration.Validate` imposes no length bound
    (`registration.go:217-241`), and `register --host` is an unconstrained
    string flag (`register.go:206`) — so one otherwise-valid row with a 70 KiB
    host exceeds the cap before row count matters. `AgentID` is
    charset-constrained but also length-unbounded (`agentIDPattern`,
    `internal/loop/inbox.go:40`); only `NAME_MAX` on `<id>.md` bounds it in
    practice. **Implement against no bounded-field assumption.**
  - **Non-row fields COUNT against the byte budget.** §4.4.3's normative rule
    is that the hub never emits a control line over 64 KiB; a rows-only
    budget balances its own books and still emits an over-cap line. Cost: one
    integer.
  - **Measure with `"truncated":false` encoded.** `true` is one byte shorter
    than `false`, so a frame that fit before the flip still fits after it —
    no second encoding pass, no re-measure.
  - **First row alone too big ⇒ `agents:[]` with `truncated:true`.** A valid
    response, not an error: `status` is read-only and pool-wide, and refusing
    the whole listing over one pathological row would lose every other
    agent's row. The client notice (WI-4.5) names the wire limit, not only
    the row cap.

- **`send-ok.committed` is MANDATORY** (F11): decode it as `*bool` and treat
  absence as `E_MALFORMED_FRAME` — never as a defaulted `false`. This is the
  field the never-replay rule is written against.
- **`send-ok.owed_note` and `send-ok.durability_note` are mandatory
  strings**, `""` when clean, never omitted (absence is `E_MALFORMED_FRAME`).
  They are independent. A non-empty `owed_note` is not a delivery failure
  and must not drive resend. `op.Send` returns a nil error once delivery
  commits. DESIGN §3.1 / §4.4.1 now match AGENTCHUTE.md §13 (this erratum
  closed the carve-out).
- **Error-path `claimed_held`** (#152 item 2, placement now pinned): a
  top-level optional boolean on the terminal `error` frame, encoded only
  as `true` and omitted otherwise, set when `Claim` returns an error with
  `ClaimSummary.Redelivered > 0`. M3 frames it; M4 arms the local latch on
  `true`. **`check-ok.redelivered` is unchanged** — `check-ok` is only
  emitted on a nil error, where residue found equals residue delivered. A
  `note` frame will **not** do — arming a latch must never depend on
  parsing display text.
- **`tick-ok.warnings` is `[]string`, always present** (F3), `[]` when empty,
  never omitted.
- **`note.level` is one of exactly `"warn"` / `"info"`** (WI-1.1).
- `codes.go` re-exports `op.CodeFor`'s mapping and ADDS **exactly eight**
  session/codec codes: `E_VERSION`, `E_IDENTITY`, `E_POOL_NOT_FOUND`,
  `E_POOL_ID_INVALID`, `E_POOL_MISMATCH` (**hub arm**), `E_MALFORMED_FRAME`,
  `E_TOO_LARGE`, `E_UNSUPPORTED`. **`E_ORDER` is NOT re-added here (R3)** — it
  is an op sentinel (WI-1.1) and adding it makes the union double-count.
- `codes.go` also carries the **emitter classification table**, one row per
  code: `hub` / `client` / `both`. Exactly one row is `both`
  (`E_POOL_MISMATCH`); it is what lets the client-only list be asserted
  disjoint without lying about that code.

(e) §10.2: round-trip every frame type; trailer boundaries byte-exact at
0 B / 1 B / 4 MiB / 4 MiB+1 (`E_TOO_LARGE`); truncated frame and truncated
trailer at every boundary; unknown `t`; unknown fields ignored; oversize line;
interleaved `note` frames. Plus **the `status-ok` budget rows**, a
`StatusResp` whose own `Truncated` is `false` being the input to every one of
them (the producer, not the op, sets the flag):

- **row cap** — 64 small rows in ⇒ 64 out with `truncated:false`; 65 in ⇒ the
  first 64 in the op's sort order with `truncated:true`.
- **wire budget, the row the row-cap misses** — a THREE-row pool in which one
  otherwise-valid registration's `host` alone exceeds 64 KiB: that row is
  excluded, `truncated:true`, and the emitted line measures ≤ 64 KiB **as a
  whole line, LF included**. This row fails against a producer that only
  counts rows, and fails against one that budgets rows without their frame.
- **prefix, not skip** — the same oversized row placed FIRST in sort order ⇒
  `agents:[]`, `truncated:true`, and the two later small rows dropped as
  well; placed LAST ⇒ the two earlier rows survive. A skip-and-continue
  producer passes the first shape and fails this pair.
- **boundary, byte-exact** — a synthetic pool sized so the encoded line lands
  exactly on the budget: the largest frame that fits ⇒ all rows,
  `truncated:false`, length exactly 64 KiB; one byte more ⇒ the last row
  drops with `truncated:true`. Assert the ENCODED length, never a row count.
- **flag flip is free** — the same pool with ONE more row appended: the kept
  rows still measure exactly 64 KiB under `"truncated":false`, the extra row
  is excluded, and the frame actually emitted (`"truncated":true`, one byte
  shorter) is still ≤ 64 KiB. That is the measure-with-`false` rule's
  no-second-pass claim, asserted rather than assumed.

Plus a
**registry completeness test, with the arithmetic that actually holds (R3)**:

- `op.CodeFor`'s output set is **9** — the eight sentinels plus the `E_HUB_IO`
  default arm (WI-1.1(d)).
- this package's own set is **8** (above; no `E_ORDER`).
- the two sets are **disjoint**, and their union is **17** = §4.4.2's hub-side
  registry **exactly** — no duplicate, no gap. Assert the union against a
  literal list, so a code added to either side without a registry row fails.
- the **client-only** list is **9** (`E_CONNECT`, `E_UNAUTHORIZED`,
  `E_HOSTKEY_CHANGED`, `E_CHANNEL_LOST`, `E_SEND_UNKNOWN`, `E_HELLO_TIMEOUT`,
  `E_HUB_NO_BINARY`, `E_NOT_JOINED`, `E_NO_SSH`) and is disjoint from that
  17-row union.
- `E_POOL_MISMATCH` is asserted to be classified `both` — present in the
  hub-side union AND emitted client-side — and is **not** a member of the
  client-only list. Requiring it in both lists is the contradiction round 2
  shipped; this replaces it.

(f) Done-when: the §10.2 round-trip suite green over `net.Pipe`.

(g) ~350 LOC + ~250 test.

**WI-3.1b — fuzz target + streaming/typed-event-stream tests.**

(a) Harden the parser (the §5.4 attack surface) and prove the streaming
contract end-to-end.

(b) §4.3, §4.4.3 (streaming reaches all the way down, C4), §10.2.

(c) NEW `internal/hubwire/fuzz_test.go`, `stream_test.go`, and a checked-in
seed corpus under `internal/hubwire/testdata/fuzz/`.

(e) A `go test -fuzz` target on the frame parser whose **seed corpus runs as an
ordinary test** in the normal (non-fuzzing) run — so it lands in CI's existing
`go` job with no CI change at all. Typed-event-stream tests: a `check` over many
maximum-size (4 MiB) messages with a peak-memory assertion (≤1 body held); a
`check` producing many interleaved `note`/`owed-item` frames with
**frame-level production order** preserved (M3 does **not** assert
rendered `warn`→stderr / `info`→stdout — that golden is WI-4.5, because
production remote rendering first lands there; a test-only renderer
would prove no production behavior, the same trap as a green W2 in M3
claiming client behavior); a `pending` with many owed entries and one 4 MiB
`show_body` body as a trailer; transport failure injected **after the first
emitted item** AND **after a mid-stream `note`** (claim durable, no partial
frame, clean abort, no note silently lost before the failure point).

(f) Done-when: the seed corpus is green under `tools/test.sh` (not only under
`-fuzz`); the peak-memory assertion holds with 32 × 4 MiB messages.

(g) ~250 LOC (tests).

**WI-3.2 — handshake.**

(a) `hello`/`hello-ok` with version + identity + pool checks.

(b) §4.3 (handshake block, pool-check bullet, `hub_time` offset).

(c) NEW `internal/hubwire/handshake.go`.

(d) `hello{proto:"agentchute-hub", v:1, min_v:1, agent, bin}`;
`hello-ok{v, agent, pool, pool12, writable, hub_bin, hub_time}`; version rule
`use = min(hub_max, client_v)`, reject below either min → `E_VERSION`;
`E_IDENTITY` on `agent` ≠ pinned; `pool12` **read from `state/pool.id`, never
an argv echo** (F1); `writable` = a temp-file create/remove probe under the
pool's `state/` (G-M5).

**`E_POOL_MISMATCH` scope for this item** (F9/X1): the handshake carries
`hello-ok.pool12` = the value READ FROM THE POOL, and nothing else. The **hub
arm** (session start, `pool.id` absent or ≠ `--pool-id`) is WI-3.3a's; the
**client arm** (recorded-vs-`hello-ok`) is M4's. Do not implement either here.

**P3 — pool.id has no production writer until M5.** Every M3 test that needs a
`state/pool.id` **mints its own fixture**:
`os.OpenFile(path, O_WRONLY|O_CREAT|O_EXCL, 0600)` writing exactly 12 lowercase
hex characters + LF, matching the J1 contract (§5.1) byte-for-byte. Pin the
helper once — `writeFixturePoolID(t *testing.T, poolDir string) string` in the
M3 test-helper file — and reuse it in WI-3.3a/3.3b. The sole PRODUCTION writer
is WI-5.4; an M3 test that assumes one exists will not pass.

(e) §10.2 handshake matrix (v1/v1, v1/v2 in both directions); `hello-ok.pool12`
carries the value read from the pool's `state/pool.id` and never an argv echo.
**No `pool.id` absent/malformed/valid-but-different rows here (R13)**: this
item deliberately implements neither `E_POOL_MISMATCH` arm and does no J1
validation, so those rows would assert against code that is not here. They live
on WI-3.3a's (e), which owns the startup validation and the hub arm.

(f) Done-when: the matrix is green and the hello deadline (10 s, both sides) is
enforced in the fake-transport harness.

(g) ~200 LOC.

**WI-3.3a — `hub session`: dispatcher, state machine, release-on-EOF.**

(a) The forced-command entry point: a framed dispatcher from wire ops to
`internal/op` under one pinned `Context`.

(b) §4.1 (carriage), §4.4 (dispatch), §5.1 (config direct from `--pool`, never
Discover), §5.3 (actor-free dispatch), §3.4/§6.1 (`register` caches the tick
template; early `tick` → `E_ORDER`), §6.4-hub (release on every exit path).

(c) NEW `internal/cli/hub_session.go`, NEW `internal/cli/hub.go` (the `hub`
subcommand family dispatcher); MODIFY `internal/cli/dispatch.go` —
`commandHandlers` is declared at `dispatch.go:20` and **populated in `init()` at
`dispatch.go:22-46`** (map keys `24-44`; CORRECTION to the round-1 `23-45`
citation): add `"hub"`, and keep it out of `ac` help per §7.3.

(d) - Config is built **directly** from `--pool`
  (`ControlRepo=<pool>`, `LoopDir=<pool>/.agentchute/loop`) and the discovery
  cascade is never called: a stray `AGENTCHUTE_CONTROL_REPO`/`AGENTCHUTE_LOOP_DIR`
  in the hub user's rc-sourced environment would otherwise outrank the pinned
  pool (`config.go:218-225` env arm, `config.go:299-329` loop-dir arms).
- **At startup, before any op and before `hello-ok`**, in this order: if
  `--pool` is invalid (does not resolve to a pool) → **`E_POOL_NOT_FOUND`**
  (`error` frame, close) — this branch is owned here and is pinned
  **before** J1 validation; then validate `state/pool.id` against the J1
  contract (regular, non-symlink, 0600, content exactly `^[0-9a-f]{12}\n$`,
  read through `loop.ReadFileLimit`, `registration.go:34-48`, with a
  64-byte cap) → `E_POOL_ID_INVALID` on failure; then compare it to
  `--pool-id` → **`E_POOL_MISMATCH` (hub arm)** when absent or unequal.
  Those last two are `error` frames followed by close.
- **The `status` dispatch is where both budgets bind (T1a)**: it passes the
  op an emitter that writes each `warn` note as a `note` frame (so the
  lenient-read warnings stream and never ride inline, WI-1.4(d)), then hands
  the response to WI-3.1a's `status-ok` encoder, which appends rows in sort
  order while the encoded line stays ≤ 64 KiB **and** the count stays ≤ 64,
  and sets `truncated:true` when either budget excluded a row. The op returns
  every row with `Truncated:false`; the budgets exist only on this path. The
  dispatcher itself performs no capping — one implementation of the rule,
  in the encoder.
- One `op.Context{ActorID: <pinned --agent>}` built once and reused for every
  dispatch; one `op.NewChannel(cfg, ctx, ChannelOpts{HeartbeatTemplate: nil})`
  — the **nil arm** from WI-1.7, so `Register` derives and caches the template
  and a `tick` before `register` is `E_ORDER`.
- **State machine**: `pre-hello → serving → (channel: leased) → closing`. EVERY
  exit path calls `Channel.ReleaseLease()` — token-checked, so a
  already-reclaimed lane is a safe `ErrFenced` no-op that never deletes the new
  owner's claim (`lease.go:345-366`). Exit paths that must be covered: stdin
  EOF, read-deadline expiry, `SIGTERM`/`SIGHUP`, a framing violation, and a
  panic recovery.

(e) In-process session tests over the fake transport: `E_ORDER`; invalid
`--pool` at session start (`E_POOL_NOT_FOUND`, before J1); `pool.id`
absent / mismatched (`E_POOL_MISMATCH` hub arm, before `hello-ok`);
`E_POOL_ID_INVALID` (the J1 malformed set); **release-on-EOF asserted for every
one of the five exit paths above** (the claim file is gone, or belongs to the
successor).

(f) Done-when: a fake-transport client completes hello → lease-acquire →
register → tick → lease-release against a temp pool with a fixture
`pool.id`, and every exit path releases.

(g) ~400 LOC + ~200 test.

**WI-3.3b — deadlines + every-op end-to-end.**

(a) Enforce §4.6's hub-side timers and drive every §4.4 op through the
dispatcher.

(b) §4.6 (hello 10 s; read 20 s channel / 30 s idle one-shot; write 30 s;
one-shot lifetime 10 min); §4.4 (full vocabulary).

(c) MODIFY `internal/cli/hub_session.go`.

(d) Deadlines are enforced with `os.Stdin.SetReadDeadline` / the write
equivalent; expiry closes the session through the same release path as EOF
(WI-3.3a). The one-shot 10-minute lifetime is a safety net, independent of the
idle deadline.

(e) Every op in §4.4 driven end-to-end over the fake transport against a temp
pool: `send`, `check`, `ack`, `register`, `status`, `gate`, `pending`,
`clean-owed`, `lease-acquire`, `tick`, `lease-release` — each asserting both
the response frame and the resulting pool state. Deadline tests: a silent
channel dies at 20 s; a silent one-shot at 30 s; a one-shot alive at 10 min is
closed.

(f) Done-when: every §4.4 op has an end-to-end test and every §4.6 hub-side
timer has an expiry test.

(g) ~200 LOC + ~150 test.

**WI-3.4 — lease boot-REFERENCE pid-proof (the ONE `internal/loop` change).**

(a) Corroborate the same-host pid-proof with a **per-boot identifier recorded
in the claim**, compared for EQUALITY.

(b) §6.9 in full (as amended, B6 — the wall-clock boot-*time* rule is
**withdrawn**; do not implement it), §4.6's last row, and §10.3's **three**
boot rows (CORRECTION, R10: the rows named `hub-reboot pid reuse`, `clock step
does not steal a live lease`, and ``boot_ref` survives the heartbeat` — round 2
said four). The seven cases this item must cover are (e)'s, which is the
authority; those three rows are where they land in the M6 matrix.

(c) MODIFY `internal/loop/lease.go`:

- add `BootRef string \`json:"boot_ref,omitempty"\`` to `ServeClaim`
  (`lease.go:55-62`);
- populate it in `AcquireServeLease` where the claim is built
  (`lease.go:160-167`);
- change **branch (d) only** (`lease.go:242-244`).

NEW `internal/loop/bootref_linux.go` (`//go:build linux`),
`internal/loop/bootref_darwin.go` (`//go:build darwin`),
`internal/loop/bootref_other.go` (`//go:build !linux && !darwin`).

**Build-tag shape (S6), decided.** `internal/loop` already partitions platforms
totally with tagged pairs and no third file — `filelock_{unix,windows}.go`,
`lease_pid_{unix,windows}.go`, `readfile_{unix,windows}.go`,
`runner_socketdir_{unix,windows}.go`, each `!windows` / `windows`. Boot ref needs
a THREE-way split, so the minimal total partition is `linux` / `darwin` /
`!linux && !darwin` — the third file covers Windows **and** every other GOOS and
returns `""` (= today's unchanged behavior). A separate `bootref_windows.go`
would be byte-for-byte identical to `bootref_other.go`; it is deliberately not
created.

(d) - **Linux**: read `/proc/sys/kernel/random/boot_id` through
  `loop.ReadFileLimit` with a small cap (a per-boot UUID, ≤64 B) and trim
  whitespace.
- **Darwin**: `golang.org/x/sys/unix.Sysctl("kern.bootsessionuuid")` — NOT an
  exec'd `/usr/sbin/sysctl`. The boot ref is read inside `AcquireServeLease`'s
  `withAgentLock` critical section, and forking a process while holding a flock
  is exactly the latency and failure surface this codebase avoids elsewhere.
  `golang.org/x/sys` is **already a direct requirement** (`go.mod:7`, v0.30.0)
  and is currently imported only from `filelock_windows.go`, so adding
  `x/sys/unix` on the unix side changes no `go.mod`/`go.sum` line — which
  `release.yaml:28-30` (`go mod tidy` + `git diff --exit-code go.mod go.sum`)
  proves.
- Deliberately **not** `kern.boottime`: that is the wall-clock-derived value
  §6.9 rules out.
- Any error, absence, or permission denial ⇒ `""`.
- **Reclaim rule, branch (d) only**: refuse (`ErrLeaseHeld`) when
  `existing.Host == host && pidAlive(existing.PID)`, EXCEPT when
  `existing.BootRef != "" && current != "" && existing.BootRef != current`, in
  which case fall through to branch (e)'s reclaim CAS (`lease.go:248`).
  Equality/inequality only — the refs are never ordered, aged, or parsed.
- **Ordering (C8), unchanged**: the freshness refusal (`lease.go:230-232`) runs
  FIRST, before any pid or boot reasoning.
- **Field preservation, verified**: `readClaim` (`lease.go:101-114`) is a plain
  `json.Unmarshal` into the closed `ServeClaim`, and `RenewLease`
  (`lease.go:317-340`) round-trips read → mutate `LastSeen` → marshal →
  `atomicWriteFile`. So `BootRef` survives every heartbeat **only because it is
  a real struct field**; it must never be smuggled as an ad-hoc JSON key.
  Corollary the tests must assert correctly: an unknown extra key **parses**
  (mixed-version tolerance) but is **dropped** on the next renew — assert
  parsing, never preservation.
- **Test seam, required**: the host's current ref must be injectable — a
  package-level `var readBootRef = platformBootRef` in `internal/loop`.
  Without it, none of the rows below can run deterministically on a CI runner.

(e) Rows: stale `LastSeen` + DIFFERING ref + live decoy pid ⇒ **reclaim**;
stale + MATCHING ref ⇒ `ErrLeaseHeld`; stale + ABSENT ref (a pre-upgrade claim)
⇒ `ErrLeaseHeld` (the upgrade path); host ref unreadable ⇒ `ErrLeaseHeld`;
FRESH `LastSeen` + differing ref ⇒ `ErrLeaseHeld` (freshness first);
**clock stepped FORWARD and BACKWARD under a live lease ⇒ NOT stolen**, and the
claim file is byte-unchanged (same `serve_token`); `boot_ref` present and
identical after repeated `RenewLease`.

(f) Done-when: all seven cases green on linux AND darwin; no other lease test
changed; `git diff` on `internal/loop` limited to `lease.go` + the three new
files.

**Deferred, not in this item (and not in M3):** #152 items 5 and 6 — the
collision-retry seam and the failing-sweep seam. WI-3.4(f) already restricts
the entire `internal/loop` delta to `lease.go` plus the three boot-ref
files; widening the one loop-touching item mid-merge is not worth two
declared coverage gaps. They get their own item **after M6**.

(g) ~160 LOC.

**WI-3.5 — conformance L vectors + the HUB-SIDE half of the W vectors.**

(a) Executable-spec coverage for the lease/fencing gap and for the hub half of
the wire lifecycle.

(b) §9.3 (L1–L4; W1–W6 and the timing rule).

**(c) Placement, pinned — this IS the finding (B3/X3).** Verified facts:
`conformance/go.mod` is exactly three lines (`module agentchute.dev/conformance`,
`go 1.21`) with no `require`, no `replace`, no `go.sum`, and there is no
`go.work` anywhere in the repo. It therefore **cannot import `internal/*`**;
root `go test ./...` never enters it; `tools/test.sh` never enters it; only
`ci.yaml:39-40` and `release.yaml:47-48` run it (`cd conformance && go test
./...`). So:

- **Drivers live in the ROOT module**, and **the assertions live in NON-test
  files (R6)** — identifiers declared in a `_test.go` compile only into that
  package's own test binary, so a second package (WI-6.4's sshd driver) cannot
  see them. As pinned in round 2 the sshd driver could not compile, or would
  have to duplicate the assertions — the one thing that item forbids. So:
  - NEW `internal/spectest/vectors.go` — a stdlib-only loader.
  - NEW `internal/spectest/lease.go` and `internal/spectest/wire.go` — the
    **exported, transport-parameterized** assertion helpers. W1 cannot be
    expressed as `AssertW1(t *testing.T, rw io.ReadWriter, v Vector)` —
    it needs a disconnect, a second session, and hub-state inspection,
    none of which `io.ReadWriter` exposes. Pin a **session-factory
    interface** (open, forced disconnect/close, state probe) shared by
    the `net.Pipe` driver and M6's SSH driver. **codex proposes the
    exact interface in its M3 PR**, since it implements both drivers.
    Do not invent the signature in this plan. `testing` is an ordinary
    stdlib import and is legal in a non-test file.
  - NEW `internal/spectest/lease_test.go` / `wire_test.go` — **thin drivers
    only**: they load the vectors and call the exported helpers over
    `net.Pipe`, and hold no assertion logic of their own. WI-6.4 passes the
    ssh transport to the same helpers.
  They run under the existing `go test ./...` — no new CI job, no new step.
- **Vectors are transport-neutral shared JSON DATA**: NEW
  `conformance/vectors/lease.json`, `conformance/vectors/wire.json`, beside the
  existing `core.json` — data, not an import, so no module boundary is crossed
  and BOTH `go.mod` files stay byte-identical. **The loader derives the
  directory ONCE from its own source path (R8)**:
  `_, self, _, _ := runtime.Caller(0)` →
  `filepath.Join(filepath.Dir(self), "..", "..", "conformance", "vectors")`.
  A bare relative `../../conformance/vectors/…` resolves against the *test's
  working directory*, so it works only while every importing package happens
  to sit exactly two levels below the root — a depth coincidence that
  `integration/sshd/` (WI-6.4) satisfies today and any third caller would
  break. `runtime.Caller` is stdlib; `go:embed` cannot reach a parent
  directory, which is why the path is computed rather than embedded.
- **`conformance/` gains one prose block** in `CONFORMANCE.md` naming the two
  files and stating that the reference driver lives in the root module (other
  bindings drive the same JSON through their own harness). **No new Go file
  under `conformance/`** — the module stays three lines and stdlib-only.
- `tools/fact-sweep.sh` is unaffected: its vector count (check 2) greps
  `conformance/vectors/core.json` specifically, not the directory.
- **`tools/test.sh` gains exactly ONE block**, closing the pre-existing gap
  that the canonical ritual never exercised the second module (CI does;
  test.sh does not). This is the **only** change this plan makes to
  `tools/test.sh`. It must use the script's own `$strip_env` and its own
  failure reporting (R7 — the literal bare `cd conformance && go test ./...`
  round 2 mandated bypasses the env strip that §3 rule 1 and E10 make
  mandatory, and swallows the failure). Inserted after the `go test ./...`
  block (`tools/test.sh:18-20`) and before `go build`, in the shipped shape
  (`tools/test.sh:14-24`), in a **subshell** so the following `go build ./...`
  still runs from the repo root:

  ```sh
  say "cd conformance && go test ./..."
  # shellcheck disable=SC2086
  (cd conformance && env $strip_env go test ./...) || { say "FAIL: conformance go test"; exit 1; }
  ```

(e) **S7 — restated, because M3 has no client.** M3's W set is HUB-SIDE ONLY:

- **W1-hub**: a `check` whose emitter returns an error mid-stream leaves the
  already-claimed items in `.claimed/`, and a second `check` on the same pool
  re-lists them with `redelivered` set.
- **W2-hub**: a `send` whose response frame cannot be written still leaves
  **exactly one** inbox file, and the hub never re-executes it.
- **W3-hub**: a reclaim between `lease-acquire` and the send's in-lock fence
  check ⇒ `op.ErrFenced` / `E_FENCED`, nothing linked.
- **W4/W5**: identity and version mismatch close the session at the handshake
  (already fully hub-side).
- **W6-hub**: unreadable claimed residue reports `claimed_held`. Setup:
  make a `.claimed` residue file unreadable hub-side (`chmod 000`). Drive
  `check`. Assert the terminal `error` frame carries `claimed_held: true`.
  W1 does **not** cover this: W1 is disconnect-after-claim, where no
  terminal `error` is written. The setup is the same probe opus-xhigh used
  to prove the M1 latch bug in two worktrees, on both platforms. Client
  latch-arming is M4.

The CLIENT halves — "reported unknown and not replayed", "next check
redelivers **with the banner**", and "arm the latch on `claimed_held`
with no `msg` frames" — cannot run here and move to **M4 (WI-4.10)**.
A green W2 in M3 that claims client behavior is testing nothing; that is the
review's canary for this item.

L1–L4 as §9.3 states them, driven in-process against the seam.

(f) Done-when: L1–L4 plus the **six hub-side W assertions across five
bullets** (W1-hub, W2-hub, W3-hub, the combined W4/W5, and W6-hub) green under
`tools/test.sh`; `git diff` shows both `go.mod` files unchanged.

(g) ~350 LOC.

**WI-3.6 — replace `op.SendTsMessageWithCommit` with a non-global seam.**

(a) An unexported package var is still process-global. A hub session
process holds many sessions, so it is shared mutable state across them.
**Pinned shape:** **no** mutable package-level state on `op.Send`'s hub
path — exported or not. Production `op.Send` takes its delivery
dependency as a parameter through an internal helper; `cmdSend` gets a
dependency-parameterized helper for the two existing CLI tests; the hub
session calls the production path directly.

(b) #150 / #152 item 4; recorded at M1 gate `ac0b6eb` and deferred because
changing it there would have edited tests outside M1's closed exception
table. M3 already touches `op.Send`.

(c) MODIFY `internal/op/send.go`. Test files that currently reassign the
exported var (must be named up front):

- `internal/cli/send_a5_test.go` (set/restore around `:230-238`)
- `internal/cli/send_b3_test.go` (set/restore around `:138-147`)
- `internal/op/send_test.go` (three set/restore sites)

`loop.SendTsMessageWithCommit` call sites in `internal/op/claim_test.go`,
`internal/op/helpers_test.go`, and `internal/loop/floor_test.go` are the
loop function, not this var.

(d) After this item, no mutable package-level send-delivery state remains
on the hub path. The hub session calls the production helper; CLI tests
inject via the cmdSend-parameterized helper only.

(f) Done-when: those three test files retarget the new seam; `tools/test.sh`
green; a check **fails** on any mutable package-level declaration
reachable from `op.Send`'s hub path, not just an exported one. **codex
proposes the exact check in its M3 PR**, the same way it owns the W
harness interface.

(g) (no new LOC estimate — existing M3 envelope.)

### M4 — client transport + remote discovery

**WI-4.1 — `config.json` format + read/write helper.** *(S5 — nothing else in
M4 can be tested without this, and there is no producer until M5.)*

(a) Pin the per-hub config file's struct, its on-disk contract, and the helper
every other M4 item reads it through.

(b) §7.4 (the layout block), §4.3/D3 (`pool`/`pool12` are RECORDED from
`hello-ok`, never derived from URL text).

(c) NEW `internal/hubclient/config.go`.

**Layering note the implementer would otherwise have to stop and ask about:**
`internal/loop` cannot import `internal/hubclient` (the direction is
`loop ← op ← hubwire ← hubclient ← cli`, §7.4), but discovery needs to know
whether a join exists in order to raise `E_NOT_JOINED`. Split accordingly: the
**path builder and the existence check** live in `internal/loop/remote.go`
(WI-4.2) — `loop.HubDir(hubID)`, `loop.HubConfigPath(hubID)`, and a stat — and
`internal/loop` **never parses** the file. The struct and the read/write helper
live here.

(d) Fixed shape, verbatim from §7.4:

```go
type HubConfig struct {
    URL      string            `json:"url"`
    JoinedAs []string          `json:"joined_as"`
    Names    map[string]string `json:"names"`
    Pool     string            `json:"pool"`
    Pool12   string            `json:"pool12"`
}
```

- Path `~/.agentchute/hub/<hub-id>/config.json`, file 0600, dir 0700.
- `ReadHubConfig(hubID string) (*HubConfig, error)` returns a **named
  not-found error** — the `E_NOT_JOINED` trigger (§7.4, C3) — distinguishable
  by the caller from a parse or I/O error. The read is bounded through
  `loop.ReadFileLimit` (`registration.go:34-48`) with a 64 KiB cap.
- `WriteHubConfig(hubID string, c *HubConfig) error` = temp file in the same
  directory + fsync + `rename` (the same discipline WI-5.3c's migration commit
  uses).
- **Unknown JSON fields are ignored on read and DROPPED on write**, stated
  explicitly because `ServeClaim` (`lease.go:101-114`) and `GuardLatch`
  (`guard.go:31-35`) behave the same way and an implementer must not assume
  round-tripping.

(e) Round-trip test; not-found vs parse-error distinction; 0600/0700
permissions asserted; and a fixture helper
`plantHubConfig(t *testing.T, hubID string, c HubConfig)` reused by every
later M4 item.

(f) Done-when: the helper round-trips and the two error classes are
distinguishable. **Every other M4 row runs against a HAND-PLANTED
`config.json`** — `hub join` does not exist until WI-5.3a, so an M4 test that
expects join to have run cannot pass.

(g) ~120 LOC.

**WI-4.2 — `ssh://` discovery arm + shadow dir (§6.8 contract).**

(a) Remote config resolution, purely local and offline-safe.

(b) §6.8 (points 1–4), §7.4 (hub-dir layout, hub-id hash, pointer lifecycle
including probe-before-pointer and the `E_NOT_JOINED` join-state check — C3),
§4.4.2 client-side codes.

(c) MODIFY `internal/loop/config.go`. **CORRECTION: `discoverControlRepo`
(`config.go:208-259`) has FOUR arms, not three** — flag (`209-216`), env
(`218-225`), pointer file (`227-246`), cwd walk-up (`248-255`), then
`ErrNoControlRepo` (`257-258`). The `ssh://` branch goes in the flag, env, and
pointer arms, **before** `validateExplicitControlRepo` (`264-273`) and before
`absExistingDir` (`359-375`).

MODIFY `internal/loop/pointer.go` — **S3, with corrected anchors.** The
round-1 citation (`pointer.go:93,105-118`) was wrong; the real functions are
`DiscoverPointer` (`pointer.go:58-103`) and `ResolvePointerTarget`
(`pointer.go:113-122`), and neither file contains the directory check itself —
it is `absExistingDir` in `config.go:359-375` (`os.Lstat`, symlink reject,
`IsDir` reject). What an `ssh://` pointer does **today**, traced:
`ParsePointerFile` (`pointer.go:31-51`) accepts the line verbatim →
`ResolvePointerTarget:118` finds `filepath.IsAbs("ssh://user@host/path")` is
**false** → `:119` joins it onto the pointer's directory and `filepath.Join`'s
`Clean` collapses the `//` into `<pointerDir>/ssh:/user@host/path` →
`absExistingDir` fails ENOENT → and `config.go:229-234` treats a pointer error
as **hard**, so the whole cascade dies before any ssh arm could run, naming a
mangled local path. Fix: `ResolvePointerTarget` returns the raw line unchanged
when it carries the `ssh://` prefix; `DiscoverPointer` propagates it; the caller
branches. `ParsePointerFile` is unchanged (its grammar is already "one
non-comment line").

NEW `internal/loop/remote.go`: `RemoteConfig`; the URL grammar (`user`/`host`
match `[A-Za-z0-9._-]+` and must not start with `-`; port 1–65535; path
absolute); the canonical form (lowercase host, port elided at 22, no trailing
slash); `hub-id = hex(sha256(canonical))[:12]`; `HubDir` / `HubConfigPath` /
shadow-loop-dir builders; and the `E_NOT_JOINED` existence check (a **stat
only** — see WI-4.1).

(d) `Config` (`config.go:34-53`) gains `Remote *RemoteConfig`. The shadow
LoopDir is `~/.agentchute/hub/<hub-id>/.agentchute/loop/` — the literal
`.agentchute` segment keeps the dotdir invariant `vendorFromLoopDir`
(`config.go:348-357`) relies on. An explicit `--loop-dir` /
`AGENTCHUTE_LOOP_DIR` combined with an `ssh://` control repo is a **hard
error** (rule 4).

(e) Unit: grammar accept/reject table; pointer-vs-env precedence; shadow path
shape; `E_NOT_JOINED` (no `config.json`) vs joined-with-the-same-path.

(f) Done-when: `guard --pre-tool-use` in a joined checkout (hand-planted
`config.json`) resolves the SHADOW latch with **no network call** — asserted by
pointing the URL at an unroutable host and requiring the call to return
promptly, not by mocking the transport; and an `ssh://` pointer no longer
produces the mangled-local-path error.

(g) ~280 LOC.

**WI-4.3 — launcher forwarding: `dispatch.go` + `shims.go`.** *(B4 — ships in
the SAME MERGE as WI-4.2; §6.8 rule 5: the arm and its launchers must ship
together, or every `ac serve` in a joined checkout silently de-remotes itself
with no error anywhere.)*

(a) Make both launcher paths forward the **locator**, not the derived local
paths.

(b) §6.8 point 5 in full; §7.4 (canonical URL form).

(c) MODIFY `internal/cli/dispatch.go`: `buildDispatchRunArgs`
(`dispatch.go:245-258`, called at `dispatch.go:241`) today always emits
`--control-repo <cfg.ControlRepo> --loop-dir <cfg.LoopDir>` (`:251-252`) from
the RESOLVED config. In remote mode it must emit
`--control-repo <cfg.Remote canonical ssh:// URL>` and **omit `--loop-dir`
entirely**. A caller-supplied `--loop-dir` peeled off the dispatcher's global
flags (`dispatch.go:220-223`) against a remote config is the rule-4 refusal,
raised **in the dispatcher, before the exec**.

MODIFY `internal/cli/shims.go`: the identical pair at `shims.go:304-305`, inside
`cmdShimsExec` (`shims.go:259-312`), whose own `loop.Discover` call
(`shims.go:285-289`) takes only `Cwd` + env.

**Executability note the implementer needs.** `cmdShimsExec` is the **legacy**
`ac-*` shim path: `removeLegacyWrapperShims` (`shims.go:237-257`) deletes those
shims during setup, and only the single `ac` dispatcher
(`renderDispatcherScript` `169-176`, `installDispatcher` `208-231`) is installed
today. So the §10.3 "launcher preserves remoteness" row must **construct** a
legacy `ac-*` shim (or call `cmdShimsExec` directly) to exercise this half — a
row that only runs `ac serve` never touches `shims.go` and passes green while
`shims.go` is still broken.

(e) Argv golden tests for **both** launchers in **both** modes: remote ⇒
`--control-repo ssh://…` present and `--loop-dir` **absent**; local ⇒ today's
pair, byte-identical. Plus the rule-4 refusal test.

(f) Done-when: the golden argv tests pass **and** a discovery round-trip on the
emitted argv yields `Remote != nil` with `LoopDir` = the shadow. The assertion
must be on `Remote != nil` — never on "the command succeeded", because today's
code succeeds while running LOCAL against a mail-free shadow.

**Cross-merge interlock (B5).** The migration attribution predicate in WI-5.3c
has a `--control-repo`-URL arm that is correct **only because of this item**.
They cannot land in one merge — the predicate has no caller until `hub join`
exists (M5) — so the interlock is enforced by review instead, and this is
binding: WI-5.3c's gate ask must re-verify this item's argv goldens at the M5
head SHA, and the M6 rows "launcher preserves remoteness" **and** "migration
attribution, normal lane" must both be green before v1.6.0. A reviewer of
either item must check the other.

(g) ~120 LOC.

**WI-4.4 — ssh invocation builder + transport sessions (+ the ControlPath
contingency).**

(a) Spawn and own `ssh` per §4.2 exactly; frame client sessions over it.

(b) §4.2 (both argv blocks verbatim, incl. `-T`, `BatchMode`, per-hub
`known_hosts`, `IdentitiesOnly` + the symlinked `-i` path,
`ClearAllForwardings`, the mux split: `ControlMaster=no` channel /
`auto`+60 s one-shot), §4.6 deadlines, §4.4.2 client-side code mapping.

(c) NEW `internal/hubclient/ssh.go`, `internal/hubclient/session.go`.

(d) Argv exactly as §4.2. Code mapping: ssh remote exit 127 → `E_HUB_NO_BINARY`
**immediately** (never burn the 10 s hello timeout); host-key stderr →
`E_HOSTKEY_CHANGED`; auth failure → `E_UNAUTHORIZED`; `ssh` binary absent →
`E_NO_SSH`.

**ControlPath contingency (X5) — DESIGN §4.2 is silent; this plan decides, and
the decision has a trigger that can actually be exercised.**

- The one-shot ControlPath is `<muxDir>/%C`. The builder refuses to emit a path
  whose worst-case expansion could exceed the unix `sun_path` limit, using the
  **shipped** threshold verbatim:
  `len(muxDir) + 1 + controlPathTokenWidth >= 100`, where `100` is the literal
  `internal/loop/config.go:170` already uses for runner sockets (**one number in
  the codebase, not two** — note it is a bare literal there, with no named
  constant), and `controlPathTokenWidth = 64` is a deliberately **conservative
  upper bound** for `%C`. Over-triggering the fallback costs one extra
  authentication; under-triggering costs a broken mux and an opaque ssh error.
- **Preferred** `muxDir` = `<hubdir>/mux`.
- **First fallback**: `<tempRoot>/agentchute-hub-<uid>/<hub-id>`, trying
  `os.TempDir()` then `/tmp`, taking the first that passes the same budget
  (macOS's `$TMPDIR` is itself long, so `/tmp` is a real second candidate, not
  decoration). Created and verified through the **shipped** owned-0700
  discipline — `EnsureRunnerSocketDir` (`config.go:200-206`) →
  `ensureOwnedRunnerSocketDir` (`internal/loop/runner_socketdir_unix.go:24-49`:
  `MkdirAll` 0700, symlink reject, uid-ownership reject, `Chmod` 0700) —
  generalized to a caller-supplied path rather than copied.
- **Second fallback**: if neither passes, **disable multiplexing for this hub**
  (`-o ControlMaster=no -o ControlPath=none` on one-shots) and emit exactly one
  `warn` note naming both attempted paths. Correctness is unaffected; only the
  per-op authentication cost changes. Never a hard refusal — an unusually deep
  `$HOME` must not make the CLI unusable.
- The **channel** is unaffected: it is already `ControlMaster=no
  -o ControlPath=none` (§4.2).

(e) Argv golden tests for both shapes; the exit/stderr → code mapping table;
and a **deterministic deep-home builder test** that injects a synthetic hub dir
of a pinned length (no real deep `$HOME`, no OS dependence): short ⇒ preferred
path; long ⇒ first fallback; long **and** an over-budget/unwritable temp root ⇒
mux disabled plus exactly one warn note.

(f) Done-when: the golden argv matches §4.2 byte-for-byte in the preferred
case, and all three ControlPath branches are asserted.

(g) ~330 LOC.

**WI-4.5 — one-shot routing + remote turn-end order.** *(P2, part a.)*

(a) Route the CLI verbs over one-shot sessions with the §4.5.1 semantics, and
implement the remote `turn-end` ordering. This item also owns the
**rendered `warn`/`info` golden** (DESIGN §10.2 moved here): production
remote rendering first lands here, so M3 only proved frame level and
production order.

(b) §4.5.1 (read-after-hello), §6.6 (remote turn-end order and the E1 arming
point), §3.5 (the `Announce` bullet — the view, the three facts it renders, and
the announce-failure-rides-in-`Warnings` rule).

(c) MODIFY `internal/cli/send.go`, `check.go`, `ack.go`, `status.go`,
`gate.go`, `pending.go`, `boot.go`, `clean.go`, `turn_end.go`,
**`self_check.go`**, **`register.go`**.

**S1 — `self_check.go` is in the file list.** `selfRepairRegistration`
(`self_check.go:117-133`) is step 0 of `turn-end` (`turn_end.go:120`) and the
whole of `self-check` (`self_check.go:71`); in remote mode it becomes a wire
`register`, and its "non-empty id even when the write failed" contract
(`self_check.go:129-131`) must survive, because `turn_end.go:121-123` aborts
only on an empty id.

**The `status` renderer switch lands HERE, not in M1.** M1 left `printStatus`
byte-unchanged and re-derived the STATUS label and the inbox depth locally
(WI-1.4(d)), which a remote client cannot do — it can neither stat the hub's
inbox dirs nor read its serve claims. In remote mode `cmdStatus` renders from
the response instead: `StatusAgent.Status`, `.InboxDepth`, and
`StatusResp.Now` for the AGE column (never the client's clock). The
lenient-read warnings arrive as `warn` **`note` frames** and are printed by the
same `warning: %s` stderr loop as local mode (WI-1.4(d)), which also keeps
them ahead of the table; `Truncated:true` — set by the hub's `status-ok`
encoder, never by the op (WI-3.1a) — renders as one trailing line that names
**the wire limit as well as the row cap**, so a truncated remote listing is
never silent AND never misattributed: the budget that binds first is often the
64 KiB line, not 64 rows (WI-3.1a(d) — one row with a large `host` can
truncate a three-agent pool), and a notice saying only "64 rows" would tell an
operator with three agents something visibly false. The notice states the two
limits and that rows were withheld; it does **not** state a total, because
`status-ok` carries only the kept rows and the flag — a `total` field is a
shape change no finding asked for, and inventing "N of M" from data the frame
does not carry is worse than omitting it.

**The notice's exact text is PINNED** (DESIGN §4.4.3 states the identical
literal — two compliant implementations must not print two different lines,
and the byte-exact assertions in (e) cannot be written against a paraphrase).
One unindented line on **stdout**, emitted only when `truncated` is true, as
the LAST line of `status` output (after the table and after any
`PROTOCOL WARNINGS:` block), preceded by one blank line:

`note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.`

Modelled on the shipped trailing notice at
`internal/cli/check.go:262 @ 1244ae4` — lowercase `note: ` prefix, no indent,
one sentence, a semicolon before the consequence. It names both budgets and
states the PREFIX rule, so the operator knows the missing rows are the TAIL
of the agent-id sort and not an arbitrary subset. Local mode keeps today's
path unchanged, so `status_test.go` stays untouched here too.

**The status HEADER is a decision this item owns, and it is: print the HUB's
pointer.** The three header lines come from local config, never from row data
(`internal/cli/status.go:98-100 @ 1244ae4`), so the settled row shape decides
nothing about them and a renderer switch that only retargets the TABLE leaves
a remote lane printing local paths above hub rows. Per DESIGN §3.6's header
sub-bullet, on a remote lane (`cfg.Remote != nil`):

- `control_repo:` prints the canonical `ssh://` URL from `cfg.Remote` — never
  `cfg.ControlRepo`, which under §6.8 rule 2 is only the nearest local
  ancestor holding `AGENTCHUTE.md` and exists for local concerns like hook
  refresh (`serve.go:159`). The rows below came from the hub; a header naming
  a local directory that holds none of them is §6.8 rule 5's failure one
  command later. Keep `formatOriginSuffix(cfg.ControlRepoOrigin)`
  (`status.go:210-215`) — the URL really is what the flag/env/pointer arm
  resolved, so `(via env)` stays true.
- `loop_dir:` keeps printing the **local shadow**, plus one parenthetical
  line under it marking it as the shadow. **That line's exact text is PINNED**
  (DESIGN §3.6 states the identical literal) — printed immediately under the
  `loop_dir:` line and ahead of `vendor:`, with **two leading spaces** and no
  trailing space, only when `cfg.Remote != nil`:

  `  (local shadow: this process's own loop dir, not the hub's)`

  It is modelled on the marker line already in this same header block,
  `  (shadowed pointer: %s)` (`status.go:101-103 @ 1244ae4`), for its
  two-space indent and parenthesised `label: value` form, and on
  `  (pull-only: senders deliver to your inbox; you poll it yourself)`
  (`internal/cli/boot.go:204 @ 1244ae4`) for a label followed by prose rather
  than by a path. It genuinely is this process's
  loop dir — guard latch, `runner.json`, the send spool all live
  there (§6.8 rule 3, §4.5.3) — so replacing it loses a real diagnostic; and
  the hub's loop dir is on another filesystem and rides on no frame
  (`hello-ok` carries `pool`, a pool path, not a loop dir), so printing it
  would mean a new wire field for a path the operator cannot open. What
  misleads is the shadow printed UNMARKED, and the marker is the whole fix.
- `vendor:` is unchanged, and needs no branch: `cfg.Vendor` is the loop dotdir
  namespace, vendor namespacing is gone (`fixedNamespace`,
  `internal/loop/config.go:19-25,348-357 @ 1244ae4`), and §6.8 rule 3 gives
  the shadow the same `.agentchute/loop` shape deliberately — so both modes
  print the same string. Do not "fix" this line to the actor's registered
  vendor: `StatusAgent` carries no `Vendor` (T1b cut it), and re-adding a
  field to feed a header line reopens a settled shape.
- `ShadowedPointers` is unchanged — a diagnostic about local pointer files
  that lost discovery, equally local in both modes.

The branch keys on `cfg.Remote != nil` — already in hand wherever the header
is printed, since `cfg` is the renderer's own argument (WI-1.4(d)) — so the
header needs no new parameter and no new wire field, and
`status_test.go:35,77,126,181` stay local-mode calls with unchanged output.

**`register.go` is in the file list because the enrollment ANNOUNCE fan-out
must move to the hub.** `cmdRegister` is the only command that fans out on
enrollment, and it does so by calling `loop.AnnounceEnrollment(cfg, reg)`
directly against the discovered config and rendering the result
(`register.go:280-294 @ 1244ae4`). On a remote lane that `cfg` is the
**mail-free shadow** (§6.8 rules 2–3), whose agents dir holds no peer
registrations at all — so the call would walk an empty directory, print
`no peers to announce to`, and exit 0: announced to nobody, silently, **no error
anywhere**. Same failure class as §6.8 rule 5's de-remoted launcher, one command
later. The request/response fields that fix it were pinned in M1 (WI-1.6:
`RegisterReq.Announce`, `RegisterResp.Announce *AnnounceView`) and deliberately
left with **no caller**; this item is where the caller lands. The rule, both
arms:

- **Local (`cfg.Remote == nil`) — unchanged.** The direct `loop.AnnounceEnrollment`
  call may stay exactly where it is and render exactly as today; M1's adapter
  already passes `Announce:false` (WI-1.6), so `register_test.go` stays
  untouched here as it did there.
- **Remote (`cfg.Remote != nil`)** — `cmdRegister` MUST NOT call
  `loop.AnnounceEnrollment`. It sets `RegisterReq.Announce` from the
  `--announce` flag (`register.go:210`) so the **hub** performs the fan-out over
  its own pool, and renders from `RegisterResp.Announce`, reproducing today's
  output line-for-line and stream-for-stream:
  - each `Announce.Warnings` entry ⇒ `warning: <w>` on **stderr**
    (`register.go:285-287`);
  - `Announce.Total == 0` ⇒ `  announce:      no peers to announce to` on
    **stdout** (`:288-289`);
  - otherwise ⇒ `  announce:      sent to %d of %d peer(s)` from `Sent` **and**
    `Total` (`:290-292`).

  A hub-side announce that failed outright arrives as `Announce == nil` plus an
  `announce failed: <err>` entry in `RegisterResp.Warnings` (§3.5), printed by
  the existing warnings loop (`:276-278`) — the same bytes in the same stderr
  position as today's `warning: announce failed: %v` (`:282-284`) — and
  `register` still exits 0. `--announce` unset ⇒ `Announce:false` on the wire and
  nothing rendered, as today.

**Boot's sweep crosses the wire here (T3).** `RegisterReq.Sweep` was pinned in
M1 with no `true` caller (WI-1.6(d)); this is where it gets one, and the two
arms must stay exclusive:

- **Local (`cfg.Remote == nil`) — unchanged.** `cmdBoot` sends `Sweep:false`
  and keeps its own `loop.SweepStaleRegistrations` call (`boot.go:99-101`),
  appending `sweep stale registrations: <err>` to `result.Warnings` on
  failure exactly as today. `boot_test.go` is untouched.
- **Remote (`cfg.Remote != nil`)** — `cmdBoot` sets `Sweep:true` on the wire
  register **and skips `boot.go:99-101` entirely**. Skipping is not an
  optimization: that call would run against the discovered cfg, which on a
  remote lane is the mail-free shadow (§6.8 rules 2–3) — a sweep of a pool
  with no peer rows, i.e. hygiene performed on the wrong filesystem while the
  hub's pool goes unswept. The hub runs it instead, after the registration
  write, and a failure comes home in `RegisterResp.Warnings`, which
  `cmdBoot` already renders through `bootStatus.Warnings` (`boot.go:143`) —
  same string, same field, same output position.
- **No other command sweeps**, verified at `1244ae4`: the only other
  production caller is the runner tick (`serve.go:640`), which is `op.Tick`'s
  (WI-1.7) and unaffected. `register`, `self-check` and `turn-end`'s step-0
  repair all send `Sweep:false` in both modes, as does the channel's startup
  `register` (WI-5.1) — whose lane sweeps on its first tick anyway. **Exactly
  one sweep per boot, in either mode; zero for every other verb.**

**Remote result paths render the RESOLVED vendor (T4).** S2 has the four
remote call sites send `Vendor:nil` so the HUB resolves it — but all three
renderers print the **pre-wire** `opts.Vendor`, which on exactly those bare
invocations is the empty string: `cmdRegister` (`register.go:269`), `cmdBoot`
(`boot.go:136`, feeding `bootStatus.Vendor` and therefore all four boot
emitters) and `cmdSelfCheck` (`self_check.go:78`). Sending nil and printing
the request field means a successful bare `register`/`boot`/`self-check` on a
remote lane renders `vendor: ` — an empty vendor on a success path. **Fix: all
three read `result.Reg.Vendor` (the resolved value the hub wrote) instead.**
This is one code path, not a mode branch: locally the two are provably the
same value, because `publishRegistrationOnce` sets `Vendor: opts.Vendor`
unconditionally (`register.go:133`) and the merge arm touches only
`WorkingRepos` and `Body` (`register.go:140-149`), so no local test output
changes. `RegisterResp.Reg` already carries `Vendor` (WI-1.6(d)'s
`RegistrationView`) — nothing new goes on the wire.

`register` is also **S2's fourth `resolveAgentVendor` call site**
(`register.go:259`); its `Vendor:nil` branch rides on the same remote arm added
here (`boot.go:86` and `self_check.go:126` are already this item's;
`serve.go:152` is WI-5.1's).

**No other command announces**, verified at `1244ae4`: `loop.AnnounceEnrollment`
has exactly one caller in the whole tree (`register.go:281`), and `boot`,
`serve`'s `registerRunner` and `self-check` all reach `performRegister` with no
announce input of any kind (`cmdBoot`'s flag set declares none,
`boot.go:26-40`). So this branch is `register`'s alone — no other file in this
item's list needs it. (§2 rule 3: WI-4.8 also touches `register.go` for the
`resolveAgentID` signature, so the two run in numeric order, never in parallel.)

(d) Remote `turn-end` order (§6.6), stated against today's local code
(`turn_end.go:120` / `144-158` / `162-166` / `169`): **(0)** best-effort wire
`register` carrying the inherited `AGENTCHUTE_SERVE_TOKEN` — a failure is warned
with `turn_end.go:126`'s text and does **not** abort; **(1)** wire `ack`;
**(2)** LOCAL `ClearGuardLatch`; **(3)** wire `gate`. The step-1 latch-ownership
test (`turn_end.go:144-150`) is local and unchanged — the latch never crosses
the wire (§6.6).

**E1**: `check`'s event emitter arms the local latch on the FIRST
`MessageEvent`, **before rendering it**, in both the normal and `--no-archive`
paths — mirroring `setLatch` at `check.go:185/243/256`, never after the wire op
returns.

(e) In-process harness (the WI-3.3a fake-transport session driven as a
subprocess over pipes): every verb round-trips; the turn-end order asserted by
call trace; `turn-end` with the hub unreachable leaves the latch armed and the
claimed mail in the hub's `.claimed/`.

Plus the row **remote `status` renders the hub, header included**: against a
hub pool of three agents, with the client's clock skewed and its shadow
holding no rows at all, run `agentchute status` remotely and assert (1) the
STATUS and INBOX columns and the AGE column come from the response
(`StatusAgent.Status`, `.InboxDepth`, `StatusResp.Now`) and are byte-identical
to the same pool driven locally with the clock pinned — a renderer still
re-deriving them locally prints `-`/`0`/a skewed age and fails; (2) the header
prints `control_repo:` = the canonical `ssh://` URL with its `(via …)` suffix,
`loop_dir:` = the shadow WITH its marker line, asserted **byte-exact** against
the marker literal pinned in (d) (two leading spaces included), `vendor:` =
the same string the local run printed; a renderer that leaves the header on
`cfg.ControlRepo` fails this row by printing a local path above hub rows;
(3) the `warning: …` lines land on stderr AHEAD of the table.

Then plant one valid registration whose `host` alone exceeds 64 KiB in the
same three-agent pool and re-run. **The fixture pins WHERE that row sorts:
the oversized `host` belongs to the lexicographically LAST of the three agent
ids** — say `alpha` / `bravo` / `zulu`, with the huge host on `zulu`. The
producer emits a PREFIX of the sort order (WI-3.1a(d)), so an oversized row
sorting first or in the middle drops every row behind it and "the other two
agents still render" would be false; the first-row case is its own
producer-level row and asserts `agents:[]` with both later rows dropped
(WI-3.1a(e)), so this integration row must not contradict it. With the row
sorting last: the other two agents still render, and the trailing truncation
line is asserted **byte-exact** against the notice literal pinned in (d) — it
names the WIRE limit as well as the row cap, and a notice naming only
"64 rows" is false on a three-agent pool and fails this row.

Plus the row **`register --announce` fans out HUB-side** — the M4 half of
§10.3's "register field semantics (C2/D1)" row, its announce clause, landing
here because the branch ships here; WI-5.7 re-runs that row whole. Against a hand-planted `config.json` (WI-4.1) and a hub pool
seeded with three peers, one of them undeliverable — the undeliverable one must
be a **readable** registration whose inbox write fails, since an unreadable
registration warns *without* incrementing `Total` (`message.go:75-83`) and would
quietly change the expected counts — run
`agentchute register --as <id> --announce` and assert: the wire request carries
`announce:true`; the **hub's** two reachable inboxes each gain one message; the
**shadow's** loop tree is byte-unchanged; stdout carries
`  announce:      sent to 2 of 3 peer(s)` and stderr the one per-peer warning,
byte-identical to the same pool driven by a local `register --announce`.
**This row fails if the local `loop.AnnounceEnrollment` call is left in place on
a remote lane**: the shadow has no peers, so the local call renders
`  announce:      no peers to announce to`, writes nothing hub-side, and still
exits 0 — which is why the assertion is on hub-side inbox files AND the rendered
counts, never on "the command succeeded". Two arms beside it: a hub pool with
**no** peers ⇒ `  announce:      no peers to announce to`; a hub-side announce
error ⇒ `Announce` nil, `warning: announce failed: <err>` on stderr, exit 0.

Plus the row **remote `boot` sweeps HUB-side, exactly once (T3)**: with a
stale, lease-dead peer row planted in the HUB pool and an identically named
row planted in the SHADOW, run `agentchute boot --as <id>` remotely and
assert the wire request carries `sweep:true`, the hub's stale row is gone, the
**shadow's row is untouched** (proving the local call was skipped, not merely
duplicated), and a hub-side sweep failure surfaces as
`sweep stale registrations: <err>` in boot's rendered warnings. Beside it:
remote `register`, `self-check` and `turn-end` each send `sweep:false` and
leave the hub's stale row in place.

Plus the row **the resolved vendor is rendered, not the request's (T4)** — the
M4 half of §10.3's "remote vendor resolution skipped client-side (S2)" row.
In a joined checkout whose hub pool holds a custom-id row with vendor
`anthropic` that no canonical-prefix rule can name, run each command **bare**
(no `--vendor`) and assert the OUTPUT, not just the wire and the exit code:
`register` ⇒ stdout carries `  vendor:        anthropic` (text only — the
command has no `--json`); `boot` ⇒ `emitBootText`'s
`Registered|Refreshed <id> (anthropic) — …` **and** `boot --json`'s
`"vendor":"anthropic"`; `self-check` ⇒ its text line **and** `self-check
--json`'s `"vendor":"anthropic"`. Each of the three fails today with an empty
vendor rendered on a success path, which is the defect. Assert the same three
outputs from a LOCAL run of the same pool, byte-identical.

(f) Done-when: all remote verbs work against the in-process harness and the
four turn-end steps execute in the pinned order with step 0 non-fatal; a
remote `register --announce` puts the announcements in the **HUB's** inboxes
(never the shadow) with its three output lines byte-identical to local mode;
remote `boot` sweeps the hub pool once and the shadow never; no
remote `register`/`boot`/`self-check` can render an empty vendor on success;
and a remote `status` renders the hub in BOTH halves — table from the response,
header on the `ssh://` pointer with the shadow marked — with its shadow marker
line and its truncation notice each byte-identical to the literals pinned in
(d).

(g) ~300 LOC.

**WI-4.6 — send ambiguity + spool.** *(P2, part b.)*

(a) The §4.5.3 fail-closed, never-replay semantics.

(b) §4.5.3 in full; §7.5's `E_SEND_UNKNOWN` text.

(c) MODIFY `internal/cli/send.go`, `internal/hubclient/session.go`.

(d) The **ambiguity window opens at the first byte handed to the ssh child's
stdin** and closes when `send-ok` or an `error` frame for that `id` is read.
This needs a deliberate test seam in the transport — a package-level
`var afterSendFirstByte func()` in `internal/hubclient`, mirroring the shipped
`afterSendPreflightHook` idiom (`internal/cli/send.go:19`) — and must **not** be
approximated as "after the response deadline". The remote spool is
`~/.agentchute/hub/<hub-id>/spool/`, deliberately **outside** the shadow loop
tree, because `rejectLoopStateBodyFile` (`send.go:600-633`) would otherwise make
the printed retry command refuse its own file. The retry command is
`--body-file <spool>` — never local mode's `< <spool>` redirection, which the
guard denies while latched.

(e) Before-window failures spool and exit 1 with a safe-retry text; in-window
failures spool, exit 1 with `E_SEND_UNKNOWN`, and never retry automatically; a
`send-ok` with a non-empty `durability_note` renders `send.go:269`'s
"delivered … Do NOT resend".

(f) Done-when: the `E_SEND_UNKNOWN` test proves **at most one inbox file**
hub-side, and the printed retry command is executed in the test and **accepted**
by `--body-file` (proving the spool path is not self-refusing).

(g) ~180 LOC.

**WI-4.7 — negative cache + hook-mode `boot`.** *(P2, part c.)*

(a) The 30 s negative cache and the hook-context degradation rule.

(b) §7.5 (the hook-context degradation paragraph and the cache), D6.

(c) NEW `internal/hubclient/cache.go`; MODIFY `internal/cli/boot.go` (the
hook-mode arms, `boot.go:15-40`), `pending.go`, `self_check.go`.

(d) Any `E_CONNECT` writes `~/.agentchute/hub/<hub-id>/hub-down.json`
`{"last_econnect":"<T>"}`; until `T+30s` the **participants** — `pending`,
`self-check`, and **hook-mode `boot`** (`--context-only` /
`--codex-hook SessionStart`, D6) — skip the dial entirely and take the
warn-to-stderr-and-exit-0 path. Any successful connection deletes the file.
**Bypassers**: `turn-end` (it is the commit path — correctness over latency) and
every human-typed command (`send`, `check`, `doctor`, interactive `boot`).

(e) Row: with the hub down, hook-mode `boot` then `pending` within 30 s ⇒
**exactly one** dial (one 5 s stall), both exit 0; a successful connection
clears the file.

(f) Done-when: that row is green and the bypass list is asserted (a `turn-end`
during the cache window still dials).

(g) ~130 LOC.

**WI-4.8 — names-map runtime resolver.**

(a) Local-name → joined-id resolution at the single identity choke point, plus
the direct-serve fallback candidate.

(b) §7.2 (the naming block and the runtime-resolution bullet in full, rounds
9/9b/10).

(c) MODIFY `internal/cli/identity.go`, **`internal/cli/identity_test.go`**.
**CORRECTION**: `resolveAgentID` is
`identity.go:13-25` and `resolveAgentIDRaw` is `identity.go:27-39` — the
round-1 citation `13-38` conflated them. The map lookup goes **inside**
`resolveAgentID` and needs the discovered config, so its signature gains a
parameter.

**`identity_test.go` calls `resolveAgentID` DIRECTLY, five times (T2)** —
`identity_test.go:19`, `:25`, `:33`, `:44`, `:55` — so the signature change
leaves those five sites uncompilable, and the widened move-set rule (M1
preamble, population 2) requires them named rather than discovered at build
time. They are the ONLY direct test calls in the tree: the same grep over
`internal/cli/*_test.go` finds no other caller, and `:20`/`:26` are format
strings inside the assertions, not calls. Each update is the same one-token
edit — pass the `cfg` the test already has, or `nil` — so the honest arm is
naming the file, not inventing a compatibility shim:
- **`nil` cfg must be legal and must mean "no map"**, which these five sites
  rely on. `TestResolveAgentIDRejectsTraversal` (`:19,:25`) and
  `TestResolveAgentIDExplicitOnly` (`:33,:44,:55`) construct no pool at all;
  they assert flag-beats-env, env fallback, traversal rejection, and the
  exact `missingAgentIdentityHint` text. Every one of those is map-free
  behavior and its assertion must not change by a byte — a nil (or
  pool-less) cfg resolves the candidate verbatim, exactly as today.
- `TestCommandsWithoutIdentityFailWithHint` (`:65-92`) drives whole commands
  and needs nothing: it never names `resolveAgentID`.
- The alternative — keeping the old signature behind a cfg-aware helper —
  is rejected: it leaves two spellings of the one identity choke point this
  item exists to make single, which is how a later caller silently gets the
  unmapped one.

**All 12 `resolveAgentID` call sites are in scope (R9), enumerated** — a
signature change updates every caller, so the file list is the union and §2
rule 3 reads it that way: nothing else may run in parallel with this item on
these files. Each site already Discovers **before** resolving except the three
called out below, so each just passes `cfg`:
`check.go:87`, `ack.go:91`, `boot.go:78`, `pending.go:63`, `send.go:127`,
`gate.go:80`, `clean.go:80`, `register.go:254`, `self_check.go:118`,
`serve.go:145`, plus the two reorder cases `status.go:34` and
`identity_cmd.go:24`. (`guard.go:175-189` is a **13th, inline** resolution that
never calls `resolveAgentID`; it is handled separately below.)

**S8 — the three call sites that resolve identity BEFORE `loop.Discover`**, all
verified, with one correction:

- `internal/cli/status.go:34` calls `resolveAgentID` and only then Discovers at
  `status.go:43` — reorder. **status.go appeared in no work item in round 1;
  it is named here.**
- `internal/cli/guard.go:175-189` resolves **inline** (deliberately not via
  `resolveAgentID`) and Discovers at `guard.go:195` — reorder and route the
  lookup through the same map. **Keep the fail-open behavior on every failure**
  (`guard.go:171-173`, `184-189`, `202-204`): a guard that starts failing
  *closed* on a resolution error is a worse regression than the one being
  fixed.
- `internal/cli/identity_cmd.go:24` — **CORRECTION to the finding**: this file
  calls `resolveAgentID` at `:24` and **never calls `loop.Discover` at all**
  (it does not even import `internal/loop`; its `--control-repo`/`--loop-dir`
  flags at `identity_cmd.go:13-18` are declared and unused). So this is not a
  reorder but an **addition**: `identity_cmd.go` gains a `loop.Discover` call
  before resolution. **That Discover MUST be non-fatal** — on any discovery
  error, fall back to today's behavior and print the resolved id unchanged.
  `agentchute identity` currently succeeds in a directory with no pool at all,
  and must keep doing so.

MODIFY `internal/cli/serve.go`: pass `launchedWrapper` (computed at
`serve.go:122-129`, today consumed only at `serve.go:159-161`) as the fallback
candidate when both `--as` and `AGENTCHUTE_AGENT_ID` are absent —
`resolveAgentIDRaw` has exactly two sources today (`identity.go:27-39`) and then
errors, which is precisely the quickstart's own bare
`agentchute serve codex` in a fresh shell.

(d) Rule: take the candidate from `--as` / `AGENTCHUTE_AGENT_ID` / the wrapper
default as today; **if the candidate is a LOCAL NAME present in
`config.json.names`, resolve it to the joined id; treat it as a pool id only
when it is not in the map or already equals the joined id.** **No send-side peer
aliasing** — `--to` is never mapped.

(e) Rows, in-process now and re-run under sshd in M6: "resolver, direct launch
with env UNSET" (this row must **not** export env — the earlier row
false-greened by doing so); "resolver with env exported", both launch paths;
unmapped passthrough; `--to` never mapped.

(f) Done-when: both resolver rows green in-process; `agentchute identity`
still succeeds outside any pool; and `identity_test.go`'s diff is limited to
the five call sites in (c) — same test names, same assertions, same
`missingAgentIdentityHint` comparison — with `go vet ./...` clean, i.e. no
other test file needed an edit.

(g) ~180 LOC.

**WI-4.9 — `agentchute identity` listing + doctor client-side checks.**

(a) `identity` lists the local-name map and the resolution; `doctor` gains the
`hub` group (client half).

(b) §7.2 (the identity listing), §7.6 (config/key/connect/identity/pool lines;
the `AGENTCHUTE_CONTROL_REPO` warning; the env local-name warning with its
EXACT text; the negative-cache report; `E_NO_SSH` FAIL).

(c) MODIFY `internal/cli/identity_cmd.go` (it gained a non-fatal Discover in
WI-4.8), `internal/cli/doctor.go` (a new check group beside `runDoctorChecks`,
`doctor.go:197`).

(e) Row "env local-name warning"; the doctor probe budget is 10 s via one
one-shot hello.

(f) Done-when: `doctor` output matches the §7.6 sample shape against the
in-process harness, each line has its FAIL form naming the §7.5 remediation
verbatim, and `identity` outside a joined checkout is byte-identical to today.

(g) ~160 LOC.

**WI-4.10 — the CLIENT half of the W vectors.** *(S7 — these could not run in
M3.)*

(a) The three W vectors whose client-side behavior M3 had no client to test.

(b) §9.3 (W1, W2, W6); §4.5.2, §4.5.3.

(c) MODIFY `internal/spectest/wire.go` — add the client-driven halves as
**exported, transport-parameterized helpers** beside the hub-side ones (R6, the
same rule WI-3.5 pins: assertions never live in a `_test.go`, or WI-6.4 cannot
import them) — and `internal/spectest/wire_test.go` to drive them over
`net.Pipe`. Both halves reuse the **same** `conformance/vectors/wire.json`
definitions, so the vector has one source and two drivers.

(e) **W1-client**: after a disconnect mid-`check`, the NEXT check redelivers and
renders the REDELIVERED banner (the `check.go:180-193` path). **W2-client**: an
in-window disconnect is reported as `E_SEND_UNKNOWN`, the body is spooled, and
nothing is replayed automatically — asserted by counting **hub-side inbox
files**, not by inspecting client state. **W6-client**: a `check` that
terminates as `error` with `claimed_held: true` and no `msg` frames arms
the local guard latch.

(f) Done-when: W1–W6 are green with hub-side halves (M3) and client halves (M4)
in one `tools/test.sh` run. The sshd re-run is WI-6.4's and gates the tag.

(g) ~120 LOC.

### M5 — remote serve channel + join/authorize UX

**WI-5.1 — channel lifecycle in `serve`.**

(a) Remote mode of `cmdServe`: a dedicated channel, the §6.1 startup order, the
tick loop, and fence-on-drop.

(b) §6.1 (hello → lease-acquire → register(+token, template) → export token →
child; the `E_LEASE_HELD` refusal text), §6.2, §6.4 (client steps; the hub-side
release is WI-3.3a's), §6.5 (last-tick counts; single channel writer), §6.8
point 1 (env exports).

(c) MODIFY `internal/cli/serve.go` (`runWrapper` branches on `cfg.Remote != nil`;
`runnerChildEnv` `serve.go:502-530`; the fence path `serve.go:616-623` /
`937-953` reused); NEW `internal/hubclient/channel.go`.

(d) - The remote startup order is **register BEFORE child** — a deliberate
  strengthening over the local order (lease `serve.go:374` → child
  `serve.go:379-385` → register `serve.go:393`), which M1 pinned as unchanged
  (C10). Both orders coexist; the branch is on `cfg.Remote`.
- Env exports (§6.8 rule 1): `AGENTCHUTE_CONTROL_REPO=<canonical ssh:// URL>`
  and **no** `AGENTCHUTE_LOOP_DIR` — strip an inherited one, the same hygiene
  `runnerChildEnv` already applies to `AGENTCHUTE_GUARD` (`serve.go:510`, with
  the five appends at `511-521` and the guarded re-add at `522-528`).
  `AGENTCHUTE_SERVE_TOKEN` (`serve.go:520`) carries `Channel.Token()`.
- **Single channel writer** (§6.5): only the tick goroutine ever writes to the
  ssh child's stdin; nothing else holds a reference to it. Two goroutines
  interleaving bytes would corrupt framing and spuriously fence the child.
- `hasPendingInboxMail` (`serve.go:757-763`) becomes "last tick's counts"
  (≤5 s stale), and so does `injectIfPending`'s immediately-before-injecting
  re-check (`serve.go:740-750`) — there is no `poll` op. Preserve the fail-open
  direction the local listing-error path already chose.

(e) In-process: the startup order asserted; tick cadence 5 s; a channel drop ⇒
SIGTERM to the child ⇒ exit path; the counts feed `enqueueWake` unchanged
(`serve.go:659-668`).

(f) Done-when: a scripted child under a fake channel gets cued on
tick-pending and fenced on drop, and the child's environment carries the URL
with no `AGENTCHUTE_LOOP_DIR`.

(g) ~400 LOC.

**WI-5.2 — supervised relaunch (default-on).**

(a) §6.7 in full.

(b) §6.7; §8 rows 22–24.

(c) MODIFY `internal/cli/serve.go`.

(d) `--relaunch` boolean, **default true iff `Remote != nil`**, error on a
local serve; trigger set = transport loss (ssh child exit, tick deadline,
`E_CONNECT` during an attempt) plus **exactly one** attempt on `E_FENCED`,
stopping for real if that attempt sees `E_LEASE_HELD`; **never** on
`E_IDENTITY` / `E_VERSION` / `E_POOL_MISMATCH` / `E_HOSTKEY_CHANGED`; backoff
1 → 2 → 4 … capped at 60 s with ±20 % jitter, unbounded attempts; a fresh §6.1
sequence and a fresh child with the **original argv verbatim**
(`opts.WrapperArgs`, `serve.go:117`); one status line per attempt to the
terminal and the runner log; the dead session's latch is inert by construction
(`internal/loop/guard.go:20-24` — a latch whose stored session differs from the
current key reads as unset to every reader).

(e) Rows (in-process now, sshd in M6): "supervised relaunch — default path
(D5)" driven with a **bare** `agentchute serve` (an implementation that kept
relaunch opt-in fails this row); "relaunch opted out"; "`E_FENCED` single
relaunch attempt".

(f) Done-when: all three green in-process, with exactly one lane instance and
the old child SIGTERM'd first in the default row.

(g) ~250 LOC.

**WI-5.3a — `hub join`: core flow.** *(S9 split, part a.)*

(a) URL validation, naming, the lock file, probing, the pointer, shims, and the
env warnings — everything except keys (5.3b) and migration (5.3c).

(b) §7.2 (naming block, shadowing refusals, the per-hub-dir mutual exclusion
block, probe-before-pointer, writability, shim install), §7.4 (pointer
lifecycle), §7.5 (the three join refusal texts this item emits).

(c) NEW `internal/cli/hub_join.go` (+ helpers in `hub.go`).

(d) - URL validated against the §7.4 grammar; canonicalized; `hub-id` derived.
- **Naming**: with `--as` omitted, mint `<local-name>-<hostname>`;
  `<local-name>` is `--name`'s value canonicalized through `wrapperForToken`
  (**`internal/cli/shims.go:51-64`** — CORRECTION: it is not in `dispatch.go`;
  `dispatch.go:133,147` merely calls it), `<hostname>` is the first DNS label of
  `os.Hostname()`. Each component sanitized to `[a-z0-9][a-z0-9-]*` **exactly**:
  lowercase; every character outside `[a-z0-9-]` → `-`; runs of `-` collapsed;
  leading/trailing `-` trimmed; an empty result is an error. `--as` at join time
  is minted **verbatim**, never suffixed. The mapping is minted once and
  recorded in `config.json.names`; a later hostname change never re-derives it.
- **A non-wrapper `--name` is REFUSED** (S4), before keygen and before any
  authorize call, with the §7.5 text. The rule is forced by both launch paths:
  `ac serve <token>` hard-refuses an unknown token at `dispatch.go:133-137`
  before any identity work, and direct `agentchute serve <token>` treats the
  positional as the **wrapper command to exec** (`serve.go:117-129`) — so a
  non-wrapper name produces a lane with no launch form at all. Teaching the
  dispatcher the `names` map is the **rejected** alternative (§7.3); do not
  implement it.
- **Local-name / pool-id shadowing is REFUSED in BOTH orders**, before keygen
  and authorize, with the two exact §7.2 texts.
- **The lock file (G2)**: `~/.agentchute/hub/.locks/<hub-id>.lock` — a
  **sibling** of the dirs it guards, because migration renames and deletes them.
  Taken **before any probe** and held to the end of the run. Idiom: the shipped
  one, generalized to a caller-supplied lock path rather than an agent state dir
  — `syscall.Flock(fd, LOCK_EX|LOCK_NB)` in a deadline-bounded poll loop
  (`internal/loop/filelock_unix.go:46-67`; `agentLockTimeout` 5 s at `:20`,
  `agentLockRetryInterval` 25 ms at `:23`). `.locks/` is created 0700 on demand.
  One acquisition per run, never nested. Timeout ⇒ the named §7.5 refusal,
  **never a silent wait**.
- **Probe before pointer**: `.agentchute-control-repo` is written only after a
  probe proves the hub is real (a successful hello, or an SSH auth refusal —
  which proves host + sshd + reachability). Connect/DNS/host-key failures write
  nothing. Join appends the pointer filename to `.git/info/exclude`; if the
  pointer is already **tracked**, warn loudly instead of hiding it.
- **Writability proof (G-M5)**: the verification hello checks
  `hello-ok.writable`, and a false verdict fails the join with the §7.2 text.
- **Auto-authorize** over the operator's own SSH (interactive, not BatchMode),
  single-quoting **per argument**; every value already excludes `'` by grammar.
  On failure, print the complete ready-to-paste line and exit 0.
- Shim install (reusing setup's machinery) + the new-shell reminder; the
  `AGENTCHUTE_CONTROL_REPO`-disagrees-with-pointer warning.

(e) Rows in-process: join idempotence; default naming; the hostname-change
invariant; shadowing both orders; the non-wrapper `--name` refusal (no key file,
no authorized_keys line, no `config.json` change); sanitization cases; lock
mutual exclusion (two concurrent joins ⇒ exactly one proceeds, the other gets
the lock-busy refusal, state identical to a single run).

**(f) Done-when — X4, the circular done-when fixed.** The §7.2 quickstart
transcript is reproduced against the **M5 in-process harness** (WI-3.3a's
fake-transport session, driven as a subprocess over pipes), **not** against real
sshd. The real-sshd quickstart transcript is WI-6.2's "join→authorize→verify
happy path" row. Round 1 pointed this item at "the M6 harness", which does not
exist while M5 is being implemented.

(g) ~320 LOC.

**WI-5.3b — `hub join`: key lifecycle.** *(S9 split, part b — specialist
hand-off inside the M5 merge; see §2 rule 1 and §4.)*

(a) Keygen, versioning, the active symlink, rotation, and the recovery
classifier.

(b) §7.2 (key handling, the version-file grammar G3, `--rotate-key`, and the
three recovery shapes E3/F3/H3/J2).

(c) NEW `internal/cli/hub_keys.go` (helpers used by `hub_join.go`).

(d) - **Idempotent by contract (G-B1)**: if `keys/<id>_ed25519` already exists,
  join **reuses** it — never a second keygen, never a surprise `--replace-key`.
  Generation happens only when absent, with the exact §7.2 argv
  (`ssh-keygen -q -t ed25519 -N "" -C agentchute:<id> -f …_ed25519.v1`), then
  the active-pointer symlink. `ssh-keygen` is **never** re-run onto an existing
  path (`-q` still prompts "Overwrite?", which hangs a scripted join).
- A pre-existing passphrase-protected key is refused (probe:
  `ssh-keygen -y -P "" -f <key>` must succeed).
- **Version-file grammar (G3), normative**: `.v<N>` is parsed as a **NUMBER**,
  `<N>` matching `^[1-9][0-9]*$`, and ordering is **numeric** — `v10` is newer
  than `v2`; minting uses `max(existing N) + 1`. A **non-numeric suffix is not a
  version file and is never guessed at**: `v01`, `v2b`, `vold`, `v` are excluded
  from the scan, neither adopted as newest nor pruned as residue, and the run
  **refuses**, naming the file with the §7.5 text. `*.pub` and `*.invalid.*` are
  never version candidates.
- **`.invalid.<ts>`** is `<original-file-name>.invalid.<stamp>` where `<stamp>`
  is the codebase's existing UTC filename stamp (`tsStampLayout`,
  `internal/loop/tsid.go:22,51`) — microsecond resolution, so two retirements
  within one second cannot collide; the `.pub` half is retired under the **same**
  stamp. Retired files are inert forever.
- **There is deliberately NO `.pub` symlink** — `keys/<id>_ed25519` is the one
  and only pointer, and the current pubkey is **derived**:
  `readlink(keys/<id>_ed25519)` → `<id>_ed25519.v<N>` → pubkey is
  `keys/<id>_ed25519.v<N>.pub`. This is what keeps promotion ONE atomic step.
  Both paste lines that need a "current pubkey" resolve it this way: §7.5's
  `E_UNAUTHORIZED` prints the **active** target's pubkey; the rotation-recovery
  branches print the pubkey of the version that branch selected (the STAGED
  `v<N+1>.pub` while resuming, the ACTIVE `v<N>.pub` in the residue branch).
- **`--rotate-key`**, five steps: mint `v<N+1>` at its versioned path (keygen,
  then dir fsync) → remote replace via `hub authorize --replace-key`, **only**
  over the operator's own unrestricted SSH or the printed paste line (never over
  the agent's pinned key, whose forced command can only run `hub session`) →
  promote (write a temp symlink, one `rename()` over the active path) → verify
  by hello with the new active key → delete the `v<N>` files.
- **Recovery classifier**, run by EVERY join/rotate, in this order: (1)
  **version files with no pointer symlink** (H3a) — probe the newest with
  `ssh-keygen -y -P ""`; pass ⇒ adopt (create the initial symlink) and continue;
  fail ⇒ `.invalid.<ts>` rename and mint fresh. (2) **A version newer than the
  symlink's target** (staged rotation) — probe hello with the STAGED key FIRST;
  success ⇒ promote if needed, then fall through to (3), never re-driving the
  remote replace over a now-revoked key; staged failure ⇒ if the active key still
  answers, resume at step 2 through the operator path; if neither answers, print
  the paste line with the STAGED pubkey. (3) **Older versions beside the active
  target** (H3b) — **VERIFY first, prune second**: probe hello with the ACTIVE
  key; only on success delete the older versions. On ANY failure prune nothing,
  and split by error class (J2): **only `E_UNAUTHORIZED` prints the paste line**;
  `E_CONNECT`, `E_HOSTKEY_CHANGED`, `E_VERSION`, `E_IDENTITY`,
  `E_POOL_MISMATCH`, `E_HUB_NO_BINARY` each get their own §7.5 remediation
  unchanged, with no reauthorize hint overlaid.
- A leftover temp symlink (crash mid-promote, before the rename) is inert and
  removed on the next run.

(e) Failure injection at EVERY transition (§10.3's staged-rotation row):
between first keygen and the initial symlink (adoption, and the probe-fail
retire path), after mint, before and after the remote replace, mid-promote,
after promote before verify, after verify before cleanup — the post-replace
cases running with the old key **already revoked**. Plus the G3 grammar rows:
`.v2` vs `.v10`; a `.vold`/`.v01`/`.v2b` file neither adopted nor deleted; two
retirements within one second not colliding; the "current pubkey" resolved by
`readlink` + `.pub` and no `keys/<id>_ed25519.pub` symlink existing.

(f) Done-when: every injected failure point converges on a re-run, in-process;
the numeric-ordering and invalid-suffix rows green.

(g) ~300 LOC.

**WI-5.3c — `hub join`: same-hub URL migration.** *(S9 split, part c —
specialist hand-off inside the M5 merge.)*

(a) The D4a migration: detect the same hub under a different spelling, refuse
while a lane is live, and move the hub dir atomically.

(b) §7.2 (the migration block in full, incl. the B5 attribution predicate, the
G1 completion boundary, and the G4 mux reaping), §6.8 rule 5 (the interlock).

(c) NEW `internal/cli/hub_migrate.go`; MODIFY `internal/cli/hub_join.go`.

(d) - **Fingerprint scan**: before any keygen, connect and obtain the host-key
  fingerprint (available pre-auth), then scan `~/.agentchute/hub/` for dirs whose
  recorded fingerprint matches — **considering only entries whose name is exactly
  12 lowercase hex characters**, so `<new-id>.partial` and `.locks/` can never be
  scan candidates. For each match, attempt a hello with that dir's existing key
  for this `--as`; success **and** `hello-ok.pool12` equal to that dir's recorded
  pool12 ⇒ same hub ⇒ migrate.
- **Two locks, ascending lexicographic hub-id order, always** — never "old then
  new" — so deadlock is structurally impossible.
- **The attribution predicate is REMOTE-SPECIFIC (B5).**
  `setupCommandMatchesPool` (`internal/cli/setup_reset.go:308-345`) **must not
  be reused unchanged**: its pool proof is an OR of two exact-value comparisons
  of argv `--control-repo` / `--loop-dir` against `cfg.ControlRepo` /
  `cfg.LoopDir` through `setupPathsEquivalent` (`setup_reset.go:340-341`). In
  remote mode argv carries the `ssh://` URL, `cfg.ControlRepo` is the LOCAL repo
  root, and `cfg.LoopDir` is the shadow — so **every** live lane would fall into
  the "alive but unattributed" branch, and every ordinary migration would print
  the pid-reuse text with its "remove runner.json" advice, which for a genuinely
  live lane is both wrong and destructive. The new predicate:
  1. **Subcommand match, unchanged**: reuse `setup_reset.go:320-332`'s test —
     `agentchute serve` (with the pre-v0.9.1 `agentchute run` alias kept for
     upgrade cleanup). Anything else ⇒ **ambiguous**.
  2. **Pool proof**, on values extracted with `setupCommandFlagValue`
     (`setup_reset.go:352+`, which already truncates at `--` at `355-358`, so a
     wrapper's own flags can never be misread):
     - argv has `--control-repo` ⇒ canonicalize with §7.4's **URL**
       canonicalizer and require equality with the old dir's recorded URL ⇒
       **attributed**; a different `ssh://` URL, or any local path ⇒
       **ambiguous**.
     - else argv has `--loop-dir` ⇒ `setupPathsEquivalent` against the old
       dir's shadow ⇒ **attributed**; mismatch ⇒ **ambiguous**. (This arm exists
       only for lanes launched by a pre-§6.8 binary or by hand.)
     - else **neither flag** — the direct `agentchute serve <wrapper>` form the
       quickstart itself prints ⇒ **attributed**, because the `runner.json` that
       named this pid lives under that hub dir's own shadow state and nothing
       else writes there.
       **Known and deliberate (R11)**: after OS pid reuse this arm can
       false-attribute — a stale `runner.json` naming a pid that now belongs to
       an unrelated flagless `agentchute serve`. The consequence is a refusal
       to migrate (fail CLOSED), never a kill and never a deletion, and the
       operator's own inspection clears it. **Do not "fix" this into
       proceeding**: trading a spurious refusal for a migration that races a
       genuinely live lane is strictly worse, and the ambiguous-pid arm exists
       for exactly the cases we cannot attribute.
  The liveness read is `loop.RunnerStatePath` (`config.go:149-151`) with the
  same-host and pid-alive gates `stopSetupRunner` uses
  (`setup_reset.go:196-224`). Three outcomes, with the §7.2 texts verbatim:
  attributed live lane ⇒ the "still running against the old URL … stop that
  session first" refusal (**never** a `kill` command); pid alive but
  unattributed ⇒ the ambiguous-pid text (inspection guidance only); pid dead or
  no `runner.json` ⇒ proceed.
- **Completion boundary (G1) — a single `rename()`, no step optional**:
  (1) copy key material + config (latch/spool residue included) into
  `~/.agentchute/hub/<new-id>.partial/` — **`mux/` is deliberately NOT copied**;
  (2) write the new `config.json` inside `.partial` (recorded URL updated;
  `joined_as`/`names`/`pool`/`pool12` carried over), then **fsync every written
  file and fsync the `.partial` directory itself**; (3)
  `rename("<new-id>.partial", "<new-id>")` — **this is the commit point**; then
  fsync `~/.agentchute/hub/`; (4) rewrite the checkout's pointer to the new URL;
  (5) delete the old hub dir. Recovery is deterministic: before step 3 the only
  complete dir is the old one and the `.partial` is **scratch, never adopted**;
  after step 3 the ordinary joined-verify path sweeps for the leftover (a sibling
  dir with the SAME fingerprint but a stale recorded URL) and finishes steps 4–5.
- **Mux reaping (G4)**: step 5 first issues a best-effort `ssh -O exit` for the
  old URL (one call, failure ignored). An unreaped master is harmless — it holds
  an unlinked socket inode and no new one-shot can attach to a ControlPath that
  no longer exists.

(e) Rows in-process: same-hub alias rejoin (zero new keys, one authorized_keys
line, pointer updated); **migration attribution, normal lane** — a lane launched
**exactly as the quickstart launches one**, both `ac serve codex` and the direct
`agentchute serve codex` form, must produce the "still running against the old
URL" refusal, never the ambiguous-pid text (a predicate that exact-matched argv
against `cfg` fails this row in the normal case); migration vs pid reuse (both
sub-cases); crash injection before and after the `rename()`; the fingerprint scan
never returning `<new-id>.partial` or `.locks/`; both locks in ascending order
under a reversed-pair concurrent run.

(f) Done-when: every crash-injection point converges on a re-run with exactly
one authoritative dir at every instant, and the normal-lane attribution row is
green. **The M6 re-runs under sshd are WI-6.2's.**

**Interlock (B4/B5):** this item's URL arm is correct only because WI-4.3
forwards the URL. This item's gate ask must re-verify WI-4.3's argv goldens at
the M5 head SHA.

(g) ~300 LOC.

**WI-5.4 — `hub authorize`.**

(a) The §7.1/§5.1/§5.2 hub-side story.

(b) §5.1 (template line verbatim incl. `--pool-id`; reject-not-encode grammars
+ `E_POOL_PATH_UNSAFE`; the pool.id read-or-mint contract; the marker
`agentchute:<id>:<pool12>`), §5.2 (marker-keyed `--list`/`--revoke`/
`--replace-key`; the duplicate-(id,pool12) refusal with the hostname-collision
hint), §7.1 (0600/0700 enforcement + the StrictModes rationale; `--list` PASS/FAIL
health audit; `--revoke`'s live-session note).

(c) NEW `internal/cli/hub_authorize.go`.

(d) **This item is the SOLE PRODUCTION WRITER of `state/pool.id` in the entire
codebase** (P3 — verified: no Go file, test, or tracked source mentions
`pool.id` or `pool12` today). It mints by **exclusive create** — the shipped
first-writer-wins idiom (`loop.WriteRegistrationExclusive`,
`registration.go:133-169`), never a plain atomic replace — 0600, content exactly
`^[0-9a-f]{12}\n$` where `pool12 = hex(sha256(EvalSymlinks(Clean(abs(--pool)))))[:12]`;
the H2 loser of the race re-reads and **adopts** the winner's value.

**Validation contract (J1)** runs on BOTH consumers and BEFORE any comparison or
interpolation: exactly one regular, non-symlink, 0600 file read through the
bounded no-follow reader (`loop.ReadFileLimit`, `registration.go:34-48`) with a
64-byte cap, content matching the grammar exactly. The two consumers are `hub
authorize` (the fresh-read path AND the H2 loser-re-read path) and `hub session`
at startup (WI-3.3a — which until this item lands runs only against the WI-3.2
test fixture). Anything else ⇒ `E_POOL_ID_INVALID`: authorize writes **nothing**
to `authorized_keys`; the session refuses before any op.

Also: authorize validates before writing (the `--pool` path must exist and
contain `AGENTCHUTE.md` + `.agentchute/loop/`; the resolved binary path must be
executable), and it must run **as the SSH login user**.

(e) Rows: durable pool identity (realpath / symlink / trailing-slash / macOS
case-alias spellings ⇒ ONE marker); the `--pool`-only key-line edit; pool
mismatch consistent re-point; pool.id validation (the full J1 malformed set, with
`authorized_keys` byte-identical before/after); concurrent alias first-authorize
(H2); authorize validation; shell-safety refusals (the C5 metacharacter set).

(f) Done-when: all authorize-side rows green — no sshd needed, these are pure
local file operations.

(g) ~470 LOC.

**WI-5.5 — `setup --wipe-state` preserves `state/pool.id`.** *(S9 — peeled out
of the authorize item because a destructive surface deserves its own review
focus. Ships in the SAME MERGE as WI-5.4: landing the writer without the
preservation bricks every key binding on the next `--wipe-state`.)*

(a) Add `pool.id` to the wipe's preserved, non-runtime scaffold (H1).

(b) §5.1 (the preservation paragraph), §9.1 third bullet (the M2 spec text this
implements).

(c) MODIFY `internal/cli/setup_wipe.go` **only**. Three places, each of which
today exempts exactly `setup.json` and nothing else:

- `wipeStateCategory` (`setup_wipe.go:295-316`): the name test at `:307`
  becomes `name == "setup.json" || name == "pool.id"` → `cat.Preserved`.
- `rescanWipeLeftovers` (`setup_wipe.go:516-536` — CORRECTION: the round-1
  citation `519-536` started mid-doc-comment): the skip at `:529` gains
  `pool.id`, or the post-wipe rescan flags the very survivor it was told to
  preserve.
- `printWipePlan`'s preserved rendering (`setup_wipe.go:703-711`) must list it,
  so `--dry-run` shows it.

(e) Row "wipe preserves pool identity (H1)": `setup --reset --wipe-state` on a
pool with `state/pool.id` ⇒ listed as Preserved in the **dry-run and the wipe**,
survives the wipe, is **not** flagged by the rescan, and a following `hub
session` startup still validates it against `--pool-id`.

(f) Done-when: `setup_wipe_test.go` green with that row added, and `git diff`
touches no file but `setup_wipe.go` and its test.

(g) ~60 LOC.

**WI-5.6 — error-catalog wiring + hub-side doctor audit.**

(a) Every §7.5 text emitted from its mapped condition, verbatim; plus the
hub-side authorize-line audit (§7.6).

(b) §7.5 (all three tables), §7.6 (the third bullet).

(c) MODIFY `internal/cli/hub_*.go`, `internal/cli/doctor.go`; texts centralized
in NEW `internal/cli/hub_errors.go`.

(d) Both `E_POOL_MISMATCH` arms have **distinct exact texts** (§7.5) and each
must be emitted from its own condition (hub arm at session start; client arm
after `hello-ok`) — a single shared string fails this item. `E_CHANNEL_LOST`
echoes the lane's **own** argv (G-m5), never a hard-coded example.

(e) Golden-text tests per catalog row — the §7.5 tables ARE the fixtures.

(f) Done-when: a table-driven test walks every §7.5 row and asserts the emitted
text byte-for-byte.

(g) ~250 LOC.

**WI-5.7 — M5 integration pass (in-process).**

(a) Wire WI-5.1–5.6 together against the fake transport, before real sshd
exists.

(e) Rows runnable in-process: live-lease self-check/turn-end (C2); register
field semantics (C2/D1), including the `Sweep` arm (WI-4.5: remote `boot`
sweeps the HUB pool once, the shadow never, and every other verb sends
`sweep:false`); **remote vendor resolution skipped client-side (S2)** —
in a joined checkout with a custom pool id, drive all four `resolveAgentVendor`
call sites bare (`serve.go:152`, `register.go:259`, `boot.go:86`,
`self_check.go:126`) and assert each sends `Vendor:nil` on the wire, **that
each renders the RESOLVED vendor from `RegisterResp.Reg.Vendor` and not the
nil-on-wire request value** — `register`'s text line, `boot`'s text **and**
`--json`, `self-check`'s text **and** `--json`, all byte-identical to a local
run of the same pool (T4: asserting only "nil on the wire, exit 0" passes
while a success path prints an empty vendor) — that
`cmdServe`'s missing-vendor refusal (`serve.go:153-155` — CORRECTION: the
surrounding block is `152-157`, not `152-158`) is **suppressed** for remote
lanes, and that an explicit `--vendor` still wins and is still validated;
**remote `status` renders the hub, header included** (WI-4.5: table from the
response, `control_repo:` on the `ssh://` pointer, `loop_dir:` on the shadow
with its pinned marker line byte-exact, and — with one 64 KiB-`host` row
planted on the lexicographically LAST of three agent ids, WI-4.5(e)'s fixture
rule, so the two earlier rows survive the prefix cut — the pinned truncation
notice byte-exact, naming the WIRE limit and not only the row cap);
mid-stream disconnect arms the latch (E1); the negative cache covers
SessionStart (D6).

(f) Done-when: `tools/test.sh` green across the repo.

(g) ~250 LOC (tests).

### M6 — real-sshd matrix + CI + docs → tag v1.6.0

**WI-6.1 — sshd harness + the test wrapper script.**

(a) A self-contained sshd fixture per the §10.3 preamble, plus the named script
every M6 done-when cites.

(b) §10.3 preamble; §5.1 (the forced-command line the harness writes).

(c) NEW `integration/sshd/harness_test.go` (build-tagged
`//go:build sshd_integration`); NEW `integration/sshd/doc.go`; NEW
`tools/sshd-test.sh`.

**`integration/sshd/` is part of the ROOT module** — no nested `go.mod`, so
cross-cutting rule 6's empty-`go.mod`-diff still holds. `doc.go` is **untagged**
and contains only a doc comment and the `package` clause, so the package always
has one buildable file and `go build ./...` / `go vet ./...` / `go test ./...`
can never hit "build constraints exclude all Go files" with the tag absent.
Every test file is tagged.

**`tools/sshd-test.sh` — the decision P7/X7 asked for, with the reasoning that
produced it.** `tools/test.sh` is 26 lines with **no argument parsing of any
kind** (it never references `$@`, has no `case`/`getopts`, no modes), and its
first action is `gofmt -w .` — which **mutates the checkout** and runs without
the env strip. Adding a tagged mode would introduce arg plumbing that does not
exist and drag a tree-mutating step into a suite that must run on a read-only CI
checkout. The repo's own precedent for "a second suite gets its own invocation"
is the conformance module (`ci.yaml:39-40`). So: a new script, exact content:

```sh
#!/bin/sh
# sshd-test.sh — the real-sshd hub integration suite (PLAN WI-6.1/6.2).
# Strips EVERY AGENTCHUTE_* var (the same idiom as tools/test.sh:7) and sets
# only AGENTCHUTE_SSHD_TEST=1, so a run from inside a serve session can never
# reach the live pool.
set -u
strip_env=$(env | awk -F= '/^AGENTCHUTE_/{print "-u " $1}')
exec env $strip_env AGENTCHUTE_SSHD_TEST=1 go test -tags sshd_integration ./integration/sshd/...
```

No gofmt, no mutation, no arguments. **Every M6 done-when cites
`tools/sshd-test.sh`, never a bare `go test`.** It is deliberately NOT added to
ci.yaml's `shell` job shellcheck list: that job checks `install.sh` and
`tests/*.sh`, and neither `tools/test.sh` nor `tools/fact-sweep.sh` is in it
today — adding one `tools/` script and not the others would be an inconsistency,
not a gate.

(d) The harness generates throwaway host + client keys in a temp dir, starts
`sshd -D -f <generated sshd_config>` on a random high port with
`AuthorizedKeysFile` pointing at a generated file containing the §5.1
forced-command line (absolute path to the freshly built test binary), and a pool
in a temp dir. It **REFUSES to run** when `AGENTCHUTE_LOOP_DIR` or
`AGENTCHUTE_CONTROL_REPO` point at a real pool.

**(f) Done-when (X4 — round 1 gave this item no done-when at all):**

1. The harness starts sshd on a random high port and a probe client
   authenticates with the generated key.
2. The forced command actually runs the freshly built test binary — asserted by
   the session recording `SSH_ORIGINAL_COMMAND` and the client's requested exec
   string being **discarded**.
3. Teardown through `t.Cleanup` leaves **no listening socket and no temp tree**,
   even when a test fails.
4. **Real-pool containment**: with `AGENTCHUTE_LOOP_DIR` or
   `AGENTCHUTE_CONTROL_REPO` pointing at a real pool the harness refuses, and
   the test asserts **both** the refusal and that nothing under the named pool
   was written.
5. All four green locally on macOS **and** in CI on ubuntu-latest **and**
   macos-latest, invoked as `sh tools/sshd-test.sh`.

Watch-item (budget, not a finding): a non-root `sshd -D` authenticates only the
invoking user, and macOS needs its host-key and config paths hand-fed.

(g) ~280 LOC.

**WI-6.2 — the full §10.3 matrix.**

(a) Every §10.3 row as a named test, including the sshd-only rows and the sshd
re-runs of the M4/M5 in-process rows.

(b) §10.3 in full.

(c) NEW `integration/sshd/*_test.go` — one file per §10.3 cluster (handshake,
disconnect/fence, lease, launcher/resolver, join/migrate/rotate, authorize/pool
identity, env contract).

(e) Sshd-only rows: happy path (**the real §7.2 quickstart transcript lands
here**, X4); identity/version mismatch; disconnect-after-claim and
disconnect-after-send; reclaim-during-send; channel drop fences the child
(≤15 s); half-open hub (20 s read deadline + release); lease-held; host-key
change; **mux reuse — run through BOTH ControlPath branches** (the harness's
normal hub dir and an injected deep hub dir forcing WI-4.4's fallback), each
asserting 1 sshd auth log entry for 3 sequential one-shots; child env contract
(§6.8, incl. the offline guard latch DENYING while armed and a child `send`
landing in the hub pool); **launcher preserves remoteness** — via `ac serve`
AND a constructed legacy `ac-*` shim (WI-4.3's note), asserting `Remote != nil`
and never merely "the command succeeded"; both resolver rows; hub-reboot pid
reuse and the clock-step row; the relaunch rows; and the sshd re-runs of the
M4/M5 in-process rows (migration attribution, staged rotation recovery,
pool identity spellings — the macOS case-alias spelling REQUIRES the macos
runner and cannot pass on Linux).

(f) Done-when: the full matrix green locally on macOS and in CI on both OSes,
run as `sh tools/sshd-test.sh`.

(g) ~500 LOC.

**WI-6.3 — CI wiring.**

(a) Add the `hub-integration` job; leave every other suite where it already
runs.

(b) §10.4.

(c) MODIFY `.github/workflows/ci.yaml` **only**.

**(d) Verified starting state (F10 — the round-1 item was written against an
assumed CI).** `ci.yaml` has four jobs: `go`, `shell`, `upgrade-smoke`,
`upgrade-hook-refresh-smoke`. The `go` job (matrix `ubuntu-latest` +
`macos-latest`, `fail-fast: false`) runs `gofmt -l` (ubuntu only, the
**non-mutating** form), `go vet ./...`, `go test ./...`, `go test -race ./...`,
`go build ./...`, and `cd conformance && go test ./...`. **No job calls
`tools/test.sh`**, and `tools/test.sh` runs `gofmt -w .`, which would mutate the
CI checkout. Therefore:

- **NEW job `hub-integration`**, matrix `ubuntu-latest` + `macos-latest`,
  `fail-fast: false`; steps: `actions/checkout@v6`, `actions/setup-go@v6` with
  `go-version: 'stable'` and `check-latest: true` (mirroring the `go` job, NOT
  release.yaml's pinned 1.21), then **ONE step**: `sh tools/sshd-test.sh`. That
  single step runs **only the tagged package**. `tools/test.sh` appears
  nowhere.
- **The §10.2 hubwire tests, the `internal/spectest` L/W drivers, and the M1
  seam tests are ordinary untagged root-module tests** — they run in the
  **existing** `go` job's `go test ./...` step and need no CI change at all.
- `hub-integration` is a **separate required check**, so a matrix flake never
  masks unit failures.

(f) Done-when: both jobs green on the PR and `gh pr checks` clean; **and** a run
with the tag ABSENT confirms `go build ./...`, `go vet ./...`, and
`go test ./...` all still pass and report nothing from `integration/sshd/` (the
untagged `doc.go` from WI-6.1 is what makes that deterministic).

(g) ~50 LOC YAML.

**WI-6.4 — sshd-backed W vector runs.**

(a) Re-drive conformance W1–W6 through the real transport (§9.3's timing rule:
they gate the tag).

(c) NEW `integration/sshd/conformance_wire_test.go`, which **imports
`internal/spectest`** (same module) and calls its exported vector loader and
assertion helpers, driving them through the real ssh transport instead of
`net.Pipe`. The vector definitions have exactly ONE source — the shared JSON
under `conformance/vectors/` — and ONE assertion implementation, parameterized
by transport.

(f) Done-when: W1–W6 (hub-side halves and client halves alike) green under sshd
on both OSes, run as `sh tools/sshd-test.sh`.

(g) ~150 LOC.

**WI-6.5 — docs (repo + web).**

(a) Ship the quickstarts and the Tailscale recipe, and put the hub on the
website.

(b) §7.1, §7.2, §7.7; post-M2 the spec wins on any conflict.

**(c) The exact artifacts (X6 — round 1 deferred this to "the integrator
confirms at merge time"). Verified layout: `web/` is hand-written static HTML
with NO templating, NO build step, and NO nav include — nav markup is duplicated
per page.**

- MODIFY `README.md`: a short "Multi-machine pools (SSH hub)" section pointing
  at the new doc.
- NEW `docs/hub.md`: the §7.1 + §7.2 quickstarts, a pointer to the §7.5 error
  catalog, and the §7.7 Tailscale recipe. (Root `docs/` is repo-internal
  markdown — `decisions/`, `internal/`, `releases/` — which is the right home
  for an operator walkthrough.)
- NEW `web/hub.html`: modelled on `web/spec.html` — links `style.css` and
  reuses `spec.html`'s `<header class="site">` + `<nav>` block
  (`web/spec.html:17-27`) verbatim, so the site chrome matches.
- MODIFY the nav of the three **current-surface** pages, by hand, because there
  is no include: `web/index.html:188-193`, `web/spec.html:20-25`,
  `web/blog/index.html:20-25`. Individual blog posts' navs are historical
  artifacts and are **not** touched — the same line `tools/fact-sweep.sh:19`
  draws when it excludes `web/blog/*` from the live-surface scan.
- MODIFY `web/sitemap.xml`: add one `<loc>https://agentchute.dev/hub.html</loc>`
  in the same bare shape as the existing 15 entries (the file carries no
  `lastmod`/`changefreq`/`priority` — match that).
- `web/_redirects` and `web/robots.txt`: no change.

**(f) Done-when:**

1. `tools/fact-sweep.sh` PASS. Its live-surface set is `README.md AGENTCHUTE.md
   web` minus `web/blog/*` (`tools/fact-sweep.sh:12,19`), so `web/hub.html` IS
   scanned — any number claimed there must match the tree.
2. **Link verification**: every local `href`/`src` in `web/hub.html` and in the
   three edited navs is enumerated and stat'd, and each target exists. A broken
   relative link is the characteristic failure mode of a hand-written site with
   no build step.
3. `web/hub.html` opens in a browser with the site chrome and the new nav link
   is present on all three edited pages.
4. grok persona-walk SHIP.

(g) ~220 LOC prose/markup.

**WI-6.6 — v1.6.0 release notes + CHANGELOG. (Integrator-owned; lands in the
M6 PR, before the tag. The single remaining release-notes item — WI-1.9
is spent.)**

(a) The tag cannot release without the notes file (`release.yaml:110` runs
`test -s "docs/releases/${tag}.md"`; `:168` uses it as `--notes-file`).
Non-empty is the **only** check — a present-but-wrong file ships silently.

(c) MODIFY `docs/releases/v1.6.0.md` (already on `main`, frozen until this
item) and `CHANGELOG.md` (`## Unreleased — operation seam` becomes the
dated `## v1.6.0` entry). **Do not create `docs/releases/v1.7.0.md`.**

(d) **Replace the top matter** of `docs/releases/v1.6.0.md` — do not merely
append. The existing in-progress / seam-scoped text disclaims the capability
this release ships (the hub). WI-6.6 appends the hub section **and** rewrites
the top matter so the published notes do not say "no hub capability" /
"nothing to do on any lane". The notes MUST also carry the rollout order:
**the hub upgrades first**, then the remotes; plus the note that a hub
`agentchute update` fences every live lane once (§8 row 24) and that
default-on relaunch brings them back under the new binary automatically.
They must also restate the P8 rule for lanes: a remote that updated ahead
of its hub gets `E_VERSION` and must **wait** — re-running `hub join` is
not the fix.

(f) Done-when: `docs/releases/v1.6.0.md` exists, is non-empty, and no longer
disclaims the hub; `tools/fact-sweep.sh` PASS; checklist §5 tag-time
re-measure and #156 URL checks both run in this same PR.

(g) ~60 LOC prose.

→ **Integrator: tag v1.6.0** (checklist §5).

---

## 3. Cross-cutting rules (binding on every implementer)

1. **Tests only via `tools/test.sh` — NEVER bare `go test ./...`.** Env-strip is
   mandatory on this fleet: a bare run from a serve session has kicked the whole
   fleet before (env leak through `AGENTCHUTE_*`). `tools/test.sh:7` builds the
   strip list dynamically from the live environment and applies it to vet/test/
   build. If `env -u` is refused by the sandbox, stop and ask — never fall back
   to bare. From M6 on, the sshd suite runs as `sh tools/sshd-test.sh`, which
   applies the identical strip (WI-6.1).
2. **Worktree isolation.** The working directory is the LIVE fleet checkout. All
   implementation happens on a worktree branch (`EnterWorktree` for scratch;
   `git worktree add .tmp/worktrees/<merge> <sha>` for pinned review checkouts,
   removed the same turn). Never edit the live tree; re-check `git status`
   rather than trusting a snapshot.
3. **Never test against the live pool.** The sshd harness and every destructive
   test refuse when env points at a real pool (WI-6.1 done-when 4); unit tests
   use temp dirs exclusively.
4. **Spec-first ordering.** M2 merges before any M3+ code. After M2 the spec
   text is authoritative over DESIGN.md; an M3+ implementer who finds a
   spec/DESIGN conflict **stops and files it** — never silently follows either.
   This erratum aligned DESIGN §3.1 / §4.4.1 with AGENTCHUTE.md §13 on
   `owed_note`; that field no longer has a precedence carve-out.
5. **Ritual per PR = `tools/test.sh`, and nothing else** (X7 — the round-1 rule
   listed `gofmt`, `go vet`, `tools/test.sh`, `go build` as if they were four
   things). `tools/test.sh` **IS** gofmt + `go vet` + `go test` + `go build`
   (`tools/test.sh:11-24`), with the env strip on the last three. There is no
   separate raw-command list. Two facts an implementer needs: `gofmt` there is
   `gofmt -w .` and **mutates the tree** (expected locally; CI uses the
   non-mutating `gofmt -l` instead, `ci.yaml:22-30`), and after WI-3.5 the
   script also runs the conformance module. From M6 on, `sh tools/sshd-test.sh`
   runs **in addition** — it is not part of `test.sh` and never will be.
6. **Zero new Go dependencies.** **BOTH** `go.mod` files — the root
   (`github.com/agentchute/agentchute`) and `conformance/`
   (`agentchute.dev/conformance`, a three-line module with no requires and no
   `go.sum`) — must be byte-identical before and after every merge.
   `golang.org/x/sys` is **already** a direct requirement (`go.mod:7`, v0.30.0),
   currently imported only from `filelock_windows.go`, so WI-3.4's
   `x/sys/unix` import adds no module; `release.yaml:28-30` (`go mod tidy` +
   `git diff --exit-code go.mod go.sum`) is the check that proves it. The client
   shells out to the system `ssh`/`ssh-keygen` only.
7. **Review freeze (E3).** Every gate ask carries pinned base+head SHAs, the
   file list, and the allowed delta. Reviewers verify against
   `git show <sha>:file`, never working-tree reads.
8. **Delta re-gates (E4).** After a FIX round, re-gate the delta only — with
   fresh pinned SHAs.
9. **Verdict mechanics.** Verdicts on the bus; mirrored to `gh pr comment` when
   authorized — never `gh pr review` (shared-token self-block).
10. **Push discipline.** Explicit refspec always; verify the PR's `headRefOid`
    equals the SHIP'd SHA before merge (a bare push from a worktree branch can
    silently push nothing).
11. **Embedded-asset coupling.** Run `gh pr checks` before any SHIP: embedded
    hooks/templates/spec mean a zero-`.go`-diff is not a zero-behavior-change.
    WI-2.4 is the item where this bites (marker-versioned enrollment prose +
    `templates_drift_test.go` / `assets_test.go`).
12. **Gate asks pin the reviewer's surface.** A gate ask MUST pin the delta
    to the named surface of the reviewer it is addressed to — files and
    hunks — not just the merge's overall base..head. An ask that hands a
    reviewer the whole merge and says the slice is somewhere inside is
    **malformed**; the correct response is `NEEDS-INFO`, not a degraded
    skim. Surfaces are in §4.

---

## 4. Lane assignments

The roster is DYNAMIC — the integrator confirms it via `agentchute status` at
execution start (this plan does not run it). **Budget reshuffle (Alex,
2026-08-17):** implementation moved off the claude lanes. Stated cost: **codex
implementing M3–M6 means codex is no longer the mandatory second gate on
those merges**, so opus-xhigh's single deep pass has to be the hard one.

Per §2 rule 1, **one lane owns each merge**. There are no specialist
implementation hand-offs left: **codex implements every M3–M6 item**,
including WI-3.4 and WI-5.3b/c. opus-xhigh reviews only, one named deep
pass per merge. M5's deep-pass surface already covers key lifecycle, so
review coverage does not change.

| merge | merge owner (implementer) | specialist hand-off inside the merge | reviewer (every remaining merge) | deep pass (one, named surface) |
|---|---|---|---|---|
| M1 | opus-high (done) | — | opus-xhigh | — (spent) |
| M2 | grok (done) | — | opus-xhigh + codex | — (spent) |
| M3 | **codex** | — | **grok** (incl. codec round-trip tests) | **opus-xhigh**: §4.4.3 producer rules FIRST (`status-ok` two budgets vs the encoded line, prefix-only truncation, never-emit-over-64-KiB, streaming that never materializes an unbounded slice); security surface (forced-command pinning, `--as`/`--from`, `pool.id` J1) rides along. Not the codec round-trips. |
| M4 | **codex** | — | **grok** | **opus-xhigh**: §6.8 contract + resolver precedence |
| M5 | **codex** | — | **grok** (persona-walk §7 quickstarts and every §7.5 text) | **opus-xhigh**: §7.2 versioned keypairs, symlink-as-only-pointer, join/rotate/migrate lock, migration renaming directories out from under it |
| M6 | **codex** | — | **grok** (walks the matrix vs §10.3 row-for-row) | **opus-xhigh**: conformance **vectors only** — not CI wiring, not the sshd matrix |

**Integrator-owned items** (claude-code, not the merge owner): **WI-6.6**
only (WI-1.9 is spent). Lands inside the M6 PR because the tag's release job
hard-fails without the notes file.

claude-code is otherwise integrator ONLY — merges, tags, releases, fleet
cutover, cross-lane sync. Never implementation.

**opus-high** finished M1 and takes no further implementation. **grok**
reviews every remaining merge and authors prose/docs (M2 trial lifted the
read-only restriction for that class of work).

Cross-merge review obligation (B4/B5): the M5 gate ask must re-verify WI-4.3's
argv goldens at the M5 head SHA (see WI-4.3 and WI-5.3c).

---

## 5. Release / rollout checklist — v1.6.0 (after M6; the only tag)

The standalone post-M1 tag is cancelled. This is the **only** release
checklist. WI-6.6 lands in the same PR that satisfies these steps.

1. All L vectors, all W vectors (in-process **and** sshd-backed), and the full
   sshd matrix green on ubuntu + macos in CI — hard gate, no exceptions (§9.3
   timing rule).
2. **`docs/releases/v1.6.0.md` + the CHANGELOG entry present** (WI-6.6), with
   top matter **replaced** so the published notes do not disclaim the hub.
3. `tools/fact-sweep.sh` over docs + spec; the §7.1/§7.2 quickstarts walked
   verbatim once on a real second machine (grok persona-walk or operator).
   **Containment, mandatory (P5): that walkthrough runs against a SCRATCH POOL,
   or as a SECOND UNIX USER on the hub host — NEVER against this checkout's live
   loop directory.** The walkthrough issues `hub join`, `serve`, and
   `hub authorize`, all of which mutate pool and machine state; the destructive-
   test env-guard rule (§3.3) applies to a human walkthrough exactly as it
   applies to a test.
4. `gh pr checks` green on the M6 PR **before** SHIP (embedded-asset rule).
5. Verify the PR's `headRefOid` == the SHIP'd SHA; squash per convention
   (unstage loop state; no stray `.agentchute/` files in the squash).
6. Annotated tag via `-F <notes-file>`; push with the explicit
   `refs/tags/v1.6.0`.
7. The release pipeline is external: a `v*` tag → smoke-gated goreleaser (the
   stub-codex gotcha applies — check the smoke logs, not just the tag).
8. Rollout order: **hub upgrades first** (the handshake's own rule), then the
   remotes. Note in the release notes that a hub `agentchute update` fences every
   live lane once (§8 row 24 — default-on relaunch brings lanes back under the
   new binary automatically), and that a remote which updated ahead of its hub
   must WAIT rather than re-join (P8).
9. agentchute.dev auto-deploys from main (Cloudflare Pages) — confirm
   `hub.html` and the three edited navs render post-merge.
10. **Tag-time re-measure (same PR as WI-6.6).** Immediately before tagging,
    re-measure every published count in `docs/releases/v1.6.0.md` and
    `CHANGELOG.md` against the tree at the **exact head being tagged**, with
    the method stated in the note (`go test -count=1 -json ./...`, pass
    events, subtests included), and correct them in that PR. Also re-verify
    any other tag-relative phrasing ("reproducible on this tag", version
    interop claims, "nothing to do on any lane").
11. **Published spec/conformance URLs (#156 handoff; blocks release
    completion and rollout).** After the GitHub Release exists and before
    declaring the release complete:
    - Update every spec/conformance URL in current root pages (`web/*.html`)
      to the just-published release tag. Preserve dated `web/blog/` URLs at
      their contemporaneous release tags.
    - Reject any `main` spec or conformance target under `web/`.
    - Reject a current-page tag that differs from `gh release view --json
      tagName --jq .tagName`.
    - Validate every versioned spec fragment against the rendered Contents
      API HTML.

    Requires **`rg`** and **`gh`**. `test -n "$refs"` and
    `test -n "$anchors"` fail loudly on an empty set: if a future
    refactor removes every versioned reference, the check must fail
    rather than pass. Observed pass at the #156 handoff:
    **`latest=v1.5.7`, six current-root references, zero mismatches,
    five versioned spec fragments resolved in rendered Contents API HTML.**

    ```sh
    set -eu
    # Do not add `set -o pipefail`: the `rg | rg` pipelines legitimately
    # produce no matches on a tree with no versioned references, and
    # pipefail plus `set -e` would abort there instead of at the explicit
    # test -n / test -z guards.

    test -z "$(rg -n 'github\.com/agentchute/agentchute/blob/main/AGENTCHUTE\.md|raw\.githubusercontent\.com/agentchute/agentchute/main/AGENTCHUTE\.md|github\.com/agentchute/agentchute/tree/main/conformance' web --glob '*.html' || true)"

    latest=$(gh release view --repo agentchute/agentchute --json tagName --jq .tagName)
    refs=$(rg -o --no-filename 'https://[^" <]+' web/*.html |
      rg 'raw\.githubusercontent\.com/agentchute/agentchute/[^/]+/AGENTCHUTE\.md|github\.com/agentchute/agentchute/(blob/[^/]+/AGENTCHUTE\.md|tree/[^/]+/conformance)' || true)
    test -n "$refs"
    bad=$(printf '%s\n' "$refs" | grep -Fv "/$latest/" || true)
    test -z "$bad"

    anchors=$(rg -o --no-filename 'https://github\.com/agentchute/agentchute/blob/[^/]+/AGENTCHUTE\.md#[^" <]+' web --glob '*.html' || true)
    test -n "$anchors"
    printf '%s\n' "$anchors" |
    while IFS= read -r url; do
      tagged=${url#*blob/}
      tag=${tagged%%/*}
      anchor=${url##*#}
      gh api "repos/agentchute/agentchute/contents/AGENTCHUTE.md?ref=$tag" \
        -H 'Accept: application/vnd.github.html+json' |
        rg -Fq "href=\"#$anchor\"" || {
          printf 'broken spec anchor: %s\n' "$url" >&2
          exit 1
        }
    done
    ```

---

## 6. Risks & watch-items

1. **The sole `internal/loop` change (WI-3.4, §6.9).** Highest blast radius: it
   alters lease reclaim for LOCAL pools too. The ordering is spec-fixed
   (freshness refusal first — C8) and the comparison is **equality only**; any
   drift toward wall-clock ordering re-creates the bug B6 caught, where an NTP
   step steals a live lane's lease. **codex implements**; grok reviews;
   opus-xhigh deep-passes the named surface. The seven cases are
   non-negotiable — including the clock-step row, which the withdrawn
   rule failed.
2. **Guard-latch M4 routing.** `guard` resolves its id inline
   (`guard.go:175-189`) — if WI-4.8 misses it, the latch lives under `codex`
   while the lane acts as `codex-tiny` and the guard silently never denies. The
   child-env-contract row (M6) must assert a **DENY while latched**, not just
   resolution. Second edge: `guard` fails **open** on every resolution and
   Discover error (`guard.go:171-173,184-189,202-204`); the fix must not turn any
   of those into a closed failure.
3. **E1 latch arming.** The client emitter must arm on the FIRST message event
   BEFORE rendering it — an implementer following the "after the op returns"
   instinct reintroduces the round-4 blocker. The mid-stream disconnect row
   exists precisely for this.
4. **Send-ambiguity honesty.** The `E_SEND_UNKNOWN` window opens at the first
   byte handed to ssh stdin and needs the deliberate transport seam (WI-4.6) —
   do not approximate it with "after the response deadline". Never auto-replay;
   the retry command must use `--body-file` and the hub-dir spool (a state-tree
   spool self-refuses, B-M1).
5. **Resolver precedence traps (rounds 9/9b/10).** Three separately-shipped
   pieces must agree: env/flag candidates remap through `names`; the
   `launchedWrapper` fallback on the direct path; the shadowing refusal at join.
   The env-UNSET direct-launch row is the canary — it false-greens if anyone
   "helpfully" exports env in the test. New in this revision: `identity_cmd.go`
   gains a `loop.Discover` call it never had, so that call **must be non-fatal**
   or `agentchute identity` starts failing outside a pool, where it succeeds
   today.
6. **`pool.id` has no owner before M5 (P3).** No Go file, test, or tracked
   source mentions `pool.id` or `pool12` today. **M3 tests mint their own
   fixture** (WI-3.2's `writeFixturePoolID`, `O_EXCL`, 12 lowercase hex + LF);
   the **sole production writer is WI-5.4**; and the wipe-preservation change
   (WI-5.5) must land in the SAME MERGE as that writer — landing the writer
   without the preservation bricks every key binding on the next `--wipe-state`,
   because `wipeStateCategory:307` and `rescanWipeLeftovers:529` exempt exactly
   `setup.json` and nothing else today. J1 validation runs on BOTH consumers and
   on the H2 loser-re-read path.
7. **Conformance module boundary (B3/X3).** `conformance/` is a separate,
   dependency-free module that **cannot** import `internal/*`, and neither root
   `go test ./...` nor `tools/test.sh` enters it today. Any attempt to put an
   internal-driving adapter there fails to compile; the drivers go in
   `internal/spectest` and the vectors travel as shared JSON data. Both `go.mod`
   files must come out of every merge byte-identical.
8. **Conformance gating discipline.** The v1.6.0 tag waits for the sshd-backed W
   runs (M6), not just the in-process ones — the §9.3 timing rule exists because
   vectors-after-release was a review finding (C7). Note the split introduced by
   S7: hub-side halves are M3, client halves are M4, sshd re-runs are M6.
9. **The M4↔M5 interlock (B4/B5).** The launcher forwarding fix (WI-4.3) and the
   migration attribution predicate (WI-5.3c) are two halves of one contract in
   two different merges. Either alone is silently wrong: without the launcher
   fix every joined lane de-remotes with no error; without the remote-specific
   predicate every ordinary migration prints destructive pid-reuse advice about a
   live lane. Enforced by the cross-merge review obligation in §4.
10. **macOS runner quirks.** User-owned sshd on a high port (no Remote Login);
    APFS case-insensitivity is an **asset** for the pool-identity row (the
    case-alias spelling test REQUIRES the macOS runner and cannot pass on
    Linux); `os.TempDir()` on darwin is a long `/var/folders/…` path, which is
    exactly why WI-4.4's ControlPath fallback tries `/tmp` as a second
    candidate.
11. **ControlPath length — decided, not deferred (X5).**
    `~/.agentchute/hub/<hub-id>/mux/%C` can approach the ~104-byte `sun_path`
    limit for deep home directories. WI-4.4 pins the threshold (the shipped
    literal `100` from `config.go:170`, with a conservative 64-byte `%C`
    allowance), the owned-0700 temp fallback, and a mux-disabled third arm. The
    residual: no CI runner has a deep enough `$HOME` to trigger it naturally,
    which is precisely why the builder test **injects** the length rather than
    relying on the environment, and why the M6 mux-reuse row runs through both
    paths.
12. **Fleet self-hosting hazard.** This repo's own pool coordinates the
    implementation. Any rehearsal of join/serve/update flows happens in the sshd
    harness, a scratch pool, or as a second unix user on the hub host ONLY —
    including the human §5.2 step-3 walkthrough. The destructive-test env-guard
    rule (§3.3) is hard, and was learned the hard way.

---

## 7. Pins this plan added beyond DESIGN.md (all non-normative packaging)

- **Package/file names.** `internal/op` was design-fixed; this plan pins
  `internal/hubwire` (codec, handshake), `internal/hubclient` (config, ssh
  transport, channel, cache), `internal/cli/hub.go` / `hub_session.go` /
  `hub_join.go` / `hub_keys.go` / `hub_migrate.go` / `hub_authorize.go` /
  `hub_errors.go`, `internal/loop/remote.go`,
  `internal/loop/bootref_{linux,darwin,other}.go`, `internal/spectest/`, and
  top-level `integration/sshd/` (with an untagged `doc.go`) for the build-tagged
  matrix.
- **Seam shapes DESIGN left implicit** (all forced by the round-1 implementer
  walk): `op.NewChannel(cfg, ctx, ChannelOpts)` with
  `AcquireLease`/`Token`/`Register`/`Tick`/`ReleaseLease` (F2), and
  `ChannelOpts.Lease` as the local **adoption** arm (R1c) so
  `refuseLiveRunnerCollision` keeps its signature; `TickResp.Warnings []string`
  with `Tick` erroring only when fenced (F3); the three `Level:"info"`
  NoteEvents `op.Claim` emits at their production points, with `ClaimSummary`
  staying DESIGN §3.2's four counts (R2, replacing round 2's
  `Listed`/`Remaining`); `NoteEvent.Level` ∈ {`warn`,`info`} with pinned
  stdout/stderr routing (F4); the exact eight-member op sentinel set plus
  `op.CodeFor(error)` with an `E_HUB_IO` default arm, and the 9/8/9 code-set
  arithmetic with `E_POOL_MISMATCH` classified `both` (F6, R3); the
  `GateReq`/`GateResp`/`StatusResp`/`StatusAgent`/`RegistrationView`/
  `CleanOwedReq`/`CleanOwedResp`/`RegisterResp` shapes (F8, R4, R1b); the
  exported `op.SendTsMessageWithCommit` test seam (F1); the `HubConfig` struct
  and its read/write helper (S5).
- **The M1 test-edit exception list** — three files, three hunks — and the
  move-set grep rule that keeps it closed (R1); the aliases/adapters that keep
  every other moved identifier invisible (`gateStatus` alias,
  `performRegister` adapter, `printStatus` and `refuseLiveRunnerCollision`
  signature preservation).
- **Test placement.** Which §10.3 rows run in-process in M4/M5 (and re-run under
  sshd in M6) vs sshd-only — assigned per work item. Conformance L/W drivers in
  the ROOT module (`internal/spectest`) with vectors as shared JSON under
  `conformance/vectors/` (B3/X3); the hub-side/client-side W split across M3/M4
  (S7).
- **Scripts and CI.** `tools/sshd-test.sh` as a NEW script rather than a mode in
  `tools/test.sh` (P7/X7), with its exact content; the `hub-integration` job
  running only the tagged package in one step, while the wire/seam/spectest
  suites stay in the existing `go` job (F10); one line added to `tools/test.sh`
  for the conformance module.
- **The spec section number.** The normative hub spec lands as AGENTCHUTE.md
  §13 — a genuinely vacant number (the file runs §12 → §14) — unless length
  forces the `HUB.md`-by-reference option DESIGN §9.1 allows.
- **ControlPath-length contingency** (WI-4.4) — DESIGN §4.2 is silent; this plan
  pins the threshold, the fallback, and the mux-disabled third arm.
- **Doc/web artifacts** (WI-6.5) — DESIGN says "a README/docs section"; this
  plan names `README.md`, `docs/hub.md`, `web/hub.html`, the three nav edits, and
  the sitemap entry, because `web/` has no nav partial and no build step.
- **Release-notes items** (WI-1.9 spent; WI-6.6 is the single remaining
  item and **replaces** the top matter of `docs/releases/v1.6.0.md`) —
  `docs/releases/<tag>.md` is a hard release-job gate
  (`release.yaml:110,168`), so the notes are work items, not checklist
  prose.

Corrections this revision made to round-1 anchors, all re-verified at `1244ae4`:
`check.go` "(inbox empty)" is `:201` (not 198), reached-limit `:207` (not 210),
the CLAIMED note `:262` (not 265), `displayConsumed` `312-334` with the
still-CLI-side renderers at `338-408` (not "335-408"); `performRegister` is
`register.go:72-98` (not 35-189); `resolveAgentID` is `identity.go:13-25` and
`resolveAgentIDRaw` `27-39` (not 13-38); `resolveAgentVendor` is
`identity.go:72-83` (not 57-82); `wrapperForToken` is `shims.go:51-64` (not in
`dispatch.go`); `commandHandlers` is populated at `dispatch.go:22-46` (not
23-45); `rescanWipeLeftovers` is `setup_wipe.go:516-536` (not 519-536);
`pointer.go`'s functions are `58-103` / `113-122` (not 93 / 105-118) and the
directory check lives in `config.go:359-375`; `discoverControlRepo` has FOUR
arms (not three); `serve.go`'s vendor block is `152-157` (not 152-158); the two
not-registered texts differ by their first word and are NOT byte-identical.

Round-3 corrections, re-verified at `1244ae4`: §10.3 has **three** boot rows,
not four (`hub-reboot pid reuse`, `clock step does not steal a live lease`,
``boot_ref` survives the heartbeat`); `op.CodeFor` outputs **9** codes and
`internal/hubwire` adds **8**, not 8 + 9; there are **12** `resolveAgentID`
call sites plus one inline resolution in `guard.go`; `cleanOwedResult` is
`clean.go:99-103` with `Pruned []string`, not a count; `loop.Registration`
(`registration.go:58-68`) carries **no JSON tags**.

**The round-1 "FLAGGED, not fixed" list is obsolete.** All six DESIGN defects
it recorded are fixed in DESIGN as it stands — cited by stable NAME, because
DESIGN co-evolves and a line number that has drifted is worse than no anchor
at all (T6):

| defect | where it is fixed in DESIGN |
|---|---|
| `tick-ok.warnings` | §4.4.1's `tick-ok` example (clean and step-failure shapes) + the "`warnings` … is **always present**" paragraph under it |
| both not-registered texts + `status.go:62` | §4.4.2's `E_NOT_REGISTERED` registry row + its §7.5 catalog entry |
| ControlPath length rule | §4.2's "**`ControlPath` length rule (normative)**" block + §8's row 25 + §10.3's `ControlPath length rule (§4.2)` row |
| legacy-shim launcher row | §6.8 rule 5 (the `cmdShimsExec` half) + §10.3's `launcher preserves remoteness (§6.8 rule 5, B4)` row |
| mixed-version parse-vs-persist | §6.9 + the second half of §10.3's ``boot_ref` survives the heartbeat` row |
| the three latch arming sites | §3.2's E1 bullet + §6.6's remote arming point + §10.3's `mid-stream disconnect arms the latch (E1)` row |

Do not re-amend them. The one
DESIGN gap this round flagged — recorded in
[`plan-reviews-r2.md`](plan-reviews-r2.md) §"Revision log (plan round 3)":
§3.5's `RegisterResp` Output line was a strict subset of the fields the CLI
renders today — is now **closed in DESIGN**, together with the defect that
survived it: the announce fan-out is `Announce *AnnounceView{Sent, Total int;
Warnings []string}`, not `AnnouncedTo int`, and the redundant `Created bool` is
gone (`Created == !ExistingFound` in every response). DESIGN §3.5/§4.4.1/§4.4.3
and WI-1.6 state one schema; do not re-amend either alone.
