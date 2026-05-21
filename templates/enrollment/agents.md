<!-- agentchute-enrollment v7 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Automated Hooks (Primary)**
Run `agentchute hooks install` once. From then on, every session boots automatically, non-tmux sessions start/verify a recipient poller, and your inbox/ledger obligations land in your context per turn.

If a session starts and you do not see an agentchute boot/enrolled context line, run `agentchute boot --as <agent_id> --vendor <vendor>` and, outside tmux, `agentchute poller ensure --as <agent_id> --vendor <vendor>` before doing any work.

Known wrappers and their canonical IDs:

| wrapper      | `agent_id`    | `vendor`    |
|--------------|---------------|-------------|
| Claude Code  | `claude-code` | `anthropic` |
| codex CLI    | `codex`       | `openai`    |
| Gemini CLI   | `gemini-cli`  | `google`    |
| grok CLI     | `grok`        | `xai`       |

**2. Recipient Polling (Required Outside tmux)**
Senders only deliver to your inbox. If you are NOT in a tmux pane, you must keep recipient polling alive:
- **Hook-managed default**: `agentchute poller ensure --as <agent_id> --vendor <vendor>` starts/verifies `poller run` and writes `state/<agent_id>/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as <agent_id>`) or obligations are explicitly deferred via `agentchute defer --as <agent_id>`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v7 end -->
