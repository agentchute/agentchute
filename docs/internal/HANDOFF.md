# agentchute — current handoff

Last updated: 2026-07-01.

Read this after `AGENTS.md` and before touching anything. This file stays short and current; release history belongs in [`CHANGELOG.md`](CHANGELOG.md), and protocol history belongs in [`AGENTCHUTE.md`](AGENTCHUTE.md).

## Current state

Latest release: **`v0.11.8`** — freeze-prep: the 1.0 gates (registrations emit `v: 2` with doctor/status readers and absent-is-silent-legacy semantics; crash-safety vectors C2/Q1 + schema-wide `applies_to` in the conformance contract; macOS CI required; SIGWINCH regression test; gemini-hook removed under an owner-logged exception; spec/policy honesty — tested-targets matrix, NFS demotion, two-axis versioning, non-product boundary). Numbered 0.11.8 by owner decision; minor-grade content. NEXT: dogfood window per the ratified exit criteria (≥72h floor + 9 observable criteria), then v1.0.0 = the zero-code-delta freeze tag (Protocol v2.0 — FROZEN + CLI 1.0.0 + subtraction-arc blog post). Prior: v0.11.1 docs patch; v0.11.0 universality proof; v0.10.x trust-boundary arc; v0.10.0 Protocol v2 STABLE.

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
