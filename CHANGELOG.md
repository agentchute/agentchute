# Changelog

All releases of the agentchute reference CLI. The protocol spec itself ([`AGENTCHUTE.md`](AGENTCHUTE.md)) tracks its own version (Working Draft v1).

The repo follows a release-squash convention: each release lands on `main` as a single squash commit, then is tagged. Intermediate tags between release squashes (e.g., feature branches) are not part of the main release history.

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
