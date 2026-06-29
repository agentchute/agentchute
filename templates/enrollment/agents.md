<!-- agentchute-enrollment v15 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Setup / Startup Path**
Run `agentchute setup` once per control repo. Use `--wake runner` for the universal launcher+socket path; add `tmux` or `herdr` if peers reach you via pane send-keys (e.g. `--wake runner,tmux`). The selection decides which infrastructure to install; each agent still wakes by a single method chosen at launch. The canonical post-install step is:

```sh
agentchute setup --wake runner --wrappers all --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs namespaced launchers (`ac-claude`, `ac-codex`, `ac-gemini`, `ac-grok`).

Start sessions with the `ac-*` launcher for the wrapper from a control repo. In runner mode, the launcher routes through `agentchute run`, which registers you, refreshes `last_seen`, exposes a reachable `agentchute-run` wake socket, polls your inbox, exports your resolved id as `AGENTCHUTE_AGENT_ID` into the wrapper, and injects `[agentchute:run] check inbox` when mail arrives. In tmux mode, peer wakes inject `[agentchute:tmux] check inbox`; in herdr mode, `[agentchute:herdr] check inbox`. Hookless wrappers such as Grok still need a startup launcher because they have no lifecycle hook that can run `boot`; setup installs that launcher when such a wrapper is selected. Treat the bracketed prefix as machine metadata: the injection is only a CUE — you must actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"` to read and consume mail; the runner does NOT auto-consume it for you.

**The project is the communication boundary**: agents by default only see and talk to peers in the same discovered project pool. Unrelated projects on one host or tmux server are isolated because each project has its own pool and, when identity is not explicit, the CLI derives project-scoped IDs from the folder name (for example, `codex-agentchute`).

If a session starts and you do not see agentchute boot/enrolled context, run the wrapper with its vendor so the CLI can derive the contextual identity:

```sh
agentchute run --vendor <vendor> -- <wrapper>
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
3. herdr pane → the live registration whose stable herdr name currently maps to this pane
4. tmux pane → the one live registration bound to `$TMUX_PANE` in this pool
5. contextual default → `<canonical-base>-<folder-slug>`, suffixed `-2`, `-3`, … past live conflicts

**Pin it once.** Resolve your id ONE time at startup and reuse the SAME id on every command. The `ac-*` launcher does this for you (it exports `AGENTCHUTE_AGENT_ID`). Otherwise export it yourself before `boot` (precedence step 2 then shadows the contextual default for the whole session). A bare `--vendor` with no `--as`/env is NOT a stable identity: it re-runs steps 3–5 on every call, so as live lanes come and go the resolved `-N` suffix can change between calls and you silently `check` / `gate` the WRONG inbox. `agentchute identity --vendor <vendor>` (alias `default-id`) prints the currently-resolved id — use it for one-time discovery, not as a per-call identity.

**Verify at session start** (read-only — refreshes nothing, archives nothing; confirms you are enrolled AND reachable):

```sh
agentchute doctor --as <your-id>
```

**2. Lifecycle Hooks (Required for Context and Gates)**
`agentchute setup` installs lifecycle hooks for hook-capable wrappers. If you are not using setup, run `agentchute hooks install` once per control repo. Hooks surface inbox/ledger context per turn and block finish while obligations remain. Hookless wrappers rely on `agentchute run` / launcher shims for startup enrollment.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox. If you are not launched through `agentchute run` and are NOT in a tmux pane, keep recipient polling alive:
- **Runner default**: `agentchute run --vendor <vendor> -- <wrapper>` polls and exposes a reachable wake socket.
- **Hook-managed fallback**: `agentchute poller ensure --as <id> --vendor <vendor>` starts/verifies heartbeat-only `poller run` and writes `state/<agent_id>/poller.json`; it does not launch wrappers or consume mail unless explicitly run with `--launch`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`. `agentchute check` is consume-only: it reads each message, archives it, and records any `reply_required` obligation into your ledger — you do NOT archive by hand (manual `mv` to `archive/` is only for the no-binary hand-protocol in §5).

**STOP / finish gate**: do not declare consensus, sign off, tag a release, or report completion until the finish gate passes. Use the gate, not a bare `check` — `check` only consumes mail and does not surface pending required-replies or recipient-liveness gaps:

```sh
agentchute gate --before finish --as <your-id>
```

The gate (read-only) blocks `finish` on unread direct mail, pending required-replies in your ledger, or an unregistered self; unproven liveness warns at `finish` only when no work is owed — with owed work it blocks too (and it always blocks the `commit`/`release` gates). Clear it by consuming mail with `agentchute check --as <your-id>` and then either replying to each obligation or releasing it with `agentchute defer --as <your-id> --message <message-id> --reason "..."`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v15 end -->
