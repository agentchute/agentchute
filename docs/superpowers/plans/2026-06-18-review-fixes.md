# agentchute Review-Fixes Implementation Plan

> **For agentic workers:** This plan is executed by the **agentchute fleet** (codex, gemini-cli, grok, claude) on the live bus, not by a single engineer. Each work-item (WI) is authored by its assigned lane, then passes a **4-way verification gate** before it is considered done. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Fix every verified finding from the 2026-06-18 four-way project review of agentchute v0.6.2, each landed on a branch and confirmed by a real 4-way review.

**Architecture:** The integrity core is a set of lock-free read-modify-write paths (registration + ledger) plus a finish-gate whose obligations are non-atomic and partly peer-controlled. Fixes add per-agent file locking, reorder record-before-archive, key obligations on the canonical filename, validate/bind wake targets, and decouple liveness from the gate. Docs/spec drift is reconciled separately.

**Tech Stack:** Go (module `github.com/agentchute/agentchute`), stdlib only; POSIX `flock` via `golang.org/x/sys/unix` or `syscall` with a Windows fallback; existing `internal/loop` package owns the protocol core.

## Global Constraints

- Go stdlib only unless a dep already exists in `go.mod`; prefer `syscall`/`x/sys` for `flock`.
- All file writes stay atomic (temp + rename/link); never weaken existing atomicity.
- Threat model: 2–10 cooperative same-user agents on a shared FS. Harden against malformed/peer-crafted registrations and message frontmatter; do NOT assume a remote adversary.
- No behavior change to the on-wire message/registration format without a spec note in `AGENTCHUTE.md` and a `templates_drift_test.go`-passing update.
- `go vet ./...` clean and `go test ./...` green after every WI. Claude re-runs the full suite per WI; grok/gemini "green" claims are NOT trusted as evidence.
- Work lands on branch `fix/review-2026-06-18`. **No merge / tag / push to main** — Alex authorizes release. "Done" = all WIs 4-way-verified, suite green, reported.

---

## Coordination protocol

### Assignment matrix (routed by reviewer character from the review)

| WI | Title | Author | Rationale |
|----|-------|--------|-----------|
| WI-1 | Per-agent write locking (registration + ledger) | **claude** | concurrency core; most reliable lane for locking |
| WI-2 | Record-before-archive + filename-keyed obligations | **codex** | protocol-integrity ordering — his strength; he found it |
| WI-3 | `wake_target` validation + recipient-binding | **claude** | security; found via claude security pass |
| WI-4 | Decouple liveness from gate + heartbeat-after-work + clock-skew clamp + PID identity | **claude** | liveness/concurrency |
| WI-5 | `WakeAdapter.Reachable`/`Resolve` interface refactor | **codex** | careful structural refactor across loop/root boundary |
| WI-6 | Runner/watchdog polling: `WithSkipped` + snapshot seen files | **codex** | ordering/poll-loop correctness |
| WI-7 | Docs + spec-cite reconciliation (Tier 3) + PATH-marker dedup | **gemini-cli** | prose-shaped, direct-edit execution |
| WI-8 | `i>100` id-suffix fallthrough → return error | **grok** | bounded, mechanical, trivially verifiable |
| WI-9 | `update` ordering: idempotent writes before destructive reset + `--no-resync` | **codex** | sensitive setup/update path; codex conservative here |
| WI-10 | Consolidate 3 frontmatter parsers → one canonical (deferrable if refactor risk high) | **codex** | careful behavior-preserving refactor |

### 4-way verification gate (per WI)

1. Author edits the tree, writes tests, runs `go test ./...`, commits on `fix/review-2026-06-18`, then `agentchute send` a `review-request` (with `--ask`) to the **other three lanes** naming the commit SHA + WI id.
2. Each of the other three does a **real** read-only review of that commit's diff and replies with an explicit verdict: `APPROVE` / `REJECT` + reasons + any file:line concerns. No rubber-stamps — a REJECT with substance is the expected default if anything is off.
3. **Claude independently re-runs `go vet ./... && go test ./...`** and reads the diff regardless of who authored it (greens are not trusted on assertion).
4. Gate rule: a WI is DONE only when ≥3 of 4 lanes APPROVE **and** claude's suite run is green **and** no unaddressed REJECT remains. A substantive REJECT bounces the WI back to its author.
5. No lane signs off (`agentchute gate`) with open review obligations; use `defer` only with a logged reason.

### Execution safety (live shared checkout)

- **Authoring is serialized** — only one WI is in its edit phase at a time, because all lanes share one working tree. Verification (read-only) runs in parallel.
- Claude is the integrator/sequencer: announces "WI-N edit phase open → @author", waits for the author's commit, opens the 4-way gate, collects verdicts, then opens the next WI.
- Branch `fix/review-2026-06-18` is created off `main` while the bus is quiet; `.agentchute/loop/` runtime state is left untouched.

---

## WI-1: Per-agent write locking (registration + ledger) — author: claude

**Files:**
- Create: `internal/loop/filelock_unix.go`, `internal/loop/filelock_windows.go` (build-tagged `withAgentLock(cfg, agentID, fn) error`)
- Create: `internal/loop/filelock_test.go`
- Modify: `internal/loop/registration.go:209-229` (`UpdateLastSeen`, `UpdateLastActive`)
- Modify: `internal/loop/ledger.go:183-313` (`RecordPendingReply`, `MarkPendingReplied`, `MarkPendingDeferred`)
- Modify: `run.go:370-379` (`markRunnerOffline`), `run.go:436-440` (`clearStaleRunnerWakeTargets`)

**Interfaces:**
- Produces: `func withAgentLock(cfg *loop.Config, agentID string, fn func() error) error` — acquires an exclusive `flock` on `AgentStateDir(agentID)/.lock` (created 0600), runs `fn`, releases. Windows fallback uses `LockFileEx`/`O_EXCL` create-loop. Bounded blocking wait (e.g. 5s) then error.

**Fix spec:** Every registration/ledger read-modify-write must run inside `withAgentLock(cfg, agentID, ...)` so concurrent writers to one agent's files cannot lose updates. `markRunnerOffline` must happen-after a confirmed poll-loop exit.

- [ ] **Step 1:** Write failing test `TestWithAgentLock_SerializesConcurrentLedgerAppends`: 50 goroutines each `RecordPendingReply` a distinct message_id; assert final ledger has all 50 entries (no lost update).
- [ ] **Step 2:** Write failing test `TestUpdateLastSeen_NoLostUpdateUnderConcurrency`: concurrent `UpdateLastSeen` + a status mutation; assert no torn/clobbered status.
- [ ] **Step 3:** Write failing regression test `TestRunnerOfflineNotResurrected`: simulate `markRunnerOffline` racing `UpdateLastSeen`; assert terminal status stays `offline`.
- [ ] **Step 4:** Run the three tests; expect FAIL.
- [ ] **Step 5:** Implement `withAgentLock` (unix + windows) and wrap the RMW funcs; ensure `markRunnerOffline` is sequenced after poll-loop stop+join.
- [ ] **Step 6:** Run `go test ./internal/loop/... ./... -run 'Lock|LastSeen|Resurrect'`; expect PASS.
- [ ] **Step 7:** `go vet ./... && go test ./...`; commit.

**Also folded in:** `atomicWriteFile` (`registration.go:456-499`) — set `cleanup=false` immediately after a successful `os.Rename`, so a post-rename `syncDir` failure is not reported as a write-failure while the new content is already live. Add `TestAtomicWrite_SyncDirFailureAfterRenameNotReportedAsWriteFail`.

**Acceptance:** stress tests green; no lost updates; dead runner cannot be resurrected; no deadlock (bounded wait); post-rename fsync failure not mis-reported.

---

## WI-2: Record-before-archive + filename-keyed obligations — author: codex

**Files:** Modify `check.go:199-212` (reorder), `check.go:279-299` (`recordReplyObligation` keying), `internal/loop/ledger.go:90,183-238` (collision semantics); Test: `check_test.go`, `check_ledger_test.go`.

**Fix spec:**
1. **Record before archive.** In `check`, durably `recordReplyObligation` (or stage it) **before** `ArchiveMessage`. On ledger failure, do NOT archive — quarantine or leave in inbox so `gate` still sees the obligation. Eliminates the silent gate-clear.
2. **Key the obligation on the canonical filename**, not sender-supplied `message_id`. The recipient already trusts the filename; use it as the obligation primary key so a peer cannot force-block, suppress, or wedge the consume loop with duplicate `message_id`s. `ErrLedgerEntryCollision` must no longer be fatal in the consume loop — dedupe on filename, not message_id.

- [ ] **Step 1:** Failing test `TestCheck_LedgerFailureDoesNotArchive`: inject a ledger-write failure; assert the message remains consumable / quarantined and the obligation is recoverable.
- [ ] **Step 2:** Failing test `TestCheck_DuplicateMessageIDDoesNotWedge`: deliver two `reply_required` msgs with same `message_id`, different filenames; assert both record as distinct obligations and `check` does not error out.
- [ ] **Step 3:** Run; expect FAIL.
- [ ] **Step 4:** Reorder record-before-archive with rollback/quarantine on failure; switch obligation key to filename; make collision non-fatal.
- [ ] **Step 5:** Run targeted tests; expect PASS.
- [ ] **Step 6:** `go vet ./... && go test ./...`; commit; send review-request.

**Also folded in (codex, same area):**
- `register.go:173` — create/verify inbox + state dirs **before** publishing the registration (or roll back on post-publish setup failure), so peers never observe a live reg with no inbox. Test `TestRegister_InboxExistsBeforeRegistrationVisible`.
- `internal/loop/inbox.go:198` — use a unique `os.CreateTemp` path per attempt (not a deterministic `.tmp_<final>`), so two concurrent same-sender collisions can't overwrite the shared temp and deliver the wrong body. Test `TestWriteInboxMessage_ConcurrentSameSenderNoBodyMixup`.
- `gate.go:127-130` — on corrupt/oversized ledger JSON, block with an actionable quarantine instruction (like the malformed-inbox path) instead of a fatal exit that bricks every gate phase. Test `TestGate_CorruptLedgerQuarantinesNotFatal`.

**Acceptance:** no archive on ledger failure; duplicate `message_id` cannot wedge `check`; gate cannot silently clear a reply-required obligation; registration never visible before its inbox; concurrent same-sender writes never mix bodies; corrupt ledger is recoverable, not fatal.

---

## WI-3: `wake_target` validation + recipient-binding — author: claude

**Files:** Create `internal/loop/wake_target.go` (`ValidateWakeTarget(method, target) error`) + test; Modify `internal/loop/registration.go:231-275` (`Validate` calls it), `internal/loop/tmux.go`, `internal/loop/herdr.go` (validate before poke), `send.go:275`/`computeWakeReceipt` + runner adapter (recipient-binding).

**Fix spec:**
- `ValidateWakeTarget`: tmux → `^%[0-9]+$` or `^[A-Za-z0-9_-]+:[0-9]+\.[0-9]+$`; herdr → agent-id slug rule; runner → `unix:` path must equal `cfg.RunnerSocketPath(recipientID)` (or temp-fallback path). Reject leading `-` for all. Enforced at registration `Validate()` AND immediately before every poke.
- Runner wake/liveness becomes recipient-aware: refuse to write to a socket whose path ≠ recipient's, or whose ping `AgentID` ≠ recipient.

- [ ] **Step 1:** Failing test `TestValidateWakeTarget_RejectsForeignPaneAndSocket` (table: foreign tmux pane `%0`, leading-`-`, arbitrary `unix:/tmp/evil.sock` for recipient X).
- [ ] **Step 2:** Failing test `TestComputeWakeReceipt_RefusesUnboundRunnerSocket`.
- [ ] **Step 3:** Run; expect FAIL.
- [ ] **Step 4:** Implement validator + wire into `Validate()` and pre-poke; add recipient-binding to runner wake/liveness.
- [ ] **Step 5:** Targeted tests PASS.
- [ ] **Step 6:** `go vet ./... && go test ./...`; commit; send review-request.

**Also folded in (claude, security):**
- `identity.go:17-24` — call `ValidateAgentID` **inside** `resolveAgentID` so traversal-safety is structural, not dependent on every call site re-validating. Test `TestResolveAgentID_RejectsTraversal`.
- `internal/loop/registration.go:25` (`ReadFileLimit`) — open with no-follow semantics where available and verify the fd with `fstat`, closing the Lstat→Open TOCTOU symlink-swap window. Test `TestReadFileLimit_NoFollowTOCTOU`.
- `watch.go:309` (`osNotify`) — stop hand-building AppleScript from untrusted `task`; pass `task` as `osascript` argv (`item 1 of argv`) or strip control chars + cap length. Test `TestOSNotify_TaskNotInterpolatedIntoScript`.
- `internal/loop/config.go:165` — runner socket temp fallback uses a per-user root (`/tmp/agentchute-run-<uid>` or `os.UserCacheDir()`) and verifies dir ownership before bind, removing the predictable-path squat/DoS.

**Acceptance:** a crafted registration cannot poke a pane/socket it doesn't own; `resolveAgentID` rejects traversal at the source; no symlink-swap window; untrusted `task` never reaches an interpreter as code; socket temp path is per-user; existing valid targets still wake.

---

## WI-4: Decouple liveness from gate + heartbeat-after-work + clock-skew clamp + PID identity — author: claude

**Files:** Modify `gate.go:283-295` (liveness → warning when nothing owed), `poller.go:182-232` (heartbeat after successful poll), `internal/loop/poller.go:84` (`PollerFreshness` clamp negative age), `active_session.go:120-124` (require fresh heartbeat AND PID identity); Tests: `gate_test.go`, `poller_test.go`, `self_poll_test.go`.

**Fix spec:**
1. In `evaluateGatePhase`, for `finish`/`continue`: if inbox empty AND no pending replies AND not missing-reg, downgrade `!LivenessOK` from blocking to a warning. (Keep blocking when work is owed.)
2. `pollerTick` refreshes the heartbeat **only after** `computeSelfPollResult` succeeds; record `last_error` consulted by liveness.
3. `PollerFreshness` (and `active_session` freshness): clamp small negative ages to fresh, matching `watchdog.go:190`.
4. Active-session liveness requires `processAlive(PID)` **and** a reasonably fresh heartbeat (or process start-time identity), not PID-existence alone.

- [ ] **Step 1:** Failing test `TestGate_EmptyInboxDeadPoller_DoesNotBlockFinish`.
- [ ] **Step 2:** Failing test `TestPollerTick_NoHeartbeatRefreshOnPollError`.
- [ ] **Step 3:** Failing test `TestPollerFreshness_FutureTimestampIsFresh`.
- [ ] **Step 4:** Run; expect FAIL.
- [ ] **Step 5:** Implement the four changes.
- [ ] **Step 6:** Targeted tests PASS.
- [ ] **Step 7:** `go vet ./... && go test ./...`; commit; send review-request.

**Also folded in (claude):** `recipient_liveness.go:33-47` — surface the dead-session reason in the `stalePollerLiveness` message instead of discarding it (`_ = reason`). `poller.go:308-317` — lengthen the `poller ensure` first-heartbeat deadline (or check spawned-PID liveness) so a slow-disk host doesn't spuriously report "no fresh heartbeat."

**Acceptance:** the documented dead-poller deadlock is gone for zero-owed agents; a crashed poller no longer shows fresh; clock skew no longer false-blocks; PID reuse no longer passes liveness alone; liveness messages name the real reason; `poller ensure` doesn't false-fail on slow disks.

---

## WI-5: `WakeAdapter.Reachable`/`Resolve` interface refactor — author: codex

**Files:** Modify `internal/loop/wake.go` (extend interface), `internal/loop/tmux.go`, `internal/loop/herdr.go`, `internal/loop/runner.go` (implement), `recipient_liveness.go:78-101` + `registrationHasReachableWake` (call interface, delete method-name switch), `tmux_state.go`/`herdr_state.go` (move reachability behind adapter). Tests: new `wake_test.go` cases.

**Fix spec:** Add `Reachable(cfg, recipientID, reg) bool` (and `Resolve` where needed) to the `WakeAdapter` interface so reachability/identity dispatch lives in one place. `recipient_liveness.go` and `registrationHasReachableWake` consume the interface instead of re-switching on method strings. Behavior-preserving refactor.

- [ ] **Step 1:** Failing test asserting `recipient_liveness` reachability for each adapter routes through the interface (e.g. a fake adapter registered in-test is consulted).
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Extend interface; implement per adapter; delete the duplicated switches.
- [ ] **Step 4:** Full suite (refactor must not change behavior) PASS.
- [ ] **Step 5:** `go vet ./... && go test ./...`; commit; send review-request.

**Acceptance:** adding an adapter touches only its file + registry; no method-name switch remains in `recipient_liveness.go`; all existing tests pass unchanged.

---

## WI-6: Runner/watchdog polling — `WithSkipped` + snapshot seen files — author: codex

**Files:** Modify `run.go:579` (runner poll), `watchdog.go:173,188` (use `ListInboxMessagesWithSkipped`, mtime-based age cap). Tests: `run_test.go`, `watchdog_test.go`.

**Fix spec:** Runner/watchdog wake on **any unseen** inbox item (valid or skipped/malformed), tracked by a seen-filename snapshot, not just the lexicographically-newest valid filename. Cap future/skewed filename timestamps using filesystem arrival/mtime so back-dated mail still wakes the repair turn that clears a gate.

- [ ] **Step 1:** Failing test `TestRunnerPoll_WakesOnMalformedFile`.
- [ ] **Step 2:** Failing test `TestRunnerPoll_WakesOnBackdatedFilename`.
- [ ] **Step 3:** Run; expect FAIL.
- [ ] **Step 4:** Switch to `...WithSkipped` + seen-snapshot; cap future timestamps via mtime.
- [ ] **Step 5:** Targeted tests PASS.
- [ ] **Step 6:** `go vet ./... && go test ./...`; commit; send review-request.

**Also folded in (codex):** `watchdog.go:114-129` — on the watchdog's OWN registration being deleted mid-run (e.g. a `setup`/`update` reset race), log-and-continue instead of returning a hard error that kills the daemon loop. Test `TestWatchdog_SelfRegDeletedDoesNotKillDaemon`.

**Acceptance:** malformed and back-dated inbox files reliably trigger a wake; a reset race doesn't permanently stop the watchdog.

---

## WI-7: Docs + spec-cite reconciliation (Tier 3) — author: gemini-cli

**Files:** `HANDOFF.md` (bump v0.6.1→v0.6.2 + runtime-reset bullet; fix stale herdr `\r` description at :66), `AGENTCHUTE.md:166` (§8 add `agentchute-run`), `internal/loop/wake.go:13` (refresh stale "ships only tmux" comment), `EXTENSIONS.md:9-15` (reframe "v0.1 ships only tmux" to include herdr+runner), `README.md:158` (drop/annotate orphan Gemini `AfterAgent` row), the 68 stale `§`-cites across `*.go` (reconcile to current `AGENTCHUTE.md` numbering or remove), and remove the stale committed `./agentchute` v0.3.7 binary (or gitignore it).

**Fix spec:** Docs match the v0.6.2 binary. Spec-cite reconciliation: for each `§x.y` comment with no target in current `AGENTCHUTE.md`, either update to the correct current section or drop the cite. No code behavior change.

- [ ] **Step 1:** Grep-enumerate every stale `§`-cite (`grep -rnE '§[0-9]' --include=*.go`) and cross-check against `AGENTCHUTE.md` headings; produce the fix list.
- [ ] **Step 2:** Apply doc edits + binary removal; ensure `templates_drift_test.go` still passes.
- [ ] **Step 3:** `go test ./...` (drift test) PASS; commit; send review-request.

**Also folded in (gemini, low-risk shell/setup):** `setup.go` + `install.sh` — make `setup` recognize and supersede install.sh's fish PATH-marker (one managed region, not two); reject `"`/`$`/`` ` ``/`\` in `--shim-dir` (`setup.go:1012`/`install.sh:348`). Claude reviews these shell edits closely.

**Acceptance:** HANDOFF reflects v0.6.2; no doc claims tmux-only; no orphan README row; no dangling `§`-cite; no committed binary; single fish PATH region; shim-dir rejects shell metacharacters; drift test green.

---

## WI-8: `i>100` id-suffix fallthrough → return error — author: grok

**Files:** Modify `identity.go:144-163` (`availableContextualAgentID`) and `register.go:218-234` (`nextContextualAgentIDByFilesystem`) to return an explicit error past the cap instead of a colliding candidate; propagate the error to callers. Tests: `identity_test.go`, `register_test.go`.

**Fix spec:** Past the suffix cap, return an error ("could not allocate a free agent id after N attempts") rather than a known-reserved id, so two lanes never silently share an inbox.

- [ ] **Step 1:** Failing test `TestAvailableContextualAgentID_ErrorsPastCap` (pre-create 1..101 registrations; assert error, not a colliding id).
- [ ] **Step 2:** Run; expect FAIL.
- [ ] **Step 3:** Return error past cap; update callers.
- [ ] **Step 4:** Targeted tests PASS.
- [ ] **Step 5:** `go vet ./... && go test ./...`; commit; send review-request.

**Acceptance:** past the cap, allocation errors cleanly; no colliding id is ever returned.

---

## WI-9: `update` ordering + `--no-resync` — author: codex

**Files:** Modify `update.go:100-107` (allow `--no-resync` / binary-only update when saved setup.json absent), `update.go:190-207` (ordering), `setup.go`/`setup_reset.go` (sequence idempotent writes before destructive reset). Tests: `update_test.go`, `setup_test.go`.

**Fix spec:** Reorder `update` so the recoverable, idempotent steps (template/hook/shim/ENROLLMENT writes) run **before** the destructive runtime reset (stop pollers/runners, clear registrations). A mid-failure then leaves wake infrastructure intact and a re-run recovers cleanly. Add `--no-resync` for binary-only refresh when no saved setup state exists, instead of hard-aborting.

- [ ] **Step 1:** Failing test `TestUpdate_NoResyncAllowsBinaryOnlyWhenNoSetupState`.
- [ ] **Step 2:** Failing test `TestSetup_TemplatesWrittenBeforeRuntimeReset` (inject a reset failure; assert hooks/shims already present).
- [ ] **Step 3:** Run; expect FAIL.
- [ ] **Step 4:** Reorder; add `--no-resync`.
- [ ] **Step 5:** Targeted tests PASS.
- [ ] **Step 6:** `go vet ./... && go test ./...`; commit; send review-request.

**Acceptance:** a setup failure during `update` never leaves a half-wired bus with no wake infra; binary-only update works without a forced destructive reset.

---

## WI-10: Consolidate frontmatter parsers (deferrable) — author: codex

**Files:** `internal/loop/inbox.go:72-84` (`InferSenderFromFrontmatter`), `internal/loop/registration.go:289` (`parseFrontmatter`), `internal/loop/message.go:160-181` (`ParseMessageFrontmatter`). Tests: parser tests across all three call sites.

**Fix spec:** Replace the three subtly-divergent frontmatter parsers with one canonical parser (consistent quote/indentation/comment handling) used by the consume-path recorder and the validate-path, removing the class of validator/recorder skew bugs. **Behavior-preserving** — every existing parse test must pass unchanged. **Deferrable:** if the 4-way review judges the regression risk too high for fleet execution, mark this WI as a tracked follow-up rather than forcing it.

- [ ] **Step 1:** Characterization tests capturing current parse behavior of all three on a shared fixture set (quotes, indentation, comments, CRLF).
- [ ] **Step 2:** Introduce the canonical parser; route all three call sites through it.
- [ ] **Step 3:** Full suite PASS unchanged.
- [ ] **Step 4:** `go vet ./... && go test ./...`; commit; send review-request.

**Acceptance:** one parser, all call sites green, no behavior change; OR a documented deferral verdict from the 4-way gate.

---

## Sequence & done-definition

Authoring order (serialized): **WI-1 → WI-3 → WI-4 → WI-2 → WI-9 → WI-6 → WI-5 → WI-8 → WI-7 → WI-10.**
(Locking first since other fixes rely on it; WI-9 update-ordering before the structural refactor; WI-5 refactor and WI-10 parser-consolidation late to avoid churn; WI-7 docs near-last; WI-8 mechanical slots in cheaply.) Each WI's 4-way verification gate runs to completion before the next WI's edit phase opens.

**DONE** = all WIs 4-way-verified (≥3/4 APPROVE, no open REJECT; WI-10 may close as a documented deferral), `go vet ./... && go test ./...` green on `fix/review-2026-06-18`, plan checkboxes complete → report to Alex for the merge decision. **No merge/push/tag without his authorization.**
