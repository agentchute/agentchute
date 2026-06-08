# agentchute — current handoff

Last updated: 2026-06-08.

Read this after `AGENTS.md` and before touching anything. This file should stay short and current; release history belongs in `CHANGELOG.md`, and protocol history belongs in `AGENTCHUTE.md`.

## Current State

Latest release: `v0.3.5`

Release URL: https://github.com/agentchute/agentchute/releases/tag/v0.3.5

The current main branch is post-`v0.3.5` and is pushed to `origin/main` at `95a252d docs: clean stale handoff artifacts`.

Important restart note: the standard installer resolves the latest GitHub Release, which is still `v0.3.5`. The setup-time live registration cleanup (`agentchute setup` clearing ignored `agents/*.md` so agents re-enroll cleanly) landed after `v0.3.5` in commit `95a252d`. To test that behavior via normal reinstall, publish a new release first or install/build from `origin/main`.

Recent shipped work:

- Contextual identity defaults: explicit `--as`, then `AGENTCHUTE_AGENT_ID`, then current tmux pane registration, then `<wrapper>-<folder>`.
- Same-folder conflict handling with suffixes such as `codex-agentchute-2`.
- v11 enrollment refresh for existing `AGENTS.md`, `CLAUDE.md`, `CODEX.md`, `GEMINI.md`, and `GROK.md` blocks.
- Worktree/project pool guidance: agents communicate inside their discovered pool by default; cross-worktree/top-project pools require explicit pointer/env/flag setup.
- v0.3.5 blog article and illustration for the improved tmux/worktree reference path.
- Post-release repo cleanup: stale `V0.1.1-HANDOFF.md` removed, `HANDOFF.md` refreshed, Grok loop example added, scratch files ignored, and setup now clears stale live registrations before installing hooks/shims.

## Local State

Current dogfood loop: `.agentchute/loop/`

Do not use `.rehumanlabs/loop`; that namespace is legacy and migration behavior is covered in `AGENTCHUTE.md` and `migrate.go`.

Tracked files under `.agentchute/loop/` are examples and README files only. Live registrations, inboxes, archives, state, and scratch files are local runtime data and should remain ignored.

Root `.claude/`, `.codex/`, and `.gemini/` hook dirs are local setup output for this working copy. The tracked canonical templates live under `examples/hooks/`.

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
