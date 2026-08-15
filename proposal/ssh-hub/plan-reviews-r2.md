# Plan review round 2 — PLAN.md @ 027f18cc / DESIGN.md @ 4600cd7c (2026-08-15)

grok: **SHIP** (1 nit). opus-xhigh: **FIX, narrow** (4, one blocking) — all 19 of
its round-1 items verified resolved. opus-high: **FIX** (2 real, 1 nit) — all
F1–F11 dispositions verified accurate. codex: **FIX** (5 blockers + stale-note
cleanup) — agrees with both deliberate deviations, as do grok and opus-xhigh.

**Both deviations are now confirmed by all three reviewing lanes** (B4 M4/M5
split with the interlock; S6 three build-tagged files). Do not revisit.

Note the convergence: **codex #4 and opus-xhigh N1 are the same defect.**

## Blocking

- **R1 (opus-high D1) — M1's zero-test-edit criterion is violated by three
  more moves.** F1 proved the seam retarget needs two test exceptions; the same
  check was never run against the other identifiers the move set removes from
  package `cli`, and three are referenced by name in existing tests:
  - (a) `gateStatus` — 11 sites in 4 files (`consume_boundary_test.go:257`,
    `gate_test.go:207,221,372`, `enforced_enrollment_test.go:106,128`,
    `turn_end_test.go:38,87,126,236,494`), all `var got gateStatus` for JSON
    unmarshal. Fix: leave a one-line **type alias** in cli
    (`type gateStatus = op.GateResp`) — alias, not defined type; state it in the
    gate item's (c).
  - (b) `performRegister`/`registerOpts` — 13 call sites in `register_test.go`
    (`:34,58,100,131,165,234,243,270,279,306,336,365,369`), and `:365` uses
    `Bio:"old", BioProvided:true`, fields the reshaped request does not have, so
    **no alias can bridge it**. Fix (recommended): keep
    `performRegister(cfg, registerOpts, now)` in cli as a thin **adapter** that
    maps to the op request and calls the op — a caller, not a callee, so no
    cycle. (Alternative: name `register_test.go` as a second exception with its
    hunk list — 13+ hunks in the merge whose claim is invisibility.)
  - (c) `runnerRuntime.lease`/`regTemplate` — `run_test.go:500-507`
    (`newPollTestRuntime`, also used by `b1_convergence_test.go:36`) builds the
    literal. Fix: name that helper as a third exception (one hunk, one helper).
  - **Preamble rule that would have caught all three**: for every identifier the
    move set removes from package `cli`, either leave a thin alias/adapter or
    name the test file and hunks as an exception — and grep each moved
    identifier across `internal/cli/*_test.go` before calling the item done.
    Then M1's criterion reads "EXCEPT the named list", which is honest.
- **R2 (opus-high D2) — F4's fix relocates three stdout lines after the owed
  lines.** `Listed`/`Remaining` reproduce the TEXT but are derived from the
  summary, which arrives after the stream, while expired-owed events are emitted
  inside the claim op. So `(inbox empty)` (check.go:201), `(reached limit of …)`
  (:207) and the CLAIMED note (:262) all move after the obligation lines —
  a silent behavior change inside the merge whose whole claim is zero behavior
  change, and `check_age_owed_test.go:196-224` stays green because it uses
  `strings.Contains`. Fix: emit all three as `Level:"info"` NoteEvents at their
  production points (exactly what the pinned note vocabulary is for; ordering
  then survives the wire too). Stop telling the renderer to print from
  `Listed`/`Remaining`.
- **R3 (codex #1) — the registry-completeness test is impossible as written.**
  PLAN:122-146 gives `op.CodeFor` eight sentinel codes plus default `E_HUB_IO`
  including `E_ORDER`; PLAN:706-717 says M3 ADDS nine including `E_ORDER`, calls
  the op side "eight codes", requires no duplicate AND requires the client-only
  list disjoint. Cannot all hold. Fix: pin op outputs = 9 including the default;
  M3-only = 8 excluding `E_ORDER`; model `E_POOL_MISMATCH` as an explicit
  both-sides classification instead of requiring disjointness.
- **R4 (codex #2) — F8's shapes still lose behavior.** `StatusAgent{…}`
  (PLAN:351-358) is not a fixed struct/JSON schema and "row's own fields" is
  ambiguous; `op.Status` has no channel for `ReadRegistrationsLenient` warnings
  that `cmdStatus` prints today (`status.go:68-74`); and
  `CleanOwedResp{Pruned, Remaining int}` (PLAN:361-365) loses the pruned refs
  required by today's text and `--json` output (`clean.go:96-103,180-192`;
  `clean_test.go:391-412`), so its unmodified-test done-when cannot pass. Fix:
  enumerate `StatusAgent` + JSON tags, preserve the status warnings, return the
  pruned refs and existing JSON semantics.
- **R5 (codex #3) — PLAN and DESIGN disagree on two wire structs**, and DESIGN
  says seam structs are serialized verbatim, so this is not packaging taste:
  DESIGN:353-367 defines `RegisterReq` WITHOUT `HostProvided` (hub maps the
  client-resolved Host to HostProvided:true) while PLAN:430-444 adds it;
  DESIGN:435 defines `GateReq{Phase string}` while PLAN:336-343 adds
  `RequireConfirm` and `AckStaleReg`. Fix: choose ONE exact schema and make both
  documents say it — pick whichever preserves current CLI behavior, and check
  the real flags before deciding.
- **R6 (codex #4 = opus-xhigh N1) — `_test.go` helpers are not importable.**
  The L/W assertions are pinned in `internal/spectest/lease_test.go` /
  `wire_test.go`, but the M6 item imports `internal/spectest` and calls those
  helpers — identifiers in `_test.go` compile only into that package's own test
  binary. As pinned, the sshd driver cannot see them, so it fails to compile or
  duplicates the assertions (the one thing the item forbids). Fix: pin the
  transport-parameterized helpers in NON-test files
  (`internal/spectest/lease.go`, `wire.go`), exported, taking the transport as
  an interface (`io.ReadWriter` for the codec half, a small seam for the lease
  half); `*_test.go` become thin drivers passing `net.Pipe`; the M6 item passes
  the ssh transport to the same exported helpers.
- **R7 (codex #5) — the canonical test-script change bypasses the env strip.**
  PLAN:972-975 mandates the literal bare `cd conformance && go test ./...`
  while E10 and PLAN:2090-2096 require every Go invocation to strip
  `AGENTCHUTE_*`. Fix: use the script's existing `$strip_env` and preserve its
  explicit failure reporting.

## Non-blocking

- **R8 (opus-xhigh N2)** Vectors path works only by depth coincidence
  (`../../conformance/vectors/…` resolves from both packages only because both
  sit two levels below the root). Fix: derive the directory once in the loader
  from `runtime.Caller(0)` (stdlib; `go:embed` cannot reach a parent).
- **R9 (opus-xhigh N3)** The identity-signature item names 5 files but there are
  12 call sites (`check.go:87`, `ack.go:91`, `boot.go:78`, `pending.go:63`,
  `send.go:127`, `gate.go:80`, `clean.go:80`, `register.go:254`,
  `self_check.go:118` beyond the named five) — each already Discovers first, so
  each just passes cfg. Matters because §2 rule 3 makes the (c) list the
  authority for parallelism. Fix: list them, or state that a signature change
  updates all callers and rule 3 reads the union.
- **R10 (opus-xhigh N4)** The boot item's (b) cites "§10.3's four boot rows";
  there are three (hub-reboot pid reuse; clock step does not steal; boot_ref
  survives the heartbeat). Its own (e)/(f) list seven cases correctly — only the
  pointer is wrong.
- **R11 (opus-xhigh note 1)** The migration item's third attribution arm can
  false-attribute after OS pid reuse (stale `runner.json` naming a pid that now
  belongs to an unrelated flagless `agentchute serve`). It fails CLOSED and never
  suggests a kill, so the direction is right — add one sentence so nobody later
  "fixes" it into proceeding.
- **R12 (opus-xhigh note 2)** `boot_id`/`kern.bootsessionuuid` are host-scoped,
  so a container restart on an unrebooted host still wedges as today. §6.9's
  stated scope ("after a hub reboot") is fine, but the M2 spec text should carry
  the clause rather than imply pid reuse is solved generally.
- **R13 (grok nit)** The M3 pool.id item's (d) says do not implement either
  `E_POOL_MISMATCH` arm, but its (e) still asks for absent/malformed/
  valid-but-different tests that already live on the handshake item's (e). Drop
  the pool.id clause from that (e).
- **R14 (opus-high D3 + codex cleanup) — stale notes now contradicting the
  frozen amended DESIGN**: PLAN:159-167 (remote-send `E_NOT_REGISTERED`
  divergence — fixed), PLAN:512-514 (DESIGN lacks `tick-ok.warnings` — fixed),
  PLAN:2356-2358 and the revision log's "FLAGGED, not fixed" section (all six
  are fixed in DESIGN @ 4600cd7c; opus-high verified each: warnings at
  DESIGN:756/759/767, both not-registered texts + status.go:62 at :791/:2330,
  ControlPath rule at :559-560/:2215-2216, shim row at :2655 with the fix at
  :1440-1477, mixed-version row at :2654/:1542-1559, three latch sites at
  :276-277/:1325/:2661). Retitle or delete so the DESIGN owner does not redo six
  amendments.

## Revision log (plan round 3)

Applied to PLAN.md; DESIGN.md touched only for R5 and R12. All anchors below
re-opened and verified at `1244ae4`.

- **R1 — FIXED**, M1 preamble ("Ground rules for every M1 item") + WI-1.4(d),
  WI-1.6, WI-1.7(d), WI-1.8(f). The criterion now reads "unmodified EXCEPT the
  named list", and the list is a closed table: `send_a5_test.go:229-237`,
  `send_b3_test.go:137-146`, `run_test.go:495-507` (`newPollTestRuntime`) —
  three files, three hunks. (a) `type gateStatus = op.GateResp` **alias**
  (verified: all 11 sites are `var … gateStatus` + `json.Unmarshal` on an
  all-exported struct, `gate.go:271-286`). (b) `performRegister` stays in `cli`
  as a **thin adapter** with its signature and both structs unchanged, so all
  13 calls + 2 `registerOpts` literals compile untouched; it also owns the
  `!HostProvided ⇒ os.Hostname()` substitution, which is where R5 needs it.
  (c) `newPollTestRuntime` named as the third exception — one helper, one
  hunk; `b1_convergence_test.go:36` calls it and is untouched, as is
  `run_resize_unix_test.go:44`. The **move-set grep rule** is now a binding
  preamble rule and a WI-1.8 done-when. Running it over the whole move set
  found **two cases beyond R1's three**, both closed by preserving a signature
  rather than adding an exception: `printStatus` (`status_test.go:35,77,126,181`
  — WI-1.4 keeps it byte-identical and defers the render switch to M4) and
  `refuseLiveRunnerCollision` (`run_test.go:130,140,159,170`, whose results feed
  `loop.RenewLease`/`ReleaseLease` at `:147,164,167,172` — kept byte-unchanged
  via the new `ChannelOpts.Lease` adoption arm, WI-1.7(d)).
- **R2 — FIXED**, WI-1.3(d)/(e). The three lines are `Level:"info"` NoteEvents
  at their production points: `(inbox empty)` at `check.go:200-202` (guard
  `len(msgs)==0 && len(redelivered)==0`), the limit line at `:206-208` (emitted
  mid-stream, between two message events — which is why no summary field can
  reproduce it), the CLAIMED line at `:261-263` (guard `!noArchive && claimed>0`)
  ahead of the expired-owed lines. `ClaimSummary` reverts to DESIGN §3.2's four
  counts, which also removes a PLAN↔DESIGN divergence against `check-ok`
  (`DESIGN.md:737`). The golden test now asserts **position**, not presence.
- **R3 — FIXED**, WI-1.1(d) + WI-3.1a(d)/(e). Pinned arithmetic: op outputs
  **9** (8 sentinels + the `E_HUB_IO` default), `internal/hubwire` adds **8**
  (`E_ORDER` removed — it is an op sentinel), union = §4.4.2's **17** hub-side
  rows exactly; client-only = **9** and disjoint. `E_POOL_MISMATCH` is an
  explicit `both` row in a `codes.go` emitter-classification table instead of a
  member of two lists.
- **R4 — FIXED**, WI-1.4(d). `StatusAgent{Reg RegistrationView; InboxDepth int;
  Status string; Claim *StatusClaim}` fully enumerated; `RegistrationView` is a
  tagged mirror of `loop.Registration`'s eight fields because that struct
  carries **no JSON tags** (`registration.go:58-68`) and `internal/loop` is
  frozen in M1. `StatusResp` gains `Warnings []string` (the
  `ReadRegistrationsLenient` channel, `status.go:67-70,91-92`) and `Now` (one
  clock for AGE + label). `CleanOwedResp` = `cleanOwedResult` verbatim
  (`clean.go:99-103`) — `{Agent string; Pruned []string; Applied bool}`, `Pruned`
  non-nil so `--json` renders `[]` (`clean.go:121`), which is what
  `clean_test.go:391-412` decodes; `Remaining` dropped (nothing prints it); the
  request field is `Apply`, matching the shipped `--yes` polarity rather than an
  invertible `DryRun`.
- **R5 — FIXED, and the two structs went in opposite directions.**
  **`RegisterReq`: DESIGN's schema wins — no `HostProvided`** (PLAN WI-1.6(d)
  amended). Verified against the real path: `performRegister` substitutes
  `os.Hostname()` only when `!HostProvided` (`register.go:80-87`), and every
  local arm is reproduced by resolving client-side and treating the field as
  provided — `--host X` ⇒ X, explicit `--host ""` ⇒ empty (`cmdRegister`'s
  `fs.Visit` capture, `register.go:226-233`), absent ⇒ the caller's own
  hostname, same warning on failure. Carrying `HostProvided:false` over the wire
  is the one shape that lets the hub write the HUB's hostname onto a remote
  lane's row, which is the bug D1a exists to prevent. **`GateReq`: PLAN's
  schema wins — `{Phase, RequireConfirm, AckStaleReg}`** (DESIGN §3.6 amended).
  Verified: both are real shipped flags parsed at `gate.go:55-56` and passed to
  `evaluateGate` at `gate.go:97` (signature `gate.go:129`); dropping them makes
  every remote `gate --require-confirm` stop refusing and every
  `--ack-stale-reg` stop acknowledging — a silent verdict change.
- **R6 — FIXED**, WI-3.5(c) + WI-4.10(c). Assertions move to NON-test files
  `internal/spectest/lease.go` / `wire.go`, exported and transport-parameterized
  (`io.ReadWriter` for the codec half, a small seam for the lease half);
  `lease_test.go`/`wire_test.go` become thin `net.Pipe` drivers; WI-6.4 passes
  the ssh transport to the same helpers, so one assertion implementation, two
  transports.
- **R7 — FIXED**, WI-3.5(c). The `tools/test.sh` addition is now
  `(cd conformance && env $strip_env go test ./...) || { say "FAIL: conformance
  go test"; exit 1; }` — the script's own strip list (`tools/test.sh:7`), its
  own failure reporting (`:14-24`), and a subshell so `go build ./...` still
  runs from the repo root.
- **R8 — FIXED**, WI-3.5(c). The loader derives the vectors dir once from
  `runtime.Caller(0)` + `filepath.Dir`, so it no longer depends on every
  importing package sitting exactly two levels below the root.
- **R9 — FIXED**, WI-4.8(c). All 12 `resolveAgentID` call sites enumerated
  (`check.go:87`, `ack.go:91`, `boot.go:78`, `pending.go:63`, `send.go:127`,
  `gate.go:80`, `clean.go:80`, `register.go:254`, `self_check.go:118`,
  `serve.go:145`, `status.go:34`, `identity_cmd.go:24`), with the explicit rule
  that a signature change updates every caller and §2 rule 3 reads the union.
  `guard.go:175-189` is called out as a 13th, **inline** resolution that never
  calls the function.
- **R10 — FIXED**, WI-3.4(b). Three boot rows, not four, with anchors
  (`DESIGN.md:2652`, `:2653`, `:2654`); (e)'s seven cases restated as the
  authority.
- **R11 — FIXED**, WI-5.3c(d). One paragraph on the third arm: it can
  false-attribute after OS pid reuse, it fails CLOSED (a refusal, never a kill),
  and it must not be "fixed" into proceeding.
- **R12 — FIXED**, PLAN WI-2.2(d)/(f) + DESIGN §6.9 (Sources bullet) + DESIGN
  §9.1's §5.4 amendment (two properties → three). The clause: both refs are
  **host-scoped**, changing on a host reboot and nothing else, so a
  container/VM/service restart on an unrebooted host is unchanged from today and
  pid reuse is not solved generally.
- **R13 — FIXED**, WI-3.2(e). The `pool.id` absent/malformed/valid-but-different
  clause is dropped and the rows are pointed at WI-3.3a(e), which owns the J1
  validation and the hub arm.
- **R14 — FIXED in PLAN; one part is out of this round's write scope.** The
  `E_NOT_REGISTERED` note (WI-1.1) and the `tick-ok.warnings` note (WI-1.7) now
  cite the amended DESIGN (`:791`/`:2330`, `:756,759,767`) instead of flagging
  it. PLAN's closing paragraph is replaced by an explicit **"the round-1
  FLAGGED-not-fixed list is obsolete — do not re-amend"** block naming all six
  with anchors (verified before deleting: `tick-ok.warnings` at `DESIGN.md:756,
  759,767`; both not-registered texts + `status.go:62` at `:791`, `:2330`;
  ControlPath rule at `:559-560`, `:2215-2216`; legacy-shim launcher row at
  `:2655`; mixed-version parse-vs-persist row at `:2654`; the REDELIVERED-first
  latch case at `:2661`). **Not done**: the heading inside
  `plan-reviews-r1.md`'s own revision log — that file is outside this round's
  authorized writes. PLAN no longer points at it, so nothing binding references
  it.

**New this round (knock-ons, flagged for reviewers):**

1. **`op.RegisterResp` was too small for M1 to be invisible** (WI-1.6(d)).
   Forced by R1b and independent of which R1b arm is chosen: `cmdRegister`
   (`register.go:270,274,276`), `cmdBoot` (`boot.go:104,111,137-142,200`),
   `selfRepairRegistration` (`self_check.go:79-80`) and `register_test.go`
   (`:144,344,347,373,376,379`) all read fields that `{Created, AnnouncedTo,
   Pending}` cannot supply. PLAN adds `Reg RegistrationView`, `InboxDir`,
   `Refreshed`, `ExistingFound`, `ResolvedHost`, `Warnings`. ~~DESIGN §3.5's
   Output line is a strict subset and needs the same five fields — flagged,
   not edited (outside the R5/R12 authorization), together with the §4.4.3
   question of whether `reg.body` rides in the control line or a trailer.~~
   **STRUCK in round 4 (T7): both clauses are stale.** DESIGN §3.5's Output
   line now carries the full set including `Announce *AnnounceView`, §4.4.1
   shows the `register-ok` frame, and §4.4.3 answers the trailer question —
   `reg.body` rides as that frame's trailer, never inside the control line.
   PLAN's own closing block already says so; nothing here is still open.
2. **`ChannelOpts.Lease`** (WI-1.7(d)) — a local adoption arm mirroring
   `HeartbeatTemplate`'s two arms, so `refuseLiveRunnerCollision` keeps its
   signature and the wire structs (`LeaseReq{}` / `LeaseResp{Token}`) are
   unchanged. `ChannelOpts` is a constructor input, never serialized.
3. **`note.level`'s `info` arm** (WI-1.1(d), WI-2.1(d)) — DESIGN §4.3's note
   bullet (`:629-631`) illustrates only `warn`→stderr. R2's fix makes the
   `info`→stdout arm load-bearing, so the M2 spec copy must spell out both arms
   and their streams.
4. **The 64-entry `status` cap applies locally too** (WI-1.4(d)) — a pool with
   >64 rows would truncate where today it does not. No existing test has >64
   rows, and capping only on the wire would make local and remote disagree;
   recorded so it is a decision rather than an accident.

Nothing was rejected.
