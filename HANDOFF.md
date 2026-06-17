# agentchute — current handoff

Last updated: 2026-06-16.

Read this after `AGENTS.md` and before touching anything. This file should stay short and current; release history belongs in `CHANGELOG.md`, and protocol history belongs in `AGENTCHUTE.md`.

## Current State

Latest release: `v0.3.9`

Release URL: https://github.com/agentchute/agentchute/releases/tag/v0.3.9

Restart note: `v0.3.9` is the latest release — a hotfix for duplicate tmux pane registrations. An agent re-enrolling in the same pane no longer accumulates multiple live registrations (which previously could split peer wakes or defeat the finish-gate). Standard `install.sh` resolves `v0.3.9`.

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

## Restart Context

After reinstall, start wrappers from this repo without custom `AGENTCHUTE_AGENT_ID` unless a named stable lane is required. The expected default identity path is `--as` > `AGENTCHUTE_AGENT_ID` > existing current-tmux-pane registration > contextual `<wrapper>-<folder>` with `-2`, `-3`, etc. for live conflicts.

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

As of 2026-06-16, local uncommitted state includes the team-review findings fixes: capped frontmatter peek in `pending.go` + `pending_test.go`; a release-preflight job (gofmt/vet/test/build + shellcheck + install_test + `goreleaser check`) that the `goreleaser` job now `needs:`, plus a least-privilege `permissions:` split in `release.yaml` (default `contents: read`, only the publish job `write`); Node-24 action major bumps (`checkout@v6`, `setup-go@v6`, `goreleaser-action@v7`) in `ci.yaml` and `release.yaml`; and the v0.3.9 doc/CHANGELOG sync (README, CHANGELOG, this file). Plus the pre-existing unstaged `GEMINI.md` identity-note hunk and untracked `.claude/`, `.codex/`, and `.gemini/` setup-output dirs. The full ritual is green and codex re-reviewed + signed off; **none of this is committed yet** — awaiting Alex's go-ahead to commit on a branch.

Open decision (2026-06-16): **herdr support** (github.com/ogulcancelik/herdr). Team converged on L1 (document running a pool inside herdr panes via the runner path, in EXTENSIONS.md) now, L2 (a `herdr` wake adapter sibling to `internal/loop/tmux.go`) deferred until real demand, L3 (socket/state import into core) rejected. No doc written yet; awaiting Alex. Full rationale + codex's L2 guardrails are in the auto-memory file `herdr_support_decision`.

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

GitHub Actions currently emits Node.js 20 deprecation warnings for `actions/checkout@v4`, `actions/setup-go@v5`, and `goreleaser/goreleaser-action@v6`. This is non-blocking today but should be reviewed before GitHub's Node 24 migration deadlines.

## Gating Rules

Do not run destructive or external actions without explicit current-message approval from Alex. This includes `git push`, force-push, tag creation/deletion, branch deletion, GitHub release publishing, and repo settings changes.

Do not declare completion, consensus, or release readiness until your agentchute inbox is clear or obligations are explicitly deferred.

## Reboot Context Save (2026-06-16T19:54 from claude)

Fleet reboot in progress (Alex). 

Key durable state to survive restart:
- v0.3.9 team-review fixes (frontmatter read-cap in pending.go; release preflight + least-privilege permissions + Node-24 action bumps in .github/workflows/; v0.3.9 doc/CHANGELOG sync) are DONE, ritual-green (gofmt/vet/test/build), codex-signed-off, but UNCOMMITTED on disk. Awaiting Alex commit go-ahead. Survives reboot.
- herdr support decision converged: L1 (document in EXTENSIONS.md: run pools inside herdr panes via existing runner) now; L2 (herdr wake adapter) deferred; L3 (core import) no.
- Uncommitted changes (as of this save): pending.go, pending_test.go, .github/workflows/{ci,release}.yaml, CHANGELOG.md, README.md, GEMINI.md, HANDOFF.md.
- All main lanes (claude/codex/gemini/grok) active in tmux pool at reboot time; registrations fresh.
- No pending obligations or inbox for grok at time of save.

Restart with normal wrapper; use `agentchute check --as grok-agentchute --vendor xai` (or status/doctor) on re-enroll. This note + git working tree (uncommitted) + .agentchute/loop/ provide the durable handoff. 

(Added per "save context" info message; no reply sent.)
