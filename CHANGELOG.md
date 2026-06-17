# Changelog

All releases of the agentchute reference CLI. The protocol spec itself ([`AGENTCHUTE.md`](AGENTCHUTE.md)) tracks its own version (Working Draft v1).

The repo follows a release-squash convention: each release lands on `main` as a single squash commit, then is tagged. Intermediate tags between release squashes (e.g., feature branches) are not part of the main release history.

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
