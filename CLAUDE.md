# CLAUDE.md

<!-- agentchute-enrollment v7 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Automated Hooks (Primary)**
Run `agentchute hooks install --wrapper claude-code` once. From then on, every session boots automatically, non-tmux sessions start/verify a recipient poller, and your inbox/ledger obligations land in your context per turn.

If a session starts and you do not see an agentchute boot/enrolled context line, run `agentchute boot --as claude-code --vendor anthropic` and, outside tmux, `agentchute poller ensure --as claude-code --vendor anthropic` before doing any work.

**2. Recipient Polling (Required Outside tmux)**
Senders only deliver to your inbox. If you are NOT in a tmux pane, you must keep recipient polling alive:
- **Hook-managed default**: `agentchute poller ensure --as claude-code --vendor anthropic` starts/verifies `poller run` and writes `state/claude-code/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as claude-code`) or obligations are explicitly deferred via `agentchute defer --as claude-code`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v7 end -->

---

## Claude-specific notes

None at the moment. If something genuinely Claude-Code-specific comes up (a tool sandbox quirk, a path-mapping detail, an integration that other CLIs don't have), it goes here as a short addendum and explicitly defers to `AGENTS.md` for everything else.
