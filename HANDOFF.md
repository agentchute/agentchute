# agentchute — current handoff

Last updated: 2026-06-17.

Read this after `AGENTS.md` and before touching anything. This file should stay short and current; release history belongs in `CHANGELOG.md`, and protocol history belongs in `AGENTCHUTE.md`.

## Current State

Latest release: `v0.6.0`

Release URL: https://github.com/agentchute/agentchute/releases/tag/v0.6.0

Restart note: `v0.6.0` adds **`agentchute update [--version <tag>]`** — a one-command full update. It downloads + checksum-verifies the target release binary (pure-Go: exact-filename sha256, tar member `agentchute` only, atomic same-dir replace), then re-execs the **new** binary's `setup` to re-sync this control repo's saved config (hooks, shims, enrollment templates), and prints a loud restart-all-agents warning (setup clears live registrations). v0.5.1 was the herdr audit hotfix. Standard `install.sh` resolves `v0.6.0`.

Final v0.5.1 release verification (confirmed 2026-06-17): tag `v0.5.1` points at commit `1aa9204`; `gh release view v0.5.1` shows a published non-draft/non-prerelease GitHub release with darwin/linux amd64/arm64 archives plus `checksums.txt`; the GoReleaser `release` workflow on the tag succeeded.

v0.6.0 pre-release verification (2026-06-17): `gofmt`, `go vet ./...`, `go test ./... -count=1` (full suite incl. 12 update tests covering exact-member extraction, traversal/oversized rejection, checksum exact-hex match, mismatch-aborts-untouched, redirect shape, shim refusal, dry-run-no-probe, `--wrappers none` replay, missing-state refusal, version compare), `go build ./...`, `sh tests/install_test.sh` (25/0), `shellcheck`, and `goreleaser check` all passed. codex (co-senior) security-reviewed: 3 blockers + 4 lower all fixed (dry-run no-mutation, faithful shim-dir/profile replay, `--wrappers none`, no-truncated-binary, hex checksum, redirect shape, durable replace) → re-LGTM. grok empirically validated the download→verify→atomic-replace→re-exec flow with real v0.5.0 artifacts (incl. corrupted-checksum-aborts-untouched).

Final v0.6.0 release verification (confirmed 2026-06-17): `main` and tag `v0.6.0` point at commit `02f6e46` (annotated tag `669599d`); `gh release view v0.6.0` shows a published non-draft/non-prerelease GitHub release at https://github.com/agentchute/agentchute/releases/tag/v0.6.0 with darwin/linux amd64/arm64 archives plus `checksums.txt`; the GoReleaser `release` workflow on the tag succeeded. v0.5.1 (`1aa9204`, tag `5be480e`) released just before it. NOTE: the live dogfood fleet still runs an older binary — `agentchute update` (or `install.sh`) + restart wrappers to pick up v0.6.0.

Final v0.5.0 / v0.5.0-era verification (confirmed 2026-06-17): `gofmt`, `go vet ./...`, `go test ./... -count=1` (incl. templates drift + the 6 herdr/identity tests), `go build ./...`, `sh tests/install_test.sh` (25/0), `shellcheck -s sh install.sh tests/install_test.sh`, and `goreleaser check` all passed. herdr poke confirmed live: an isolated raw-mode probe proved `herdr agent send <name> "...\r"` submits the wake (CR 0x0d; an LF 0x0a does not), and the herdr poke woke the live codex lane end-to-end. codex (co-senior) reviewed; all four findings (herdr identity adoption, missing-binary skip, explicit-bind-failure fields, `register` warning surfacing) are fixed and covered by tests.

Final v0.5.0 release verification (confirmed 2026-06-17): `main` and tag `v0.5.0` point at commit `bfeadb4` (annotated tag `f06fc51`); `gh release view v0.5.0` shows a published non-draft/non-prerelease GitHub release at https://github.com/agentchute/agentchute/releases/tag/v0.5.0 with darwin/linux amd64/arm64 archives plus `checksums.txt`; the `release` (GoReleaser, 1m15s) and `ci` (26s) workflow runs on `bfeadb4` both succeeded. NOTE: the live dogfood fleet still runs the v0.4.0 binary — reinstall (`install.sh`) or restart wrappers to pick up the herdr adapter; all current lanes are `ac-*`/runner so they keep the runner-socket wake regardless.

Final v0.4.0 release verification (confirmed 2026-06-17): `main`, `origin/main`, and tag `v0.4.0` point at commit `28ee9ac`; `git describe` reports `v0.4.0`; `gh release view v0.4.0` shows a published non-draft/non-prerelease GitHub release at https://github.com/agentchute/agentchute/releases/tag/v0.4.0 with darwin/linux amd64/arm64 assets plus `checksums.txt`; the final GoReleaser release workflow run succeeded on head SHA `28ee9ac49b7b272afbbe891b15ce6e1921f023e4`.

Final v0.3.9 release verification (confirmed 2026-06-16): tag `v0.3.9` points at commit `64a696a`, `git describe` reports `v0.3.9` at `HEAD`, and `gh release view v0.3.9` shows the published GitHub release with darwin/linux amd64/arm64 assets plus `checksums.txt`; the GoReleaser release workflow run succeeded on 2026-06-09.

Final v0.3.8 release verification on 2026-06-09: `main`, `origin/main`, and tag `v0.3.8` point at commit `e7a990d`; GitHub CI and the GoReleaser release workflow both passed; release assets exist for darwin/linux amd64/arm64 plus `checksums.txt`; `install.sh --no-setup --dry-run` resolves `v0.3.8`.

Final v0.3.7 release verification on 2026-06-09: `main`, `origin/main`, and tag `v0.3.7` point at commit `fa58ab9`; GitHub CI and the GoReleaser release workflow both passed; release assets exist for darwin/linux amd64/arm64 plus `checksums.txt`; `install.sh --no-setup --dry-run` resolves `v0.3.7`; a temp GitHub install downloaded the `v0.3.7` darwin/arm64 asset, verified SHA256, installed successfully, and reported `agentchute 0.3.7`.

Local pre-release verification on 2026-06-09: `gofmt -w .`, `go vet ./...`, `go test ./...`, and `go build ./...` passed; Grok and Gemini both reported no findings on the unstaged diff; live dogfood registrations were cleaned to one agent each for Claude, Codex, Gemini, and Grok.

Recent shipped work:

- Contextual identity defaults: explicit `--as`, then `AGENTCHUTE_AGENT_ID`, then current tmux pane registration, then `<wrapper>-<folder>`.
- Same-folder conflict handling with suffixes such as `codex-agentchute-2`.
- v12 enrollment refresh for existing `AGENTS.md`, `CLAUDE.md`, `CODEX.md`, `GEMINI.md`, and `GROK.md` blocks.
- Worktree/project pool guidance: agents communicate inside their discovered pool by default; cross-worktree/top-project pools require explicit pointer/env/flag setup.
- v0.3.5 blog article and illustration for the improved tmux/worktree reference path.
- Post-release repo cleanup: stale `V0.1.1-HANDOFF.md` removed, `HANDOFF.md` refreshed, Grok loop example added, scratch files ignored, and setup now clears stale live registrations before installing hooks/shims.
- v0.3.7 hotfix: same-pane contextual registration adoption, atomic exclusive registration publish, SessionStart self-check dedup, and first-class Grok runner/shim setup support.
- v0.3.8 hotfix: tmux-mode setup now keeps launcher shims for hookless selected wrappers, especially Grok, so startup enrollment is automatic even without lifecycle hooks.
- v0.3.9 hotfix: duplicate tmux pane registrations reconciled — same-pane re-enrollment no longer accumulates multiple live registrations (`identity.go`, `register.go`, `tmux_state.go`).
- v0.4.0 release: namespaced `ac-*` launchers, fish/profile PATH fixes, full runner-mode shim install, stable active-session liveness via `AGENTCHUTE_RUNNER_PID`, runner socket ping/ack health checks, Stop-hook `self-check` before finish gates, and the `{{AGENT_ID}}` enrollment-template fix.
- v0.5.0 release: native herdr wake adapter (`internal/loop/herdr.go`, `herdr_state.go`) — env detection, `herdr agent rename` identity binding, herdr pane identity adoption, coexist precedence, `setup --wake herdr`, doctor/recipient-liveness probes, and enrollment/README/EXTENSIONS/AGENTCHUTE docs.
- v0.5.1 hotfix: herdr audit fixes — explicit `--wake-method herdr` outside a herdr pane now warns (no longer silently non-pokable), herdr-before-tmux identity-adoption ordering, `setup`/`install.sh`/usage `--wake` herdr docs alignment, and `herdr agent rename` stderr surfaced.
- v0.6.0 release: `agentchute update` — one-command full update that downloads + checksum-verifies the new release binary, replaces the current executable atomically, and re-execs the new binary's `setup` to re-sync the pool's saved config (hooks, shims, enrollment templates), with a loud restart-all-agents warning.

## Restart Context

If you upgrade agentchute, remember to run `agentchute update` or re-run `agentchute setup` to sync the control repo. After reinstall or update, restart wrappers from this repo with `ac-claude`, `ac-codex`, `ac-gemini`, and `ac-grok`. Do not use custom `AGENTCHUTE_AGENT_ID` unless a named stable lane is required. The expected default identity path is `--as` > `AGENTCHUTE_AGENT_ID` > existing current herdr/tmux pane registration > contextual `<wrapper>-<folder>` with `-2`, `-3`, etc. for live conflicts.

If restart behavior looks wrong, first run:

```sh
agentchute version
agentchute status
agentchute doctor --as <actual-agent-id>
```

## Local State

Current dogfood loop: `.agentchute/loop/`

Do not use `.rehumanlabs/loop`; that namespace is legacy and migration behavior is covered in `AGENTCHUTE.md` and `migrate.go`.

Tracked files under `.agentchute/loop/` are examples and README files only. Live registrations, inboxes, archives, state, and scratch files are local runtime data and should remain ignored.

Root `.claude/`, `.codex/`, and `.gemini/` hook dirs are local setup output for this working copy. The tracked canonical templates live under `examples/hooks/`.

The v0.5.0 squash carries the herdr adapter (new `internal/loop/herdr.go`, `herdr_state.go` + tests) plus the touched Go/docs/templates. Local untracked state that stays uncommitted: `.claude/`, `.codex/`, `.gemini/` setup-output hook directories (and `.bak` files), and anything under `.agentchute/loop/` that is runtime data (registrations, inboxes, archives, state, scratch).

**herdr support** (github.com/ogulcancelik/herdr): SHIPPED in v0.5.0 as the L2 native wake adapter (sibling to `internal/loop/tmux.go`). L3 (socket/state import into core) remains rejected. `wake_method=herdr` targets the stable agent name; the poke is one argv call `herdr agent send <agent_id> "[agentchute:herdr] check inbox\r"`. Grok stays on the runner path (hookless; its multiline always-approve TUI does not reliably submit on an injected CR). Decided via 4-way (claude+codex senior, grok+gemini junior): name=agent_id (no workspace/tab encoding — panes move between tabs), runner precedence preserved under `agentchute run`. No outstanding herdr follow-ups.

## Verification

Before a commit or release, keep the normal ritual green:

```sh
gofmt -w .
go vet ./...
go test ./...
go build ./...
sh tests/install_test.sh
```

When available, also run:

```sh
shellcheck -s sh install.sh tests/install_test.sh
goreleaser check
```

GitHub Actions workflow files now use `actions/checkout@v6`, `actions/setup-go@v6`, and `goreleaser/goreleaser-action@v7`; the v0.4.0 final release workflow succeeded after earlier failed release-tag attempts were fixed.

## Gating Rules

Do not run destructive or external actions without explicit current-message approval from Alex. This includes `git push`, force-push, tag creation/deletion, branch deletion, GitHub release publishing, and repo settings changes.

Do not declare completion, consensus, or release readiness until your agentchute inbox is clear or obligations are explicitly deferred.

Restart with normal `ac-*` launchers from this repo; run `agentchute status` and `agentchute doctor --as <actual-agent-id>` after re-enroll. This note plus the clean git state and local `.agentchute/loop/` runtime files are the durable handoff.
