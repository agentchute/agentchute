<!-- agentchute-enrollment v7 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Automated Hooks (Primary)**
Run `agentchute hooks install --wrapper {{AS}}` once. From then on, every session boots automatically, non-tmux sessions start/verify a recipient poller, and your inbox/ledger obligations land in your context per turn.

If a session starts and you do not see an agentchute boot/enrolled context line, run `agentchute boot --as {{AS}} --vendor {{VENDOR}}` and, outside tmux, `agentchute poller ensure --as {{AS}} --vendor {{VENDOR}}` before doing any work.

**2. Recipient Polling (Required Outside tmux)**
Senders only deliver to your inbox. If you are NOT in a tmux pane, you must keep recipient polling alive:
- **Hook-managed default**: `agentchute poller ensure --as {{AS}} --vendor {{VENDOR}}` starts/verifies `poller run` and writes `state/{{AS}}/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as {{AS}}`) or obligations are explicitly deferred via `agentchute defer --as {{AS}}`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v7 end -->
