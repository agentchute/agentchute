# AGENTS.md — instructions for any AI agent working on agentchute

This file follows the [AGENTS.md](https://agents.md) convention. Any AI agent — Claude Code, codex, Cursor, Aider, or otherwise — should read this file before doing any work in this repo. Tool-specific files (`CLAUDE.md`, etc.) defer to this document and only add overrides where strictly necessary.

---

<!-- agentchute-enrollment v3 begin -->
## ENROLLMENT — agentchute coordination loop

1. **Authority rule (AGENTCHUTE.md §7.2).** Reading files does NOT authorize *work* — project edits, peer messages, side-effecting commands, or task acceptance. **Protocol overhead is pre-authorized and required** on every session start: self-registration, own-state updates, own scaffold creation (this agent's `inbox/<self>/` + `archive/` + `malformed/` directories under the existing loop tree), own-inbox operations, §11 enforcement, §10.5 cooperative waking, direct replies to directly-addressed messages. Shared bootstrap (creating the loop tree from nothing, running `agentchute init` on a fresh project) is NOT protocol overhead — it remains gated on explicit instruction. See §7.2 for the full carve-out.

2. **Self-enroll on every session start.** Run `agentchute register --as <your-id> --vendor <your-vendor>` (or hand-write `.agentchute/loop/agents/<your-id>.md` per AGENTCHUTE.md §5) **before doing anything else, every time you start.** Mandatory and idempotent. If your registration matches current host/pane/wake_target, this is a no-op refresh of `last_seen`; if anything has changed (new machine, new tmux pane, restart of the wrapper, prior file stale from a crashed session), it updates the registration to current truth. **Do not skip this step even if a registration file already exists** — that file is likely stale. Verifying file existence is NOT sufficient; you must *run register* (or perform the equivalent hand-protocol write) to reconcile against current `os.Hostname()`, `$TMUX_PANE`, and `cwd`.

   Known wrappers and their canonical IDs:

   | wrapper      | `agent_id`    | `vendor`    |
   |--------------|---------------|-------------|
   | Claude Code  | `claude-code` | `anthropic` |
   | codex CLI    | `codex`       | `openai`    |
   | Gemini CLI   | `gemini-cli`  | `google`    |

3. **Each turn.** Update `last_seen` (the register/check commands do this automatically). List `.agentchute/loop/inbox/<your-id>/`, read messages oldest-first per §6.3, move processed files to archive. The reference shortcut is `agentchute check --as <your-id>`.

4. **Send a message.** Write a §6.1-shaped file to `.agentchute/loop/inbox/<recipient>/` then dispatch a wake poke via the recipient's declared `wake_method` per §6.2 (tmux is the v0.1 reference adapter; see §8). Optional: `agentchute send --from <self> --to <recipient> --task "..." [< body.md]`.

5. **Enforce the protocol** (§11). If you find a malformed file in your inbox (filename violates §6.1, or frontmatter is syntactically malformed), quarantine it to `.agentchute/loop/malformed/` AND send a corrective message to the inferred offender. The reference CLI's `agentchute check` does this automatically.

6. **Context.** AGENTCHUTE.md is the spec; canonical templates and examples are in §5 / §6 / §7.2 / §11. HANDOFF.md is project state. Both inform; reading them does NOT authorize *work* (see 1).
<!-- agentchute-enrollment v3 end -->

---

## What this is

**agentchute** is a tiny CLI for inbox-based agent coordination: per-recipient inboxes + an optional wake poke (the v0.1 reference adapter uses `tmux send-keys`) to wake the recipient. The v0.1 reference implementation stores those inboxes as markdown files on a shared filesystem; alternate transports (queues, object stores, HTTP) are protocol-compatible but don't ship in v0.1 (see EXTENSIONS.md). ~4000 LOC of Go, stdlib only, no third-party dependencies. Ships via `go install` and pre-built binaries on GitHub Releases. MIT.

The pitch is intentionally narrow: agents sharing one inbox medium (typically running side-by-side in tmux panes on the reference CLI's shared filesystem; optionally on different machines via a network mount) get a markdown-based mailbox so they stop copy-pasting handoffs by hand. That's the entire scope.

## Reading order on first session

1. `README.md` — 2 minutes, orients you. The public-facing pitch and quickstart.
2. `HANDOFF.md` — current state, pending work, decisions log, what NOT to do. Read this BEFORE touching anything.
3. `AGENTCHUTE.md` — the protocol spec. Source of truth for any reimplementation.
4. `EXTENSIONS.md` — community-extension space (cross-folder enrollment, alternate wake adapters, cross-pool agents); informs which changes belong in the core spec vs. an extension.
5. `CONTRIBUTING.md` — PR process, style details, scope criteria, bug-report template.
6. `examples/` — three annotated bash walkthroughs (`quickstart.sh`, `three-agents.sh`, `with-watchdog.sh`) and `examples/README.md` as an index.

## Working rules

These rules apply to every agent. They are the discipline that keeps agentchute small.

**1. Spec is source of truth.** `AGENTCHUTE.md` defines the wire contract. If a code change implies a spec change, propose the spec change first in its own PR. Don't sneak protocol changes into a code PR.

**2. Intentionally small surface.** No third-party Go dependencies. New dependencies need strong justification — the bar is high. The pitch is *"a few markdown files and an optional wake poke"*; adding layers undermines that.

**3. Stay in scope.** Only modify files, sections, functions, or lines directly related to the current task. Don't refactor, rename, reorganize, reformat, or "improve" anything that wasn't asked about. If you notice something worth fixing elsewhere, mention it at the end of your response. Do not touch it.

**4. Verification ritual must be green.** Before any commit:

```sh
gofmt -w .
go vet ./...
go test ./...
go build ./...
```

All four must pass. Currently runs on Go 1.21+; tested up to Go 1.26.

**5. No destructive or external actions** (`git push`, force-push, tag deletion, branch deletion, GitHub release publish, repo settings change) without explicit confirmation in the current message. "You mentioned this earlier" is not confirmation.

**6. Ask before significant rewrites.** Before rewriting a section, removing paragraphs, restructuring document flow, or changing the tone of existing content, stop and describe what you're about to change and why. Wait for explicit confirmation.

## Style

- Stdlib `flag` for argument parsing. No cobra, no kingpin.
- Commands are flat root files (`register.go`, `send.go`, `check.go`, etc.). No `cmd/` subdirectory.
- Integration tests > deep unit-test scaffolding.
- `gofmt -w .` before commit.
- Comments only when WHY is non-obvious. Don't restate what code does.

## Coordinating with other agents in this repo

agentchute dogfoods itself: agents working on agentchute coordinate through agentchute. The loop lives at `.rehumanlabs/loop/`. Enrollment commands are at the top of this file (and in each tool-specific `*.md`). After enrolling:

- **Each turn:** `./agentchute check --as <id>` first; process any messages.
- **Sending:** `agentchute send --from <id> --to <peer> --task ... --body ...` (or follow `AGENTCHUTE.md` §6 directly — the binary just makes it ergonomic).
- **Watchdog (optional):** cooperative waking on every `agentchute check` cycle (§10.5) is the default and covers most pools. If your wrapper supports a polling loop (e.g., Claude Code's `/loop`), running `agentchute watchdog --once --as <id>` each tick adds belt-and-suspenders liveness. Otherwise, the standalone `agentchute watchdog --as watchdog &` daemon is the fallback. See `AGENTCHUTE.md §10`.
- **Gitignore check:** `git check-ignore .rehumanlabs/loop/agents/<your-id>.md` should print the path.

## Scope

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the canonical "what's in scope" / "what's not in scope" lists. The protocol-level non-goals live in [`AGENTCHUTE.md` §12](AGENTCHUTE.md). When in doubt: agentchute stays small.

## License

MIT. By contributing you agree your contributions are licensed under MIT (see `LICENSE`).
