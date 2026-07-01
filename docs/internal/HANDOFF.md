# agentchute — current handoff

Last updated: 2026-07-01.

Read this after `AGENTS.md` and before touching anything. This file stays short and current; release history belongs in [`CHANGELOG.md`](CHANGELOG.md), and protocol history belongs in [`AGENTCHUTE.md`](AGENTCHUTE.md).

## Current state

Latest release: **`v0.9.1`** — the lean release, finished: completes the v0.9.0 subtraction toward **inbox + Markdown + pull**. Removed the `run` verb alias + the redundant `default-id` command (`serve`/`identity` are the sole verbs), collapsed `--wake` to runner-only (tmux/herdr/all/both machinery gone), dropped the deprecated no-op `--wrapper`/`--aliases` flags, and relocated the flat root package into `internal/cli` (root is a thin `main.go` wiring layer). Built on the v0.9.0 subtraction release and the v0.8.0 "simple again" / protocol-v2 redesign.

Coordination is **pull-only**: senders only ever write files and never poke a recipient. A loopless wrapper runs under the runner (`agentchute serve`, launched via `ac serve <wrapper>`) — a per-agent PTY supervisor that polls the agent's own inbox and injects a `check inbox` cue. There is no watchdog, no reachability cache, and no tmux/herdr wake adapters; those were removed. Message identity is the durable `(to, from, seq)` tuple; consumption is two-phase (`check` claims, `ack` commits — at-least-once, so handlers must be idempotent). Presence is the `.live` fact. Reply obligations are asker-owned (`.owed`). Id-uniqueness rides a serve lease + fencing token. The protocol's invariants ship as an executable suite in [`conformance/`](conformance/).

## Source of truth

- Protocol semantics + filename grammar: [`AGENTCHUTE.md`](AGENTCHUTE.md).
- What shipped in each release: [`CHANGELOG.md`](CHANGELOG.md).
- Running a pool, hooks, commands: the root [`README.md`](README.md) and [`examples/`](examples/) (`examples/README.md` + `examples/hooks/`).

## What NOT to do

- Don't reintroduce sender-side wake/pokes, a watchdog, reachability tracking, or per-vendor wake adapters — the redesign deleted them on purpose.
- Behavior changes start with the spec and the conformance suite, not the CLI.

## Verification ritual

Before a commit or release, keep the suite green:

```sh
gofmt -l .            # must be empty (CI gates on it)
go vet ./...
go test ./... -race
go build ./...
sh tests/install_test.sh
( cd conformance && go test ./... )
```

When available: `shellcheck -s sh install.sh tests/install_test.sh` and `goreleaser check`.

## Gating rules

Do not run destructive or outward-facing actions without explicit current-message approval from Alex: `git push`, force-push, tag creation/deletion, branch deletion, GitHub release publishing, repo-settings changes. Do not declare completion, consensus, or release readiness until your agentchute inbox is clear or obligations are explicitly deferred.
