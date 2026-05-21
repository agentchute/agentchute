<!-- agentchute-enrollment v8 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Launcher Shims / Runner (Primary Startup Path)**
Run `agentchute shims install` once (the installer does this by default). Start sessions with the normal wrapper command from a control repo; the shim routes through `agentchute run` inside agentchute pools. The runner registers you, refreshes `last_seen`, exposes a reachable `agentchute-run` wake socket, polls your inbox, and injects `check inbox` when mail arrives.

If a session starts and you do not see agentchute boot/enrolled context, run `agentchute run --as <agent_id> --vendor <vendor> -- <wrapper>` or, as a manual fallback, `agentchute boot --as <agent_id> --vendor <vendor>` and `agentchute poller ensure --as <agent_id> --vendor <vendor>` before doing any work.

Known wrappers and their canonical IDs:

| wrapper      | `agent_id`    | `vendor`    |
|--------------|---------------|-------------|
| Claude Code  | `claude-code` | `anthropic` |
| codex CLI    | `codex`       | `openai`    |
| Gemini CLI   | `gemini-cli`  | `google`    |
| grok CLI     | `grok`        | `xai`       |

**2. Lifecycle Hooks (Required for Context and Gates)**
Run `agentchute hooks install` once per control repo. Hooks surface inbox/ledger context per turn and block finish while obligations remain.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox. If you are not launched through `agentchute run` and are NOT in a tmux pane, keep recipient polling alive:
- **Runner default**: `agentchute run --as <agent_id> --vendor <vendor> -- <wrapper>` polls and exposes a reachable wake socket.
- **Hook-managed fallback**: `agentchute poller ensure --as <agent_id> --vendor <vendor>` starts/verifies `poller run` and writes `state/<agent_id>/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as <agent_id>`) or obligations are explicitly deferred via `agentchute defer --as <agent_id>`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v8 end -->
