# Plan review round 3 — PLAN.md @ e13f901b / DESIGN.md @ 34da31bd (2026-08-15)

grok: **SHIP** (round 2). opus-xhigh: **SHIP** — all findings resolved; states it
can execute its assigned items without asking a question. opus-high: **FIX** (1
real + 1 nit). codex: **FIX** (4). Both opus lanes and codex independently
recommend the same treatment for the DESIGN line-number drift (T1 below).

Everything else in the round-3 delta was verified correct by at least two lanes,
including all three follow-on defects and R1's two extra move-set cases. Do not
revisit those.

## Blocking

- **T1 (opus-high N1 + codex #1) — the status shape is wrong in two ways, and
  they interact. Fix them together.**
  - (a) **Local-mode silent truncation.** The 64-entry cap on
    `StatusResp.Agents` sits INSIDE the op (PLAN:418-426), and M1 wires the
    local renderer as op → rebuild map → `printStatus` unchanged
    (PLAN:459-461). `printStatus` (`status.go:97`, 4 test call sites) renders no
    truncation notice, so a pool with >64 rows silently drops rows locally — a
    behavior change inside the merge whose only acceptance criterion is that it
    has none, and whose release notes say "no behavior change" (WI-1.9(d)).
    The stated justification (uniformity per §4.4.3) does not hold: §4.4.3 is a
    FRAMING rule downstream of the 64 KiB line cap, and there is no line in
    local mode; local/remote already diverge here unavoidably, which is exactly
    what `truncated:true` admits. **Fix: move the cap one layer out** — the op
    returns every row and always reports `Truncated:false`; the hub dispatcher
    applies the 64-entry cap and sets `truncated:true` when writing `status-ok`
    (WI-3.3a dispatch, shape in WI-3.1a). Sorting stays in the op so truncation
    stays deterministic. Delete WI-1.4(d)'s "the cap applies in LOCAL mode too"
    paragraph.
  - (b) **The row itself is unbounded on the wire.** `StatusAgent.Reg` is the
    full eight-field `RegistrationView` including `Body` and `WorkingRepos`
    (PLAN:417-455), DESIGN §4.4.1's "carry/return the §3 request/response
    structs … verbatim" clause says status returns the seam struct as-is,
    yet §4.4.3's `register-ok`/`status-ok` body bullet says status rows carry
    no body — and a body may be
    1 MiB (`registration.go:18`), which cannot ride a 64 KiB control line.
    **Fix: define a status-specific row view containing only the fields status
    actually renders** (check `printStatus` for the real set), and make DESIGN
    and PLAN agree.
  - (c) **`StatusResp.Warnings` is unbounded too** — `ReadRegistrationsLenient`
    appends one error per malformed `*.md` (`registration.go:522-549`), and the
    64-agent cap does not bound it. **Fix: stream status warnings as `note`
    frames** (consistent with R2's note routing), or specify an explicit
    deterministic cap + truncation contract.
- **T2 (codex #2) — the signature-change inventory omits direct TEST callers.**
  WI-4.8 (PLAN:1738-1753) claims every `resolveAgentID` caller is in scope, but
  `internal/cli/identity_test.go:19,25,33,44,55` calls it directly; the
  specified signature change leaves those five sites uncompilable. **Fix:** add
  `identity_test.go` + its five call updates to WI-4.8's file list and
  acceptance (or preserve the old signature behind a cfg-aware helper). **Also
  widen R1's grep rule**: it currently covers identifiers the move set REMOVES;
  it must equally cover identifiers whose SIGNATURE changes. Re-run that widened
  grep across `internal/cli/*_test.go` for every such identifier in the plan and
  close whatever else it catches.
- **T3 (codex #3) — `RegisterReq` cannot express boot-only sweep semantics.**
  The shipped code runs `SweepStaleRegistrations` only in boot, after
  registration (`boot.go:94-101`), but DESIGN §3.5's **Wraps** bullet and its
  `Warnings` bullet have the
  shared register op wrap that sweep and return its warnings, while
  PLAN:581-605 gives the request no discriminator. Always sweeping changes
  register/self-check/serve behavior; never sweeping loses remote boot's
  hub-side trigger. **Fix:** add `Sweep bool` set only by `cmdBoot`, executed
  hub-side when true, with local AND remote warning propagation covered and no
  double-sweep.
- **T4 (codex #4) — remote vendor routing can render an empty vendor on
  success.** Remote register callers correctly send `Vendor:nil` for hub-side
  resolution (PLAN:596-600, DESIGN §3.5's `Vendor` bullet — its "Client call
  sites, normative (S2)" paragraph), but the shipped renderers print
  from the pre-wire `opts.Vendor` (`register.go:268-270`, `boot.go:134-143`,
  `self_check.go:76-81`), and WI-4.5 plus the S2 row only assert nil-on-wire and
  success (PLAN:1568-1617, 2268-2275) — never that callers render from the
  response. **Fix:** each remote result path renders the resolved vendor from
  `RegisterResp.Reg.Vendor`; assert text AND JSON output for a bare custom-id
  `register`, `boot`, and `self-check`.

## Non-blocking

- **T5 (opus-high N2)** WI-1.7(d)'s parenthetical cites `run_test.go:147,164,
  167,172` for the lease values passed to `RenewLease`/`ReleaseLease`; actual is
  `:137,:147` (Release), `:163` (Renew), `:166,:174` (Release). Load-bearing
  anchors in the same item are correct.
- **T6 (all three lanes agree) — DELETE the `DESIGN.md:NNN` line suffixes; do
  NOT regenerate them.** Section references are stable and were verified correct
  throughout; line numbers point into a co-evolving document and are already
  wrong — opus-xhigh followed three citations in the closing block and each
  landed in a DIFFERENT section (`:756/:759/:767` → §4.3's version bullet;
  `:791` → §4.4's header; `:2654-2655` → §9.1's amendment bullet), which reads
  as support for a claim the target does not make. A wrong anchor is worse than
  no anchor. Keep CODE anchors with line numbers (frozen at 1244ae4, verified
  accurate — T5 is the only slip in three rounds). Where a DESIGN line number is
  currently load-bearing, replace it with the stable row/section NAME (e.g. "the
  boot_ref survives the heartbeat row", "the mixed-version parse-vs-persist
  row"), which is greppable.
- **T7 (opus-xhigh)** `plan-reviews-r2.md`'s "New this round" item 1 still says
  DESIGN §3.5's Output "is a strict subset … flagged, not edited" — no longer
  true (DESIGN §3.5's **Output** line carries the full set incl.
  `Announce *AnnounceView`;
  §4.4.1 shows the register-ok frame with the trailer; §4.4.3 states the
  reg.body trailer rule). Strike that clause and the §4.4.3 open question, both
  answered. PLAN's own closing block already says so correctly.

## Revision log (plan round 4)

Applied to PLAN.md; DESIGN.md touched for T1, T3, T4 (T6 required no DESIGN
edit — the drifted anchors are all in PLAN). Every code anchor below was
re-opened at `1244ae4` before it was used. Nothing was rejected; two sub-parts
went further than the finding asked, both flagged below.

- **T1 — FIXED, all three parts, one coupled change.** PLAN WI-1.4(b)(c)(d)(e),
  WI-3.1a(d)(e), WI-3.3a(d), WI-4.5; DESIGN §3.5's `RegistrationView` bullet,
  §3.6's Status bullet, §4.4.1, §4.4.3. Both documents now state the identical
  shapes.
  - **(a)** The cap left the op. `StatusResp{Agents, Truncated, Now}` — the op
    returns every row, sorted by `loop.RegistrationsByAgentID`
    (`status.go:111 @ 1244ae4`), and always reports `Truncated:false`; the
    `status-ok` **producer** (WI-3.1a) writes at most 64 rows and is the only
    writer of `truncated:true`, applied on the WI-3.3a dispatch path.
    WI-1.4(d)'s "the cap applies in LOCAL mode too" paragraph is deleted and
    replaced by the argument for its removal. New test rows on both sides: a
    >64-row pool must render every row locally (WI-1.4(e)); 64-in/65-in
    framing rows (WI-3.1a(e)).
  - **(b)** `StatusAgent` is now the status-specific view itself — flat, with
    no nested `Reg`: `{AgentID, LastSeen, Host, ProtocolVersion, InboxDepth,
    Status}`, JSON `agent_id`/`last_seen`/`host`/`v`/`inbox_depth`/`status`.
    The set is `printStatus`'s exact render surface, verified column by column
    against `internal/cli/status.go:97-139,151-163,242-247 @ 1244ae4` (AGENT,
    STATUS, INBOX, LAST_SEEN **and** AGE — both from `LastSeen` — HOST, PROTO,
    plus the `PROTOCOL WARNINGS:` block, which needs only `AgentID` +
    `ProtocolVersion`). `Vendor`/`ControlRepo`/`WorkingRepos`/`Body` are gone:
    the header's `vendor:` line is `cfg.Vendor` (`status.go:100`), not a row's.
    **Beyond the finding:** `Claim *StatusClaim` is cut too. No renderer reads
    it — `registrationStatusLabel` consumes the claim hub-side
    (`status.go:152-154`) and the only surviving fact is the `Status` label —
    and the finding's own criterion is "only the fields status actually
    renders"; on a row that is pool-visible to every member, that is the same
    rule that already excludes `ServeClaim.ServeToken` (`lease.go:59`). Cut,
    with an explicit "re-add it in the merge that ships a renderer for it".
    `RegistrationView` (still eight fields) moves to WI-1.6(d), its only
    remaining user, and the four shared JSON tags stay spelled identically.
  - **(c)** Note frames, not a cap. The lenient-read warnings stream as
    `NoteEvent{Level:"warn"}`, so `op.Status` gains the emitter parameter
    `op.Claim`/`op.Pending` already have, and `StatusResp` has no `Warnings`
    field. Chosen over a cap-and-truncate contract because the list is
    genuinely unbounded and NOT bounded by the pool-scale assumption — it is
    one error per malformed `*.md` under the agents dir
    (`registration.go:522-549 @ 1244ae4`), and a malformed file is not a
    registration — which is precisely §4.4.3's "unbounded lists never ride
    inside one control frame"; because the pinned `warn`→stderr routing
    already renders exactly today's `warning: %s` (`status.go:67-70`); and
    because a note necessarily precedes the terminal frame, so the warnings
    keep their position AHEAD of the table by construction, in both modes. A
    cap would have needed a second truncation flag and would have restored the
    silent-corrupt-row failure R4 opened.
- **T2 — FIXED**, WI-4.8(c)/(f) + the M1 preamble's move-set rule.
  `identity_test.go` is named in the file list with its five call sites
  (`:19`, `:25`, `:33`, `:44`, `:55`), plus the constraint those sites impose:
  a nil/pool-less cfg must resolve the candidate verbatim, so the traversal,
  flag-beats-env, env-fallback and `missingAgentIdentityHint` assertions keep
  their exact texts. The cfg-aware-helper alternative is rejected in-line: it
  leaves two spellings of the identity choke point this item exists to unify.
  The **rule itself is widened** to two populations — identifiers an item
  REMOVES from `cli`, and identifiers it RE-SIGNATURES — and unscoped from M1
  to every merge, which is where round 3's version failed: `resolveAgentID`
  changes signature in M4, a merge the M1-only rule never reached. M1's own
  exception table stays closed at three files / three hunks.
  **Re-ran the widened grep** over `internal/cli/*_test.go` at `1244ae4` for
  every identifier the plan moves or re-signatures (`evaluateGate`,
  `finishGateClear`, `gateStatus`, `archiveAllClaimed`, `performRegister`,
  `publishRegistrationOnce`, `registrationLiveElsewhere`, `hasPendingInboxMail`,
  `sendTsMessageWithCommit`, `displayConsumed`, `printStatus`,
  `registrationStatusLabel`, `countInbox`, `refuseLiveRunnerCollision`,
  `resolveAgentID`, `resolveAgentIDRaw`, `resolveAgentVendor`,
  `readRegistrations`, `selfRepairRegistration`, `registerOpts`,
  `registerResult`, `cleanOwedResult`, `regTemplate`, `lastSweep`,
  `heartbeatTemplate`). It closes with **no sixth case**: `resolveAgentID`'s
  five sites are the only new code hits. `publishRegistrationOnce`
  (`clean_test.go:476`), `hasPendingInboxMail` (`run_test.go:595,845`),
  `displayConsumed` (`check_age_owed_test.go:241`), `performRegister`
  (`hooks_sessionstart_test.go:11`, `turn_end_test.go:512`,
  `register_test.go:32,52`) and `resolveAgentVendor`
  (`turn_end_test.go:507,511,538`) are comment-only; `heartbeatTemplate`
  (`run_test.go:505`) sits inside the already-named `newPollTestRuntime` hunk;
  `evaluateGate`, `finishGateClear`, `archiveAllClaimed`,
  `registrationLiveElsewhere`, `registrationStatusLabel`, `countInbox`,
  `resolveAgentIDRaw`, `registerResult`, `selfRepairRegistration` and
  `readRegistrations` have zero hits. One knock-on recorded from this sweep:
  `readRegistrations` (`status.go:84-95`) is the one identifier WI-1.4 removes
  that its (c) never named — now named, with its clean grep stated.
- **T3 — FIXED**, PLAN WI-1.6(d)/(e) + WI-4.5 + WI-5.7(e); DESIGN §3.5's
  `Sweep` bullet, its **Wraps** bullet, its `Warnings` bullet, and §10.3's
  register-field-semantics row. `RegisterReq` gains `Sweep bool`.
  **Verified against the shipped boot path first**: `cmdBoot` calls
  `performRegister` (`boot.go:89 @ 1244ae4`) and only then
  `loop.SweepStaleRegistrations(cfg, agentID, now)` (`boot.go:99-101`) — C11's
  register-self-first-then-sweep order — appending
  `sweep stale registrations: %v` to `result.Warnings` on failure and never
  failing boot. The only other production caller is the runner's throttled
  tick (`serve.go:640`), which is `op.Tick`'s and untouched. So: `Sweep:true`
  from `cmdBoot` alone, executed hub-side AFTER the write, warning text
  pinned verbatim (no `agentchute serve: ` prefix — that one belongs to the
  tick). **Local arm**: `Sweep:false`, `cmdBoot` keeps its own call, M1's
  adapter cannot even express `true` (`registerOpts` has no such field), so
  M1 has no `true` caller and nothing local changes. **Remote arm** (WI-4.5):
  `Sweep:true` **and** the local call is skipped — it would otherwise sweep
  the mail-free shadow while the hub's pool went unswept. **No double-sweep**
  anywhere: the channel's startup `register` sends `false` because that lane's
  first tick sweeps immediately; `register`/`self-check`/`turn-end` send
  `false` in both modes. Test rows assert the hub row is gone, the shadow row
  is untouched (which is what proves "skipped", not merely "duplicated"), and
  the failure warning renders.
- **T4 — FIXED**, PLAN WI-4.5 (new "renders the RESOLVED vendor" block + two
  test rows), WI-5.7(e), DESIGN §10.3's S2 row. All three renderers switch
  from the pre-wire `opts.Vendor` to `result.Reg.Vendor`: `cmdRegister`
  (`register.go:269 @ 1244ae4`), `cmdBoot` (`boot.go:136`, which feeds
  `bootStatus.Vendor` and therefore all four boot emitters) and `cmdSelfCheck`
  (`self_check.go:78`). Stated as ONE path, not a mode branch, because the two
  values are provably equal locally: `publishRegistrationOnce` sets
  `Vendor: opts.Vendor` unconditionally (`register.go:133`) and the merge arm
  touches only `WorkingRepos` and `Body` (`register.go:140-149`) — so no local
  test output moves. Output assertions are now required and are specific about
  what exists: `register` is **text-only** (it has no `--json` flag,
  `register.go:198-211`), while `boot` and `self-check` assert **text AND
  `--json`** (`"vendor":"<resolved>"`), each byte-identical to the same pool
  driven locally.
- **T5 — FIXED**, WI-1.7(d). The anchors are now `loop.ReleaseLease` at
  `run_test.go:147`, `loop.RenewLease` at `:163`, `loop.ReleaseLease` at
  `:166` and `:174`. One refinement on the finding itself: its fifth anchor,
  `:137`, releases a lease `refuseLiveRunnerCollision` never returned — it is
  `loop.AcquireServeLease`'s own from `:126` — so it is cited as such rather
  than folded into the helper's four.
- **T6 — FIXED**, seven `DESIGN.md:NNN` citations removed from PLAN.md,
  swept one at a time with each target re-opened, none regenerated. Section
  refs (`§x.y`) and CODE anchors keep their line numbers. Where the number was
  load-bearing it became a stable NAME: the `E_NOT_REGISTERED` registry row +
  §7.5 catalog entry (WI-1.1); §4.4.1's `tick-ok` example + its "always
  present" paragraph (WI-1.7); §4.3's "Hub→client `note` frames" bullet
  (WI-1.1); §10.3's three boot rows by title, `hub-reboot pid reuse` /
  `clock step does not steal a live lease` / `` `boot_ref` survives the
  heartbeat `` (WI-3.4, §7); §10.3's `register field semantics (C2/D1)` row
  (WI-4.5); and §7's obsolete-list block, now a six-row table of names
  (`ControlPath length rule` → §4.2's normative block + §8 row 25 + its §10.3
  row; the legacy-shim launcher → §6.8 rule 5 + `launcher preserves
  remoteness`; mixed-version parse-vs-persist → the second half of the
  `boot_ref` row; the three latch sites → §3.2's E1 bullet + §6.6 + `mid-stream
  disconnect arms the latch (E1)`). The five `DESIGN:NNN` refs inside this
  file's own findings were converted the same way, each verified first. Two
  replacements re-read to confirm they land where the sentence claims: §4.3's
  note bullet does carry the routing rule (it now carries BOTH arms, see the
  knock-on below), and §4.4.2's `E_NOT_REGISTERED` row does name all three CLI
  sites including `status.go:62`. **PLAN now contains zero `DESIGN.md:NNN`
  suffixes** (`grep -E 'DESIGN[^ ]*\.md:[0-9]|DESIGN:[0-9]'` → no match).
- **T7 — FIXED**, `plan-reviews-r2.md` "New this round" item 1. Both clauses
  are struck (struck through, not deleted, so the record still reads as a
  record) with the reason: DESIGN §3.5's Output line now carries the full set
  including `Announce *AnnounceView`, §4.4.1 shows the `register-ok` frame,
  and §4.4.3 answers the `reg.body` question — trailer, never the control
  line.

**Knock-ons this revision had to fix, and two flags for the reviewers:**

1. **`RegistrationView` was homeless** once T1b took it off the status row —
   WI-1.6 pointed at "WI-1.4's `RegistrationView`". Its definition (eight
   fields, frontmatter tags, the no-JSON-tags rationale) moved into WI-1.6(d),
   its only remaining user; WI-1.4 now points forward to it for the four tag
   names `StatusAgent` shares. Side effect worth noting: WI-1.6 no longer
   depends on a type WI-1.4 declares.
2. **A stale claim about DESIGN §4.3's note bullet.** WI-1.1(d) and WI-2.1(d)
   both said the bullet "illustrates only the `warn` arm". DESIGN has since
   been amended — it carries both example frames, the "the level IS the
   stream" rule, and the three `check` `info` lines — so both sentences were
   corrected while T6 was rewriting that citation. Round 3's knock-on note 3
   in `plan-reviews-r2.md` is the stale source; PLAN no longer repeats it.
3. **FLAG — `plan-reviews-r2.md`'s knock-on item 4 is now superseded** ("The
   64-entry `status` cap applies locally too"). T1a reverses exactly that
   decision. This round's write scope for that file was T7 only, so it is left
   standing and recorded here instead; the binding text is PLAN WI-1.4(d),
   which now says the opposite and says why.
4. **FLAG — the same stale lease anchors T5 corrects also appear in
   `plan-reviews-r2.md`'s R1 revision-log entry** (`:147,164,167,172`). Same
   scope limit, same treatment: PLAN is corrected, the record is not.

## Revision log (plan round 5)

Four findings, all applied; nothing rejected. R5-1/R5-2 were the round-5
review proper; R5-3/R5-4 are the two narrow follow-ups from the fourth
reviewer lane against the revised text. Every code anchor below was
re-opened at `1244ae4` before it was used. PLAN.md and DESIGN.md state the
identical `status-ok` rule, the identical fixture rule, and the identical
output literals (the seam structs are the wire schema, so a divergence would
be a real defect).

- **R5-1 (the blocker) — a 64-ROW cap does not bound a 64 KiB LINE. FIXED**,
  as specified: the producer now enforces BOTH limits against the **encoded**
  response. DESIGN §3.6's Status bullet, §4.4.1's exception 2, §4.4.3's
  `register-ok`/`status-ok` body bullet and its `status-ok` bullet, §4.4.3's
  closing normative bullet, §10.2; PLAN WI-1.4(d)(e), WI-3.1a(d)(e),
  WI-3.3a(d), WI-4.5.
  - **The rule.** Rows are appended from the op's sorted slice **in order**,
    kept only while (1) the complete encoded control line stays ≤ 64 KiB and
    (2) the kept-row count stays ≤ 64. The first row failing either check ends
    the append — prefix semantics, never skip-and-continue, which would omit a
    middle row while showing later ones with no way to say which.
    `"truncated":true` is written whenever **either** budget excluded a row; it
    is no longer a row-cap flag.
  - **Non-row fields COUNT against the byte budget** — decided and stated in
    both documents. The budget is measured over the whole line: `t`, `re`,
    `now`, `truncated`, the `agents` array's punctuation, and the terminating
    LF. A rows-only budget would balance its own books and still emit an
    over-cap line, which is the exact defect the rule exists to close; the
    cost is one integer. Corollary pinned so the implementer needs no second
    pass: measure with `"truncated":false` encoded — `true` is one byte
    shorter, so a frame that fit before the flip still fits after it.
  - **Degenerate case pinned**: if the FIRST row alone does not fit,
    `agents:[]` with `truncated:true`. A valid response, not an error —
    `status` is read-only and pool-wide, and refusing the listing over one
    pathological row would lose every other agent's row.
  - **The false claim is corrected.** DESIGN §4.4.3 said the status row "holds
    six fields, none of them free-form". `Host` is free-form and
    length-unbounded: `ReadRegistration` copies frontmatter `host` through
    verbatim under only the 1 MiB whole-file cap
    (`internal/loop/registration.go:72,88 @ 1244ae4`), `Registration.Validate`
    imposes no length bound (`registration.go:217-241 @ 1244ae4`), and
    `register --host` is an unconstrained string flag
    (`internal/cli/register.go:206 @ 1244ae4`). The bullet now says dropping
    `Body` removes the 1 MiB field but does **not** make the row a bounded
    shape. **Beyond the finding**: `AgentID` is length-unbounded too
    (`agentIDPattern = [a-z0-9][a-z0-9-]*`, `internal/loop/inbox.go:40 @
    1244ae4`) — only the filesystem's `NAME_MAX` on `<id>.md` bounds it — so
    the rule is written as "depend on NO field being bounded" rather than
    "bound `Host`".
  - **Streaming rejected, with the justification the finding asked for.** The
    budget preserves the required behavior: status is a display table with a
    truncation notice, no per-row delivery guarantee exists, and the only row
    the byte budget drops that the row cap would not is one that cannot be
    rendered into a table cell anyway (a 70 KiB HOST column). Streaming would
    add a frame type, a second terminal-count contract, and a renderer that
    must buffer the whole listing regardless to compute tabwriter column
    widths — so unlike `msg`/`owed-item` it buys no memory bound here. Recorded
    in DESIGN §4.4.3 as a rejected alternative, together with the real root
    fix (a length bound on `host` at the registration layer), which is
    shipped-code scope, deliberately out of this proposal, and which the hub
    must not depend on landing.
  - **Test rows added.** WI-3.1a(e) / DESIGN §10.2: the row cap (64 in ⇒ 64
    out `truncated:false`; 65 in ⇒ first 64 `truncated:true`); **one valid
    registration whose `host` alone exceeds 64 KiB in a THREE-row pool** ⇒ that
    row excluded, `truncated:true`, emitted line ≤ 64 KiB measured whole
    (fails a rows-only producer); the same oversized row placed FIRST ⇒
    `agents:[]` and every later row dropped, placed LAST ⇒ earlier rows survive
    (fails a skip-and-continue producer); a byte-exact boundary pair asserted
    on the ENCODED length including the LF; and a flag-flip row proving the
    measure-with-`false` rule needs no second pass. WI-1.4(e) gains the LOCAL
    mirror: the same 64 KiB-`host` registration must come back from `op.Status`
    intact with `Truncated:false` and must still print locally — the budget is
    framing-only and must not leak into the op.
  - **Notice text.** WI-4.5's trailing truncation line now names the WIRE limit
    as well as the row cap: on a three-agent pool the binding budget is the
    line, and a notice saying only "64 rows" would state something visibly
    false. It deliberately states no total — `status-ok` carries only the kept
    rows plus the flag, and a `total` field is a shape change no finding asked
    for. (Its exact bytes were pinned in the R5-3 follow-up below.)
- **R5-2 (non-blocking) — the status HEADER is local config, not row data.
  FIXED by deciding it**, at the item that owns the renderer switch: **PLAN
  WI-4.5** (new "The status HEADER is a decision this item owns" block, a test
  row in (e), and a clause in (f)), mirrored in DESIGN §3.6's Status bullet as
  a new header sub-bullet, plus a §10.3 row. Verified first that the header
  really is config-sourced (`internal/cli/status.go:98-100 @ 1244ae4`).
  **Decision: a remote lane prints the HUB's pointer, with the shadow marked
  rather than hidden.**
  - `control_repo:` ⇒ the canonical `ssh://` URL from `cfg.Remote`, never
    `cfg.ControlRepo`, which under §6.8 rule 2 is only the nearest local
    ancestor holding `AGENTCHUTE.md`. The rows below are the hub's; a header
    naming a local directory holding none of them is §6.8 rule 5's failure one
    command later. `formatOriginSuffix` (`status.go:210-215`) is kept and stays
    truthful.
  - `loop_dir:` ⇒ the local shadow, plus one parenthetical marker line in the
    style of the existing `  (shadowed pointer: %s)` line
    (`status.go:101-103`). It genuinely is this process's loop dir (guard
    latch, `runner.json`, send spool), and the hub's loop dir is on another
    filesystem and rides on **no** frame — `hello-ok` carries `pool`, a pool
    path, not a loop dir — so printing it would mean a new wire field for a
    path the operator cannot open. The defect is the shadow printed UNMARKED;
    the marker is the whole fix.
  - `vendor:` ⇒ unchanged, and it needs no branch: `cfg.Vendor` is the loop
    dotdir namespace, vendor namespacing is gone (`fixedNamespace`,
    `internal/loop/config.go:19-25,348-357 @ 1244ae4`), and §6.8 rule 3
    deliberately gives the shadow the same `.agentchute/loop` shape — so both
    modes print the same string. Explicitly do NOT retarget it at the actor's
    registered vendor: `StatusAgent` has no `Vendor` (T1b cut it) and re-adding
    one to feed a header line reopens a settled shape.
  - `ShadowedPointers` ⇒ unchanged; a local-pointer diagnostic in both modes.
  - No new wire field and no signature change: the branch keys on
    `cfg.Remote != nil`, already in hand via the renderer's own `cfg`.
  - **Test row** (WI-4.5(e), §10.3): a three-agent hub pool with the client
    clock skewed and an empty shadow — STATUS/INBOX/AGE come from the response
    byte-identically to a pinned-clock local run; `control_repo:` is the
    `ssh://` URL with its `(via …)` suffix; `loop_dir:` is the shadow WITH its
    marker; `vendor:` matches the local run; warnings land on stderr ahead of
    the table. Then plant the 64 KiB-`host` row and re-run: the other two
    agents still render and the notice names the wire limit — a notice naming
    only "64 rows" fails the row on a three-agent pool. (The row now pins
    WHERE that planted row sorts, and asserts both output lines byte-exact —
    R5-3/R5-4 below.)
- **R5-3 — the integration fixture contradicted PREFIX semantics. FIXED by
  making it deterministic.** The row said a three-agent pool with one
  64 KiB-`host` agent leaves "the other two agents still render", which holds
  only if the oversized row sorts LAST: the producer stops at the FIRST row
  that fails either budget and drops every row behind it, and the dedicated
  producer-level row asserts exactly that (oversized row FIRST ⇒ `agents:[]`,
  both later rows dropped). **Decision: pin the fixture, not the
  expectation** — the oversized `host` belongs to the lexicographically LAST
  of the three agent ids (`alpha` / `bravo` / `zulu`, huge host on `zulu`).
  Weakening the expectation to "only the rows preceding it render" would have
  been true but would have left the more valuable assertion (that a
  pathological row costs the operator nothing but its own row) untested, and
  the producer suite already covers the first-row case. Applied identically in
  PLAN WI-4.5(e) and WI-5.7(e), and DESIGN §10.3's `remote status renders the
  HUB` row. The §10.2 / WI-3.1a(e) producer rows already stated FIRST and LAST
  explicitly and needed no change.
- **R5-4 — two new user-visible lines had no literal. FIXED by pinning both
  in BOTH documents**, with the test rows now asserting the bytes. Each was
  modelled on shipped output re-opened at `1244ae4`, not invented:
  - **Truncation notice** — PLAN WI-4.5(d), DESIGN §4.4.3 (with a pointer from
    §3.6):

    `note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.`

    One unindented stdout line, emitted only when `truncated` is true, LAST in
    the output (after the table and any `PROTOCOL WARNINGS:` block), preceded
    by one blank line. Modelled on `internal/cli/check.go:262 @ 1244ae4`
    (lowercase `note: ` prefix, no indent, one sentence, semicolon before the
    consequence). It names both budgets, says rows were withheld, states the
    prefix rule, and still states no total.
  - **`loop_dir:` shadow marker** — PLAN WI-4.5(d), DESIGN §3.6:
    `  (local shadow: this process's own loop dir, not the hub's)`, two
    leading spaces, printed immediately under `loop_dir:` and ahead of
    `vendor:`, only when `cfg.Remote != nil`. Modelled on the marker already
    in that header block, `  (shadowed pointer: %s)`
    (`internal/cli/status.go:101-103 @ 1244ae4`), for indent and parenthesised
    `label: value` form, and on `  (pull-only: senders deliver to your inbox;
    you poll it yourself)` (`internal/cli/boot.go:204 @ 1244ae4`) for a label
    followed by prose rather than by a path.

**Noticed, not changed (for the reviewers):**

1. **`host` has no length bound in shipped code** — `Registration.Validate`
   (`registration.go:217-241 @ 1244ae4`) bounds `agent_id` (charset only),
   `vendor`, `control_repo`, `working_repos` and `last_seen`, and says nothing
   about `host`; `register --host` is a free string. That is the root cause of
   R5-1, and it is shipped-code scope, outside this proposal's write scope. The
   hub's rule is written so it does not depend on that ever being fixed.
2. **`status-ok` carries no total**, so a truncated remote listing cannot say
   "showing N of M". Deliberate (no finding asked for the field); recorded here
   because it is the visible cost of the chosen notice text.
3. **`status`'s local table has no truncation notice at all**, by design (T1a):
   local mode never truncates. If a later merge ever adds a local cap, that
   notice becomes a prerequisite, not a follow-up.
4. **The producer-level "wire budget, the row the row-cap misses" row
   (WI-3.1a(e) / §10.2) still does not pin its oversized row's sort
   position**, and was deliberately left alone: unlike the integration
   fixture, every assertion it makes (that row excluded, `truncated:true`,
   emitted line ≤ 64 KiB measured whole) holds in every position, and the two
   rows beside it pin FIRST and LAST explicitly. An implementer who wants one
   less degree of freedom may reuse R5-3's `alpha`/`bravo`/`zulu` fixture
   there too.
