# Changelog

All releases of the agentchute reference CLI. The protocol spec itself ([`AGENTCHUTE.md`](AGENTCHUTE.md)) tracks its own version (Working Draft v1).

The repo follows a release-squash convention: each release lands on `main` as a single squash commit, then is tagged. Intermediate tags between release squashes (e.g., feature branches) are not part of the main release history. (v0.9.0 was landed as a sequence of dual-gated PRs rather than one squash.)

## Unreleased

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
