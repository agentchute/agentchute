# AGENTS.md — instructions for any AI agent working on agentchute

This file follows the [AGENTS.md](https://agents.md) convention. Any AI agent — Claude Code, codex, Cursor, Aider, or otherwise — should read this file before doing any work in this repo. Tool-specific files (`CLAUDE.md`, etc.) defer to this document and only add overrides where strictly necessary.

---

<!-- agentchute-enrollment v13 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Setup / Startup Path**
Run `agentchute setup` once per control repo. Choose `tmux` when tmux is the primary wake path, `runner` when launcher shims should route wrappers through `agentchute run`, or `both` for mixed pools. The canonical post-install step is:

```sh
agentchute setup --wake runner --wrappers all --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs namespaced launchers (`ac-claude`, `ac-codex`, `ac-gemini`, `ac-grok`).

Start sessions with the `ac-*` launcher for the wrapper from a control repo. In runner mode, the launcher routes through `agentchute run`, which registers you, refreshes `last_seen`, exposes a reachable `agentchute-run` wake socket, polls your inbox, and injects `[agentchute:run] check inbox` when mail arrives. In tmux mode, peer wakes inject `[agentchute:tmux] check inbox`. Hookless wrappers such as Grok still need a startup launcher because they have no lifecycle hook that can run `boot`; setup installs that launcher when such a wrapper is selected. Treat the bracketed prefix as machine metadata and follow the inbox-check instruction.

**The project is the communication boundary**: agents by default only see and talk to peers in the same discovered project pool. Unrelated projects on one host or tmux server are isolated because each project has its own pool and, when identity is not explicit, the CLI derives project-scoped IDs from the folder name (for example, `codex-agentchute`).

If a session starts and you do not see agentchute boot/enrolled context, run the wrapper with its vendor so the CLI can derive the contextual identity:

```sh
agentchute run --vendor <vendor> -- <wrapper>
```

As a manual fallback, run `agentchute boot --vendor <vendor>` and `agentchute poller ensure --vendor <vendor>` before doing any work, then run `agentchute check --vendor <vendor>`. If a first `check` says you are not registered, do this fallback immediately instead of stopping. For a custom stable lane name, set `AGENTCHUTE_AGENT_ID=<roster-id>` or pass `--as <roster-id>` explicitly.

Known wrappers and their canonical IDs:

| wrapper      | `agent_id`    | `vendor`    |
|--------------|---------------|-------------|
| Claude Code  | `claude-code` | `anthropic` |
| codex CLI    | `codex`       | `openai`    |
| Gemini CLI   | `gemini-cli`  | `google`    |
| grok CLI     | `grok`        | `xai`       |

The IDs above are wrapper bases. With no explicit identity, the reference CLI derives `<base>-<folder>` and reserves live conflicts with `-2`, `-3`, etc. **When several agents of one vendor share a bus** (e.g. `claude-l1`/`claude-l2`/`merger` all on the `claude-code` wrapper), each process must still enroll under its own id. Use contextual defaults for ordinary project/worktree lanes; use `AGENTCHUTE_AGENT_ID=<roster-id>` or `--as <roster-id>` for named lanes.

**2. Lifecycle Hooks (Required for Context and Gates)**
`agentchute setup` installs lifecycle hooks for hook-capable wrappers. If you are not using setup, run `agentchute hooks install` once per control repo. Hooks surface inbox/ledger context per turn and block finish while obligations remain. Hookless wrappers rely on `agentchute run` / launcher shims for startup enrollment.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox. If you are not launched through `agentchute run` and are NOT in a tmux pane, keep recipient polling alive:
- **Runner default**: `agentchute run --vendor <vendor> -- <wrapper>` polls and exposes a reachable wake socket.
- **Hook-managed fallback**: `agentchute poller ensure --vendor <vendor>` starts/verifies heartbeat-only `poller run` and writes `state/<agent_id>/poller.json`; it does not launch wrappers or consume mail unless explicitly run with `--launch`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --vendor <vendor>`, or pass `--as <agent_id>`) or obligations are explicitly deferred via `agentchute defer --vendor <vendor> --message <message-id> --reason "..."`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v13 end -->

---

## What this is

**agentchute** is a tiny coordination protocol for AI agents: per-recipient inboxes + an optional wake poke (the reference adapters include `tmux send-keys` and the local `agentchute-run` socket) as convenience accelerators. The reference implementation stores those inboxes as markdown files on a shared filesystem; alternate transports (queues, object stores, HTTP) are protocol-compatible but don't ship in the reference CLI (see [`EXTENSIONS.md`](EXTENSIONS.md)). Small Go codebase, mostly stdlib, with one PTY dependency for the runner. Ships via `go install` and pre-built binaries on GitHub Releases. MIT.

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

**2. Intentionally small surface.** No new third-party Go dependencies beyond the existing PTY runner dependency (`github.com/creack/pty`) without strong justification — the bar is high. The pitch is *"a few markdown files and an optional wake poke"*; adding layers undermines that.

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

**7. Communication & Response Style.**
Apply to every response, all contexts:
- **Tone**: professional, direct, completely objective. No filler/pleasantries/self-celebration ("Sure I can help", "Great choice", "Let me know if you need anything else").
- **Brevity**: shortest response that completely answers. Raw technical clarity.
- **Formatting**: lead with the direct answer/solution first. Bullets or concise code blocks over wordy intros/explanations.
- **No YAGNI**: implement only what's explicitly requested; no speculative features/edge cases unless asked.
- **Error handling**: if a requirement is ambiguous/missing context, stop and ask exactly ONE concise clarifying question rather than assume.
- **Candor**: if an approach/draft is inefficient, insecure, or incorrect, state it plainly and give the superior alternative immediately. Don't soften or hedge.

## Style

- Stdlib `flag` for argument parsing. No cobra, no kingpin.
- Commands are flat root files (`register.go`, `send.go`, `check.go`, etc.). No `cmd/` subdirectory.
- Integration tests > deep unit-test scaffolding.
- `gofmt -w .` before commit.
- Comments only when WHY is non-obvious. Don't restate what code does.

## Coordinating with other agents in this repo

agentchute dogfoods itself: agents working on agentchute coordinate through agentchute. The loop lives at `.agentchute/loop/`. **The project is the communication boundary**: agents by default only see and talk to peers in the same pool. Enrollment commands are at the top of this file. After enrolling:

- **Each turn:** run `agentchute check --vendor <vendor>` first, or pass `--as <id>` for a custom/non-wrapper lane. If it says you are not registered, immediately run `agentchute boot --vendor <vendor>` plus `agentchute poller ensure --vendor <vendor>`, then rerun `check`. Process any messages.
- **Sending:** `agentchute send --to <peer> --task ... --body ...` from a registered pane, or pass `--from <id>` explicitly (or follow `AGENTCHUTE.md` §6 directly — the binary just makes it ergonomic).
- **Watchdog (optional):** cooperative waking on every `agentchute check` cycle (§10.5) is the default and covers most pools. If your wrapper supports a polling loop (e.g., Claude Code's `/loop`), running `agentchute watchdog --once --as <id>` each tick adds belt-and-suspenders liveness. Otherwise, the standalone `agentchute watchdog --as watchdog &` daemon is the fallback. See `AGENTCHUTE.md §10`.
- **Gitignore check:** `git check-ignore .agentchute/loop/agents/<your-id>.md` should print the path.

## Scope

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the canonical "what's in scope" / "what's not in scope" lists. The protocol-level non-goals live in [`AGENTCHUTE.md` §12](AGENTCHUTE.md). When in doubt: agentchute stays small.

## License

MIT. By contributing you agree your contributions are licensed under MIT (see `LICENSE`).
