# Changelog

All releases of the agentchute reference CLI. The protocol spec itself ([`AGENTCHUTE.md`](AGENTCHUTE.md)) tracks its own version (Protocol v2 — stable as of v0.10.0).

The repo follows a release-squash convention: each release lands on `main` as a single squash commit, then is tagged. Intermediate tags between release squashes (e.g., feature branches) are not part of the main release history. (v0.9.0 was landed as a sequence of dual-gated PRs rather than one squash.)

## v1.0.0 (2026-07-02) — done, not big

**The declaration release.** Protocol v2 is **stable — declared final**: it will not break under you. The covenants (primitives, envelope, filename/identity grammar, lifecycle guarantees, conformance invariants) change only through the versioned deprecation process; extensions can come; a breaking change would be Protocol v3. The reference CLI is versioned **1.0.0** as the artifact carrying the declaration — contract: **CLI 1.x implements Protocol v2** — and the two-axis versioning policy in CONTRIBUTING.md is now in full effect.

**Said plainly (the honesty clause):** this release adds almost no new technical guarantee over what v0.10.0's STABLE covenant regime already promised. The 1.0 deltas are exactly three, and all shipped in v0.11.8: the protocol version self-evidences on the wire (`v: 2`), the versioning policy names both version lines, and the non-product boundary is stated in writing. 1.0 marks the promise *finished and declared*, not enlarged.

**Zero code/wire/conformance-semantic delta** over the dogfooded v0.11.8 tree. This tag carries only: the declaration texts (spec status line; CONTRIBUTING/README already reworded in #69), the one-word §2 scope fix from the v0.11.8 review (F2: the stale "(v2)" parenthetical → "a future protocol major"), the internal handoff sweep, this CHANGELOG, the launch GIFs (`social/gif/`), and the "0.8→1.0 subtraction arc" blog post.

**Owner exception (logged per the policy's mechanism):** the ratified dogfood window (≥72h + 9 observable criteria) was closed early by owner decision at the tag. Baseline verification at window-open was fully green — entire pool on v0.11.8 release binaries installed via the real paths, `v: 2` live fleet-wide (PROTO=v2 on all five lanes), doctor clean, conformance green against the release tree — and 4 of 9 criteria were already satisfied; the remaining observational criteria continue as ordinary post-release monitoring on the live pool that builds agentchute with agentchute.

**Not in 1.0, by decision (the rejects are the statement):** native Windows; signing beyond checksums; non-filesystem transports in the reference CLI; broadcast/routing/coordinator agents; maintained SDKs or a maintained second implementation (the language-neutral vectors are the deliverable — a 261-line disposable Python proof already passes them); packaging/distribution channels; supervision/rollback tooling; default retries or exactly-once; dashboards, daemons, config systems. This is a protocol's 1.0, not a product's.

**Post-1.0 (recorded, unscheduled, each behind its own subtract-default design round):** self-serve conformance certification (`conformance --binding <cmd>` — the first-priority round); the cue-channel ladder (hooks → per-session stdio MCP view → PTY floor; constraint pre-ratified: never a daemon).

## v0.11.8 (2026-07-02) — freeze-prep: the 1.0 gates

The release that carries every gate of the agreed 1.0 plan (the freeze itself is declared at v1.0.0, over this release's dogfooded artifacts). **Version-number note (owner decision):** this release is numbered 0.11.8 by Alex's instruction; its content is minor-grade (one addition, one owner-excepted removal) and is treated as a minor under the versioning policy.

**Protocol version now self-evidences on the wire (#59 spec, #64 code)**
- Registrations emit **`v: 2`** — a frozen protocol's records should self-certify their version instead of the claim living in prose. `doctor`/`status` gain a `protocol_version` reader: **absent `v:` is a silent legacy state** (mixed fleets are normal — no warning); only an explicit non-2 value warns, and it is diagnostic only, never delivery-blocking.

**Crash-safety joins the language-neutral conformance contract (#65)**
- Vectors gain schema-wide **`applies_to`** applicability (array; omitted = universal; existing vectors unchanged) — machine-readable, not a convention for third parties to reverse-engineer.
- **C2 sender-crash-resume** (universal) and **Q1 malformed-quarantine** (`["inbox"]` — the shared-log binding has no bad-filename analogue) port the crash half of the lifecycle promises into the portable contract.
- The disposable Python proof now **skips unknown vector kinds and non-applicable vectors with a printed note** instead of crashing — modeling correct third-party consumer behavior; it still proves its original seven invariants, out of CI as always.

**Hardening + honesty (#62, #59, #60)**
- **macOS CI** — the test/vet/build/conformance matrix now includes `macos-latest`, and the macOS leg is a **required** check (we ship darwin binaries; findings are findings, never flakes). First macOS run in project history passed green.
- **A real SIGWINCH regression test** — mid-session terminal resize propagates to the child PTY and the session survives (the previously-untested gap on the runner's weakest pillar).
- **`gate --gemini-hook AfterAgent` removed** — **owner-logged one-time exception (Alex)** to the deprecate-then-remove window, per the policy's own exception mechanism (precedent: the `run` alias, v0.9.1). The surface was verified dead on every shipped config (templates use `BeforeAgent --json`; the negative guard test stays).
- **Spec honesty (#59):** §6.5 documents the `v:` emission + absent-is-legacy semantics; all three "(v1)" headings retitled; the §13 wishlist deleted after folding its true non-goals into §12 (resolving a deferred-vs-never contradiction; transcript-export parked as issue #63); §2 gains the "tested targets and assumptions" matrix; cross-host NFS is explicitly documented-assumptions-not-CI-verified; PTY injection = best-effort cue, not a compliance guarantee; Appendix C notes the hand-protocol has no multi-writer/lease equivalent; Appendix D reconciles covenant-stability vs prose growth.
- **Policy (#60):** CONTRIBUTING gains the two-axis versioning contract (protocol vs CLI; version-line-agnostic release rules; effective in full at v1.0.0) with the owner-exception clause retained and the deferred-cleanup ledger convention relocated from the spec; README gains the non-product Status section ("protocol + reference implementation, not a product") and the tightened any-wrapper phrasing.

## v0.11.1 (2026-07-02) — docs: the durable-key rule + blog honesty

A docs-only patch. **§6.2 now states the durable-key rule** for `send --idempotency-key`: the key must be caller-durable — the same value across every retry of one logical send (a task id, the triggering message ref) — because a fresh key per attempt (e.g. `$(uuidgen)`) gives zero resume protection: it either double-delivers under two sequence numbers or degrades to unverified, accidental at-least-once. The flag help points to the rule. Also: the pre-0.8 blog posts are marked historical (they describe mechanics deleted in the pull-only redesign), the runner-PTY incident note moved under `docs/decisions/`, and CONTRIBUTORS.md's dead README link now points at the CHANGELOG.

## v0.11.0 (2026-07-02) — the universality proof

A minor release with two additions that earn their mass and one subtraction — the outcome of a 5-way "what would make agentchute whole" design review run under the subtract-default rule (most of the candidate additions were rejected or deferred), plus the findings of a third independent deep review (of v0.10.2 itself, which verified the v0.10.2 trust-boundary fixes as "correct and complete").

**Conformance: language-neutral vectors + the spike name retired (#54)**
- The conformance module is renamed `agentchute.dev/spike/conformance` → **`agentchute.dev/conformance`** — the prototype name had outlived two audits on a STABLE protocol.
- The seven invariants (R1/D1/D2/O1/C1/E1/B1) are now encoded as **language-neutral JSON vectors** (`conformance/vectors/core.json`, schema `agentchute-conformance-vectors-v1`); the Go suite is a vector *runner* driving the abstract Binding interface, and both bindings (private-inbox + shared-log) pass the identical vector set. The vectors are the language-neutral conformance contract the "executable spec" covenant implies.
- A **disposable second-language proof** (`conformance/example-python-binding/`, ~260 lines, Python stdlib only) reads the same vectors and passes them against a minimal inbox binding — proving the protocol's central claim (substrate/language independence) in a second language. It is deliberately a labeled point-in-time snapshot, outside CI and every release gate: the vectors are the maintained deliverable, not a second SDK.

**`send --idempotency-key` — opt-in at-least-once (#52 spec, #55 code)**
- The idempotency machinery that already existed in the sequencer is now exposed: `send --idempotency-key <key>` re-issues the **same** seq on a re-send with the same key, giving callers that opt in at-least-once semantics across a crash-mid-send (previously every send was non-resumable; a crash between seq allocation and link double-sent or silently dropped). Default (unset) behavior is unchanged at-most-once. Guardrails: opt-in only, no default body-hash key, no retries, no delivery guarantee beyond the write.
- Internal correctives now use it (N8): `SendCorrective` derives a stable sha256 key from its own arguments, so an identical-args retry dedups instead of double-delivering an enforcement notice. (Boot announce deliberately unkeyed: no clean retry point, and a duplicate "I'm here" is harmless.)

**Fixes from the v0.10.2 deep review (#53)**
- **The ghost `priority` field is gone (B1).** `pending`/`boot` read and rendered a frontmatter field that nothing could produce, nothing validated, and the spec dropped back in v0.3.3 — the reverse of the task/status cleanup (spec updated, reader lingered). Removed outright; `pending`/`boot` now surface only spec'd fields. Recorded in Appendix D.
- **`doctor spec_freshness` message honesty (C1).** The WARN no longer recommends re-running `init`/`setup` (which never resolves spec divergence — `init` skips any recognizable spec by design) and now names both directions: stale disk → update your checkout; deliberately newer/locally-edited disk → expected, update the binary instead.
- **The N1 foundation is pinned (P2).** A new regression test proves discharge identity comes from the canonical *filename* sender and never the body frontmatter `from:` — hand-planted disagreeing messages in both polarities — so a future refactor can't silently invert the property the v0.10.2 owed-discharge fix rests on. (Validated by mutation: flipping the guard fails the test.)

**Spec (#52) + release-prep precision**
- §6.2 documents the opt-in `--idempotency-key` flag; Appendix D records the `priority` reader removal. Release-prep adds two precision notes from the final adversarial gate: §6.2 now names the 256-entry re-issue window (a past-window resend allocates a fresh seq and may duplicate — still at-least-once) and the flag help warns that reusing a key for *different* content is silently dropped as a duplicate of the original.

## v0.10.2 (2026-07-02) — trust boundary + operational honesty

A patch release — fixes, docs, and test-hardening only. Source: a second independent deep-analysis pass (against v0.10.1) probing the trust model, plus four operational findings surfaced by a live fleet restart. Triaged by a 4-way senior review; every fix developed by one agent and gate-reviewed by two (independent adversarial reviewers stood in where a reviewer was safety-blocked). Additive items (registration `v:`, `--idempotency-key`, a lease-timeout knob, mtime-based liveness, language-neutral conformance vectors + a second-language implementation) are deferred to v0.11.0 — a patch stays a patch.

**Trust-boundary fixes (the compromised-peer boundary §15 named, now enforced in code)**
- **Owed-reply discharge now verifies the replier's identity (N1).** `check` clears an `.owed` obligation only when the consumed reply's canonical sender is the agent that owed it (`msg.Sender == key.To`), not merely when the reply names us as asker. Previously any peer echoing another pair's `in_reply_to` — adversarially, or via a stale/hallucinated ref from a well-meaning peer — could clear an obligation it did not own. Impact was bounded to silently suppressing a non-blocking gate warning; no state or authority was ever breached. The §6.6 spec sentence was tightened first (spec-first, per CONTRIBUTING), then the code guard landed with a regression test.
- **Consumed message bodies are sanitized before display (N3).** `check` (and the `pending` text view of the frontmatter-derived priority field) now strips C0/C1 control code points — keeping only `\n`/`\t` — before printing peer-controlled text to a terminal, closing an ANSI/OSC/DEL terminal-injection vector (screen repaint, prompt spoofing, window-title, OSC-52 clipboard). Applied unconditionally (bodies are spec'd UTF-8 free-form text; a control sequence is never legitimate payload), so no platform-specific TTY detection is needed; `--json`/`--show-body` output is unchanged (already escaped).

**Operational honesty (from the live fleet restart)**
- **`doctor` detects a stale on-disk spec (M1).** New `spec_freshness` check byte-compares `AGENTCHUTE.md` against the binary's embedded spec and WARNs (with short sha256s) on mismatch — catching the split-brain where an agent reads a months-stale spec from a checkout parked on an old branch and enforces a superseded protocol. It compares against the embedded spec, not a version token (which would false-WARN on every patch).
- **`doctor` no longer emits false `unenrolled_presence` warnings (M2).** The tmux-pane source was dropped: pull-only removed the pane→registration correlation, so every in-pool pane (including plain shells/editors) was flagged as unenrolled. Registration, `.live` freshness, runner-socket, and raw-process signals remain.
- **The runner no longer injects spurious wake cues (M4).** `serve` re-checks the inbox immediately before injecting the `check inbox` cue and skips if it emptied since the wake was enqueued (a claim race), while still failing open (an extra cue is acceptable; a suppressed real one is not).

**Docs / spec honesty**
- **New hand-protocol scoping covenant (M3).** An agent with the reference CLI available MUST use it; the hand-protocol (Appendix C) is exclusively for environments without the binary. `README.md` no longer invites mixing hand-driven and CLI agents in one pool; the wrapper/`AGENTS.md` hand-protocol pointers were corrected from `§5` (Discovery) to Appendix C. This closes the failure mode where a binary-holding agent followed stale hand-protocol memory and bypassed the CLI's validation.
- **§9 presence honesty (N7).** A fresh `.live` proves the supervisor/heartbeat is ticking, not that the agent wrapper is processing (the runner heartbeats `.live` unconditionally each tick).
- **§15 enforcement (docs).** The Security Considerations section now names the owed-discharge sender check (N1) and body sanitization (N3) as the enforced mechanisms of the compromised-peer boundary — framing only, no crypto.
- **Enrollment marker v22 → v23** (the §5→Appendix C wrapper pointer sits inside the marked block). Re-run `setup` to re-stamp.

## v0.10.1 (2026-07-02) — edges and honesty

A patch release — fixes, docs, and test-hardening only, the first release governed end-to-end by the new deprecation & versioning policy. Source: an independent deep analysis of v0.10.0 ("the implementation is excellent; the findings are edges and honesty gaps, not rot"), triaged by a unanimous 3-way senior design pass. Additive items from the same analysis (`registration v:`, `--idempotency-key`, a lease-timeout knob, mtime-based liveness) were deliberately deferred to v0.11.0 — a patch stays a patch.

**Runner diagnostics no longer garble the terminal (#40)**
- All runner diagnostics that could fire while the terminal is in raw mode now go to `state/<agent-id>/runner.log` instead of stderr — raw-mode stderr writes were landing at the cursor position (typically the wrapper TUI's status rows) with output post-processing off, causing the episodic smears/vanishing statuslines. Two true fatals (serve-lease fenced; terminal-restore failure) are buffered and printed to stderr once, after the terminal is restored, so a dying runner still says why. (RFC3339 timestamps on the log lines landed in #42.)

**`ack` commits unconditionally (#41) — user-visible behavior change**
- `ack` now always archives `.claimed` residue (the recipient's own state) and *then* reports any remaining finish-gate blockers, instead of refusing to commit when an unrelated blocker (e.g. a third party dropping a malformed file into the inbox after `check`) was present. The finish gate itself is unchanged and still blocks finishing.
- Exit contract: default mode exits 0 when the gate is clear after commit, 2 (the `gate --before finish` sentinel) when other obligations remain — scripts can no longer mistake "committed" for "done". `--quiet` is hook mode: it suppresses output *and* the exit-2 signal, because in the Stop-hook chain `gate` is the sole authoritative block signal and a silent duplicate block from `ack` would be indistinguishable from a reason-less failure.

**Crash-residue visibility + code fixes (#42)**
- `doctor` gains a `stale_temp_files` check reporting `.tmp_*` files older than an hour across all four places in-flight writes can crash-orphan them: `inbox/*/`, `state/*/`, `agents/`, and `live/`.
- `writeSeqMessage` adopts defer-based temp cleanup (panic-safe, matching the lease writer), closing a strictly larger orphan window.
- `runner.log` lines gained RFC3339 timestamps (so smear/episode reports can be correlated).
- Stale "transitional" comments in the sequencer/send path rewritten to state the blessed at-most-once semantics.
- `internal/cli/run.go` renamed `serve.go` (the verb changed in v0.9.1; the file finally follows).

**Spec honesty + Security Considerations (#43)**
- New **§15 Security Considerations** — the spec's first threat-model position: cooperative trust for operators (absorbing README/SECURITY.md framing) plus the compromised-peer relay threat; message bodies are untrusted data, not operator instructions; §7.2 task authority is not instruction authority; wrappers MUST require human confirmation for scope-expanding instructions arriving in inbox bodies. The standing rule now ships in all four wrapper files and both enrollment templates (**marker v21 → v22** — re-run `setup` to re-stamp). Framing only; no crypto/auth machinery.
- Spec–implementation honesty fixes: §6.2 states the send crash-window semantics (at-most-once as shipped; an idempotency key is a library-API affordance); §6.4/§6.5 state that `from` is required *information* satisfied by the canonical filename (body-only messages are well-formed) and that the registration `v:` field is reserved, not yet emitted; §5.4 gains cross-host NFS mount requirements (NFSv4, `actimeo` below the lease timeout, the flock caveat); §9 states the presence clock-skew assumption; §6.3 documents backpressure (a dead recipient's inbox grows by design; the remedy) and Appendix C.4 documents sequence-counter recovery.
- The documented retention one-liner gains a third clause sweeping stale `.tmp_*` files loop-wide.

## v0.10.0 (2026-07-01) — the finish line

The release that completes the protocol. The finish-line worklist from the independent 0.9.1 post-release audit, executed end-to-end by the five-agent team (every PR developed by one agent and review-looped by two seniors until all happy; unanimous 5-way final review), plus a high-severity runner fix found by the tmux verification team. **Protocol v2 is declared STABLE as of this release** — the primitives, envelope, filename/identity grammar, lifecycle guarantees, and conformance invariants are now covenants, changed only through the deprecation & versioning policy (CONTRIBUTING.md) that also takes effect with this release.

**Runner fix (high severity) — wrappers no longer boot on a 0×0 PTY**
- `ac serve` started the child before any winsize was set; fast-booting TUIs drew a blank frame and the healing SIGWINCH raced their resize-handler install — intermittently permanently-blank panes (grok/codex/gemini observed). The child PTY is now sized from the runner's terminal **before exec** (`StartInheritSize`), with a clean fallback when the runner has no terminal. Found, root-caused (with a deterministic repro), and fixed by the tmux verification team; report at `docs/decisions/fix-runner-pty-initial-size.md`.

**Retention model for `archive/` + `malformed/` (C/P2)**
- retention model specified for archive/+malformed/ (caller-managed, outside the delivery guarantee) + documented cleanup one-liner; no code/command added.

**Removed the `task`/`status` workflow-vocabulary residue (P1 of the 0.9.1 post-release finish-line worklist)**
- Deleted `send`'s `--task`/`--status` flags outright — they had been silently discarded by `ComposeMessage` since v0.9.0's protocol-v2 envelope cut and carried no wire meaning.
- `ComposeMessage`'s signature dropped its four dead compat parameters (`now`, `to`, `task`, `status`); it now takes just `(from, replyTo, body)`.
- **User-visible output change**: `pending`/`boot` no longer display a message's legacy `task:` frontmatter value (the trailing `— <task>` on unread-message lines/JSON `task` field). That was the one still-live compat-read path; task/status are now fully gone, not just unemitted.
- `AGENTCHUTE.md` §6.4 updated: `to`/`task`/`status` no longer get special-case compat framing — they're unrecognized fields, ignored per §6.5 like any other.
- No replacement field added. A message's subject, if any, remains a body convention (first Markdown line) per the RFC's leanest option.

**Precise malformed/error model (P5 of the 0.9.1 post-release finish-line worklist)**
- `AGENTCHUTE.md` §11.1 now states the invariants that were previously only implicit in the code: a quarantined message is never claimed/archived (never counts as consumed), never touches `seq` (a sender-owned counter quarantine has no path to), and is never silently dropped — it persists under `.agentchute/loop/malformed/` until inspected. Cites the existing `doctor`/`pending`/`boot`/`gate` surfacing rather than inventing new UX.
- **Executable-spec change, not a docs pass**: added a malformed-quarantine invariant to `conformance/` (previously zero coverage there). Inbox-only by design — quarantine is a wire/filename-grammar concern specific to the FS+frontmatter substrate; the shared-log binding has no analogous concept (a log record is already-typed data, nothing like a bad filename can occur once it's in the stream), so it's not run via `eachBinding`, matching the existing `TestInbox_PostConsumeResendRelandsThenKeyDedup` precedent. Extends `inboxBinding` with a `DeliverRaw`/`MalformedItems` test-only affordance rather than the shared `Binding` interface — malformed-ness isn't one of the seven substrate-agnostic invariants.

**Replaced the dropped-wrapper legacy shim selector cleanup (D2 of the 0.9.1 post-release finish-line worklist)**
- `removeSetupShimsForWrapper` now uses a static map keyed by setup wrapper name (`claude-code`, `codex`, `gemini-cli`, `grok`) to remove only that dropped wrapper's legacy `ac-*` and same-name shims.
- Deleted the `selectShimSpecs`/`shimInstallNames` selector helpers; cleanup still marker-checks files, preserves the `ac` dispatcher, and leaves user-owned same-name files untouched.
- Removed the now-satisfied §13.1 deferred ledger row from `AGENTCHUTE.md`.

**Deprecation & versioning policy (D/P3)**
- New `CONTRIBUTING.md` section, in force from v0.10.0 onward: patch releases are fixes-only; minor releases may remove/rename CLI surface after one deprecation window; timelines are binding unless the owner logs an explicit CHANGELOG exception (retroactively applied to the early v0.9.1 `run`-alias removal); the enrollment-marker-bump contract is stated.
- Also fixed the web/blog featured excerpt to note `self-poll`/`doctor --generate-service` were removed in v0.9.0 (they were presented as current).

**Implementation guidance now linked (E/P4)**
- README links `EXTENSIONS.md`; `EXTENSIONS.md` points to `AGENTCHUTE.md` Appendix C's copy-pasteable hand-protocol walkthrough. No new content — the guide already existed, just wasn't discoverable from README.

**Spec reads as the current contract (F/P6a)**
- The six historical "DONE in v0.9.x" migration paragraphs moved verbatim from §13.1 into a new Appendix D (compatibility history); §13.1 is now an empty deferred-cleanup ledger pointing there.

## v0.9.1 (2026-07-01) — the lean release, finished

A fast-follow that completes the v0.9.0 subtraction: remove all remaining legacy not in the current protocol spirit, then relocate the code into a clean shape. No new features; no wire-format change. Six dual-gated PRs (five code + one release-prep), each reviewed by codex + sonnet, plus a full 4-way docs sweep.

**Deprecated command/flag surface removed**
- Dropped the `run` verb alias early (Exception: authorized by Alex's explicit max-lean directive to remove in v0.9.1 instead of the promised v0.10.0) — `serve` is the only launch verb; `agentchute run` / `ac run <wrapper>` now error as unknown. `setupCommandMatchesRunnerPool` still *attributes* a live pre-upgrade `agentchute run` supervisor for teardown only, so `setup --reset`/`--wipe-state`/`update` still stop an orphaned old runner cleanly. (The poller's own `poller run` subcommand is unaffected.)
- Dropped the redundant `default-id` command — `identity` is the single id-resolution command.
- Collapsed `--wake` to **runner-only**: the `tmux`/`herdr`/`both`/`all` aliases and the whole multi-value wake-set machinery (`wakeSetContains`, `canonicalizePersistedWake`, the deprecation-note plumbing) are gone. Any persisted legacy wake value reads back as `runner`.
- Removed the deprecated no-op flags `shims install --wrapper`/`--aliases` and `setup --aliases` (passing them now errors), the persisted `aliases` state field, and update's `--aliases` re-pass. The `ac` dispatcher cutover is complete, so these were never doing anything.

**Dead code + structure**
- Deleted the test-only `renderShimScript` (moved to a test fixture helper) and the zero-caller `removeSetupAliasShimsForWrapper`; renamed the misnamed `gitignoreBeginV1`/`EndV1` constants.
- **Tier D**: relocated the flat root `package main` (72 files) into `internal/cli` (`package cli`); the repo root is now a thin `main.go` that embeds the root assets (spec, enrollment templates, hook templates) and injects them into `cli.Main`. Pure structural move — behavior byte-identical (crown-jewel PTY-injection + enrollment-drift tests pass unchanged).

**Docs + CI**
- Full 4-way stale-info sweep: corrected leftover `task`/`status` wire terminology, reworded the §13.1 compat ledger, and updated `CONTRIBUTING.md`/`AGENTS.md`/`HANDOFF.md` for the `internal/cli` layout.
- CI now runs the `conformance/` module (previously `go test ./...` from the root skipped the nested module).

**Compatibility** — enrollment marker bumped v19 → v21 (re-run `setup` to re-stamp). A pre-upgrade `agentchute run` runner is still cleaned up on the next `setup --reset`/`update`, but restart wrappers with `ac serve <wrapper>` after upgrading. Deferred: swapping the shim selectors for a static legacy-name list (still wired to the dropped-wrapper cleanup path), and the `ComposeMessage`/`send --task`/`--status` workflow-vocabulary cleanup (a remove-vs-repurpose design call for its own PR).

## v0.9.0 (2026-07-01) — the subtraction release

A pure subtraction toward the target shape — **inbox + Markdown + pull**: a recipient reads its own inbox; no pokes, no checks on others; best-effort only. **Net −8,281 lines** (+1,308 / −9,589) across nine dual-gated PRs. No new features — the wire contract shrinks, the CLI surface shrinks, and the reference implementation gets smaller. New standing guardrail: every release should trend to fewer commands, files, and docs; if a change doesn't remove something, it probably isn't moving toward the goal.

**Reply obligations are asker-owned only**
- Removed the recipient-side `pending-replies.json` ledger **and** the `defer` command entirely. `.owed` (asker-owned; a non-blocking overdue warning + expiry) is now the sole reply-obligation mechanism; a recipient is **never blocked at finish** by a `reply_required` message (best-effort delivery — no forcing function once a message lands). Gated on the legacy-pending gauge reading zero pool-wide. ~−3,000 lines. `send --reply-to` still threads `in_reply_to`, which discharges the asker's `.owed` on their next `check`.

**Wire-format compatibility removed**
- Stop emitting the `message_id` frontmatter field — the wire identity is `(to, from, seq)`. Legacy in-flight messages that carry `message_id` still parse (the field is ignored on read).
- Removed the one-release legacy-nonce inbox reader **and** writer; the canonical `from-<from>_seq-<020d>.md` is now the only inbox filename format (the drain gauge was zero pool-wide).

**Wake surface gone**
- Removed the last vestiges of the pre-0.8 push apparatus: `send`'s `--no-wake` flag + the wake-receipt output (`wake_attempted`/`wake_result`), the deprecated `--wake-method`/`--wake-target` flags, the dead gate `WakeStale` field, and all stale wake/poke/tmux teaching across docs, comments, scaffold, and example registrations. The runner's injected cue is now verb-agnostic: `[agentchute] check inbox`.

**Vestigial commands dropped**
- Removed `self-poll` + the generated-scheduler feature (`doctor --generate-service`), `watch`, and `presenced`. `poller` (own-inbox heartbeat fallback) stays; `presence_scan.go` (tmux-pane detection for `doctor`/`status`) stays.

**`run` → `serve`**
- The launch verb is now `serve` (`ac serve <wrapper>` / `agentchute serve`). `run` remains a **silent deprecated alias** for one release (removed in v0.10.0); the `ac` dispatcher and the legacy shims exec `serve`, and the pool matcher attributes supervisors launched via either verb.

**Leaner CLI + repo**
- Top-level `agentchute` help is two-tier: the 8 everyday commands (`setup`, `init`, `serve`, `send`, `check`, `ack`, `status`, `doctor`) are prominent; internal/hook-driven commands are demoted to a compact line (all still runnable).
- Repo cleanup: deleted the committed `proposal/` launch bundle + ephemeral build docs; moved durable design records to `docs/decisions/`; relocated `HANDOFF.md` to `docs/internal/`; added `SECURITY.md`; rewrote the stale `EXTENSIONS.md` for the pull-only world (alternate substrates/transports; the conformance suite is the executable spec).

**Enrollment marker** bumped **v16 → v19** across the release (dispatcher docs, wake/`defer` removals, `run`→`serve`); templates and all wrapper files stay drift-consistent (`TestTemplatesMatchRepoWrappers`).

**Compatibility (one release)** — legacy `message_id` frontmatter still parses (ignored); pre-existing recipient-ledger entries were drained to zero before removal; `run` remains a working alias. **Deferred to a follow-up:** splitting the flat root package into `internal/` (Tier D), and removing the deprecated `--wrapper`/`--aliases` no-op flags (gated on the live-pool cutover to the dispatcher).

## v0.8.8 (2026-07-01) — single `ac` dispatcher + clean install

A small, additive follow-up to v0.8.0. **No protocol changes** — the pull-only wire contract and the conformance suite are unchanged. Direct jump 0.8.0 → 0.8.8; there are no intermediate 0.8.x releases.

**One launcher, not four**
- `setup` now installs a single **`ac` dispatcher** instead of the four generated `ac-claude`/`ac-codex`/`ac-gemini`/`ac-grok` shims. `ac run <wrapper>` launches a wrapper (`ac run codex`); `ac <command>` routes to the matching agentchute command (`ac doctor`, `ac check`). A bounded parser resolves intent unambiguously — a known command always wins, `run <wrapper>` launches, ambiguous input fails closed, no PATH inference. Existing `ac-*` shims are removed on setup by marker match (never a user file of the same name); the dispatcher is installed **before** the legacy cleanup, and it refuses to overwrite a non-agentchute `ac` or a symlink. `setup.json` records `dispatcher_installed`.

**Clean install**
- The guarded **clean-all** audit runs as a phase of **`setup --reset --wipe-state`** (and `install.sh --fresh`, which drives it) — there is no standalone `--clean-all` flag. It audits and removes stale install remains (orphaned binary backups) under strict guards: allowlisted roots only, no symlinks (`Lstat`), regular files, current-user-owned, individual `os.Remove` (never recursive), and it refuses a live bus. Orphaned processes and PATH shadows are **reported, not touched** (clean-all never kills a process or edits PATH). The system `/usr/sbin/ac` is never affected.

**Docs & enrollment**
- README gains an explicit **upgrade box** for users on v0.7.x and below — one command to re-install and re-enroll, no instruction hunting. Enrollment marker bumped **v15 → v16**; templates and all wrapper files stay drift-consistent (`TestTemplatesMatchRepoWrappers`).

**Hardening (from adversarial review)**
- Runner attribution no longer depends on `--as` (runners launch without it): matched by `runner.json` binding + liveness + pool, with **exact** `--control-repo`/`--loop-dir` value comparison (a sibling-prefix path can no longer be mistakenly signalled), stopping at `--` (wrapper argv ignored).
- ppid-walk suppression revalidates the ancestor is live + same-user + a genuine pool runner before trusting it; stale `runner.json` pids can't suppress or misattribute.

**Compatibility**
- The crown-jewel PTY submit/injection byte sequences are **unchanged** (the only `run.go` change is a `shimSpecForName` → `wrapperSpecForName` vendor-resolution rename; `TestPromptInjectionBytes*` guard it). After upgrading, open a new shell (or `hash -r`) and relaunch wrappers with `ac run <wrapper>`.
- **All v0.8.0 deferred follow-ups remain deferred** (unchanged in this release): the `run` verb is not yet renamed to `serve` (`run.go` present, no `serve.go`); `message_id` is still emitted as a compat frontmatter field; the legacy nonce inbox reader is retained; `.owed` still runs alongside the recipient-side pending-reply ledger.

## v0.8.0 (2026-06-30) — "simple again" + protocol-v2

A correctness-driven redesign to **pull-only** coordination. Senders only ever write files; nothing pokes a recipient. ~4,800 net lines of code removed (+6,640 / −11,422 across the redesign). The protocol's invariants now ship as an executable conformance suite. The redesign was staged across eight compile-green gates (g0–g7); the release also folds in clean wipe-and-reinstall, prompting-profile overlays, and a conformance fix.

**Pull-only coordination (the headline)**
- **No wake, no push.** Deleted the entire push apparatus: the watchdog, cooperative/sender-side wake, the tmux/herdr wake adapters, the runner receive-socket, the reachability cache, cross-agent liveness tracking, and the `wake_method`/`wake_target`/`reachable_at`/`reachability_*`/`wake_endpoints` registration fields. A sender's only job is durable delivery.
- **The runner is the wake path.** A loopless wrapper runs under `agentchute run` (PTY supervisor): it polls the agent's OWN inbox and injects `[agentchute:run] check inbox` into the child. `setup --wake` installs the runner path only (`tmux`/`herdr` rejected; `all`/`both` are deprecated aliases for `runner`).

**Identity & ordering (protocol-v2)**
- **Durable per-`(sender,recipient)` `seq` replaces the random nonce as identity + sort key.** Canonical filename `from-<from>_seq-<020d>.md`; `to` is encoded by inbox location. The committed identity is `(to, from, seq)` — not a sender-asserted `message_id`. Write-ahead durable, so a crash gaps the counter, never reuses it.
- **Exact per-sender FIFO with no clock** (plain lexicographic sort); cross-sender order is advisory arrival order. Fixes the live O1 violation where two same-microsecond messages sorted randomly.
- **One-release dual-read drain.** Inbox listing still reads the legacy `<ts>_from-<s>_msg-<nonce>.md` format; `gate`/`doctor` surface a non-blocking gauge of remaining legacy-named messages.

**Delivery & consume**
- **Atomic `link()`-no-clobber** delivery; `EEXIST` = "this exact `(to,from,seq)` already landed" (a crash-uncertain resend is a no-op, NFS-safe).
- **Act-then-archive, two-phase consume (at-least-once).** `check` CLAIMS (moves `inbox/<id>/<name>` → `inbox/<id>/.claimed/`) and displays, re-displaying any uncommitted residue with a `REDELIVERED` banner; `ack` COMMITS (archives). A crash between them re-delivers; handlers must be idempotent. The Stop hook runs `ack` then the read-only finish gate. (Replaces the old archive-on-display, which was at-most-once for the agent's work.)

**Presence & obligations**
- **`.live` presence with freshness** (`<loop>/live/<id>.live`, written every heartbeat): fresh ⇒ alive, stale/absent ⇒ not-alive. `busy` is advisory and never affects aliveness (avoids false-dead). `gate`/`doctor`/`status` read `.live`, not registration `last_seen`.
- **Asker-owned `.owed` reply obligations.** `send --ask` records "I am owed a reply to `(to,from,seq)` by `<T>`" in the asker's own ledger; a reply with the matching `in_reply_to` clears it; the gate surfaces expired obligations as a non-blocking dead-recipient warning. Wire `reply_required` is now an advisory hint.

**Id-uniqueness**
- **Serve lease + fencing token.** The runner acquires a `state/<id>/serve.claim` lease (fails closed if a fresh claim is held) with a `serve_token` epoch verified on every heartbeat and every `seq` write, so a reclaimed zombie/paused holder is fenced and cannot become a dup-writer.

**Registration & namespace**
- Registration drops to a small no-wake record (`id`, `vendor`, `host`, `last_seen`, `status`, advisory provenance) plus the inbox dir + `.live`. Namespace is fixed at `.agentchute/loop` (vendor-namespacing and the legacy `migrate.go`/`.rehumanlabs` path removed).

**Conformance suite**
- `conformance/` encodes the seven invariants (`R1`/`D1`/`D2`/`O1`/`C1`/`E1`/`B1`) as a Go test suite driven against two bindings (private inbox dir + shared log). Any substrate that passes is conformant; the suite is the executable spec.
- The inbox binding now frees its dedup key at consume-commit, so a post-consume resend re-lands (matching real filesystem semantics) instead of being silently swallowed; regression test added.

**Clean wipe & reinstall**
- **`setup --reset --wipe-state`** — a guarded destructive wipe of runtime loop state (inbox/archive/`.claimed`/malformed/live/scratch/state contents), preserving the scaffold + `setup.json`. Requires both flags, refuses anything but the canonical loop dir, refuses a live bus, and is symlink-safe; never `RemoveAll`s the loop dir.
- **`install.sh --fresh`** (and `AGENTCHUTE_FRESH=1`) — wipe-then-install for a clean upgrade; non-TTY runs fail closed before downloading unless `--yes` / `AGENTCHUTE_YES=1`.

**Prompting profiles (presentation overlays, not a wire schema)**
- Per-vendor prompting dialects are documented as **presentation overlays** over the one canonical message contract — never a per-vendor wire schema. `AGENTS.md` R8 codifies the anti-schema rule (an overlay never adds, drops, or renames the required sections); CLAUDE/CODEX/GEMINI/GROK.md carry per-wrapper presentation guidance under the same guard.

**Compatibility (one release)**
- Dual-read of legacy nonce filenames; `message_id` still emitted as a compat frontmatter field; the recipient-side pending-reply ledger remains the blocking authority at the finish gate (the `.owed` flip runs alongside as a non-blocking signal).

**Deferred follow-ups** (intentionally not in this change): rename the `run` verb to `serve`; cut `message_id` emission; remove the legacy nonce reader once every inbox reports zero; make `.owed` the sole reply-obligation authority (drop the recipient ledger block); finish residual setup/socket-helper cleanup.

## v0.7.0 (2026-06-29)

Enrollment-reliability + communication-quality release. Closes the gap where an agent could be "registered + active" yet unreachable, makes the wake path truthful under model/transport changes, and adds the agent-to-agent communication rules that keep recipients from mis-executing tasks written with a sender's wrong assumptions. All work was four-way reviewed (claude/codex/grok/gemini) and gated.

**Enrollment reliability**
- **Reachability-aware `status` / `doctor` + present-but-not-enrolled scan.** `status` shows live/cached reachability; `doctor` adds wake-target validity, launch provenance, and a read-only scan for local wrappers that are present but not enrolled. (WI-E1)
- **Self-healing wake.** A recipient's off-turn loop re-proves and rebinds its OWN wake target each tick and caches an advisory `reachable_at` fact (endpoint-bound; never suppresses delivery). `IsPokable()` stays structural; reachability is a separate cached fact. (WI-E2)
- **Truthful primary wake selection + runner-demotion fix.** `selectTruthfulPrimary` resolves the live primary with precedence runner > herdr > tmux; a boot/self-check under `agentchute run` no longer demotes the runner socket to tmux when `$TMUX_PANE` is also set. A moved live process (e.g. herdr → tmux) reselects its primary on its own tick — no sender-side multi-wake needed.
- **Herdr resolves by name, everywhere.** Both the reachability probe and the wake poke resolve the stable agent name via `herdr agent list` + name match (robust to handle ≠ bound name, e.g. gemini's `agy`), fixing silent wake failures + spurious duplicate lanes.
- **Launch provenance.** Registrations record `launched_by` (`runner`/`hook`/`manual`/`poller`/`presenced`), `shim_name`, `hook_event` for truthful verify views and a raw-launch warning. (WI-E3)
- **Opt-in host presence daemon** (`agentchute presenced`) — discovers and enrolls/repairs high-confidence local wrappers with zero agent cooperation; off by default, never started by setup. (WI-E4)

**Correctness fixes**
- Inbox listing no longer hard-fails `check`/`gate` when a file vanishes mid-scan (tolerates `os.ErrNotExist` only).
- Runner startup no longer marks a transiently-slow but live peer offline; it clears a stale wake target only on corroborating evidence, and `markRunnerOffline` clears the reachability cache.
- tmux/herdr reachability probes honor their timeout (no hangs).
- Message archive is idempotent under concurrent `check` (benign already-archived races no longer error; a genuine destination collision still fails).
- `gate --help` states the real block/warn matrix and hook-mode exit codes; shipped Gemini hooks use BeforeAgent + `--json`.

**Simplification**
- **Deprecated `WakeEndpoints` multi-wake escalation** — it was inert (no producer) and added a hot-path fork. The wake path is primary-endpoint only; `wake_endpoints` is reserved in the spec, with the multi-wake design documented in `EXTENSIONS.md` as a future extension.
- Removed dead herdr `agent get` fallback + the unused `herdrAgentLookup`; deduped boot-summary, doctor probe, and presence enumeration; dropped an observability-only watchdog runner probe; made the lock-timeout test injectable (~5s/run reclaimed) and the watch-loop tests deterministic.

**Communication quality**
- **Agent-to-agent Communication Rules** in `AGENTS.md`: a neutral task envelope (GOAL/CONTEXT/CONSTRAINTS/ACCEPTANCE/OUTPUT), ACTION MODE + authority, exact provenance (no deictic refs), OUTPUT-as-contract, and a clarify-before-acting / NEEDS-INFO rule — so recipients stop guessing through ambiguity.
- **Per-wrapper communication profiles** (`CLAUDE.md`/`CODEX.md`/`GEMINI.md`/`GROK.md`): per-family reference + reminder blocks for adapting the neutral envelope to the live model.
- **Enrollment template v15**: pin-identity-once guidance, `doctor --as` verify, finish via `gate --before finish`, and the full identity precedence (`--as` → `AGENTCHUTE_AGENT_ID` → herdr pane → tmux pane → contextual).

**Docs**
- README, the agentchute.dev landing page, and `AGENTCHUTE.md` synced to the shipped behavior (reachability diagnostics, self-heal, provenance, `presenced`, wake adapters, the watchdog-probe removal); CI/release preflight hardened with `go test -race` + `shellcheck`.

## v0.6.2 (2026-06-17)

Hotfix release: setup/update now resets local runtime state so restarted agents re-enroll cleanly. Previously `setup` cleared live `agents/*.md` but left poller/runner runtime state and herdr names behind, so restarted agents re-enrolled inconsistently (Claude missing, Codex without herdr, suffix drift).

- **Repo-scoped runtime reset on setup**: before clearing registrations, setup now stops local agentchute pollers/runners for *this* control repo — verified by matching the live process command line against this pool (`agentchute poller run` / `run` plus this control-repo/loop-dir path) — removes runtime-only files (`poller.json`, `session.json`, `runner.json`, `runner.sock`), and releases this repo's herdr names for all known canonical wrapper IDs (path-scoped via `herdr agent list`, skipping unrelated repos). The pending-replies ledger and `poller.log` are preserved.
- **Stop safety**: poller stop honors the heartbeat host; runner stop records and checks `RunnerState.host`, verifies the runner socket's `agent_id`/`pid` before requesting shutdown, and falls back to an exact command-line agent-id token match (`--as` / `--agent-id` / `AGENTCHUTE_AGENT_ID`, never a substring — so `codex-agentchute` no longer matches `codex-agentchute-2`) before SIGTERM. A bounded post-signal wait runs before runtime files are removed.
- **Herdr same-pane registration**: registration retry now adopts an existing same-pane registration before suffixing (matching tmux), preventing fake `-2` ids from same-pane startup races; genuinely distinct panes still suffix.
- **Messaging**: `update` and `install.sh` next-steps text now describe the runtime reset and herdr-name release.

## v0.6.1 (2026-06-17)

Hotfix release for the herdr wake submit path and `setup --wake` mode selection.

- **Herdr wake submit fix**: herdr `agent send` writes literal text and does not submit turns on a trailing carriage return. The herdr adapter now resolves the stable agent name to the current pane with `herdr agent get`, sends the wake prompt as text, waits the same inter-key delay used by tmux, then submits with `herdr pane send-keys <pane> Enter`.
- **Composable setup wake paths**: `agentchute setup --wake` now accepts any comma-separated combination of `runner`, `tmux`, and `herdr`, or `all`. The old `both` value remains as a deprecated alias for `all`. Setup planning, shim cleanup, doctor diagnostics, update re-sync, persisted legacy `both` state, and enrollment guidance are set-aware.
- **Enrollment v14**: generated `AGENTS.md` / wrapper enrollment blocks now describe the composable wake-path model and refresh older v13 blocks.
- **Installer help**: `install.sh --wake` and `AGENTCHUTE_WAKE` help now document the new wake-set syntax.

## v0.6.0 (2026-06-17)

- **One-command full update (`agentchute update [--version <tag>]`)**: self-updates the binary and re-syncs the control repo in one step. Pure Go (no `curl | sh`): resolves the target release (latest by default), downloads the release archive + `checksums.txt`, verifies the exact-filename SHA-256 before extracting only the `agentchute` member, and atomically replaces the running binary (same-dir temp → fsync → rename; any download/verify failure leaves the binary untouched). It then re-execs the **new** binary's `setup` — replaying the pool's saved wake mode, wrappers (including `--wrappers none`), shim dir, and profile — so hooks, shims, and enrollment templates re-sync to the new version. Refuses to run from a launcher shim or without saved setup state; `--dry-run` prints the plan (and active agents it would disrupt) without mutating anything. Because `setup` clears live registrations, it prints a loud warning that every active agent must be restarted.

## v0.5.1 (2026-06-17)

Hotfix release from a deep audit of the v0.5.0 herdr wake adapter. No protocol or wire changes.

- **Explicit herdr wake observability**: `--wake-method herdr` outside a herdr pane (`HERDR_PANE_ID` unset) now warns and enrolls non-pokable instead of silently registering an unwakeable agent (matches the tmux path).
- **Identity-adoption precedence**: herdr pane identity adoption now runs before tmux adoption, matching the wake-detection precedence (herdr > tmux) so a stray `TMUX_PANE` cannot win.
- **Diagnostics + docs**: `herdr agent rename` failures surface herdr's stderr; `setup --wake` help/usage, `install.sh --wake`, and the enrollment/usage text now list `herdr` consistently.

## v0.5.0 (2026-06-17)

Native herdr wake adapter — the herdr analog of the tmux adapter, for pools that run inside herdr (github.com/ogulcancelik/herdr) panes.

- **herdr wake adapter**: new `wake_method: herdr`. A bare wrapper launched inside a herdr pane auto-registers herdr wake; peers poke it with a single argv call — `herdr agent send <agent_id> "[agentchute:herdr] check inbox\r"` — whose trailing carriage return (0x0d) submits the turn in one shot (no second keypress and no inter-key delay, unlike tmux's send-keys + Enter; a line feed would only insert a newline and never fire a turn). No Go dependency on herdr: the adapter fork/execs the installed `herdr` binary, argv-only, and never shell-evaluates its target.
- **Stable name target**: at registration and on every `self-check`, the agent binds its pane to its agentchute `agent_id` via `herdr agent rename <HERDR_PANE_ID> <agent_id>` and records `wake_target: <agent_id>`. The wake target is the stable name (herdr resolves it to the current pane), never an ephemeral pane id — so it survives pane re-layout, tab moves, and pane-id reuse across herdr restarts.
- **Coexistence / precedence**: auto-detection prefers herdr for bare launches but preserves the runner-socket wake for agents launched through `agentchute run` (`ac-*`) even inside herdr — the runner owns the PTY and is never overridden by HERDR_ENV. Precedence: explicit `--wake-method` → runner (under the runner) → herdr → tmux → non-pokable. Hookless wrappers such as Grok stay on the runner path.
- **Identity-collision safety**: a contextual identity is already suffix-disambiguated, so its herdr name is unique. An explicit `--as`/`AGENTCHUTE_AGENT_ID` whose herdr name is already bound to a different live pane skips the herdr wake (with a warning) rather than registering an ambiguous `herdr agent send` target.
- **Setup / doctor / liveness**: `agentchute setup --wake herdr` (installs lifecycle hooks plus a runner shim for hookless wrappers); `doctor` validates `wake_method=herdr` read-only via `herdr agent get <agent_id>`; recipient-liveness treats a resolvable herdr name as reachable. Enrollment templates, README, AGENTCHUTE.md §8, and EXTENSIONS.md document the herdr path.

## v0.4.0 (2026-06-16)

Namespaced `ac-*` launcher shims end the same-name PATH/Volta collision, plus a fresh-install wake-reliability overhaul.

- **Namespaced launcher shims**: default setup now installs `ac-claude`, `ac-codex`, `ac-gemini`, and `ac-grok` instead of same-name wrapper shims. Same-name compatibility aliases are opt-in with `--aliases`. Bare-wrapper auto-launches (poller/scheduler) export `AGENTCHUTE_SHIM_BYPASS=1` and target the real binary, so they never recurse through a legacy shim. `doctor` `wrapper_shadowing` is now WARN/OK around namespaced-launcher reachability instead of a runner BLOCKER.
- **Fresh-install wake reliability**: `install.sh` now supports fish (`config.fish`) and writes a precedence-correct PATH block; `setup` treats shim-dir precedence as an invariant, writes the PATH block to all plausible profiles per shell family, and always installs all four shims in runner/both mode. New `doctor` `wrapper_shadowing` diagnostic catches a shim dir shadowed by a real wrapper binary on PATH.
- **Stable active-session liveness**: `agentchute run` exports `AGENTCHUTE_RUNNER_PID`; `boot`/`self-check` capture the stable wrapper PID for `state/<agent>/session.json` instead of a transient hook-shell ppid, fixing false finish-gate blocks for attended hook-managed sessions.
- **Runner health handshake**: `RunnerSocketReachable` requires a JSON ping/ack (runner pid, child pid, pending-wake state); live-runner collisions verify runner/child PID; stale same-host runner wake targets are cleared on healthy runner start. `watchdog` probes runner sockets (not just tmux); `send` notes an unreachable runner target.
- **Stop-hook registration refresh**: Claude and Codex Stop hooks now run hook-safe `self-check` before `gate --before finish`, so a long-lived session whose registration was cleared by `setup` re-registers before the read-only finish gate runs.
- **Fix**: `init` renderWrapperBlock substituted `{{AS}}` while the template used `{{AGENT_ID}}`, leaking literal `{{AGENT_ID}}` placeholders into generated `CLAUDE.md`/`CODEX.md`/`GEMINI.md`/`GROK.md`. Now substitutes `{{AGENT_ID}}` (with `{{AS}}` legacy alias); regression test added.

## v0.3.9 (2026-06-09)

Hotfix release for duplicate tmux pane registrations.

- **Duplicate pane-registration fix**: an agent that restarts or re-enrolls in the same tmux pane no longer accumulates multiple live registrations for that pane. Same-pane re-registration now reconciles to a single lane, so peer wakes are not split across stale registrations and the finish-gate is not defeated (`identity.go`, `register.go`, `tmux_state.go`).

## v0.3.8 (2026-06-09)

Hotfix release for Grok startup enrollment in tmux-first pools.

- **Grok tmux setup fix**: `agentchute setup --wake tmux --wrappers grok` now installs the Grok launcher shim because Grok has no lifecycle hook that can run startup enrollment.
- **Mixed-wrapper shim cleanup**: tmux-mode setup now keeps shims only for hookless selected wrappers; switching from runner mode to tmux removes hook-capable wrapper shims while retaining Grok.
- **Enrollment guidance**: generated `AGENTS.md` / wrapper enrollment text now tells agents to run `boot` + `poller ensure` if an initial `check` reports missing registration, instead of stopping.
- **Docs/tests**: README and Grok notes now distinguish hook-capable SessionStart enrollment from hookless runner-shim startup; tests cover Grok-only, mixed-wrapper, and mode-switch setup behavior.

## v0.3.7 (2026-06-09)

Hotfix release for contextual identity startup races and Grok parity.

- **Contextual registration race fix**: concurrent same-pane startup registrations now adopt the already-published same-pane same-vendor registration instead of minting a spurious `-2` identity.
- **Atomic exclusive registration publish**: exclusive registration writes publish fully-written files via temp-file + hard-link semantics, so losing racers do not observe empty registrations.
- **Hook template dedup**: removed redundant `self-check` from SessionStart hook templates; `boot` owns startup registration while per-turn `self-check` remains.
- **Grok first-class setup path**: `setup`, `shims`, `init`, `doctor`, `GROK.md`, and drift tests now treat Grok as a first-class wrapper through the runner/shim wake path. Grok remains hookless by design because the Grok CLI has no repo lifecycle hook system.
- **Tests**: added concurrent same-pane registration coverage plus Grok setup/shim parity and hook SessionStart assertions.

## v0.3.6 (2026-06-08)

Hotfix release for reinstalling upgraded control repos before restarting agent teams.

- **Setup clears stale live registrations**: `agentchute setup` removes ignored live `agents/*.md` files while preserving tracked examples and `agents/README.md`, forcing agents to re-enroll with fresh contextual IDs and wake targets after install/upgrade.
- **Gitignore drift fix**: the embedded init `.gitignore` stanza and quickstart example now ignore `.agentchute/loop/scratch-*`.
- **Repo cleanup**: removed obsolete `V0.1.1-HANDOFF.md`, refreshed `HANDOFF.md`, added a tracked Grok loop example, and clarified contextual-registration examples.

## v0.3.5 (2026-06-08)

Contextual identity and worktree support for the tmux/reference setup.

- **Contextual agent IDs**: commands can resolve identity from explicit `--as`, `AGENTCHUTE_AGENT_ID`, the current tmux pane registration, or a `<wrapper>-<folder>` default.
- **Same-folder conflict handling**: contextual registrations reserve live names and retry with suffixes such as `codex-agentchute-2`, using exclusive registration writes to close startup races.
- **Enrollment refresh**: `setup` / `init` upgrade existing v10 enrollment blocks to v11 while preserving local notes outside the marked region.
- **Worktree/project boundaries**: docs now spell out that agents stay in their discovered project pool by default and join worktree/top-project pools only through explicit pointer/env/flag setup.
- **Blog**: added "v0.3.5: tmux teams, worktrees, and contextual identity".

## v0.3.4 (2026-06-08)

Dogfood release after the v0.3.3 simplification pass.

- **Generated hooks honor environment identity**: repo hook templates omit hardcoded `--as` values and allow `AGENTCHUTE_AGENT_ID` to supply per-process identity.
- **Legacy namespace migration**: `setup` / `init` migrate safe `.rehumanlabs/loop` cases into `.agentchute/loop` and refuse ambiguous live-state merges.
- **Fixture hardening**: lifecycle gate and doctor unread fixtures were refreshed for current hook/gate behavior.
- **Blog**: added "The agents debugged their own message bus".

## v0.3.3 (2026-06-08)

Simplification pass.

- Collapsed stale design docs and release scaffolding into the current README / AGENTCHUTE.md / CHANGELOG shape.
- Simplified enrollment guidance and wrapper-specific files.
- Removed obsolete runner-design and script-test artifacts.

## v0.3.2 (2026-05-21)

The **setup command and one-line install** release. Install + repo wiring collapses into a single command; peer wake events become visibly machine-typed; launcher shims route normal wrapper commands through the runner without the user learning a new command.

- **`agentchute setup`** (new): one-command repo wiring. Prompts for `tmux` / `runner` / `both` wake path; installs lifecycle hooks; installs launcher shims (runner/both modes only); writes sentinel-bounded shell-profile PATH block with backup; idempotent across re-runs; reconciles wrapper-set and mode changes across re-invocations. Flags: `--wake`, `--wrappers`, `--yes`, `--dry-run`, `--shim-dir`, `--profile`, `--no-profile`, `--init`.
- **`curl ... | install.sh | sh`** now auto-runs `agentchute setup` when a tty is available, so the documented install path is genuinely one line.
- **Init guard**: setup refuses to scaffold a non-project directory (no `.git`, `go.mod`, `package.json`, etc.) without explicit `--init` opt-in. Prevents curl-piped install from silently turning `$HOME` into a control repo.
- **`agentchute run`** (new in v0.3, formalized here): launches a wrapper under a PTY, registers `wake_method: agentchute-run` with a local Unix socket as wake target, refreshes `last_seen` on every poll, watches the inbox, and injects the wake prompt when mail arrives. The launcher-shim mechanism that makes `claude` / `codex` / `gemini` route through the runner inside a control repo and pass through outside one.
- **Bracketed wake prompts**: `[agentchute:tmux] check inbox` and `[agentchute:run] check inbox` replace the bare `check` injection. The leading bracket is machine metadata so a model can tell a wake event from a typed prompt; AGENTCHUTE.md §8 spells out that the prefix is reference-adapter-specific and other implementations are free to use different wake prompts.
- **Same-host stale tmux registration cleanup** (§7.2, §11.1): narrow GC removes a peer registration when it points at an unreachable local tmux pane. Five exact conditions enforced; never touches cross-host or non-tmux peers; never quarantines malformed registrations.

## v0.2.3 (2026-05-21)

Reliable self-registration and self-check hooks. Adds hook-safe `self-check` registration refreshes, tmux wake-target validation, hook-drift checks, and updated enrollment docs for reliable startup across Claude, Codex, and Gemini.

## v0.2.2 (2026-05-21)

Hotfix: init namespace guard + dogfood consolidation. `guardInitLoopNamespace` refuses to scaffold a sibling vendor loop dir when one already exists in the cwd; recommends `--namespace` or migration when the user really wants two pools.

## v0.2.1 (2026-05-20)

The **enforced enrollment** release. Self-registration was normative in the spec (§5) but the reference CLI treated it as optional. v0.2.1 closes the gap end-to-end.

- **AGENTCHUTE.md §5.7** (new, normative): conforming implementations MUST refuse active agent operations without a registration record.
- **Active commands refuse on missing self-registration**: `check`, `send --from`, `watch`, `status --as`, and `gate --before finish|continue` now exit with a clear pointer to `agentchute boot --as <id> --vendor <vendor>`.
- **`internal/loop.ErrInboxMissing` sentinel**: distinguishes "inbox dir doesn't exist" from "inbox is empty".
- **`pending` surfaces `needs_boot`** in text / `--json` / `--claude-hook UserPromptSubmit` / `--codex-hook UserPromptSubmit` modes.
- **`agentchute hooks install --wrapper <name>`** (new): writes canonical hook template into `.claude/settings.json` / `.codex/hooks.json` / `.gemini/settings.json`. Atomic, idempotent, `--scope repo|user`, `--dry-run`, `--force` with `.bak` backup.

**Breaking change**: callers running `send` / `check` / `watch` / `status --as` / `gate finish|continue` without a prior `boot` now error out.

## v0.2.0 (2026-05-20)

The **no-tmux release**. Recipient-side polling becomes the canonical discovery mechanism; tmux is demoted to one optional convenience adapter.

- **§8.2 wake responsibility** (AGENTCHUTE.md): normative text declaring recipients MUST discover unread mail through their own inbox scans on their own cadence. Wake adapters are best-effort latency optimizations.
- **`agentchute self-poll --as <id>`**: side-effect-free "should I wake the wrapper?" helper. Exits 2 on unread mail, pending replies, malformed inbox files, or first-run `needs_boot`.
- **`agentchute gate --before continue`**: in-session continuation gate, sibling of `--before finish` with wrapper-specific output framing.
- **`agentchute doctor --generate-service <kind>`**: emits launchd / systemd-service / systemd-timer / portable shell-script unit files for the preflighted-scheduler pattern.
- **Three-tier polling model** (AGENTCHUTE.md §8.1): native loop / preflighted scheduler / finish-hook continuation.

## v0.1.3 (2026-05-20)

- **`watch` dedupe by filename**: two distinct files sharing a `message_id` no longer suppress the second notification (§6.4.1 compliance fix).
- **`AGENTCHUTE_BIN` executable check**: `doctor` requires the override to be a real regular file with the executable bit set.

## v0.1.2 (2026-05-20)

- **`agentchute doctor`**: diagnostic aggregator with severity-tagged checks (scaffold, hook content, registration, ledger, wake target).
- **`agentchute watch --as <id> --notify`**: non-consuming watcher; OS notification / print / exec on new mail.
- **`agentchute status` without `--as`**: pool overview as a side-effect-free read.
- **Claude Code `UserPromptSubmit` hook JSON**: `pending --claude-hook UserPromptSubmit` emits the nested `hookSpecificOutput.additionalContext` contract.

## v0.1.1 (2026-05-19)

- **Lifecycle primitives**: `boot`, `pending`, `gate`, `defer` for mechanical protocol compliance.
- **Universal hook templates**: Claude Code, codex, Gemini CLI session-start and turn-gate hooks.
- **Pending-reply ledger**: durable local state at `<loop>/state/<agent>/pending-replies.json` tracking `reply_required` obligations.
- **Protocol additions**: `reply_required`, `priority`, `in_reply_to` frontmatter fields (AGENTCHUTE.md §6.4).
- **`AGENTCHUTE_BIN` env override** for binary discovery.

## v0.1.0 (2026-05-13)

Initial reference CLI release.
