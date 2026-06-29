# agentchute Enrollment-Reliability + Docs Implementation Plan

> **For agentic workers:** executed by the **agentchute fleet** (claude authors via subagents; codex + gemini-cli + grok review). Each work-item (WI) passes a **4-way verification gate** before it's done. Steps use checkbox (`- [ ]`) syntax.

**Goal:** make agentchute enrollment automatic, self-healing, and verifiable (so a human never has to ask "did you register?"), and make the protocol/enrollment docs clear + accurate vs the binary — **building every capability but leaving all new behavior opt-in/inert and NOT activated on the live bus.**

**Architecture:** Two tracks. **Track 1 (code)** fixes the core defect — registration and reachability are two facts agentchute never reconciles — via a verifiable view (D), a self-healing reachability fact (B), opt-in launcher hardening + launch provenance (A), an opt-in host presence daemon (C), and opt-in multi-wake escalation (F). **Track 2 (docs)** makes the enrollment blocks + protocol spec + CLI help clear and binary-accurate, fixing the GENERATED templates so `setup`/`update` can't reintroduce drift.

**Tech Stack:** Go (module `github.com/agentchute/agentchute`), stdlib + `golang.org/x/sys` (already a dep). `internal/loop` is the protocol core.

## Global Constraints (apply to EVERY WI)

- **Build everything; activate nothing.** All NEW behavior is opt-in / inert by default: the runner default is NOT flipped (A stays opt-in), the presence daemon (C) is off by default, multi-wake escalation (F) is off by default. Default runtime behavior is unchanged unless a user explicitly opts in.
- **Do NOT touch the live running bus.** Source changes on branch `feat/enrollment-reliability-and-docs` only — do NOT rebuild/reinstall the `agentchute` binary, do NOT run `setup`/`update`/`register` against the live `.agentchute/loop/`. Alex adopts on merge.
- **Keep `IsPokable()` STRUCTURAL** (non-empty wake strings). Reachability is a SEPARATE cached fact (`IsReachable`/`ReachableAt`), consulted via cache+TTL — never a live network probe on every send (codex guardrail).
- Go stdlib + existing deps only. Atomic writes stay atomic (temp+rename/link). Threat model: 2–10 cooperative same-user agents on a shared FS.
- **No wire/registration-format change without** an `AGENTCHUTE.md` spec note AND a passing `templates_drift_test.go`. New registration fields must be backward-compatible (absent = old behavior). **(codex) Each field-adding WI (E2, E3, E5) MUST update the `AGENTCHUTE.md` §5.1 field docs IN THE SAME COMMIT** — do not defer the spec note to a later docs WI.
- **(codex) Reachability cache is ADVISORY only.** It informs status / gate / escalation. It MUST NOT suppress inbox delivery (`send` always enqueues) or the structural poke attempt (`PokeRegistration` always tries the wake) — a stale/false cache can never silence durable mail.
- `go vet ./...` clean, `go test -race ./...` green, `GOOS=windows go build ./...` builds after every WI. **Claude re-runs the full suite per WI; grok/gemini "green" claims are NOT trusted as evidence** (verify the code, not just the test run).
- Branch only. **No merge / tag / push to main** — Alex authorizes release. "Done" = all WIs 4-way-verified + suite green + reported → hybrid PR for Alex's merge.

## Coordination protocol

### Assignment (claude authors via subagents — applies codex's depth to REVIEW; grok/gemini author their doc strengths)
| WI | Title | Author |
|----|-------|--------|
| E1 | Reachability-aware status/doctor + read-only presence scan (D) | claude |
| E2 | Self-healing reachability fact (B) | claude |
| E3 | Launcher hardening (detect+warn) + launch provenance (A, opt-in) | claude |
| E4 | Host presence daemon (C, opt-in/inert) | claude |
| E5 | Multi-wake escalation (F, opt-in/inert) | claude |
| D1 | Enrollment-block clarity — templates + regenerated wrapper/AGENTS docs | gemini-cli |
| D2 | AGENTCHUTE.md protocol-spec accuracy | codex |
| D3 | README.md accuracy | gemini-cli |
| D4 | EXTENSIONS.md + HANDOFF.md accuracy | grok |
| D5 | CLI `--help` + code-comment fixes | codex |

### 4-way gate (per WI)
1. Author commits on the branch, then sends a `review-request` (`--ask`) naming the commit SHA + WI id to the other three lanes.
2. Each of the other three does a real read-only review of that commit's diff → explicit **APPROVE / REJECT** + file:line. Substantive REJECT is the expected default if anything is off.
3. **Claude independently re-runs `go vet ./... && go test -race ./...`** and reads the diff regardless of author.
4. DONE only when ≥3/4 APPROVE **and** claude's suite is green **and** no unaddressed REJECT — **and codex has weighed in** (do not close at 3/4 before codex; it is the deep backstop).
5. Authoring is **serialized** (one editor on the shared tree at a time); reviews run in parallel.

---

# TRACK 1 — Enrollment reliability (code)

## WI-E1 — Reachability-aware status/doctor + read-only presence scan (D) — author: claude

**Files:** Modify `status.go` (`cmdStatus`, the table at :130), `doctor.go` (add a presence check); Create `presence_scan.go` (read-only host enumeration) + `presence_scan_test.go`; Test: `status_test.go`, `doctor_test.go`.

**Interfaces:**
- Produces: `func scanUnenrolledWrappers(cfg *loop.Config) []UnenrolledProcess` — read-only enumeration of wrapper processes / herdr agents / tmux panes / runner sockets on this host whose cwd maps (via `loop.Discover`) to this pool and that have NO matching `agents/<id>.md`. `UnenrolledProcess{Kind, Hint, Cwd, Suggestion}`. NO writes.
- Consumes: `loop.RegistrationReachable(cfg, reg, timeout)` (internal/loop/wake.go:138).

**Fix spec:** `agentchute status` (and a richer `doctor`) must answer "who is enrolled AND reachable?" in one command. Add a `REACHABLE` column to the status table — a **LIVE probe** of each registration via `RegistrationReachable` (short timeout; tolerate probe errors as ✗), show ✓/✗. **(codex #5) E1 must NOT depend on E2 fields — it is a live probe only; the cached `reachable_at` age column is added by E2, not here.** Append a read-only "PRESENT BUT NOT ENROLLED" section from `scanUnenrolledWrappers`. This is the operator answer that replaces "ask each agent"; the scan is the read-only slice of C.

- [ ] Step 1: failing `TestStatus_ShowsReachableColumn` — a registration with an unreachable wake target renders `reachable=✗` (today status prints the wake string and looks healthy).
- [ ] Step 2: failing `TestScanUnenrolledWrappers_FindsUnregisteredPaneInPool` (seam the enumerators so the test injects a fake herdr/tmux/ps lister).
- [ ] Step 3: failing `TestStatus_PresentButNotEnrolledSection`.
- [ ] Step 4: run → FAIL.
- [ ] Step 5: implement the REACHABLE column (probe under a short timeout, tolerate probe errors as ✗) + `presence_scan.go` (read-only; never writes a registration) + the doctor presence check.
- [ ] Step 6: `go vet ./... && go test -race ./...` green; commit; send review-request.

**Acceptance:** one command shows `enrolled✓/reachable✗` for the gemini-class black-hole AND "wrapper X present in this pool, NOT enrolled"; scan performs ZERO writes; existing status output (no `--as`) still works.

## WI-E2 — Self-healing reachability fact (B) — author: claude

**Files:** Modify `internal/loop/registration.go` (add fields + `IsReachable`), `run.go` (runner `pollLoop`), `self_poll.go`/`poller.go` (pane-context re-bind), `recipient_liveness.go` (consult cached reachability); Create `reachability.go` (re-prove/re-bind helper) + tests; Test: `registration_test.go`, `run_test.go`, `self_poll_test.go`.

**Interfaces:**
- Produces (Registration, backward-compatible — absent fields = old behavior; **spec-note the fields in AGENTCHUTE.md §5.1 in this same commit**): `ReachableAt *time.Time`, `ReachabilityMethod string`, **`ReachabilityTarget string`** (codex #2 — endpoint-bound), `ReachabilityError string`. `func (r *Registration) IsReachable(now time.Time, ttl time.Duration) bool` returns true ONLY when `ReachableAt` is set AND within `ttl` AND `ReachabilityMethod==WakeMethod` AND `ReachabilityTarget==WakeTarget` (any wake-target change invalidates the cache). **`IsPokable()` UNCHANGED (structural).**
- Produces: `func reproveAndRebindOwnWake(cfg *loop.Config, agentID string) (rebound bool, err error)` — re-resolves the agent's OWN wake target; if it no longer maps to us AND we have the binding context (`HERDR_PANE_ID`/`TMUX_PANE`/owned runner socket), RE-BIND it (herdr rename / tmux pane re-detect); then write `ReachableAt`/method/error into our registration under the per-agent lock.

**Fix spec:** the recipient's OFF-TURN loop re-proves + re-binds its own wake binding each tick and stores `ReachableAt`/method/target — breaking the herdr circular deadlock (repair no longer needs an inbound wake). Runner: call `reproveAndRebindOwnWake` from `pollLoop` (run.go) each tick. Non-runner: only a poller started WITH pane context (`HERDR_PANE_ID`/`TMUX_PANE`) may re-bind; a context-less poller may PROBE + write `ReachableAt`/`ReachabilityError` but MUST NOT pretend to re-bind (codex).
**(gemini input — herdr handle≠name) Robust herdr resolution:** the herdr `agent` HANDLE can differ from the bound `name` (e.g. gemini's binary/handle is `agy` while its bound name is `gemini-cli-agentchute`, so `herdr agent get gemini-cli-agentchute` returns `agent_not_found`). `reproveAndRebindOwnWake` (and the wake/reachability resolve path) MUST resolve via `herdr agent list` + match on the `name` field (not `agent get <name>`), so a renamed/rebranded wrapper still resolves. Add a regression test with handle≠name.
**(codex #3) Backward-compat fallback:** `recipient_liveness`/senders consult `IsReachable` as a cache+TTL; on a VALID cache hit it proves reachability, but an ABSENT or EXPIRED cache (incl. all pre-upgrade registrations) MUST FALL BACK to the current live reachability / active-session / poller behavior — never default to "unreachable." **(codex #4) Advisory only:** this cache feeds status/gate/escalation; it MUST NOT block `send` delivery or the structural poke. Also extend the status REACHABLE column (from E1) to show the cached `reachable_at` age here.

- [ ] Step 1: failing `TestRegistration_IsReachableCacheTTL` (set/expired `ReachableAt`); confirm `IsPokable` unchanged.
- [ ] Step 2: failing `TestReproveAndRebind_HerdrRebindWithPaneContext` (stale binding + `HERDR_PANE_ID` set → re-binds + writes `ReachableAt`); and `TestReprove_NoPaneContextProbesButDoesNotRebind`.
- [ ] Step 3: failing `TestRunnerPollLoop_WritesReachableAt`.
- [ ] Step 4: run → FAIL.
- [ ] Step 5: add the fields + `IsReachable` (TTL); implement `reproveAndRebindOwnWake` (re-read under per-agent lock from the WI-1 sweep, write atomically); wire into runner `pollLoop` + the pane-context poller; make `recipient_liveness`/senders read `IsReachable` with a TTL.
- [ ] Step 6: suite green; commit; send review-request.

**Acceptance:** a stale herdr binding self-repairs within one poll interval WITHOUT an inbound wake (the gemini black-hole is gone for a polling agent); `IsPokable` stays structural; reachability is a cached fact with TTL, not a per-send probe; backward-compatible (old registrations with no `ReachableAt` behave as before).

## WI-E3 — Launcher hardening (detect+warn) + launch provenance (A, opt-in) — author: claude

**Files:** Modify `internal/loop/registration.go` (provenance fields), `boot.go`/`run.go`/`shims.go` (populate provenance), `doctor.go`/`status.go` (bypass warning); Test: `register_test.go`, `doctor_test.go`, `shims_test.go`.

**Interfaces:**
- Produces (Registration, backward-compatible; **spec-note the fields in AGENTCHUTE.md §5.1 in this same commit** — codex #1): `LaunchedBy string` (`runner|hook|manual|poller`), `ShimName string`, `HookEvent string`. Populated by the enroll path: `run.go`→`runner`, hook `boot`/`self-check`→`hook`+event, shim passthrough→`manual`+shim, poller→`poller`.
- Produces: a doctor/status check `launch_provenance` that WARNS (does not block) when a wrapper is running raw (no runner/shim provenance) while a runner path is available — using `pathIsPrioritized` (shims.go:348).

**Fix spec:** record HOW each agent enrolled (provenance) so the verify view (E1) is truthful, and add a **detect-and-warn** bypass check (the chosen hardening style — 4-way to confirm). **Do NOT flip the runner default; do NOT install same-name shadowing.** Runner remains opt-in.
**(gemini input — wrapper aliases) Known alternate binary names:** the wrapper candidate/alias lists in `setup.go` + `shims.go` must include every name a wrapper runs under — notably **`agy`** for the gemini-cli wrapper (the actual binary on PATH; `gemini`/`gemini-cli` are legacy/shim names). Update the gemini-cli entry to `Aliases: ["gemini","gemini-cli","agy"]` (and audit the others). This closes the herdr-handle-mismatch at the launcher layer, complementing E2's resolve-by-name. Add a test asserting `agy` is a recognized gemini-cli alias.

- [ ] Step 1: failing `TestRegistration_LaunchProvenanceRoundTrips` (write/read all three fields).
- [ ] Step 2: failing `TestBootSetsHookProvenance` / `TestRunnerSetsRunnerProvenance` / `TestShimPassthroughSetsManualProvenance`.
- [ ] Step 3: failing `TestDoctor_WarnsOnRawWrapperBypass` (raw wrapper + runner available → WARN, NOT blocker).
- [ ] Step 4: run → FAIL.
- [ ] Step 5: add fields + populate at each enroll site; add the warn-only provenance check.
- [ ] Step 6: suite green; commit; send review-request.

**Acceptance:** registration records `launched_by`; doctor/status WARN (never block) on a raw-launch bypass; runner default unchanged; no same-name shadowing installed.

## WI-E4 — Host presence daemon (C, opt-in / inert) — author: claude

**Files:** Create `presenced.go` (the `agentchute presenced` command + loop) + `presenced_test.go`; Modify `main.go` (register the subcommand). Reuse `scanUnenrolledWrappers` (E1) + `active_session.go` ps-tree helpers.

**Fix spec:** an OPT-IN, off-by-default daemon that periodically runs the E1 scan and, for a wrapper process it can identify with HIGH confidence (cwd→pool via `loop.Discover` AND a known-wrapper process AND a resolvable wake target), creates/repairs a registration. **Guardrails (codex/gemini):** identity mis-attribution is the risk — register ONLY on a strong, unambiguous identity signal; on ANY ambiguity, report (read-only) but DO NOT write. The daemon does nothing unless explicitly started (`agentchute presenced`); it is NOT wired into setup/hooks and NOT started here.

- [ ] Step 1: failing `TestPresenced_RegistersOnlyHighConfidenceMatch` (unambiguous wrapper→pool → registers; ambiguous → read-only report, no write).
- [ ] Step 2: failing `TestPresenced_DefaultOffNotAutoStarted` (no setup/hook references it).
- [ ] Step 3: run → FAIL.
- [ ] Step 4: implement the command + loop + confidence gate; register subcommand in main.go.
- [ ] Step 5: suite green; `GOOS=windows go build` ok; commit; send review-request.

**Acceptance:** `agentchute presenced` exists and works on demand; it is OFF by default, not referenced by setup/hooks; it never auto-registers an ambiguously-identified process; identity-mis-attribution guardrail is tested.

## WI-E5 — Multi-wake escalation (F, opt-in / inert) — author: claude — DEPRECATED (Gate B/B1)

**Status: DEPRECATED / removed.** WI-E5 shipped as a fully inert opt-in (no flag, command, or default path ever populated it) and was removed in Gate B/B1. It added escalation loops, a wire format, and a hot-path fork in the two central wake functions for redundancy that primary-truthfulness + a durable inbox + recipient-side polling already provide; the reliability win came from making the primary endpoint truthful, not from fallbacks on top of it. The wake path is now primary-endpoint only. The idea (ordered backup endpoints; sender escalates primary→backups, first reachable/successful wins) is preserved as a future premium design in [`EXTENSIONS.md`](../../../EXTENSIONS.md), and the `wake_endpoints` field name is RESERVED in `AGENTCHUTE.md` §5.1 for it. The original spec below is retained for historical context.

- ~~Step 1: failing escalation test (primary unreachable, second reachable → poke via second).~~
- ~~Step 2: failing no-backup test (absent backups → identical to today).~~
- ~~Step 3: run → FAIL.~~
- ~~Step 4: add the optional field + escalation logic guarded on non-empty backups.~~
- ~~Step 5: suite green; commit; send review-request.~~

**Acceptance (superseded):** the deprecation keeps the primary path byte-identical; `wake_endpoints` is reserved, not read or written by the reference CLI.

---

# TRACK 2 — Docs / template / help

## WI-D1 — Enrollment-block clarity (top-5) via templates — author: gemini-cli

**Files:** Modify `templates/enrollment/wrapper.md`, `templates/enrollment/agents.md` (the GENERATED sources), then regenerate `CLAUDE.md`/`CODEX.md`/`GEMINI.md`/`GROK.md`/`AGENTS.md`; Test: `templates_drift_test.go` must pass.

**Fix spec (the 4-way-agreed top-5):** (A) **pin identity** — derive ONCE at startup and persist (`export AGENTCHUTE_AGENT_ID=<resolved-id>` or pass the same `--as <resolved-id>`); state plainly that bare `--vendor` can resolve to a different suffix between calls and check the wrong inbox; do NOT imply repeated `--vendor`/`identity --vendor` is stable (codex). (B) add a session-start **verify** line: `agentchute doctor --as <your-id>` (read-only; confirms enrolled AND reachable). (C) STOP/done uses **`agentchute gate --before finish --as <id>`** (not `check`, which is consume-only and misses pending-replies + liveness). (F) document the FULL identity precedence in AGENTS.md (canonical): `--as → AGENTCHUTE_AGENT_ID → herdr-pane → tmux-pane → contextual`. Bump the enrollment template version + keep all four wrapper blocks byte-identical-modulo-substitution.

- [ ] Step 1: edit the templates; regenerate the wrapper/AGENTS docs.
- [ ] Step 2: `go test -run TestTemplatesMatchRepoWrappers ./...` PASS (drift gone); `go test ./...` green.
- [ ] Step 3: commit; send review-request.

Also folded in (K, L, grok): fix "archive consumed messages" wording in the wrapper-specific sections (CODEX.md/GROK.md) — `check` already consumes+archives; reserve manual archive language for the hand-protocol only. Clarify wrapper setup as "single-agent: `--wrappers <self>`; shared pool: `--wrappers all` (see AGENTS.md)" so `--wrappers <one>` vs `all` isn't ambiguous. Add the GROK.md runner-injection cue: on a `[agentchute:run] check inbox` injection the agent must actually run `agentchute check --as <id>` (the runner polls + injects the cue but does NOT auto-consume mail).

**Acceptance:** every enrollment block tells the agent to pin its id, verify with `doctor --as`, finish with `gate --before finish`; AGENTS.md documents the correct full precedence incl. herdr; "archive" wording + setup-scope clarified; drift test green.

## WI-D2 — AGENTCHUTE.md protocol-spec accuracy — author: codex

**Files:** `AGENTCHUTE.md`. **Fix spec:** wire-vocab `agentchute-run` (not "runner") in §5.1/§8 (G); correct the §8 adapter list + wake-pattern bullets to include `agentchute-run` socket wake + `[agentchute:run]`; fix the §10 watchdog ALGORITHM (uses `last_seen` AGE ≥ stale threshold + mtime-based oldest-arrival age + restart/offline deferrals — not "last_seen > 5min") (E); add herdr to the §1/§8 summaries (H); fix dead self-`§`-cites (§5.3/§6.4/§11.1 pointing at themselves). No code change.

- [ ] Step 1: apply edits; verify every `§`-cite resolves to a real heading (`grep -nE '^#{1,4} '`).
- [ ] Step 2: `go test ./...` (drift) green; commit; send review-request.

**Acceptance:** spec uses `agentchute-run` wire vocab, lists all 3 adapters incl. herdr, the watchdog algorithm matches `watchdog.go`, no dead self-cite.

## WI-D3 — README.md accuracy — author: gemini-cli

**Files:** `README.md`. **Fix spec:** liveness-gate prose (D) — finish/continue with NO work owed → liveness is a WARNING (not a block); owed-work + commit/release still block (match `gate.go`); poller fallback mentions a reachable wake target = tmux/herdr/`agentchute-run` (H); identity summary uses the real precedence incl. herdr (F); align the "five-tier polling" label/count with the actual tiers. No code change.

- [ ] Step 1: apply edits. Step 2: `go test ./...` green; commit; send review-request.

**Acceptance:** README liveness/poller/identity sections match current binary behavior.

## WI-D4 — EXTENSIONS.md + HANDOFF.md accuracy — author: grok

**Files:** `EXTENSIONS.md`, `HANDOFF.md`. **Fix spec:** EXTENSIONS.md `wake_method: agentchute-run` (note `setup --wake runner` installs it) (G); HANDOFF.md latest-release consistency (v0.6.2 throughout — remove stale v0.6.1 "latest"), `herdr`-before-`tmux` in the identity-adoption note, enrollment version → current (v14+). No code change.

- [ ] Step 1: apply edits. Step 2: `go test ./...` green; commit; send review-request.

**Acceptance:** EXTENSIONS wire vocab correct; HANDOFF internally consistent on release + identity order.

## WI-D5 — CLI `--help` + code-comment fixes — author: codex

**Files:** `gate.go` (help ~470), `poller.go` (help ~579), `boot.go`/`self_check.go` (vendor enumeration + wake help), `internal/loop/wake.go` (stale ":14 ships-only-tmux" comment), and the dead `§A.x`/Appendix-A/`rev3` code comments (`register.go`, `check.go`, `send.go`, `gate_test.go`, etc.). Test: existing suite.

**Fix spec:** `gate --help` must say finish/continue no-work liveness WARNS (commit/release block) — match the binary (D); `poller ensure` help says it no-ops on ANY reachable wake target (tmux/herdr/agentchute-run), not just tmux; `boot`/`self-check` vendor examples include `xai` (J); refresh the `internal/loop/wake.go` registry comment to list tmux/herdr/agentchute-run; scrub or correct dead `§A.x`/`rev3` code-comment cites (I). **Comments/help only — no behavior change** (assert via full suite still green).

- [ ] Step 1: apply edits. Step 2: spot-check via `agentchute gate --help` / `poller --help` / `boot --help`. Step 3: `go vet ./... && go test -race ./...` green; commit; send review-request.

**Acceptance:** help text matches behavior (liveness, poller, xai); no stale `§A.x`/ships-only-tmux comments; suite green (no behavior change).

---

## Sequence & done-definition

**Plan first goes through a 4-way DISCUSSION and is iterated until all four lanes agree** (the user's requirement) — only then does authoring begin.

Authoring order (serialized): **E1 → E2 → E3 → D1 → D2 → D5 → D3 → D4 → E5 → E4.**
(E1 gives the verify surface + the read-only scan E4 reuses; E2 the root self-heal; E3 provenance feeds E1's truthfulness; docs next while code settles; E5/E4 the inert opt-ins last. Each WI's 4-way gate closes before the next edit phase opens.)

**DONE** = all 10 WIs 4-way-verified (≥3/4 APPROVE incl. codex, no open REJECT), `go vet ./... && go test -race ./...` green, every new capability opt-in/inert + nothing activated on the live bus → **REPORT to Alex and STOP.** **(codex #7) No `git push`, no PR creation, no merge/tag/release without Alex's EXPLICIT authorization at that time** — the branch sits local and verified; "hybrid PR" is the *recommended* next step to offer, NOT a preauthorization.
