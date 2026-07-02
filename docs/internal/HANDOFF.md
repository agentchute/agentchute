# agentchute — current handoff

Last updated: 2026-07-02 (v1.0.0).

Read this after `AGENTS.md` and before touching anything. This file stays short and current; release history belongs in [`CHANGELOG.md`](CHANGELOG.md), and protocol history belongs in [`AGENTCHUTE.md`](AGENTCHUTE.md).

## Current state

Latest release: **`v1.0.0`** — the declaration release: **Protocol v2 — stable, declared final** (covenants change only through the versioned deprecation process; a breaking change would be Protocol v3), carried by reference **CLI v1.0.0** (contract: CLI 1.x ⇒ Protocol v2; the two-axis versioning policy in CONTRIBUTING is in full effect). Zero code/wire/conformance-semantic delta over the dogfooded v0.11.8 tree — the tag carries declaration texts, the F2 wording fix, this handoff, the CHANGELOG, launch GIFs (social/gif/), and the 0.8→1.0 subtraction-arc blog post. 1.0 means done, not big. Post-1.0 queue (each needs its own design round; none scheduled): self-serve conformance certification (first priority), the cue-channel ladder (per-session stdio constraint pre-ratified). Prior: v0.11.8 freeze-prep; v0.11.x; v0.10.x; v0.10.0 Protocol v2 STABLE.

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
