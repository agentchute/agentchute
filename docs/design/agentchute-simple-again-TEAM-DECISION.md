# Team Decision Record — agentchute runtime redesign ("simple again")

**Status:** DECIDED. 4-way bus consensus 2026-06-30 (claude · codex · grok · gemini), ratified by the owner (Alex). This document is **input for the RFC author** — it records what was decided, the evidence behind each call, and the constraints the RFC must honor. It is not itself the RFC.

**Scope:** the **runtime** (launch, wake, presence, liveness). The **wire/spec** is a separate, orthogonal track — see *Coupling* below; one decision there (the §5 fork) can delete part of this one, so read that section before finalizing presence.

**Source proposal:** `proposal/agentchute-simple-redesign-proposal-2.md` (the clean-slate runtime redesign). This record supersedes the conceptual deletion table in that doc §8 with the **code-verified** buckets below.

---

## 1. The decision (one paragraph)

Adopt **pull-only** coordination. Three concerns, all served by pull, never push:
- **delivery** → inbox directory + atomic no-overwrite write
- **reply obligation** → recipient-local gate on its own `reply_required`
- **presence** → one `.live` file per agent, read on demand *(provisional — see Coupling)*

One OS-specific component: **`serve`**, a per-agent PTY supervisor that launches the agent, polls its inbox, injects the consume trigger, and writes its `.live`. Wake, liveness, and busy/idle all fall out of the single PTY fd `serve` owns. **Delete the entire push apparatus** (watchdog, cooperative wake, sender-side wake, published wake state, reachability cache, status/restart/provenance fields, vendor-namespacing, five-tier polling) and **drop herdr + tmux as wake transports**.

This is a **correctness** argument, not a simplicity one: push presence is *unreliable* (stale caches, watchdog races, gates on phantom liveness). Parent-child supervision is ground truth. Simplicity is the byproduct.

**Honest framing for the RFC:** this is *not* a reversal of v0.7.0. v0.7.0 made the **missable** wake (tmux/herdr) truthful; this redesign **deletes the missable wake** and keeps the **unmissable** one (the PTY supervisor). The v0.7.0 herdr-resolver/reachability work was the correct fix *for a transport we are now retiring* — deleting it is removing scaffolding for a retired transport, not undoing a correct call.

---

## 2. Owner ratifications

| Question | Ratified call |
|---|---|
| **Multi-host policy** | **Keep multi-host, conditioned on a shared filesystem across hosts.** Framing = "small shared-FS pool with recipient-local serve." Each recipient runs its own local `serve` writing its own `.live`; there is no sender-side cross-host wake (serve removes it anyway). Literal single-host is *not* the constraint; shared-FS is. |
| **Native-loop wake** | **`serve` is the default launch path for the current wrappers, claude included.** Do not build a claude-specific in-process self-poll loop. True native-loop remains a *future* tier only. |

Both are conditions for the deletes to be clean. The whole subtraction **flips** if the roadmap becomes *dozens of agents* or *distributed / no-shared-FS* — confirmed by the team as **not** the current target.

---

## 3. Conditions (unanimous, with evidence)

1. **Evolve `agentchute run` into `serve` — do NOT ship the spike as product code.** The shipped runner is already a parent-child PTY supervisor (`run.go:242-251`: `exec.Command` → `pty.Start` → `cmd.Wait` → `markRunnerOffline`). The spike (`proposal/spike-serve-main.go`) **regresses** on three things the runner already solves:
   - per-vendor submit bytes — spike injects bare `\n` (`spike-serve-main.go:198`); codex needs bracketed-paste + enhanced-enter (`run.go:809-824`). PTY ownership guarantees byte *delivery*, not *submission*; the spike's own `serve -- codex` example would not submit.
   - injection-window / idle-interrupt safeguard — spike injects regardless of busy; runner gates on `waitForInjectionWindow` (`run.go:755-794`).
   - raw-mode stdin + SIGWINCH — spike sets a fixed 40×120 once (`spike-serve-main.go:122`); runner ships the real terminal/signal handling (`run.go:273-333`).
   What **deletes** is the socket / sender-poked wake side (`run.go:584-643`), not the PTY-supervision lessons. *(codex reached this independently; all four agreed.)*

2. **Reframe the kill-criterion** from "single-host permanent" → **"small shared-FS pool with recipient-local serve."** Literal single-host is contradicted by live in-tree multi-host code: `host` is a first-class field (`boot.go:33`, `register.go:92-99` → `os.Hostname()`); cross-host handling is woven through `watchdog.go:159-162` ("expected steady-state in multi-host pools"), `recipient_liveness.go`, `tmux_state.go:77`; network-mount multi-machine is documented (`AGENTS.md:97`, `README.md:243`). `serve` liveness is machine-local, so multi-host survives **iff** each recipient runs a local `serve` writing its own `.live`.

3. **`serve` is the default launch path for the current wrappers, claude included.** The proposal's tier table assumed "native-loop agents (claude) need no serve" — **false for the current code.** `.claude/settings.json` is hook-only (`poller ensure` at SessionStart `:9` and UserPromptSubmit `:27`; no self-poll loop); `self_poll.go` `cmdSelfPoll` is one-shot (computes once, single heartbeat `:101`, returns); the recurring between-turn waker is the **detached** `poller run` (`poller.go:154` `for{}` + `:161` `time.Sleep`, spawned by `startDetachedPoller` `:451/468`) — which this redesign deletes. The same applies to codex (`.codex/hooks.json:9`) and gemini (`.gemini/settings.json:10`). So once poller + socket + tmux + herdr are gone, a hook-only direct-launched agent has **no wake path**. Resolution: make `serve` the launch path; do not write a claude-specific loop.

4. **Relocate `underAgentchuteRunner()` (~7 lines); do NOT `rm herdr_state.go` wholesale.** That function (def `herdr_state.go:215`) is the "launched-by-supervisor?" discriminator `serve` itself needs, used by registration wake selection (`register.go:506/546/577/587/605`). A wholesale delete breaks `register.go` compilation. Move it (e.g. into the surviving `runner_reachable.go` trim). The herdr *poke adapter* (`internal/loop/herdr.go`) drops cleanly.

5. **Staged migration — no big-bang delete after v0.7.0.** Order: introduce `serve` + `.live` → make `send` no-wake by default → remove watchdog/cooperative/liveness gates → then remove adapters/fields/docs. **Sequencing constraint the RFC must encode:** there are **zero `.live` readers** in `check`/`gate`/`doctor`/`send` today, so a serve-launched agent is invisible to those consumers until they migrate to `.live`. Field deletion (step 4) depends on the gate trim (step 3); shipping field-deletion alone hard-blocks an owed-work agent's finish gate (the documented dead-poller deadlock).

---

## 4. Action items (resolved; all four ran them)

- **(a) Consume crash-semantics → act-then-archive + idempotent handlers (at-least-once).** Make it explicit in the trimmed §3. Two framing corrections to the proposal text: the current spec is **not** ambiguous about consume *order* — it pins archive-before-act (`AGENTCHUTE.md` §6.3.3 and §C.3); only the crash *consequence* (redelivery/idempotency) is undefined. And reply_required obligations are **not** silently lost — a durable ledger is recorded *before* archive (`check.go:204-222`) and the finish gate enforces it. The genuine at-most-once gap is strictly the agent's **non-reply work** in the post-check / pre-completion window.
- **(b) Atomic writes — already done.** `atomicWriteFile` (tmp+rename, `registration.go:575-623`) and inbox `linkNoClobber` (no-overwrite) are the pervasive existing pattern. The new `.live` reuses tmp+rename verbatim. No new work for existing artifacts.
- **(c) macOS — PASS.** All four lanes ran `make bus` + `make demo` (darwin/arm64, go1.26.4): full alice→bob reply-required handoff via serve-injected `recv`, carol stale-detection, `.live` dropped on child exit. (Doc-lag only: `spike-README.md:112-115` still says "not run on macOS" — update if published.)

---

## 5. Verified deletion buckets (supersedes proposal §8's conceptual map)

Result of a 44-agent independent verification against the v0.7.0 tree (31 confirmed / 6 corrected). The RFC author should treat these as the surgical truth; proposal §8 is a conceptual map the author disclaims at its line 152.

**CLEAN DELETE** (push-only, co-deletions are compiler-discoverable):
`watchdog.go` (+ the cooperative block in `check.go`, the main dispatch, and the now-orphaned `context` import) · `recipient_liveness.go` (note: proposal §8 omits the `doctor.go:817` and `gate.go:167` callers; also co-remove `IsReachable`) · reachability fields (`reachable_at`/`reachability_*`) · `migrate.go` + `guardInitLoopNamespace` · vendor-namespacing dotdir prefix in `loop.Discover` (**but** the per-agent `Registration.Vendor` field STAYS) · `launched_by`/`shim_name`/`hook_event` · `last_active` · the `SetHerdr*`/`SetTmux` hook seam + reserved `wake_endpoints`.

**ENTANGLED but COHERENT** (every dependent is itself in scope — larger teardown than the one-liner implies, not a hard blocker):
`runner_reachable.go` (its `init()` is the sole injection of the loop-package tmux/herdr hooks; delete only alongside the adapter drop; `runnerReachableForRecipient` survives as a trim) · `poller.go`/`self_poll.go` (SPLIT: the pull half `computeSelfPollResult` survives **as** serve; must STRIP `poller.go:220 reproveAndRebindOwnWake`, not rehome it; "five-tier" overstates — 3 of 5 tiers live elsewhere) · `gate.go` liveness branch (trims cleanly, but **keep** unread + malformed + pending-replies + corrupt-ledger — "gate on reply_required only" understates; `StaleReg` reads `reg.LastSeen` → needs a last_seen→`.live` rewrite, not a line-delete) · `tmux_state.go` + tmux adapter (in-function surgery on `register.go` WakeMethod guards, not a clean `rm`) · `wake_method`/`wake_target` (clean ONLY at the endpoint of the full serve migration — freed by the runner-SOCKET removal, not the herdr/tmux drop alone) · `register.go` heavy path → inbox-dir + `.live` (a **feature trade**, not a field-collapse: drops `agents/<id>.md` frontmatter and the auto per-vendor `-N` id-suffix allocation that CLAUDE.md leans on — ids become operator/serve-chosen) · `status`/`restart_at` (**subsystem removal, not a field→`.live` collapse**: encodes a 3-state machine read by the watchdog poke-gate AND contextual-identity allocation; valid only because BOTH are also in scope).

**TRUE BLOCKER to a literal wholesale delete (trivial fix):** `herdr_state.go` houses `underAgentchuteRunner()` — relocate, don't delete (see condition 4).

**DIAGNOSIS-WRONG (deletion still fine, stated rationale false):** the mtime-staleness item (proposal §10.1c / "two-clock skew in ordering") — ordering is **already** filename-only (`inbox.go:334`); the watchdog's mtime use is deliberate skew-immunity, not an accidental bug. Delete it with the watchdog, but not for the stated reason.

---

## 6. Coupling to the protocol-v2 RFC — RESOLVED (presence is final)

This runtime decision specifies **presence via `.live`** (a recipient-written file). The **separate protocol-v2 deltas RFC** had one coupled decision — **§5, N private inboxes vs. one shared append-only log** — whose outcome could have **deleted `.live`** (if the log won, presence would fall out of the read cursor).

**The protocol team decided §5 (2026-06-30, 4/4) — see `proposal/agentchute-protocol-v2-TEAM-DECISION.md`:** the fork is a *false binary* (one record-file substrate, two config knobs), and the **default profile keeps `.live` + serve pid presence**; cursor-derived presence was **rejected** (it false-dies a busy native-loop agent — the dangerous direction — and has no liveness fallback). The `shared-log` profile is opt-in and does not change the default.

**Therefore: the `.live` + serve presence mechanism in this runtime decision is FINAL, not provisional.** The RFC author may specify it without waiting on anything. Everything else here (serve, wake, the deletions, the migration order, the action items) was always independent of the fork.

---

## 7. Defaults still to set (low-stakes; pick one each)

- **Stale threshold for `.live`** — a single default, e.g. 3× poll interval. *(Moot if the log wins.)*
- **`busy` in v1?** — `serve` computes it for free; the only question is whether the roster consumes it. Recommendation: keep it, advisory-only; never gate protocol behavior on it.

## 8. Out of scope / non-goals (do not relitigate)

Auth/signing/encryption (cooperative trust; only hard rule = argv-only wake, no shell-eval) · routing/coordinator/wildcard inboxes · cross-agent liveness tracking (deleted on purpose) · busy/idle from tmux scraping · multi-host transports beyond shared-FS.

## 9. Verdict trail

Per-lane round-1 and round-2 verdicts are in `.agentchute/loop/archive/` (round-2 messages dated 2026-06-30 04:32–04:33Z, from `codex-agentchute`, `grok-agentchute`, `gemini-cli-agentchute`). The claude (rater-A) position is grounded in the 44-agent verification workflow summarized in §5. No code changes or deletions have been executed — this is decision/design only.
