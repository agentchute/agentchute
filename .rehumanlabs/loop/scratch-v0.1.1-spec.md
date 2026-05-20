# agentchute v0.1.1 implementation spec (rev 3)

Supersedes the earlier R3 team proposal and v0.1.1 spec rev 1/rev 2. Updated 2026-05-19 after two review rounds (R1+R2 + rev2 verification) from codex + gemini-cli + grok.

**Rev 3 changes** (after codex's rev2 NEEDS-FIX verdict; gemini signed off on rev2 outright):
- `boot` gains a hook-safe **context-only mode** (`--context-only` generic, `--codex-hook SessionStart` codex-specific) that always exits 0 and emits unread mail as model-visible context. Interactive `boot` retains exit 2 on blocked.
- All three SessionStart hook templates switched to context-only mode.
- Test 11 wording fix ("archived file" → "inbox file" or `check` step added).
- Summary count corrected (13 mods, not 10).

Changes from rev 1 (marked **[REV2]** at each touchpoint below):
- Exit code 2 (not 1) is the canonical "blocked" signal for hook compatibility
- Codex hook output modes added (`pending`/`gate` with `--codex-hook` flag)
- Gemini schema verified: `.gemini/settings.json`, nested matcher+hooks structure, finish gate moved to `BeforeAgent` since `SessionEnd` is non-blocking
- `boot` exits 0 on refresh success (not 2)
- `defer` moved from Phase 3 to Phase 1
- Pending-reply ledger schema expanded with `to`, `original_filename`, `archive_path`, `status`, `reply_message_id`, etc.
- `pending --max-age` removed; `--stale-after` annotates rather than filters
- `## ASK` is salience-only; frontmatter `reply_required` is canonical
- `gate --before reply` cut; phases are `consensus|commit|release|finish`
- Test 1 rewrite for ledger-flow correctness
- Failure 9 added (fresh session, no hooks yet)
- `AGENTCHUTE_BIN` + binary-on-PATH defense

---

# Part 1 — The shipset

## A. The CLI surface changes

### A.1 `agentchute boot --as <id> --vendor <vendor>` (new command)

One command, idempotent, replaces the three-step session-start ritual.

**Behavior:**
1. Resolve control repo via the discovery cascade (`--control-repo` flag > `AGENTCHUTE_CONTROL_REPO` env > `.agentchute-control-repo` pointer > `cwd`).
2. Register or refresh the agent's registration file (updating `last_seen`, `wake_method`, `wake_target` from `$TMUX_PANE` if applicable).
3. Run `pending` (peek-only) against the agent's inbox; emit a short status summary.
4. **[REV2/REV3] Exit code (interactive mode):** `0` if clear (registration fresh or just refreshed AND no unread/reply-due blockers). `2` if blocked (unread direct mail or pending required-reply). `1` reserved for command failure (binary error, filesystem error, etc.). **[REV3] In context-only mode (see flags below), exit 0 always except for command failure** — unread mail is reported as context, not as a block.

**Flags:**
- `--quiet` — suppress success output, only emit on warnings/blockers. For hook use.
- `--no-archive` — paranoia flag — even if the underlying `register`/`pending` ever grew side effects, boot itself never archives.
- `--emit-prompt-line` — emit a single line designed to be appended to a wrapper's system prompt (e.g., `You have 2 unread agentchute messages. Run agentchute pending to view.`). For wrappers without hooks support.
- `--json` — structured output including `refreshed: true|false`, `unread_count`, `replies_pending`, `stale_reg`, `wake_stale`.
- **[REV3]** `--context-only` — hook-safe mode. Registers/refreshes, reports unread/pending state as plain text (suitable for `SessionStart` developer-context injection in Claude Code and `SessionStart` in Gemini). **Always exits 0** unless command/filesystem failure (exit 1). Use this in all SessionStart hooks.
- **[REV3]** `--codex-hook SessionStart` — codex-specific context-only mode. Emits the codex hook JSON shape: `{"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": "You have N unread agentchute messages..."}}`. Always exits 0 unless command failure.

### A.2 `agentchute pending --as <id> [--json] [--stale-after 5m] [--fail-if-any] [--codex-hook <event>]` (new command)

Side-effect-free read. Lists unread inbox messages without archiving, quarantining, or poking peers.

**Behavior:**
- Reads inbox dir, parses frontmatter of each `.md` file, returns metadata (no body content unless explicitly requested with `--show-body`).
- `--json` emits machine-readable output.
- **[REV2]** `--stale-after <duration>` *annotates* (not filters) — output marks messages older than the threshold as stale, but all unread are still listed. **Removes the dangerous `--max-age` filter that would hide exactly the messages we care about.**
- `--fail-if-any` exits 2 if any unread messages exist (for hook integration).
- **[REV2]** `--codex-hook UserPromptSubmit` — emits codex-specific hook output JSON with `hookSpecificOutput.hookEventName` and `additionalContext` populated. Required for codex's `UserPromptSubmit` to inject pending info as developer context.
- Never writes to disk. Never wakes peers. Never archives.

**Frontmatter-only:** [REV2] `pending` consults the message's frontmatter (`reply_required`, `priority`, etc.). Body-level `## ASK` is a salience convention for the model, not a machine-checkable signal — `pending` does NOT parse body to discover obligations.

### A.3 `agentchute gate --as <id> --before <phase> [--codex-hook <event>]` (new command)

Lifecycle gate. Exits nonzero with a blocking message if the agent isn't ready to proceed past `<phase>`.

**[REV2] Phases (v0.1.1):**
- `consensus` — blocks if unread direct inbox OR pending required-replies
- `commit` — same as `consensus` plus checks `STALE_REG` (registration older than 30m by default)
- `release` — same as `commit` plus warns on `WAKE_STALE` peer registrations
- `finish` — blocks if unread direct inbox OR pending required-replies (strongest gate; intended for "agent declares done with current user turn")

(Dropped: `gate --before reply` — the predicate was narrow and ambiguous. `finish` covers the user-reply case.)

**Flags:**
- `--json` — structured output
- **[REV2]** `--codex-hook Stop` — emits codex Stop-hook JSON. On block, emits `{"decision":"block","reason":"..."}` to stdout and exits 0 (codex's preferred shape) OR exits 2 with the reason on stderr. Both forms trigger codex's turn continuation.
- `--require-confirm` — refuses unless an explicit `--ack-stale-reg` or similar acknowledgment flag is also passed (for risky operations)

**[REV2] Exit codes:**
- `0` — clear to proceed
- `2` — blocked (the message explains why); for hooks this is the "block/continue" signal
- `1` — command failure (binary error, etc.) — distinct from blocked

Exit code 2 is the canonical "blocked" because both codex (`Stop` hook continues on exit 2 OR `decision:"block"` JSON) and gemini (treats exit 2 as emergency-brake) honor it. Exit 1 in hooks is generally treated as command failure, not block.

### A.4 `agentchute send --ask` (flag extension)

Convenience flag for `send` that:
1. Sets frontmatter `reply_required: true`
2. Prepends `## ASK` as the first body heading if not already present

**[REV2] New defense:** `send` (regardless of `--ask`) warns if the message body or `--reply-to` references a sender with pending entries in our ledger but `--reply-to` is not provided. Helps catch "agent forgot to clear the ledger when replying."

### A.5 `agentchute defer --message <id> --reason "..." [--until <duration-or-timestamp>]` (new command) **[REV2: Phase 1, not Phase 3]**

The deferral path. Required companion to `gate --before finish` — without it, the gate can trap legitimate work. **[REV2] Now Phase 1 alongside `reply_required` + ledger + `gate --before finish`.**

**Behavior:**
- Records a deferral in the pending-reply ledger entry for `<message-id>`, with the reason, current timestamp, and optional `deferred_until`.
- Sends an automatic acknowledgment back to the original sender with `task: deferred-reply` and the reason.
- Clears the entry from the gate's blocking set; sender's `status` shows the message as `deferred` rather than `pending`.

### A.6 `agentchute watch --as <id> [--notify] [--print] [--exec <cmd>]` (new command)

Recipient-side persistent watcher. Polls or filesystem-watches the agent's inbox directory and emits configurable actions on new mail.

**Actions:**
- `--notify` — OS notification (macOS `osascript`, Linux `notify-send`). Wakes the **human operator**, not the agent. Use this when no tmux pane exists.
- `--print` — stdout line per new message. For shell pipelines.
- `--exec <cmd>` — execute a shell command on new mail. `<cmd>` receives `$AGENTCHUTE_MSG_ID`, `$AGENTCHUTE_FROM`, `$AGENTCHUTE_TASK` env vars. Use carefully — this is the only path that can wake an agent without tmux, IF the agent's wrapper supports being invoked from shell.

### A.7 `agentchute doctor --as <id>` (new command)

Diagnostic aggregator. Checks and reports on:
- Registration freshness (`STALE_REG` if older than 30m)
- Inbox state (`UNREAD`, `REPLY_DUE`)
- Wake target validity (tmux pane exists; declared `wake_method` adapter available)
- Binary location stability (warns if running from `/tmp/` or otherwise non-canonical path)
- **[REV2]** Binary on PATH — checks if `agentchute` is resolvable via `which`; warns if hook templates reference unqualified `agentchute` but binary not on PATH
- Hook file presence (checks `.claude/settings.json`, `.codex/hooks.json`, `.gemini/settings.json` for wrappers in this repo)
- **[REV2]** Hook content sanity — flags hook files containing bare `agentchute check` invocations (silent-drain risk)
- Loop dir scaffold (inbox/<self>/, archive/, malformed/ exist with correct permissions)

Exit code nonzero if any blockers found. Designed to be invoked by `boot` or by a `SessionStart` hook.

### A.8 Frontmatter additions

These are optional. Recipients that don't recognize them ignore them.

- `reply_required: true|false` — flags message as requiring a reply before archive can be considered complete. `check` renders a loud banner; `gate --before finish` blocks if pending.
- `priority: low|normal|high` — display/gate field only, not an ordering rule. Oldest-first processing unchanged.
- `reply_kind: signoff|pushback|answer|review|ack` — optional hint to the recipient about what kind of reply is expected.

### A.9 Pending-reply ledger **[REV2: expanded schema]**

A new local-state file at `<loop>/state/<agent>/pending-replies.json`. Recipient-owned.

**[REV2] Expanded schema:**
```json
{
  "pending": [
    {
      "message_id": "2026-05-19T17:53:59.561894Z",
      "from": "codex",
      "to": "claude-code",
      "task": "R1 protocol improvements",
      "original_filename": "2026-05-19T17-53-59-561894Z_from-codex_msg-8cbd.md",
      "archive_path": ".rehumanlabs/loop/archive/2026-05-19T17-54-30Z_to-claude-code_2026-05-19T17-53-59-561894Z_from-codex_msg-8cbd.md",
      "recorded_at": "2026-05-19T17:54:30Z",
      "status": "pending",
      "reply_sent_at": null,
      "reply_message_id": null,
      "deferred_at": null,
      "deferred_until": null,
      "deferred_reason": null
    }
  ]
}
```

**Lifecycle:**
- `check` archives a message with `reply_required: true` → ledger entry created with `status: pending`.
- `send --reply-to <msg-id>` → ledger entry updated to `status: replied`, `reply_sent_at` + `reply_message_id` populated.
- `defer --message <msg-id> --reason "..."` → ledger entry updated to `status: deferred`.
- `gate --before finish` reads the ledger; nonzero exit if any entries with `status: pending`. Entries with `status: deferred` or `status: replied` are not blockers.

### A.10 Sender-side wake-attempt receipt

`send` output extended to always emit:
```
Sent <filename>
  from: <id>
  to: <recipient>
  path: <full-path>
  wake_method: <method-or-"none">
  wake_attempted: <yes|no>
  wake_result: <ok|failed|skipped (no method declared)|skipped (--no-wake)>
```

`--json` flag for structured output.

### A.11 `--json` flag everywhere

All commands gain optional `--json` output: `status`, `pending`, `check`, `send`, `gate`, `doctor`, `boot`.

### A.12 `AGENTCHUTE_BIN` env variable **[REV2: new defense]**

Hook templates and operator scripts honor `AGENTCHUTE_BIN` env var. If set, used instead of bare `agentchute` (which assumes binary on PATH). Documented as the recommended pattern when binary lives at a non-canonical path.

---

## B. The three hook templates

These live under `examples/hooks/` and are committed to the repo. Users copy them into their wrapper's expected location.

### B.1 `examples/hooks/claude-code/.claude/settings.json`

```json
{
  "hooks": {
    "SessionStart": [
      {
        "command": "${AGENTCHUTE_BIN:-agentchute} boot --as claude-code --vendor anthropic --context-only"
      }
    ],
    "UserPromptSubmit": [
      {
        "command": "${AGENTCHUTE_BIN:-agentchute} pending --as claude-code --json"
      }
    ],
    "Stop": [
      {
        "command": "${AGENTCHUTE_BIN:-agentchute} gate --as claude-code --before finish --json"
      }
    ]
  }
}
```

**Notes:**
- `SessionStart` runs once per Claude Code session start. Boot is quiet on success (exit 0).
- `UserPromptSubmit` runs before each user prompt is sent to the model. Pending output is injected as developer context.
- `Stop` runs when Claude is about to stop responding. Gate's exit 2 signals "blocked" → Claude continues the turn.
- Tested against Claude Code current version (2026-05-19).

### B.2 `examples/hooks/codex/.codex/hooks.json` **[REV2: codex-hook output modes]**

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear",
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} boot --as codex --vendor openai --codex-hook SessionStart",
            "statusMessage": "agentchute: boot"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} pending --as codex --codex-hook UserPromptSubmit",
            "statusMessage": "agentchute: pending inbox"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} gate --as codex --before finish --codex-hook Stop",
            "timeout": 30,
            "statusMessage": "agentchute: finish gate"
          }
        ]
      }
    ]
  }
}
```

**Notes:**
- Tested against codex-cli 0.131.0 (verified by codex in our pool).
- **[REV2]** `pending --codex-hook UserPromptSubmit` emits the codex-specific hook JSON shape with `hookSpecificOutput.hookEventName` + `additionalContext`, so codex injects pending info as developer context.
- **[REV2]** `gate --codex-hook Stop` emits `{"decision":"block","reason":"..."}` JSON on block (exit 0 with the JSON) or exit 2 with reason on stderr. Both forms trigger codex to continue the turn.
- Project-local `.codex/` hooks require trust; users approve via `/hooks` TUI.

### B.3 `examples/hooks/gemini/.gemini/settings.json` **[REV2: verified schema]**

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "name": "agentchute-boot",
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} boot --as gemini-cli --vendor google --context-only"
          }
        ]
      }
    ],
    "BeforeAgent": [
      {
        "matcher": "*",
        "hooks": [
          {
            "name": "agentchute-pending",
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} pending --as gemini-cli --json"
          },
          {
            "name": "agentchute-gate",
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} gate --as gemini-cli --before finish --json"
          }
        ]
      }
    ]
  }
}
```

**Notes:**
- **[REV2]** Verified schema: `.gemini/settings.json` (not config.json), nested `matcher` + secondary `hooks` array.
- **[REV2] Critical:** `SessionEnd` in gemini-cli is **best-effort and cannot block**. The finish-gate is moved to `BeforeAgent` (gemini's pre-turn check) — this enforces "you can't START a new turn until your previous turn's obligations are met." This is the only reliable blocking surface in gemini-cli.
- Tested against gemini-cli 0.42.0 (verified by gemini in our pool).
- Exit 2 from `gate --before finish` is honored as block/emergency-brake by gemini.

### B.4 Fallback for non-hook wrappers: operator scheduler

For unattended/headless work, or for wrappers that don't support hooks, the operator-scheduler pattern remains:

`examples/hooks/operator/wrap-codex.sh`:
```sh
#!/bin/sh
# Operator-scheduler wrapper for headless / unattended codex use.
# For interactive codex, prefer the .codex/hooks.json template instead.
set -e
agent_id=codex
vendor=openai
AC="${AGENTCHUTE_BIN:-agentchute}"

while true; do
  "$AC" boot --as "$agent_id" --vendor "$vendor" --quiet
  codex exec -C "$(pwd)" \
    "Follow AGENTS.md. Run agentchute check, process inbox, reply to any reply-required messages, then stop."
  "$AC" gate --as "$agent_id" --before finish || \
    codex exec -C "$(pwd)" "Your inbox or pending-reply ledger blocks finishing. Process them now."
  sleep 30
done
```

Same shape for `wrap-gemini.sh`. Document as the fallback path, not the canonical one.

---

## C. Documentation changes

### C.1 Top-of-file enrollment block (replaces current 3KB blocks in CLAUDE.md / CODEX.md / GEMINI.md / AGENTS.md)

```markdown
## ENROLLMENT — agentchute coordination loop

**STOP: before any user work, run:**

```
agentchute boot --as <id> --vendor <vendor>
```

For Claude Code: `--as claude-code --vendor anthropic`
For codex: `--as codex --vendor openai`
For Gemini: `--as gemini-cli --vendor google`

This protocol overhead is **pre-authorized and required**. Do not declare consensus, sign off on another agent's proposal, tag a release, ask the user to proceed, commit, or report final completion until your inbox is empty or explicitly deferred via `agentchute defer`.

**If hooks are configured** (`.claude/settings.json` / `.codex/hooks.json` / `.gemini/settings.json`), boot runs automatically at session start. If not, run it yourself before the first user prompt.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
```

### C.2 README addition: "Do not silently drain mail in hooks"

A prominent warning in the README near the hooks section:

> ⚠ **Never use `agentchute check` in a hook unless you're certain the hook output reaches the model's context.** Bare `check` archives messages, so a silent hook can drain mail without the agent ever seeing it. **Always use `agentchute pending` or `agentchute boot` in hooks.** Use `check` only when the agent itself is invoking it interactively.

### C.3 Spec change: AGENTCHUTE.md §10.5 clarification

Current §10.5 (cooperative waking) says senders should attempt to wake recipients. Updated text:

> **§10.5 (clarified)**: when a sender's wake_method adapter is available and the recipient declares a reachable wake_method, the sender SHOULD attempt a wake poke immediately after delivery. The wake is best-effort: if the adapter fails or the recipient declares `wake_method: none`, the sender records the attempt in its send output and proceeds without blocking. **Senders MUST NOT wait synchronously for read receipts by default**; this turns mailbox delivery into blocking RPC and risks deadlock when peers are offline. Optional `send --wait-for-read --timeout <duration>` is reserved for v1.0.

### C.4 Spec change: AGENTCHUTE.md §6.1 frontmatter additions

Add to the canonical frontmatter list:
- `reply_required: true|false` (optional; default false)
- `priority: low|normal|high` (optional; default normal; display/gating only, not ordering)
- `reply_kind` (optional)
- `in_reply_to: <message-id>` (optional; threading hint)

### C.5 Spec change: new AGENTCHUTE.md §6.4 "Reply obligations" **[REV2]**

> §6.4 Reply obligations.
>
> A message with frontmatter `reply_required: true` places a reply obligation on the recipient. Archiving the message does not discharge the obligation; `agentchute check` records the pending obligation in `<loop>/state/<agent>/pending-replies.json` (the pending-reply ledger). The ledger entry status transitions: `pending` (on archive) → `replied` (on `send --reply-to`) or `deferred` (on `agentchute defer`). Lifecycle gates that consult the ledger MUST report the obligation as still open until the entry status leaves `pending`.
>
> **Body convention:** A body-level `## ASK` heading is a salience convention for the model (drawing attention to the obligation) but is NOT machine-checkable. Tooling MUST consult frontmatter, not body, to determine reply obligation state. `agentchute send --ask` sets both the frontmatter and the body heading.

---

# Part 2 — Bullet-proof guarantees: defense-in-depth analysis

For each failure mode the report and team identified, here are the overlapping defenses that v0.1.1 ships.

## Failure 1: Agent doesn't enroll (registration missing or stale)

| Layer | Mechanism |
|---|---|
| L1 | SessionStart hook (all three wrappers) runs `agentchute boot` automatically |
| L2 | Tiny first-screen enrollment block at top of CLAUDE.md / CODEX.md / GEMINI.md / AGENTS.md |
| L3 | `agentchute gate --before commit/release` flags `STALE_REG` and refuses to proceed |
| L4 | Other peers' cooperative waking (§10.5) — `status` surfaces `STALE_REG` to operator |
| L5 | `boot --emit-prompt-line` for wrappers without hooks — single-line system prompt nudge |
| L6 (v1.0) | MCP server auto-registers on first connection |

## Failure 2: Agent doesn't check inbox at the right moment (msg-43b6 class)

| Layer | Mechanism |
|---|---|
| L1 | UserPromptSubmit / BeforeAgent hook (all three wrappers) runs `agentchute pending` — peek only, injects unread state into context |
| L2 | `agentchute gate --before consensus/commit/release/finish` blocks declaring "done" |
| L3 | Codex `Stop` hook returns `decision:"block"` JSON (or exit 2) to FORCE turn continuation when gate fails. Gemini `BeforeAgent` hook blocks the next turn from starting until previous obligations cleared. |
| L4 | `agentchute watch --notify` for regular-terminal sessions — OS notification on new mail |
| L5 | Status flags (`UNREAD`, `REPLY_DUE`) visible in `status --json` |
| L6 | CLAUDE.md / CODEX.md / GEMINI.md enrollment block names blocking actions explicitly |
| L7 (v1.0) | MCP server with `wait_for_message` tool for blocking-tool-call wait |

## Failure 3: Agent reads but doesn't reply to direct question

| Layer | Mechanism |
|---|---|
| L1 | `reply_required: true` frontmatter — banner shown by `check` |
| L2 | `## ASK` body convention — first heading, model salience (not machine-checkable) |
| L3 | Pending-reply ledger — durable state at `<loop>/state/<agent>/pending-replies.json` |
| L4 | `agentchute gate --before finish` reads the ledger, blocks if any `status: pending` entries |
| L5 | Stop / BeforeAgent hook integrates with gate |
| L6 | Sender's `send --ask` flag sets both frontmatter and body convention automatically |
| L7 | Deferral path (`agentchute defer`) — explicit punt, sender notified, gate clears |
| L8 | Sender-side wake-attempt receipt — visibility into "delivered but not woken" |
| L9 **[REV2]** | `send` warns when body references a sender with pending entries but `--reply-to` not provided |

## Failure 4: Wake doesn't reach the agent

| Layer | Mechanism |
|---|---|
| L1 | `wake_method: none` explicit value — sender warns when targeting it |
| L2 | `agentchute watch` for recipient-side polling regardless of declared method |
| L3 | OS notification fallback (`wake_method: macos-notification` / `linux-notify-send`) — wakes human, who pokes agent |
| L4 | Cooperative waking (§10.5) — other peers eventually poke stale agents |
| L5 | `last_seen` freshness — `gate` and `status` surface stale |
| L6 | Sender-side wake-attempt receipt — explicit visibility |
| L7 | Operator-scheduler fallback for headless/unattended (`wrap-codex.sh`, `wrap-gemini.sh`) |
| L8 (v1.0) | Webhook wake for cross-machine; MCP `wait_for_message` for headless |

**[REV2] Honest caveat:** When `wake_method: none` AND no operator-side watcher is running AND headless (no human at the terminal), only L7 (operator-scheduler) remains. This is called out as "human-relay required" in the docs.

## Failure 5: Hook drains mail silently without model seeing it

| Layer | Mechanism |
|---|---|
| L1 | Hook templates use `pending` (peek), never `check` (archive) |
| L2 | README warning: "Never use `agentchute check` in a hook unless..." |
| L3 | `pending` is genuinely side-effect-free at the CLI level — no archive, no quarantine, no peer wake |
| L4 | Hook template comments inline explain why peek not check |
| L5 | `--json` output enables structured parsing |
| L6 | `agentchute doctor` flags hooks files containing bare `check` invocations **[REV2]** |

## Failure 6: Wrapper CLI updates and breaks hook schema

| Layer | Mechanism |
|---|---|
| L1 | `agentchute init --print-hooks` shows current schema for each wrapper |
| L2 | `agentchute doctor` validates hook files against current schemas |
| L3 | Version notes inline in each template (which CLI version each was tested against) |
| L4 | Quarterly verification ritual (operator or test runner pings each wrapper for current schema) |

## Failure 7: Multiple readers of one inbox (wrapper + watcher both consume)

| Layer | Mechanism |
|---|---|
| L1 (v0.1.1) | `watch --notify` is default; explicit `--exec <cmd>` is opt-in with operator awareness |
| L2 (v0.1.1) | `pending` is genuinely read-only; multiple `pending` callers cannot corrupt state |
| L3 (v1.0) | Inbox lease semantics for true multi-reader safety |

## Failure 8: Agent runtime forgets to honor pre-authorized protocol overhead

| Layer | Mechanism |
|---|---|
| L1 | Tiny enrollment block (≤200 words) at top of wrapper file with explicit "pre-authorized and required" language |
| L2 | SessionStart hook runs automatically |
| L3 | Stop/BeforeAgent hook fails turn if gate blocked |
| L4 | `pending` output appears as developer context per turn — model sees it |
| L5 | `--emit-prompt-line` injects single line for non-hook wrappers |

## Failure 9: Fresh session, no hooks installed yet (post-init, pre-template-copy) **[REV2 — new]**

| Layer | Mechanism |
|---|---|
| L1 | Tiny enrollment block in CLAUDE.md / CODEX.md / etc. is the only operational instruction the agent reads |
| L2 | `agentchute boot --emit-prompt-line` output can be appended to the system prompt by operator at session start |
| L3 | `agentchute init` recommends hook template installation and points to `examples/hooks/<wrapper>/` |
| L4 | `doctor` warns "no hook template detected for current wrapper" |
| L5 (operator) | Manual `agentchute boot` at session start before any user work |

This is the weakest defense-in-depth set. Mitigation is to make the post-init UX guide operators to install hooks immediately.

## Failure 10: Binary not on PATH **[REV2 — new]**

| Layer | Mechanism |
|---|---|
| L1 | Hook templates use `${AGENTCHUTE_BIN:-agentchute}` so an env var can override |
| L2 | `agentchute doctor` checks PATH resolution and warns |
| L3 | `init --install-hooks` defaults to embedding the absolute path in generated hook files (with operator confirmation) |
| L4 | Hook failure surfaces in wrapper logs; operator catches and adjusts |

---

# Part 3 — Implementation order

Phase 1 is the must-ship v0.1.1 minimum. Phases 2-3 round out the release.

## Phase 1 (ship-it minimum) **[REV2: defer added]**

1. **`agentchute pending`** (read-only) — schema-free addition. Hook-safe output modes including `--codex-hook UserPromptSubmit`.
2. **`agentchute gate --before consensus|commit|release|finish`** — exit 2 on block; `--codex-hook Stop` mode for codex.
3. **`reply_required: true` frontmatter + pending-reply ledger + `send --ask` + `send --reply-to` clearing**.
4. **`agentchute defer`** — required companion to finish-gate. [REV2: was Phase 3, now Phase 1.]
5. **`agentchute boot`** — thin wrapper over `register` + `pending`. Exit 0 on refresh success; exit 2 on block.

These five deliver the entire msg-43b6 fix.

## Phase 2 (canonical patterns)

6. **Three hook templates** in `examples/hooks/{claude-code,codex,gemini}/` — committed and documented. Ship alongside Phase 1.
7. **Top-of-file enrollment block** rewrites in CLAUDE.md / CODEX.md / GEMINI.md / AGENTS.md / wrapper template.
8. **README updates** — hooks section, "do not silently drain mail" warning, AGENTCHUTE.md §6.4 reply obligations, §10.5 clarification.

## Phase 3 (polish + completeness)

9. **`agentchute watch`** with `--notify`, `--print`, `--exec` actions.
10. **OS notification wake adapter** (`wake_method: macos-notification` / `wake_method: linux-notify-send`) with explicit human-relay labeling.
11. **Sender-side wake-attempt receipt** + `send` warning when missing `--reply-to`.
12. **`--json` flag** added to all commands.
13. **`agentchute doctor`** aggregator with `AGENTCHUTE_BIN` / PATH check + hook sanity scan.
14. **Operator-scheduler skeleton scripts** (`wrap-codex.sh`, `wrap-gemini.sh`) in `examples/hooks/operator/`.
15. **Migration story** — `agentchute init` updates existing enrollment blocks idempotently with diff/dry-run.
16. **Test fixtures** reproducing the acceptance criteria below.

---

# Part 4 — Acceptance criteria (must pass before v0.1.1 tag)

## Test 1: msg-43b6 reproduction **[REV2: rewritten]**
- Setup 2-agent pool. Send a reply-required message with `## ASK` to agent B.
- Agent B runs `check` (NOT just `pending`) — message is archived AND ledger entry created with `status: pending`.
- Agent B starts a consensus workflow, calls `gate --before consensus` — must exit 2 (blocked, ledger entry pending).
- Agent B runs `send --reply-to <msg-id>` — ledger entry transitions to `status: replied`, `reply_sent_at` populated.
- `gate --before consensus` now exits 0.

**Separate test:** Verify `pending` alone (without `check`) leaves the inbox unchanged, so the message still appears as unread direct mail.

## Test 2: Hook safety (no silent drain)
- Configure Claude Code SessionStart + UserPromptSubmit hooks per the template.
- Send 3 messages to claude-code. Trigger several user prompts.
- Verify: (a) inbox file count unchanged after the hooks run; (b) hook output (JSON) appears in agent's context.

## Test 3: Stale registration block
- Skip `boot` for >30m. Try to send. `gate --before commit` reports `STALE_REG` (exit 2). After `boot`, gate passes (exit 0).

## Test 4: No-tmux fallback
- Spin up an agent in a regular terminal (not tmux). Run `agentchute watch --as <id> --notify`.
- Send to it from another shell. Verify: OS notification fires; agent's `pending` shows the message after operator pokes it.

## Test 5: Deferral path **[REV2: now Phase 1]**
- Reply-required message arrives. Agent runs `check` (archive + ledger entry `pending`).
- Agent runs `defer --message <id> --reason "needs research"`.
- Ledger entry status transitions to `deferred`; `deferred_at` and `deferred_reason` populated.
- `gate --before finish` exits 0 (deferred is not a blocker).
- Sender's `status` shows the message as `deferred` rather than `pending`.

## Test 6: Three-wrapper hook smoke test **[REV2: explicit codex hook output check]**
- For each of `.claude/settings.json`, `.codex/hooks.json`, `.gemini/settings.json`:
  - Start the wrapper with the template installed.
  - Verify SessionStart hook fires (boot output observed).
  - Send a reply-required message to the agent; submit a user prompt; verify pending output appears in context.
  - Have the wrapper try to declare "done"; verify Stop / BeforeAgent hook fails gate.
  - **[REV2]** For codex specifically: verify `UserPromptSubmit` hook's `--codex-hook` output produces model-visible developer context; verify `Stop` hook's `--codex-hook` output triggers turn continuation.

## Test 7: `pending` is truly read-only
- Run `agentchute pending --as <id>` 1000 times against a populated inbox.
- Verify: inbox file count unchanged, archive count unchanged, no peer wake pokes sent.

## Test 8 **[REV2: new]**: `boot` exit-code matrix
- Fresh registration → `boot` exits 0, `--json` shows `refreshed: true`.
- Registration <30m old, no unread → `boot` exits 0, `--json` shows `refreshed: true`.
- Registration with 1 unread direct mail → `boot` exits 2, `--json` shows `unread_count: 1`.
- Registration with 1 pending-reply ledger entry → `boot` exits 2, `--json` shows `replies_pending: 1`.
- Binary error (e.g., loop dir missing) → `boot` exits 1 (command failure, distinct from blocked).

## Test 9 **[REV2: new]**: Ledger identity (no collision)
- Send two reply-required messages from same sender with same `task` value but different message_ids.
- Verify: ledger has two separate entries, distinguished by `message_id` AND `original_filename`.
- Reply to one (`send --reply-to <msg-id>`); other remains `pending`.
- `gate --before finish` still blocks (one pending entry remains).

## Test 10 **[REV2: new]**: Binary-not-on-PATH warning
- Run `agentchute doctor` in an environment where `agentchute` is not on PATH (e.g., binary at `/tmp/agentchute` only).
- Verify: doctor warns "binary not on PATH; hook templates may fail; consider setting `AGENTCHUTE_BIN`".

## Test 11 **[REV2: new; REV3 wording fix]**: `send --ask` writes frontmatter + body heading
- Run `agentchute send --from <a> --to <b> --task "review" --ask --body "look at the diff"`.
- **[REV3]** Verify: the file delivered to `<b>`'s inbox has `reply_required: true` in frontmatter AND `## ASK` as first body heading.
- When `<b>` runs `check`, the message is archived AND the reply-required banner is shown.

---

# Part 5 — Open verification items (before ship)

**[REV2: most items now resolved.]**

1. ~~**Gemini hooks schema/filename**~~ — **RESOLVED** (gemini's R2 verified). `.gemini/settings.json`, nested matcher+hooks, `BeforeAgent` is the blocking surface (not `SessionEnd`).

2. ~~**Codex `Stop` hook decision: "block"**~~ — **RESOLVED** (codex's R2 verified). Codex continues on stdout JSON `{"decision":"block","reason":"..."}` with exit 0, OR exit 2 with reason on stderr. Our `gate --codex-hook Stop` flag emits one of these forms.

3. **Codex App Automations** — VERIFY-AS-WE-SHIP. Cron-like scheduling in the *desktop app*, not the CLI. Defer to v1.x; document for future reference.

4. **MCP server scope** — DECISION. Defer to v1.0 to avoid v0.1.1 scope creep. Codex/Gemini/Claude all consume MCP; codex also serves it (`codex mcp-server`). Designing `agentchute-mcp` server can begin in parallel but ships in v1.0.

5. **`agentchute init --install-hooks`** — DECISION. Default to `--print-hooks` (preview only); `--install-hooks --yes` requires explicit operator consent.

6. **Watcher process supervision** — VERIFY-AS-WE-SHIP. Document operator-owned supervision (launchd/systemd unit files) in `examples/hooks/operator/` but don't auto-install.

---

# Part 6 — What's NOT in v0.1.1 (explicit non-goals)

- Read receipts and reply receipts (`read_at`, `replied_at` state files)
- Authenticated webhook wake (`wake_method: webhook` with HMAC)
- MCP `agentchute` server (auto-registration, `wait_for_message` tool)
- Hard `inbox_blocks_work: true` declared profile
- Threading metadata (`in_reply_to` value usage beyond display)
- Inbox leases / multi-reader safety
- Cross-pool index at `~/.agentchute/pools.json`
- Mention syntax `@agent-id` (beyond display highlighting)
- Priority as ordering rule
- Escalation policies (notify operator after N minutes)
- Capability advertisement
- `expires_at` / TTL with auto-archive

All v1.0 work. Resist.

---

# Summary of mods applied

**REV2** applied 13 mods from codex + gemini review round:

1. ✅ Exit code 2 (not 1) for blocked, in `gate` and `boot`
2. ✅ Codex hook output modes (`--codex-hook UserPromptSubmit` / `--codex-hook Stop`)
3. ✅ Gemini schema verified: `.gemini/settings.json`, nested structure, finish-gate moved to `BeforeAgent`
4. ✅ `boot` exits 0 on refresh success
5. ✅ `defer` moved to Phase 1
6. ✅ Pending-reply ledger schema expanded (10 fields)
7. ✅ `--max-age` removed; `--stale-after` annotates rather than filters
8. ✅ Body `## ASK` is salience-only; frontmatter `reply_required` is canonical
9. ✅ `gate --before reply` cut; phases are `consensus|commit|release|finish`
10. ✅ Test 1 rewritten for ledger-flow correctness
11. ✅ Failure 9 + Failure 10 added (fresh session no hooks; binary-not-on-PATH)
12. ✅ `AGENTCHUTE_BIN` env var across templates
13. ✅ New `send` warning when missing `--reply-to` for pending senders

**REV3** applied 4 mods from codex's rev2 verification:

14. ✅ `boot --context-only` flag (generic) — hook-safe mode for SessionStart, exit 0 always
15. ✅ `boot --codex-hook SessionStart` flag — codex-specific JSON output shape
16. ✅ All three SessionStart templates updated to use context-only / codex-hook modes
17. ✅ Test 11 wording corrected ("archived file" → "delivered to inbox + check shows banner")

— claude-code, rev3 after codex + gemini rev2 verification. Codex flagged SessionStart/boot exit-code interaction; gemini signed off outright. Ready for re-verification of rev3 changes.
