# Plan review round 1 — PLAN.md @ sha256 bdd0847d… (2026-08-15)

All four lanes returned FIX. Condensed findings; full texts in the mail
archive. Reviser: fix all, log dispositions at the end. Findings marked
[DESIGN] require amending DESIGN.md, not just PLAN.md.

## grok (executability/pacing)

- **P1** Split oversized items, same owners: 3.1a codec+roundtrip / 3.1b
  fuzz+streaming; 3.3a dispatcher+state machine+release-on-EOF / 3.3b
  deadlines+every-op e2e; 5.3 split (see also S9).
- **P2** Split WI-4.3: 4.3a one-shot routing + turn-end order; 4.3b send
  ambiguity + spool; 4.3c negative cache + D6 boot.
- **P3** pool.id has no owner before M5: pin in WI-3.2 that M3 tests mint a
  fixture pool.id (O_EXCL, valid hex); sole production writer is WI-5.4; add
  to risk 6.
- **P4** No CHANGELOG/tag-notes owner: add WI-1.9 (v1.6.0 notes) and WI-6.6
  (v1.7.0 notes + CHANGELOG + hub-upgrades-first), integrator-owned, landing
  with the tag.
- **P5** §5.2.2 "real second machine" walk must say: scratch pool only (or
  second unix user on the hub host), NEVER this checkout's live loop.
- **P6** One implementer per merge; items in numeric order; no parallel WIs
  sharing a file — state once under §2.
- **P7** Name the sshd test wrapper: tools/test.sh tagged path or new
  tools/sshd-test.sh (strip AGENTCHUTE_*, set only AGENTCHUTE_SSHD_TEST=1);
  WI-6.3 done-when cites that script, never bare go test. (= codex #7.)
- **P8** Release checklist line: a remote updating before the hub gets
  E_VERSION and must wait — do not "fix" by re-running join. [DESIGN §7.5 or
  checklist only — reviser's call]

## opus-high (M1 implementer walk; anchors verified @ 1244ae4)

- **F1** WI-1.2 vs the sendTsMessageWithCommit test seam (send.go:21;
  send_a5_test.go:229-237, send_b3_test.go:137-146 reassign it): pick option
  (a) — op owns an equivalent injectable var, named exception to the
  zero-test-edit rule for those two files; reword WI-1.8(f) accordingly.
- **F2** op.Tick unpinned and cannot be stateless (template serve.go:237,
  lastSweep, token): pin a stateful handle, e.g. op.NewChannel(cfg, ctx) with
  Register/Tick/ReleaseLease methods; runnerRuntime stores it. The seam IS
  the wire schema — M3/M5 inherit this shape.
- **F3** Tick non-fatal warnings (pollOnce logs+continues on renew/heartbeat/
  sweep errors, serve.go:609-677): error only for ErrFenced; else
  TickResp.Warnings []string rendered as today's logf lines; note tick-ok
  example gains the field.
- **F4** Three load-bearing stdout lines unrepresented: "(inbox empty)"
  (check.go:198), "(reached limit…)" (check.go:210 — add Remaining int to
  ClaimSummary or pin as info note), CLAIMED note (check.go:265). Pin
  NoteEvent Level vocabulary + routing (warn/error→stderr, info→stdout);
  these three are info notes in production order (or the pinned alternative).
- **F5** WI-1.3 contradicts itself on ClearOwed: displayConsumed
  (check.go:312) splits — ClearOwed half (321-331) into op.Claim (suppressed
  under NoArchive), print half to renderer; "stays" range becomes 335-408.
- **F6** WI-1.1 sentinel table mixes M3-only codes, existing loop sentinels,
  and catch-all E_HUB_IO: list the exact M1 op-layer sentinel set; op
  re-exports loop's four; define op.CodeFor(error) (default E_HUB_IO);
  completeness test over that function. Also: ErrNotRegistered rendered texts
  (send.go:145, check.go:120) stay byte-identical via Context.ActorID — say
  so.
- **F7** Import-cycle ordering: WI-1.7's gate move is a prerequisite of
  WI-1.4 (op.Ack needs finishGateClear, gate.go:262); delete WI-1.6's
  non-compiling "stays in cli" branch. (= codex #2, B1.)
- **F8** WI-1.7 has no fixed-shapes clause: pin StatusResp (+ per-agent entry,
  64-cap + Truncated bool), GateResp, CleanOwedReq/Resp — M3 serializes them.
- **F9** E_POOL_MISMATCH is on both sides: correct the registry — hub-emitted
  at session start (pool.id absent/≠ --pool-id), client-emitted on
  hello-ok.pool12 mismatch; fix WI-1.1 enumeration + WI-2.1 spec text.
  [DESIGN §4.4.2 + §5.1] (= codex #1.)
- **F10** WI-6.3 CI job: hub-integration runs ONLY the tagged package, one
  step, no test.sh (ci.yaml never calls test.sh; test.sh mutates via gofmt
  -w); §10.2 hubwire tests stay in the existing go job; pin whether
  integration/sshd/ is root-module (decides go build/vet visibility).
- **F11** send-ok example missing "committed" (load-bearing for never-replay):
  add to §4.4.1 example + WI-2.1 spec copy. [DESIGN §4.4.1]

## codex (plan-vs-design gate)

- **X1** = F9 [DESIGN]: resolve before M1/M2.
- **X2** = F7/B1: mandate move of validate/mutate/evaluate cores into
  internal/op; thin CLI adapters; pin gate core before op.Ack; delete
  stays-in-cli option.
- **X3** = B3: conformance/ is module agentchute.dev/conformance — cannot
  import internal/*; root go test and test.sh never enter it. Put
  internal-driving L/W adapters/tests in the ROOT module; transport-neutral
  vectors as shared JSON data; define how M6 consumes them; add any nested
  run to the canonical script.
- **X4** Circular/absent done-whens: WI-5.3 gates on the M5 in-process
  harness (real quickstart transcript moves to M6); WI-6.1 gains a done-when
  (sshd start/auth/forced-command/cleanup + real-pool containment, both OSes).
- **X5** ControlPath contingency has no trigger: pin a conservative
  expanded-path threshold + owned 0700 per-user temp fallback; deterministic
  deep-home builder test; M6 mux-reuse runs both paths.
- **X6** Doc placement deferred: name the exact repo doc and web/ artifacts
  now (web/hub.html + nav/sitemap if intended); render/link verification in
  WI-6.5 done-when.
- **X7** = P7 + rule-5 conflict: cross-cutting rule 5 is exactly tools/test.sh
  (drop the separate raw-command line); pin the sshd script/mode semantics.

## opus-xhigh (senior implementer; anchors verified)

- **B1** [also X2/F7] Every wrapped helper MOVES to internal/op (list:
  evaluateGate/finishGateClear/gateStatus gate.go:129,262,271;
  archiveAllClaimed ack.go:164; performRegister & co register.go:35-189;
  hasPendingInboxMail serve.go:757); op MUST NOT import cli; add a
  dependency-direction test; re-price M1 to 2,000-2,500 LOC.
- **B2** Second cycle — transport selection: pin direction: op is a leaf over
  loop; hubwire imports op; hubclient imports hubwire+op; CLI selects the
  transport (or op declares a Transport interface cli injects). [DESIGN §7.4
  wording "op picks the transport" must change accordingly]
- **B3** = X3.
- **B4** [DESIGN §6.8] Launcher de-remotes joined lanes: buildDispatchRunArgs
  (dispatch.go:246-257) and shims (shims.go:300-306) forward --control-repo
  <local root> + --loop-dir <shadow>; flag arm outranks pointer → Remote=nil,
  lane runs local against the shadow, NO error. Fix: in remote mode forward
  --control-repo <canonical ssh URL>, omit --loop-dir; own dispatch.go +
  shims.go in a WI; row: ac serve in joined checkout ⇒ Remote != nil.
- **B5** [DESIGN §7.2] Migration attribution predicate broken for normal
  lanes: setupCommandMatchesPool (setup_reset.go:308-344) exact-matches argv
  flags against cfg — wrong strings in remote mode ⇒ every live lane
  unattributed ⇒ migration permanently fail-closed. Define the remote
  predicate (argv --control-repo vs old hub dir's recorded ssh URL, or
  --loop-dir vs its shadow); test with a lane launched exactly as the
  quickstart launches it.
- **B6** [DESIGN §6.9] Clock-step lease theft: both boot-time sources move
  with wall-clock steps; NTP step ⇒ live lane's LastSeen stale + StartedAt
  pre-boot ⇒ live lane reclaimed (branch (d) lease.go:242-244 was its only
  protection). Fix: record boot_ref IN the claim at acquire (linux
  /proc/sys/kernel/random/boot_id; darwin recorded kern.boottime), reclaim in
  (d) only when a PRESENT ref DIFFERS (equality, no ordering); absent ref ⇒
  today's behavior. RenewLease preserves extra fields (lease.go:315-339,
  verified). Add row: clock stepped forward under a live lane ⇒ NOT stolen.
- **S1** self_check.go (selfRepairRegistration :117, used by turn-end :120)
  into WI-1.6 + WI-4.3 file lists.
- **S2** [DESIGN §3.5/§6.8 note] Client-side vendor resolution in remote mode:
  all four resolveAgentVendor sites (serve.go:152, register.go:259,
  boot.go:86, self_check.go:126) skip local resolution, send Vendor:nil;
  cmdServe's missing-vendor refusal (serve.go:153-155) suppressed for remote
  lanes.
- **S3** internal/loop/pointer.go must change (DiscoverPointer/
  ResolvePointerTarget pointer.go:93,105-118 require an existing directory;
  ssh:// pointer dies before the ssh arm) — add to WI-4.1's files.
- **S4** Non-wrapper --name yields unlaunchable lane (wrapperForToken
  dispatch.go:133-136; direct serve treats positional as wrapper command
  serve.go:117-129): refuse non-wrapper --name at join OR teach the
  dispatcher the names map; add a --name work row. [DESIGN §7.2 join rules]
- **S5** config.json ownership: pin struct + read/write helper in M4; M4 rows
  run against a hand-planted config.json until M5.
- **S6** WI-3.4 build tags: add boottime_windows.go + !linux&&!darwin&&
  !windows fallback ("unreadable" = unchanged behavior); prefer
  x/sys/unix.SysctlRaw on darwin (x/sys already in go.mod) over exec'ing
  sysctl.
- **S7** W2/W1-client cannot run in M3: restate M3's W set as hub-side
  assertions; client halves move to M4 (or W wholesale to M4).
- **S8** Three identity call sites resolve before Discover (status.go:34,
  identity_cmd.go:24, guard.go:175-181): need reorder; status.go named in no
  WI.
- **S9** Splits (with P1): 5.3a join core / 5.3b key lifecycle / 5.3c
  migration (D4a/F2), lanes per sub-item; peel setup_wipe H1 change out of
  WI-5.4 into its own small item.
- **G1** [DESIGN §7.2] Migration completion: copy into <newid>.partial,
  fsync, rename() into place, delete old — pin the sequence.
- **G2** [DESIGN §7.2] Lock file over ~/.agentchute/hub/<hub-id>/ (flock
  idiom, filelock_unix.go) for join/rotate/migrate concurrency.
- **G3** [DESIGN §7.2] Version-file details: numeric .v<N> ordering,
  .invalid.<ts> format, whether .pub follows the active symlink (both paste
  lines need a defined current-pubkey path).
- **G4** [DESIGN §7.2] Mux masters survive migration up to ControlPersist=60s:
  reap with ssh -O exit or declare harmless — one line.
- Watch-item (budget, not a finding): non-root sshd -D auths only the
  invoking user; macOS needs host-key/config paths hand-fed — WI-6.1.

## Revision log (plan round 2)

PLAN.md revised in place (2026-08-15). Section refs below are PLAN.md's, after
the renumbering §2 rule 2 forced. Every code anchor used in the revision was
re-opened at `1244ae4`; where a finding's own citation was wrong, the corrected
anchor is in PLAN.md and noted here.

**Sizing/count after revision: 45 work items, ≈10,700 LOC; M1 re-priced to
2,000–2,500 (plan carries ~2,060).**

### grok (executability/pacing)

- **P1** FIXED — WI-3.1a (codec+round-trip) / WI-3.1b (fuzz+streaming);
  WI-3.3a (dispatcher+state machine+release-on-EOF) / WI-3.3b (deadlines+
  every-op e2e); 5.3 split under S9. Same owners.
- **P2** FIXED — WI-4.5 (one-shot routing + turn-end order), WI-4.6 (send
  ambiguity + spool), WI-4.7 (negative cache + hook-mode boot/D6).
- **P3** FIXED — WI-3.2 pins the fixture (`writeFixturePoolID`, `O_EXCL`, 12
  lowercase hex + LF, reused by WI-3.3a/b); WI-5.4 is named the sole production
  writer; risk 6 rewritten. Verified and stated: no Go file, test, or tracked
  source mentions `pool.id`/`pool12` today.
- **P4** FIXED, and stronger than proposed — WI-1.9 (`docs/releases/v1.6.0.md`
  + CHANGELOG) and WI-6.6 (`docs/releases/v1.7.0.md` + CHANGELOG +
  hub-upgrades-first), integrator-owned, each landing inside its merge's PR.
  Reason for the upgrade from "nice to have": `release.yaml:110` runs
  `test -s "docs/releases/${tag}.md"` and `:168` passes it as `--notes-file`, so
  a missing file fails the release job *after* the tag is pushed.
- **P5** FIXED — §5.2 step 3 now reads "SCRATCH POOL, or a SECOND UNIX USER on
  the hub host — NEVER this checkout's live loop directory", with the reason
  (the walkthrough issues `hub join`/`serve`/`hub authorize`, all mutating).
- **P6** FIXED — §2 "Execution rules for every merge", stated once: one lane per
  merge (with named specialist hand-offs *inside* the merge), numeric order with
  the load-bearing orderings restated on the items, and no parallel items
  sharing a file.
- **P7** FIXED — new `tools/sshd-test.sh`, exact content pinned in WI-6.1, with
  the reasoning: `tools/test.sh` is 26 lines with **no** argument parsing of any
  kind and opens with `gofmt -w .`, which mutates the checkout; the repo's own
  precedent for a second suite is the conformance module's own CI step
  (`ci.yaml:39-40`). Every M6 done-when cites the script, never bare `go test`.
- **P8** FIXED (plan side; DESIGN already carries the `E_VERSION` text and the
  §9.1 fourth rule) — WI-2.4 now says **four** lane-facing rules, not three, and
  WI-6.6's notes restate the wait-don't-rejoin rule.

### opus-high (M1 implementer walk)

- **F1** FIXED, with one deliberate refinement to option (a). `internal/op`
  declares an **exported** `var SendTsMessageWithCommit = loop.SendTsMessageWithCommit`
  rather than an unexported var plus a test-only setter: the two reassigning
  tests live in package `cli`, so an unexported var would still leave them
  nothing to patch, and an exported var needs no setter function and no test
  relocation — their surrounding assertions keep exercising the real `cmdSend`.
  WI-1.2 names the exception (`send_a5_test.go:229-237`,
  `send_b3_test.go:137-146`, verified, including what each stub does) and
  WI-1.8's acceptance criterion is reworded: zero test edits **except** those two
  files, whose diffs must show only the retarget hunks.
- **F2** FIXED — `op.NewChannel(cfg, ctx, ChannelOpts{HeartbeatTemplate *loop.Registration})`
  with `AcquireLease`/`Token`/`Register`/`Tick`/`ReleaseLease`, pinned in WI-1.7
  and explicitly marked as inherited by M3's hub session and M5's remote serve.
  The `HeartbeatTemplate` two-arm shape (non-nil = the local runner's verbatim
  `heartbeatTemplate(cfg, opts)`, so M1 changes no heartbeat byte; nil = derived
  and cached by `Register`, with `Tick`-before-`Register` ⇒ `op.ErrOrder`) is the
  one addition to the proposed shape, and it exists so M3 does not have to
  reshape the seam. `runnerRuntime` stores one `*op.Channel` in place of three
  fields (lease, `regTemplate` `serve.go:237`, `lastSweep` `serve.go:242`).
- **F3** FIXED — `TickResp.Warnings []string`; `Tick` errors only on
  `op.ErrFenced`; the three warning sources are pinned with their exact
  `r.logf` strings (`serve.go:622`, `:631`, `:641`) minus the trailing newline,
  rendered `r.logf("%s\n", w)`. `serve.go:675`'s runner-state write failure is
  explicitly NOT a tick concern. `lastSweep` still advances on sweep failure
  (`serve.go:643`). The `tick-ok` wire example is **flagged** below.
- **F4** FIXED — both halves, deliberately. Two count fields on `ClaimSummary`:
  `Listed` (= `len(msgs)` at `check.go:200`, so the renderer reproduces the
  `(inbox empty)` guard verbatim — counts alone cannot, because an all-quarantined
  inbox has `Claimed == 0` and must NOT print it) and `Remaining`
  (= `len(msgs)-claimed` at `check.go:207`). The CLAIMED note needs **no** field:
  the renderer derives it from `NoArchive == false && Claimed > 0`, today's guard
  at `check.go:261`. Separately, `NoteEvent.Level` is pinned to exactly
  {`"warn"`,`"info"`} with routing (warn→stderr as `warning: <Msg>`, info→stdout)
  and `Msg` never carrying its own prefix.
  Anchors corrected: `(inbox empty)` is `check.go:201`, reached-limit `:207`,
  CLAIMED `:262`.
- **F5** FIXED — WI-1.3 now states the split explicitly (ClearOwed half
  `check.go:321-331` + its two warning sites `:324`/`:328` → `op.Claim`,
  suppressed under `NoArchive` to match `displayConsumedReadOnly`
  `check.go:338-341`; print half → renderer). The "stays" range is corrected:
  not `335-408` (`:335` is blank) but `displayConsumedReadOnly` 338-341,
  `printConsumedBody` 348-364, `sanitizeControlBytes` 374-388,
  `printReplyRefIfRequired` 393-408.
- **F6** FIXED, with one correction to the finding's premise. The exact M1
  op-layer sentinel set is enumerated as a table of **eight** — four re-exports
  (`var X = loop.X` aliases, so `errors.Is` matches across packages) and four
  new, including the split of `*loop.ErrRecipientStale` into `ErrRecipientStale`
  (preflight, `seq.go:240`) and `ErrRecipientRacing` (under lock, `seq.go:253`),
  which the hub needs because §4.4.2 gives them two codes. `op.CodeFor(error)` is
  a function with an `E_HUB_IO` default arm; `loop.ErrInboxMissing` takes that arm
  deliberately; the completeness test runs over the function plus a name-list test
  so a ninth sentinel without a code arm fails. The codes M1 does NOT own (M3
  session/codec, M4 client-only) are listed so the test cannot over-reach.
  **Correction**: the two rendered not-registered texts are *not* byte-identical
  today — `send.go:145` says `sender %q …`, `check.go:120` says `agent %q …`,
  identical thereafter. So the rule is one sentinel, two renderers, each keeping
  its own wording; the local/remote divergence this creates is **flagged** below.
- **F7** FIXED — resolved structurally rather than by a note. M1's items were
  renumbered so dependencies run in numeric order (P6): the read-ops + gate core
  is now **WI-1.4** and `op.Ack` is **WI-1.5**, with the prerequisite stated on
  both. The "stays in cli / moves wholesale — implementer's choice" branch is
  deleted everywhere in M1; the M1 preamble lists the full move set and states
  that the alternative does not compile.
- **F8** FIXED — WI-1.4 gains a fixed-shapes clause: `GateReq` (exactly
  `evaluateGate`'s parameters, `gate.go:129`), `GateResp` (= `gateStatus`'s
  fields verbatim, `gate.go:271-286`, so `gate --json` and `turn-end --json`
  stay byte-identical), `StatusResp{Agents, Truncated}` with the 64-entry cap
  enforced **in the op** and sorting applied before truncation,
  `StatusAgent{…, Claim *StatusClaim}` with `ServeClaim.ServeToken`
  (`lease.go:59`) explicitly excluded as the fence secret, `PendingReq`/
  `PendingSummary`, and `CleanOwedReq`/`CleanOwedResp`.
- **F9** [DESIGN] — DESIGN.md already amended; PLAN.md made consistent: WI-1.1's
  enumeration says neither arm is an op-layer sentinel; WI-3.2 carries only
  `hello-ok.pool12`; WI-3.3a owns the hub arm at session start; M4 owns the
  client arm; WI-2.1 requires the registry to record each code's emitter side;
  WI-5.6 requires two distinct texts from two distinct conditions.
- **F10** FIXED, against the verified CI rather than an assumed one. WI-6.3
  records the real starting state (four jobs; the `go` job runs gofmt -l/vet/
  test/test -race/build plus `cd conformance && go test ./...`; **no** job calls
  `tools/test.sh`, which would mutate the checkout via `gofmt -w .`). The new
  `hub-integration` job is ubuntu+macos with **one** step, `sh tools/sshd-test.sh`,
  running only the tagged package; §10.2/spectest/seam tests stay in the existing
  `go` job with no CI change. `integration/sshd/` is pinned as **root-module**
  (no nested `go.mod`, so rule 6 holds) with an **untagged `doc.go`** so
  `go build ./...`/`go vet ./...`/`go test ./...` are deterministic with the tag
  absent — and the done-when requires proving that empirically.
- **F11** [DESIGN] — DESIGN.md already amended; PLAN.md made consistent: WI-2.1's
  spec copy carries `"committed":true`, and WI-3.1a decodes it as `*bool` with
  absence ⇒ `E_MALFORMED_FRAME`, never a defaulted `false`.

### codex (plan-vs-design gate)

- **X1** = F9. FIXED as above.
- **X2** = F7/B1. FIXED — the move is mandated with a full list, thin CLI
  adapters, the gate core before `op.Ack`, and the stays-in-cli option deleted.
- **X3** = B3. FIXED as below.
- **X4** FIXED — WI-5.3a's done-when now gates on the **M5 in-process harness**
  (WI-3.3a's fake-transport session), with the real quickstart transcript
  explicitly moved to WI-6.2's happy-path row; WI-6.1 gains a five-part
  done-when (sshd start/auth, forced command actually running the test binary
  with the client's exec string discarded, `t.Cleanup` teardown leaving no
  socket or tree, real-pool containment asserted *both* as a refusal and as
  "nothing was written", and green on both OSes).
- **X5** FIXED — WI-4.4 pins a conservative trigger
  (`len(muxDir) + 1 + 64 >= 100`, reusing the shipped literal at
  `config.go:170` so there is one number in the codebase), a first fallback
  under an owned 0700 per-user temp dir created through the shipped
  `ensureOwnedRunnerSocketDir` discipline (`runner_socketdir_unix.go:24-49`)
  trying `os.TempDir()` then `/tmp` (macOS's `$TMPDIR` is itself long), a
  mux-disabled third arm with one warn note rather than a hard refusal, a
  deterministic **length-injecting** builder test, and WI-6.2's mux-reuse row
  running through both paths.
- **X6** FIXED — WI-6.5 names `README.md`, NEW `docs/hub.md`, NEW
  `web/hub.html` (modelled on `web/spec.html:17-27`), the **three** nav edits
  (`web/index.html:188-193`, `web/spec.html:20-25`, `web/blog/index.html:20-25`
  — there is no nav partial and no build step; blog posts' navs are historical
  and untouched, mirroring `tools/fact-sweep.sh:19`'s own exclusion), and one
  bare `<loc>` in `web/sitemap.xml`. The done-when adds fact-sweep PASS,
  enumerate-and-stat link verification, and a render check.
- **X7** FIXED — cross-cutting rule 5 rewritten to "`tools/test.sh`, and nothing
  else", stating that it IS gofmt+vet+test+build (`tools/test.sh:11-24`), that
  its gofmt is `-w` and mutates (which is why CI uses `-l`), and that
  `tools/sshd-test.sh` runs in addition from M6 and is never folded in.

### opus-xhigh (senior implementer)

- **B1** FIXED — the M1 preamble lists the full move set with verified anchors
  (`evaluateGate` 129-256, `finishGateClear` 262-268, `gateStatus` 271-286,
  `archiveAllClaimed` 164-179, `performRegister` **72-98** — the finding's
  `35-189` conflated three declarations — plus `publishRegistrationOnce`
  107-187, `registrationLiveElsewhere` 189-195, `hasPendingInboxMail` 757-763,
  and the delivery seam var `send.go:21`), states that `internal/op` MUST NOT
  import `internal/cli`, adds the dependency-direction test in WI-1.8
  (`go/build` import scan, stdlib only), and re-prices M1 to 2,000–2,500.
- **B2** [DESIGN] — DESIGN §7.4 already pins the direction; PLAN.md made
  consistent (M1 preamble, WI-1.8's test, §7 pins). No "op picks the transport"
  wording survives in the plan.
- **B3** FIXED — WI-3.5 rewritten around the verified module facts
  (`conformance/go.mod` is three lines, no requires, no `go.sum`, no `go.work`;
  root `go test ./...` and `tools/test.sh` never enter it; only `ci.yaml:39-40`
  and `release.yaml:47-48` do). Drivers go in the ROOT module as
  `internal/spectest/{vectors.go,lease_test.go,wire_test.go}`; vectors travel as
  transport-neutral shared JSON at `conformance/vectors/{lease,wire}.json`, read
  by relative path (data, not an import); `conformance/` gains prose only and no
  Go file; WI-6.4 consumes the same vectors by importing `internal/spectest` and
  swapping the transport; both `go.mod` files must come out byte-identical.
  `tools/test.sh` gains the one missing nested run (`cd conformance && go test
  ./...`) — the only change this plan makes to that script.
- **B4** [DESIGN] — DESIGN §6.8 already amended; PLAN.md gains **WI-4.3**
  (dispatch.go + shims.go), shipping in the same merge as WI-4.2, with the
  §10.3 row asserting `Remote != nil` rather than "the command succeeded".
  **PARTIALLY REJECTED as stated in the task framing** ("must ship in one merge
  with the migration-attribution rule"): they cannot, and DESIGN does not ask
  them to. §6.8 rule 5 requires the launcher fix to ship with the **discovery
  arm** (M4); the migration predicate has **no caller** until `hub join` exists
  (M5), so putting it in M4 would merge dead code and putting the launcher fix in
  M5 would leave M4 shipping a discovery arm every launcher silently defeats.
  §7.2's own words are "the two changes ship together (M4/M5) and a review of
  either must check the other" — a *review* obligation across two merges, which
  is what the plan now encodes: an explicit cross-merge interlock on WI-4.3 and
  WI-5.3c, a mandatory re-verification of WI-4.3's argv goldens at the M5 head
  SHA (§4), and risk 9.
  One executability addition the finding did not cover: `cmdShimsExec` is the
  **legacy** `ac-*` path — `removeLegacyWrapperShims` (`shims.go:237-257`)
  deletes those shims at setup and only the single `ac` dispatcher is installed
  — so the row must construct a legacy shim or it silently never tests
  `shims.go`. Also **flagged** for DESIGN below.
- **B5** [DESIGN] — DESIGN §7.2 already carries the remote predicate; PLAN.md's
  WI-5.3c restates it with verified anchors (`setupCommandMatchesPool`
  **308-345**, its OR-of-two-exact-matches at `340-341`, `setupCommandFlagValue`
  at `352+` truncating at `--` at `355-358`, `stopSetupRunner` 196-224) and with
  the normal-lane row as the item's own done-when.
- **B6** [DESIGN] — DESIGN §6.9 already replaced boot *time* with `boot_ref`;
  WI-3.4 rewritten end to end: `ServeClaim.BootRef` (`lease.go:55-62`),
  population at `lease.go:160-167`, branch (d) only (`lease.go:242-244`),
  freshness first (`230-232`), equality never ordering, absent ⇒ unchanged, the
  `readClaim`/`RenewLease` round-trip (`101-114`, `317-340`) requiring a real
  struct field, an injectable `readBootRef` seam without which none of the rows
  are deterministic, and seven rows including both clock-step directions.
  Filename pin changed from `boottime_*` to `bootref_*` to match the semantics.
- **S1** FIXED — `self_check.go` added to WI-1.6 (op.Register) and WI-4.5
  (one-shot routing) file lists, with the verified anchors:
  `selfRepairRegistration` is `self_check.go:117-133`, driven from
  `self_check.go:71` and `turn_end.go:120`, and its "non-empty id even when the
  write failed" contract (`self_check.go:129-131`) must survive because
  `turn_end.go:121-123` aborts only on an empty id.
- **S2** [DESIGN] — DESIGN §3.5 already normative; PLAN.md consistent: WI-1.6
  states `resolveAgentVendor` is UNCHANGED in M1 and the four call sites branch
  in M4; WI-5.7 carries the row asserting `Vendor:nil` at all four sites and the
  suppression of `cmdServe`'s refusal. Anchor corrected: `resolveAgentVendor` is
  `identity.go:72-83`; the vendor block in `serve.go` is `152-157`.
- **S3** FIXED, with corrected anchors — the finding cited
  `pointer.go:93,105-118`; the real functions are `DiscoverPointer`
  (`pointer.go:58-103`) and `ResolvePointerTarget` (`pointer.go:113-122`), and
  the existing-directory check is not in `pointer.go` at all but in
  `absExistingDir` (`config.go:359-375`). WI-4.2 traces exactly what an `ssh://`
  pointer does today (`filepath.IsAbs` false ⇒ joined onto the pointer dir ⇒
  `Clean` collapses `//` ⇒ ENOENT on `<pointerDir>/ssh:/user@host/path`, and
  `config.go:229-234` makes a pointer error **hard**, killing the cascade before
  any ssh arm) and pins the fix.
- **S4** [DESIGN] — DESIGN §7.2/§7.3/§7.5 already carry the refusal; PLAN.md's
  WI-5.3a implements it before keygen and before any authorize call, validating
  through `wrapperForToken` (**`shims.go:51-64`** — not `dispatch.go`), and names
  the rejected dispatcher-teaching alternative so nobody re-invents it.
- **S5** FIXED — WI-4.1 pins the `HubConfig` struct verbatim from §7.4, the
  0600/0700 contract, `ReadHubConfig` with a **named** not-found error (the
  `E_NOT_JOINED` trigger) distinguishable from parse/IO errors,
  `WriteHubConfig` as temp+fsync+rename, and the explicit statement that unknown
  fields are dropped. It also resolves the layering question the finding implies:
  `internal/loop` cannot import `internal/hubclient`, so the path builder and the
  existence **stat** live in `internal/loop/remote.go` and only the struct and
  helper live in `hubclient`. WI-4.1's done-when states that every other M4 row
  runs against a hand-planted `config.json` until WI-5.3a.
- **S6** FIXED, one part **modified with an argument**. Accepted in full:
  `x/sys/unix` over an exec'd `sysctl`, with an added reason the finding did not
  give — the boot ref is read inside `AcquireServeLease`'s `withAgentLock`
  critical section, and forking under a flock is exactly the failure surface this
  codebase avoids; plus the verification that `golang.org/x/sys` is already a
  **direct** requirement (`go.mod:7`, v0.30.0, today imported only from
  `filelock_windows.go`), so rule 6's empty-`go.mod`-diff survives and
  `release.yaml:28-30` proves it.
  **Modified**: the plan pins **three** files (`bootref_linux.go`,
  `bootref_darwin.go`, `bootref_other.go` = `!linux && !darwin`) rather than the
  four the finding asked for (`…_windows.go` plus a
  `!linux && !darwin && !windows` fallback). Argument: `internal/loop`'s existing
  convention is a **total** platform partition with no uncovered GOOS and no
  third file — all four tagged pairs are `!windows`/`windows` — and a
  `bootref_windows.go` returning `""` would be byte-for-byte identical to
  `bootref_other.go` returning `""`. Three files is the minimal total partition,
  covers Windows and every other GOOS, and preserves "unreadable ⇒ unchanged
  behavior" exactly as the finding requires. If a reviewer wants the fourth file
  anyway, it is a one-line addition and nothing else in the item changes.
- **S7** FIXED — WI-3.5's W set is restated as four **hub-side** assertions
  (W1/W2/W3 rewritten to assert hub-side state only, W4/W5 already hub-side),
  and the client halves move to the new **WI-4.10**, which reuses the same
  `conformance/vectors/wire.json` definitions. The plan names the failure mode
  explicitly: a green W2 in M3 that claims client behavior is testing nothing.
- **S8** FIXED, with a correction that changes the work. `status.go:34` (before
  `Discover` at `:43`) and `guard.go:175-189` (before `Discover` at `:195`) are
  reorders, and status.go is now named in WI-4.8 — it appeared in no item
  before. **`identity_cmd.go:24` is NOT a reorder**: that file calls
  `resolveAgentID` at `:24` and **never calls `loop.Discover` at all** — it does
  not even import `internal/loop`, and its `--control-repo`/`--loop-dir` flags
  (`identity_cmd.go:13-18`) are declared and unused. So it is an **addition**, and
  the plan requires that new Discover to be **non-fatal**, because
  `agentchute identity` succeeds today in a directory with no pool and must keep
  doing so (risk 5). Also pinned: `guard`'s fail-open behavior must not become
  fail-closed. Anchors corrected: `resolveAgentID` is `identity.go:13-25`,
  `resolveAgentIDRaw` `27-39`.
- **S9** FIXED — WI-5.3a (join core) / WI-5.3b (key lifecycle) / WI-5.3c
  (same-hub migration), with the lane table assigning 5.3b and 5.3c to the
  specialist as hand-offs **inside** the M5 merge (reconciling S9 with P6's
  one-implementer-per-merge rule, stated in §2 rule 1). The `setup_wipe.go`
  H1 change is peeled into its own **WI-5.5**, with its own row and its own
  done-when, and is required to ship in the same merge as WI-5.4.
- **G1** [DESIGN] — DESIGN §7.2 already pins the sequence; WI-5.3c restates it
  step by step (`.partial` → fsync files and dir → `rename` as the commit point →
  fsync the parent → pointer → delete old; `mux/` deliberately not copied) with
  crash-injection rows on both sides of the rename.
- **G2** [DESIGN] — DESIGN §7.2 already pins the lock; WI-5.3a implements it
  (sibling `.locks/<hub-id>.lock`, 0700 on demand, taken before any probe, the
  shipped `syscall.Flock` idiom generalized from `filelock_unix.go:46-67` with
  `agentLockTimeout` `:20` / `agentLockRetryInterval` `:23`, named refusal on
  timeout), and WI-5.3c adds the two-lock ascending-order rule.
- **G3** [DESIGN] — DESIGN §7.2 already pins the grammar; WI-5.3b implements
  numeric `.v<N>` ordering, the refuse-don't-guess rule for non-numeric
  suffixes, `.invalid.<ts>` with `tsStampLayout` (`tsid.go:22,51`) at microsecond
  resolution, and **no `.pub` symlink** — the current pubkey derived as
  `readlink(...) + ".pub"`, used by both paste-line sites.
- **G4** [DESIGN] — DESIGN §7.2 already covers it; WI-5.3c's step 5 issues the
  best-effort `ssh -O exit` and states why an unreaped master is harmless.
- **Watch-item** (non-root `sshd -D`; macOS host-key/config paths) — carried
  verbatim into WI-6.1 as a budget note.

### Design defects found while revising — FLAGGED, not fixed

DESIGN.md is owned by another agent; these are recorded here instead of edited.

1. **§4.4.1's `tick-ok` example does not carry `warnings`** — the direct
   consequence of F3. The plan requires the field in the M2 spec copy (WI-2.1)
   and in the codec's fixed shape (WI-3.1a), so the design example is now the
   only place it is missing.
2. **§7.5 gives `E_NOT_REGISTERED` one exact text, but the CLI has two.**
   `send.go:145` renders `sender %q is not registered. …` and `check.go:120`
   renders `agent %q is not registered. …`. §7.5's single text is `check.go`'s
   wording, so a remote `send` will print "agent …" where a local `send` prints
   "sender …" — a one-word local/remote divergence in a catalog that is otherwise
   byte-exact. Either the catalog needs a send-side row, or §7.5 should say the
   client re-renders it per call site.
3. **§4.2/§7.4 give the mux `ControlPath` no length rule.** With
   `~/.agentchute/hub/<12 hex>/mux/%C` a sufficiently deep `$HOME` can approach
   the ~104-byte `sun_path` limit. PLAN WI-4.4 decides a contingency (threshold,
   owned-0700 temp fallback, mux-disabled third arm), but if that behavior is
   meant to be normative it belongs in §4.2 rather than in the plan.
4. **§10.3's "launcher preserves remoteness" row names `ac-*` shims as a live
   launcher, but they are legacy.** `removeLegacyWrapperShims`
   (`shims.go:237-257`) deletes generated `ac-*` shims at setup and only the
   single `ac` dispatcher is installed (`installDispatcher`, `shims.go:208-231`);
   `cmdShimsExec` (`shims.go:259-312`) is reachable only from a surviving old
   shim. As written, an implementer can satisfy the row by exercising the
   dispatcher alone while `shims.go:304-305` stays broken. The plan requires the
   row to construct a legacy shim; the design row should say so too.
5. **§6.9/§10.3's "unknown extra key still parses (mixed-version tolerance)" is
   true but easy to misread as preservation.** `readClaim` (`lease.go:101-114`)
   is a plain `json.Unmarshal` into the closed `ServeClaim` and `RenewLease`
   (`lease.go:317-340`) re-marshals only that struct, so an unknown key **parses
   and is then dropped on the first renew**. The design's own field-preservation
   paragraph says this correctly; the §10.3 row's phrasing does not, and the
   plan's WI-3.4 spells out "assert parsing, never preservation".
6. **§3.2 names two latch-arming sites but the code has three.** DESIGN cites
   `check.go:243,256`; the `setLatch` closure (`check.go:169-175`) is also called
   at `check.go:185` for the redelivered-residue path. An implementer following
   §3.2 alone would leave the redelivery path unarmed — precisely the E1 failure
   the design exists to prevent. PLAN WI-1.3 lists all three.
