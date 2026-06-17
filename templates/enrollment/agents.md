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
