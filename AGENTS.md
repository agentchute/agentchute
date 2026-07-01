# AGENTS.md — instructions for any AI agent working on agentchute

This file follows the [AGENTS.md](https://agents.md) convention. Any AI agent — Claude Code, codex, Cursor, Aider, or otherwise — should read this file before doing any work in this repo. Tool-specific files (`CLAUDE.md`, etc.) defer to this document and only add overrides where strictly necessary.

---

<!-- agentchute-enrollment v20 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Setup / Startup Path**
Run `agentchute setup` once per control repo. `runner` is the only supported wake path: coordination is pull-only, so senders write your inbox and never poke you, and the runner polls your own inbox to wake you. (`--wake all`/`both` are deprecated aliases for `runner`; `tmux`/`herdr` adapters were removed.) The canonical post-install step is:

```sh
agentchute setup --wake runner --wrappers all --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the single `ac` dispatcher (`ac serve <wrapper>`).

Start sessions with `ac serve <wrapper>` from a control repo. The dispatcher routes through `agentchute serve`, which registers you, acquires a serve lease (id-uniqueness + fencing token), refreshes `last_seen` and your `.live` presence, polls your OWN inbox, exports your resolved id as `AGENTCHUTE_AGENT_ID` into the wrapper, and injects `[agentchute] check inbox` when mail arrives. The runner publishes no wake target — peers never poke it (pull-only). Hookless wrappers such as Grok still need the dispatcher for startup because they have no lifecycle hook that can run `boot`; `ac serve <wrapper>` enrolls them. Treat the bracketed prefix as machine metadata: the injection is only a CUE — you must actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"` to claim mail (then `ack` to commit); the runner does NOT consume it for you.

**The project is the communication boundary**: agents by default only see and talk to peers in the same discovered project pool. Unrelated projects on one host or tmux server are isolated because each project has its own pool and, when identity is not explicit, the CLI derives project-scoped IDs from the folder name (for example, `codex-agentchute`).

If a session starts and you do not see agentchute boot/enrolled context, run the wrapper with its vendor so the CLI can derive the contextual identity:

```sh
agentchute serve --vendor <vendor> -- <wrapper>
```

As a manual fallback, pin your identity ONCE and then enroll under it before doing any work:

```sh
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor <vendor>)"   # or a named roster id
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor <vendor>
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor <vendor>
agentchute check --as "$AGENTCHUTE_AGENT_ID"
```

If a first `check` says you are not registered, do this fallback immediately instead of stopping. Capture the id with `identity` (or pick a roster id) BEFORE `boot`, because once a live registration reserves the base id a later bare resolve returns a different `-N` suffix.

Known wrappers and their canonical IDs:

| wrapper      | `agent_id`    | `vendor`    |
|--------------|---------------|-------------|
| Claude Code  | `claude-code` | `anthropic` |
| codex CLI    | `codex`       | `openai`    |
| Gemini CLI   | `gemini-cli`  | `google`    |
| grok CLI     | `grok`        | `xai`       |

The IDs above are wrapper bases. With no explicit identity, the reference CLI derives `<base>-<folder>` and reserves live conflicts with `-2`, `-3`, etc. **When several agents of one vendor share a bus** (e.g. `claude-l1`/`claude-l2`/`merger` all on the `claude-code` wrapper), each process must still enroll under its own id. Use contextual defaults for ordinary project/worktree lanes; use `AGENTCHUTE_AGENT_ID=<roster-id>` or `--as <roster-id>` for named lanes.

**Identity precedence** (the reference CLI resolves your `agent_id` in this exact order, first match wins):

1. `--as <id>` flag
2. `AGENTCHUTE_AGENT_ID` env var
3. contextual default → `<canonical-base>-<folder-slug>`, suffixed `-2`, `-3`, … past live conflicts

(Pull-only registrations carry no wake target, so there is no longer a herdr/tmux pane to map back to a prior registration — id comes from `--as` / `$AGENTCHUTE_AGENT_ID` or the contextual default.)

**Pin it once.** Resolve your id ONE time at startup and reuse the SAME id on every command. The `ac` dispatcher does this for you (it exports `AGENTCHUTE_AGENT_ID`). Otherwise export it yourself before `boot` (precedence step 2 then shadows the contextual default for the whole session). A bare `--vendor` with no `--as`/env is NOT a stable identity: it re-derives the contextual default (step 3) on every call, so as live lanes come and go the resolved `-N` suffix can change between calls and you silently `check` / `gate` the WRONG inbox. `agentchute identity --vendor <vendor>` prints the currently-resolved id — use it for one-time discovery, not as a per-call identity.

**Verify at session start** (read-only — refreshes nothing, archives nothing; confirms you are enrolled AND present via a fresh `.live`):

```sh
agentchute doctor --as <your-id>
```

**2. Lifecycle Hooks (Required for Context and Gates)**
`agentchute setup` installs lifecycle hooks for hook-capable wrappers. If you are not using setup, run `agentchute hooks install` once per control repo. Hooks surface inbox context per turn and block finish while unread mail remains. Hookless wrappers rely on the `ac` dispatcher (`ac serve <wrapper>`) for startup enrollment.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox (pull-only; nobody pokes you). If you are not launched through `agentchute serve`, keep recipient polling alive so your `.live` presence stays fresh:
- **Runner default**: `agentchute serve --vendor <vendor> -- <wrapper>` polls your own inbox, keeps `.live` fresh, and injects the `check inbox` cue.
- **Hook-managed fallback**: `agentchute poller ensure --as <id> --vendor <vendor>` starts/verifies heartbeat-only `poller run` and writes `state/<agent_id>/poller.json` + `.live`; it does not launch wrappers or consume mail unless explicitly run with `--launch`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh heartbeat.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`. Consumption is two-phase: `agentchute check` CLAIMS each message (moves it to `inbox/<id>/.claimed/`) and displays it — it does NOT archive; `agentchute ack` commits (archives) the claimed mail. A crash between `check` and `ack` re-delivers (at-least-once), so handlers must be idempotent. You do NOT archive by hand (manual `mv` to `archive/` is only for the no-binary hand-protocol in §5).

**STOP / finish gate**: do not declare consensus, sign off, tag a release, or report completion until the finish gate passes. Use the gate, not a bare `check` — `check` only claims mail, while the gate is the read-only STOP verdict (unread/malformed mail, unregistered self):

```sh
agentchute gate --before finish --as <your-id>
```

The gate (read-only) blocks `finish` on unread direct mail or an unregistered self; it does NOT check `.live` at `finish`/`continue` (a stale/absent `.live` blocks only the `commit`/`release` gates). Reply obligations are asker-owned only: outstanding/expired `.owed` obligations surface as non-blocking warnings, and a `reply_required` message never blocks the recipient. Clear the gate by consuming mail with `agentchute check --as <your-id>` (then `ack`); reply to any message that needs one with `agentchute send --reply-to <ref>`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v20 end -->

---

## What this is

**agentchute** is a tiny **pull-only** coordination protocol for AI agents: per-recipient inboxes where senders only ever write files and never poke a recipient. A loopless wrapper is supervised by the runner (`agentchute serve`), a per-agent PTY supervisor that polls the agent's own inbox and injects a `check inbox` cue. The reference implementation stores those inboxes as markdown files on a shared filesystem; alternate transports (queues, object stores, HTTP) are protocol-compatible but don't ship in the reference CLI (see [`EXTENSIONS.md`](EXTENSIONS.md)). Small Go codebase, mostly stdlib, with one PTY dependency for the runner. Ships via `go install` and pre-built binaries on GitHub Releases. MIT.

The pitch is intentionally narrow: agents sharing one inbox medium (typically running side-by-side in tmux panes on the reference CLI's shared filesystem; optionally on different machines via a network mount) get a markdown-based mailbox so they stop copy-pasting handoffs by hand. That's the entire scope.

## Reading order on first session

1. `README.md` — 2 minutes, orients you. The public-facing pitch and quickstart.
2. `docs/internal/HANDOFF.md` — current state, pending work, decisions log, what NOT to do. Read this BEFORE touching anything.
3. `AGENTCHUTE.md` — the protocol spec. Source of truth for any reimplementation.
4. `EXTENSIONS.md` — community-extension space (cross-folder enrollment, alternate substrates/transports, cross-pool agents); informs which changes belong in the core spec vs. an extension.
5. `CONTRIBUTING.md` — PR process, style details, scope criteria, bug-report template.
6. `examples/` — start at [`examples/README.md`](examples/README.md) (the index) and `examples/hooks/` (the per-wrapper lifecycle hook templates the installer wires). The tmux/wake/watchdog-era walkthrough scripts were removed in the pull-only redesign; the runner (`ac serve <wrapper>`) flow lives in the root README quickstart.

## Working rules

These rules apply to every agent. They are the discipline that keeps agentchute small.

**1. Spec is source of truth.** `AGENTCHUTE.md` defines the wire contract. If a code change implies a spec change, propose the spec change first in its own PR. Don't sneak protocol changes into a code PR.

**2. Intentionally small surface.** No new third-party Go dependencies beyond the existing PTY runner dependency (`github.com/creack/pty`) without strong justification — the bar is high. The pitch is *"a few markdown files and a recipient that polls its own inbox"*; adding layers undermines that.

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

- **Each turn:** run `agentchute check --vendor <vendor>` first (claims + displays), or pass `--as <id>` for a custom/non-wrapper lane. If it says you are not registered, immediately run `agentchute boot --vendor <vendor>` plus `agentchute poller ensure --vendor <vendor>`, then rerun `check`. Process any messages, then `agentchute ack` to commit (the Stop hook does this for you).
- **Sending:** `agentchute send --to <peer> --task ... --body ...` from a registered lane, or pass `--from <id>` explicitly (or follow `AGENTCHUTE.md` §6 directly — the binary just makes it ergonomic). Sending only writes the recipient's inbox; it never wakes them.
- **No watchdog / cooperative waking:** coordination is pull-only. There is no watchdog and no sender-side or cooperative poke — a recipient discovers its own mail via the runner / its native loop, and a dead recipient is detected via stale `.live` + the asker's expired `.owed` (not by a liveness daemon). The `watchdog` command was removed.
- **Gitignore check:** `git check-ignore .agentchute/loop/agents/<your-id>.md` should print the path.

## Scope

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the canonical "what's in scope" / "what's not in scope" lists. The protocol-level non-goals live in [`AGENTCHUTE.md` §12](AGENTCHUTE.md). When in doubt: agentchute stays small.

## License

MIT. By contributing you agree your contributions are licensed under MIT (see `LICENSE`).

## Agent-to-Agent Communication Rules

These rules govern task messages between agents on the bus. They exist because recipients mis-execute when a sender writes with its own assumptions/dialect. Senders and recipients MUST follow them.

### Definitions
- **Task message** — asks the recipient to do work; its ACTION MODE is one of `implement | review-only | research | decision`. **Non-task message** — purpose is `status | question | info | ack | needs-info` (these are purposes, NOT task modes). A reply that assigns new work IS a task message and MUST use the full envelope.
- **Mode meanings** — `implement`: make the changes needed to satisfy ACCEPTANCE. `review-only`: inspect and report; MUST NOT modify tracked files. `research`: gather/analyze/report; MUST NOT modify tracked files unless CONSTRAINTS authorizes. `decision`: choose/approve/reject/recommend; MUST NOT modify tracked files unless CONSTRAINTS authorizes.
- **Stable pointer** — a repo-relative path, commit SHA, branch name plus its SHA/tip, message-id, exact command (with cwd if relevant), or quoted log/error excerpt with source. A bare deictic reference (`this`, `that`, `above`, `current`, `latest`, `the patch`, `the previous thing`) is NOT a stable pointer. A branch name alone is not stable for review-grade context; include the SHA/tip observed at send time.
- **Blocking ambiguity** — the recipient cannot determine, from the message text alone, any of: GOAL, ACCEPTANCE, ACTION MODE, allowed touch/no-touch scope, or the authoritative CONTEXT pointers. Missing, conflicting, or unparseable GOAL/ACCEPTANCE/ACTION MODE/scope is always blocking.
- **Non-blocking uncertainty** — a detail that does not prevent determining GOAL, ACCEPTANCE, ACTION MODE, scope, and CONTEXT.
- **Irreversible work** — a published/shared side effect that cannot be undone by editing files in the recipient's own worktree: git push, merge to a shared branch, tag, release, deleting shared/remote state, changing repo/service settings, or external (non-agentchute) messages. Normal agentchute replies, NEEDS-INFO replies, status replies, and the requested final response are NOT irreversible by themselves. **If you are unsure whether an action is irreversible, treat it as irreversible.**

### Sender rules
- **S1 (MUST)** A task message body MUST contain these six labels, in this exact order, each label on its own line, content following until the next label:
  - `GOAL:` one sentence — the outcome wanted, not the steps.
  - `CONTEXT:` the stable pointers the recipient needs. If none are needed, write `CONTEXT: none; message is self-contained`.
  - `CONSTRAINTS:` invariants, conventions, and the allowed/no-touch file scope. If there are no extra constraints, write `CONSTRAINTS: none; touch only files required for GOAL`.
  - `ACCEPTANCE:` a done-condition the recipient can verify without asking. MUST NOT be `none`/`N/A`.
  - `OUTPUT:` the exact required response format. MUST NOT be `none`/`N/A`.
  - `ACTION MODE:` exactly one task mode token: `implement`, `review-only`, `research`, or `decision`.
  The body sections are authoritative; the frontmatter `task:` field MAY summarize the work but does not replace them. If frontmatter and body conflict, the message is ambiguous.
- **S2 (MUST)** Every CONTEXT reference is a stable pointer; no deictic references.
- **S3 (MUST)** Before sending, verify every CONTEXT pointer resolves at send time. If one cannot be verified, remove it or mark it `unverified` in CONTEXT with the reason.
- **S4 (MUST NOT)** Do not add persona framing, motivational text, chain-of-thought requests, or model-specific stylistic scaffolding. Facts, constraints, and required output only.
- **S5 (SHOULD)** Non-task messages should be self-contained and use stable provenance when they reference code/commits/files/logs/earlier messages; they are exempt from S1 unless they assign new work.
- **S6 (MUST)** agentchute is direct-addressed (one recipient per message). If the same task goes to several agents, each agent's message MUST state that recipient's own GOAL, ACCEPTANCE, ACTION MODE, and OUTPUT. A shared task with unclear ownership is ambiguous.

### Recipient rules
- **R1 (MUST)** Treat ACCEPTANCE as the definition of done and OUTPUT as the required shape. Do not exceed ACCEPTANCE, expand scope, or change OUTPUT format without first asking and receiving clarification.
- **R2 (MUST)** On blocking ambiguity: do no task work; reply `NEEDS-INFO` with one concrete question, or a numbered list of the exact missing facts needed to proceed.
- **R3 (MAY/MUST)** For reversible work with non-blocking uncertainty you MAY proceed, but MUST state your assumptions in your first substantive (or final) response; if an assumption proves wrong, stop and ask.
- **R4 (MUST)** Do not perform irreversible work unless the message explicitly authorizes the exact action and target. If authorization is missing or imprecise, reply NEEDS-INFO and wait. No assumptions for irreversible work.
- **R5 (MUST)** Your first visible response to a task message MUST be one of: a NEEDS-INFO reply (R2/R4); an acknowledgement restating GOAL and ACCEPTANCE before work continues; or a final response reporting the result against ACCEPTANCE.
- **R6 (MUST)** For `review-only`, `research`, and `decision`, do not modify tracked files unless CONSTRAINTS explicitly authorizes it (read-only inspection is fine).
- **R7 (MAY/MUST)** You MAY reshape the envelope for your own model per your wrapper profile, but MUST preserve the semantics of GOAL, CONSTRAINTS, ACCEPTANCE, OUTPUT, and ACTION MODE.

### Recipient profile (presentation overlay)
- **R8 (MAY)** When composing a task, the sender MAY apply the recipient's vendor profile as a **presentation overlay** over this canonical envelope — reshaping wording, density, and order only, while preserving the semantics of GOAL, CONTEXT, CONSTRAINTS, ACCEPTANCE, OUTPUT, and ACTION MODE (the same invariant R7 requires of the recipient). A profile is never a per-vendor schema: it never adds, drops, or renames the required sections. Resolve the recipient's `vendor` from its registration; an unknown or missing vendor means compose the generic canonical envelope with no overlay. Profiles never appear on the wire and never affect delivery, ordering, liveness, gates, or reply obligations. See [`docs/decisions/agentchute-prompting-profiles-v2.md`](docs/decisions/agentchute-prompting-profiles-v2.md) for the per-vendor overlay summary.
