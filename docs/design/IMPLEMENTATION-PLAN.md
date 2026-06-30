# agentchute "simple again" + protocol v2 — Implementation Plan (gated)

> **For agentic workers:** implement gate-by-gate. The tree MUST compile and `go test/vet/build ./...` MUST pass at the END of every gate. Each gate is one reviewable unit (4-way review or self-review). No gate merges until green + reviewed. **No push to main, no tag, no release** — land on `feat/simple-again-v2`, open a PR, stop before merge.

**Goal:** ship the two decided designs — runtime "simple again" (pull mailbox + `.live` + per-agent `serve` PTY supervisor; delete the push apparatus) and protocol v2 (one `link()`-no-clobber record substrate; per-`(sender,recipient)` seq as identity; act-then-archive consume; asker-owned obligations; serve-lease + fencing token for id-uniqueness) — via the **Hybrid** approach (4/4 team decision).

**Approach (decided 4/4):** greenfield ONLY the small spec-covered new core; keep-verbatim the battle-tested substrate (atomic delivery, PTY supervision, submit-bytes, termios/SIGWINCH); evolve-in-place the rippled bits (nonce→seq, consume, ledger ownership, gate liveness); clean-delete the push cluster (~2,792 LOC). The 7-invariant conformance suite (in-tree at `conformance/`, 25/25) is the safety net for exactly the new core + substrate.

**Branch:** `feat/simple-again-v2` (worktree `/Users/alex/code/agentchute-v2`, off `main`=v0.7.0). Live dogfood bus is decoupled (installed binary + gitignored loop state) — source edits here do not perturb it.

## Global constraints (every gate inherits these)
- `go build ./...`, `go vet ./...`, `go test ./...` all green at gate end (root module).
- `cd conformance && go test ./...` green (25/25) — the new core is proven here BEFORE it touches live paths.
- The tree compiles between gates; deletions co-remove their callers in the same gate.
- POSIX-only; argv-only wake, no shell-eval. Out of scope: auth/signing, routing/coordinator.
- Decision records are authoritative: `docs/design/agentchute-simple-again-TEAM-DECISION.md`, `docs/design/agentchute-protocol-v2-TEAM-DECISION.md`.

---

## Gate 0 — Foundation (no behavior change) — DONE/in-progress
**Goal:** in-tree safety net + remove the one compile blocker, with zero behavior change.
**Scope:**
- NEW (done): `conformance/` canonical suite in-tree (binding/inbox_binding/log_binding/durability + conformance_test + seq_durability_test + cmd/acdemo); `docs/design/*` decision records.
- EVOLVE: relocate `underAgentchuteRunner()` (`herdr_state.go:215`, ~7 lines) → a neutral home (new `runner_provenance.go`, package main) so deleting `herdr_state.go` later doesn't break `register.go` (callers `register.go:506/546/577/587/605`). Leave `herdr_state.go` otherwise intact for now.
**Acceptance:** `go build/vet/test ./...` green; `conformance` green; `underAgentchuteRunner` still resolves from register.go.
**Reviewer focus:** relocation is a pure move (no semantic change); confirm no other refs.
**Risk:** trivial.

## Gate 1 — Clean-delete the LEAST-ENTANGLED push files (codex-co-authored narrowing)
**Goal:** remove cross-agent liveness/reachability; pull-poll delivery unaffected (already redundant — `computeSelfPollResult` survives as serve's inbox scan). Deliberately NARROW — only what deletes without touching run.go, register.go autodetect, or generated hooks.
**Scope — DELETE:** `watchdog.go`; `recipient_liveness.go`; `reachability.go` (+ their `_test.go`).
**Scope — CO-TRIM (intended behavior deletions, not regressions):** `main.go` `watchdog` dispatch/help; the cooperative-waking/liveness-sweep block in `check.go`; `gate.go:167` `checkRecipientLiveness` (gate keeps own-obligation checks only); `doctor.go:817` recipient-liveness; the `reproveAndRebindOwnWake` call in `poller.go`; orphaned imports. If deleting `reachability.go` forces edits to `register.go` wake-autodetect or `run.go`, STOP and defer it to Gate 6 (do not touch those files here).
**DEFERRED (NOT Gate 1 — codex):** `migrate.go`/`guardInitLoopNamespace` → **Gate 7** (namespace/init safety, not push; `init.go:224-236`/`:297-332`). `internal/loop/herdr.go`+`tmux.go`, `wake.go` adapter registry, `runner_reachable.go` init hooks, `register.go` autodetect, run socket(`584-657`)/peer-heal(`435-570`) → **Gate 6**. detached poller / `self_poll` detached half / `doctor_service.go` → **Gate 7** (blocked until generated hooks/setup/doctor stop emitting `poller`/`self-poll`, else new installs emit dead commands).
**KEEP:** pull-poll path; `computeSelfPollResult`; `wake.go` CORE + runner adapter (until Gate 6); `wake_method`/`wake_target` fields (until Gate 6).
**Acceptance:** `go build/vet/test ./...` green; pull bus handoff works. **Compile gate:** no `cmdWatchdog`, `checkRecipientLiveness`, `recipient_liveness`, or `Reachability*` runtime refs remain.
**Risk:** medium — co-deletion is wide but compiler+tests guard it.

## Gate 2 — New protocol core (greenfield, off-bus, proven on conformance)
**Goal:** build the genuinely-new spec core and prove it on the conformance suite WITHOUT touching live paths.
**Scope — NEW (~300-500 LOC), e.g. `internal/loop/seq.go`, `internal/loop/lease.go`, `internal/loop/live.go`, `internal/loop/owed.go`:**
- **seq allocator:** per-`(sender,recipient)` durable+monotonic sequence; the committed identity is `(to, from, seq)` / canonical filename. EEXIST = "this exact message already landed".
- **serve lease + fencing token:** claim file `{id, host, pid, serve_token, started_at, last_seen}` acquired via `link`-no-clobber; fresh claim ⇒ launch fails closed; stale ⇒ reclaim via R1 liveness; `serve_token` verified on every heartbeat + seq write.
- **`.live` writer:** atomic tmp+rename `{last_seen, busy?}`.
- **asker-owned `.owed` ledger:** single-writer atomic-rename `{owed (to,from,seq) by T}`; timeout-based expiry.
**Acceptance:** `conformance` 25/25 stays green; new-core unit tests green (mirror the conformance asserts at the loop-package level: D1 fsync-order, D2 no-clobber, O1 seq-FIFO, C1 sender-crash-resume/EEXIST, lease fail-closed + fencing token mismatch). No live path changed → `go test ./...` green unchanged.
**Reviewer focus:** does the new core match the conformance contract exactly? seq scope per-(sender,recipient)? fencing token verified on BOTH heartbeat and seq write?
**Risk:** low-ish — greenfield but spec-covered (this is where the suite gives full cover).

## Gate 3 — `.live` writer + readers, together
**Goal:** presence via `.live` instead of registration `last_seen`.
**Scope — EVOLVE:** serve writes `.live` each tick (Gate 2 writer); `check.go`/`gate.go` (`StaleReg`, `gate.go:159`)/`doctor.go` READ `.live` for roster/liveness. Land writer + readers in the SAME gate (writer-alone makes the agent invisible).
**Acceptance:** `go build/vet/test ./...` green; roster + gate + doctor reflect `.live`; bus handoff green.
**Reviewer focus:** no reader still reads registration `last_seen` for liveness; stale `.live` ⇒ not-alive; busy/idle advisory only.
**Risk:** medium — touches gate/doctor; keep recipient-local gate obligations (unread/malformed/pending/corrupt-ledger).

## Gate 4 — nonce→seq identity (DUAL-READ DRAIN window) — HIGHEST RISK, isolated
**Goal:** replace the `crypto/rand` nonce identity with per-`(sender,recipient)` seq; one-and-only on-wire change.
**Scope — EVOLVE `internal/loop/inbox.go`:** filename schema (`inbox.go:52-54`) → `(to,from,seq)`; sort key (`inbox.go:333-334`) → seq (fixes the LIVE O1 random-sort bug); `InferSenderFromFilename`; ledger keys `OriginalFilename`→`(to,from,seq)`; archive names. **DUAL-READ DRAIN:** accept BOTH nonce- and seq-named files so in-flight messages aren't orphaned at cutover.
**Acceptance:** prove O1/D1/D2/C1 on `conformance` FIRST; then `go build/vet/test ./...` green; bus handoff green; dual-read verified (a pre-existing nonce-named message is still consumed).
**Reviewer focus (codex):** the filename schema IS the wire identity — verify sort, ledger dedup, archive, sender-inference all consistent; verify the dual-read window doesn't double-deliver.
**Risk:** HIGH — single load-bearing ripple. Ship ALONE, harness-green first, never bundled.

## Gate 5 — consume flip + obligation flip
**Goal:** at-least-once + idempotent consume; asker-owned obligations.
**Scope — EVOLVE `check.go` + `internal/loop/ledger.go`:** consume archive-at-display (`check.go:197-223`) → **act-then-archive** + `.consumed` high-water + idempotent-handler contract + sender-crash-resume/EEXIST. Ledger ownership recipient→**asker `.owed`** + timeout (machinery/locking kept); recipient `reply_required` becomes advisory.
**⚠️ THE COMMIT-BOUNDARY TRAP (codex, load-bearing):** the CLI `check` prints and EXITS; the agent acts AFTER `check` returns. So archiving *during* `check` (today's behavior) is at-most-once for the WORK no matter what the in-memory conformance C1 says. Gate 5 MUST define the real cross-turn boundary: `check` **claims/displays** (move to a `claimed/` or mark in-place, NOT archive); **commit (archive) happens only after** the controlled finish/ack path confirms the message was acted on (e.g. the Stop/gate hook, or an explicit `ack`). A crash between claim and commit re-delivers (at-least-once). Designing this boundary is the gate's real work — in-memory C1 green is necessary but NOT sufficient; prove the live cross-turn path too.
**Acceptance:** `conformance` C1 (incl. sender-crash-resume) green; `go build/vet/test ./...` green; reply-required bus handoff via asker `.owed` works; dead-recipient surfaces as asker's expired obligation; **live cross-turn claim→commit demonstrated** (a message survives a crash between `check` and finish). **Compile gate:** no display-path `ArchiveMessage` before the explicit consume commit; no recipient-owned pending-ledger authority for new messages.
**Reviewer focus:** the claim/commit boundary is real (not in-memory-only); crash window re-acts in-flight mail → handlers idempotent (contract surfaced loudly); gate reads asker `.owed` for the asker side.
**Risk:** HIGH — semantic flip with a cross-process commit boundary the conformance harness can't fully model.

## Gate 6 — run→serve + collision-guard rebase + strip remaining push
**Goal:** finish serve; remove the last push surfaces now that serve owns launch.
**Scope — EVOLVE:** `run`→`serve` (keep PTY/termios/SIGWINCH/submit-bytes/injection-window VERBATIM; runtime decision: evolve, don't rewrite); collision guard (`refuseLiveRunnerCollision:402`) socket-ping → `processAlive` + `.live`-freshness + **fencing-token lease** (Gate 2); strip `pollOnce` reprove (`run.go:685`). **DELETE now-dead:** `wake_method`/`wake_target` fields + `wake.go` dispatcher + remaining runner-socket refs.
**Acceptance:** `go build/vet/test ./...` green; rebase `run_test.go:121` collision test; bus handoff via `serve` (codex submit-bytes still exact — `run_test.go:61-97` MUST stay green VERBATIM).
**Reviewer focus:** PTY/submit-bytes/termios UNCHANGED; lease integrated into launch + heartbeat + seq write; no wake field readers remain.
**Risk:** medium — the crown-jewel PTY code must stay byte-identical (its 3 exact-byte tests are the guard).

## Gate 7 — setup/doctor/shims trims + final cleanup
**Goal:** collapse the surfaces neither design needs; finalize field cuts + envelope.
**Scope — EVOLVE:** `shims.go` `run`→`serve` verb (1 verb, 3 refs); `setup.go` wake-combo collapse; `doctor.go` rewrite subsystem-tied checks (keep the framework + the 7 subsystem-free checks); finalize `registration.go` field cuts (drop reachability/status/restart/launched_by/shim_name/hook_event; keep id/last_seen→.live/v/vendor?/host?); envelope cuts in `send.go` (`to`/`message_id`/`task`/`status` → drop or compat-read for one release; normative = from/reply_required?/in_reply_to?/idempotency_key?); vendor-namespacing → fixed `.agentchute/loop`.
**Acceptance:** full `go build/vet/test ./...` green; `doctor` green; `setup` green; `conformance` green.
**Reviewer focus:** old envelope fields readable for one release (compat); no orphaned readers; PATH/profile/reset-ordering untouched (keep-verbatim).
**Risk:** medium — wide but mostly mechanical.

## Gate 8 — Final integration + PR
**Goal:** prove the whole thing and hand off for review→release.
**Scope:** full suite green; a real multi-agent bus handoff end-to-end (alice→bob reply-required via serve + seq + `.live` + asker `.owed`, carol stale via `.live`); update CHANGELOG/docs; 4-way review of the entire diff; open the PR.
**Acceptance:** `go build/vet/test ./...` + `conformance` green; live handoff demonstrated; 4-way (or self-review) sign-off; PR opened on `feat/simple-again-v2`. **STOP before merge.**
**Reviewer focus:** whole-diff integrity; no push-cluster remnant reachable (compile-time removal gates); semantic-leakage check (no timestamp sort, no recipient-ledger authority, `.live` seen by gates, no stale runner bypassing the lease).
**Risk:** integration — the prior gates' greenness compounds here.

---

## Sequencing invariants (codex's containment, adopted)
- Steps that change the wire (Gate 4) ship ALONE, harness-green first, behind the dual-read drain.
- Compile-breakers (omitted callers, `underAgentchuteRunner` relocate, orphaned imports) land in Gates 0-1 BEFORE any protocol edit → tree always compiles between gates.
- New core proven OFF-BUS (Gate 2) before any cutover (Gates 3-6).
- Keep dogfood on the installed (old) binary until the new path passes full `go test/vet/build` + a real bus handoff.
- Add compile-time removal gates so push code can't silently remain reachable.

## Biggest risk (carry it explicitly)
nonce→seq (Gate 4): the filename schema IS wire identity → sort + ledger keys + archive + sender-inference. Botch = O1 + dedup break at once, untestable on the live bus. Mitigation: prove off-bus on O1/D1/D2/C1; dual-read drain; ship as its own gate.

## Resolved with codex (co-author, 2026-06-30)
- **Package boundary:** `internal/loop/{seq,lease,live,owed}.go` — NOT a new `internal/protocol` package (the substrate/config/locks/registration/inbox/atomic helpers already live in `internal/loop`; a new package duplicates FS concepts or creates import pressure). Keep the boundary by file/API discipline; conformance stays the independent harness.
- **Compat window:** one release + drain-empty, no operator signal. Stop EMITTING old envelope fields at the cutover gate but compat-READ for one release; dual-read nonce+seq filenames for one release; don't remove the legacy reader until a drain check scans all live inboxes and reports zero nonce-named files (archives may stay old-format — only live streams block removal); add a visible doctor/gate warning for legacy live files so drain state is observable.

## Compile-time verification gates (codex; run as grep gates at each gate's close)
- **After Gate 1:** no `cmdWatchdog` / `checkRecipientLiveness` / `recipient_liveness` / `Reachability*` runtime refs.
- **After Gate 3:** gate/doctor/status liveness reads `.live`, not registration `last_seen`.
- **After Gate 4:** no nonce writer path (`generateNonce`, `_msg-`) except legacy parser/tests; all identity call sites use `(to,from,seq)`.
- **After Gate 5:** no display-path `ArchiveMessage` before the explicit consume commit; no recipient-owned pending-ledger authority for new messages.
- **After Gate 6/7:** no runtime `wake_method` / `wake_target` / `PokeWakeTarget` / tmux/herdr/runner wake-adapter refs except compat docs/tests explicitly marked for removal.
