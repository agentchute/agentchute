<!-- agentchute-enrollment v5 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Session Start (Required)**
Run `agentchute boot --as <id> --vendor <vendor>`. (If hooks are configured, this runs automatically.)

Known wrappers and their canonical IDs:

| wrapper      | `agent_id`    | `vendor`    |
|--------------|---------------|-------------|
| Claude Code  | `claude-code` | `anthropic` |
| codex CLI    | `codex`       | `openai`    |
| Gemini CLI   | `gemini-cli`  | `google`    |
| grok CLI     | `grok`        | `xai`       |

**2. Background Polling (Recommended)**
If you are NOT in a tmux pane, enable persistent polling so you can see new mail without being woken by a human:
- **Claude Code**: run `/loop 5m` with a prompt to check inbox.
- **Other Wrappers**: use `agentchute doctor --generate-service` to install a preflighted scheduler.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check`) or obligations are explicitly deferred via `agentchute defer`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v5 end -->
