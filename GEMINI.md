# GEMINI.md

<!-- agentchute-enrollment v7 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Automated Hooks (Primary)**
Run `agentchute hooks install --wrapper gemini-cli` once. From then on, every session boots automatically, non-tmux sessions start/verify a recipient poller, and your inbox/ledger obligations land in your context per turn.

If a session starts and you do not see an agentchute boot/enrolled context line, run `agentchute boot --as gemini-cli --vendor google` and, outside tmux, `agentchute poller ensure --as gemini-cli --vendor google` before doing any work.

**2. Recipient Polling (Required Outside tmux)**
Senders only deliver to your inbox. If you are NOT in a tmux pane, you must keep recipient polling alive:
- **Hook-managed default**: `agentchute poller ensure --as gemini-cli --vendor google` starts/verifies `poller run` and writes `state/gemini-cli/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as gemini-cli`) or obligations are explicitly deferred via `agentchute defer --as gemini-cli`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v7 end -->

---

## Tool-Specific Notes

- **CLI Quirks:** You operate in a monospaced CLI environment. Keep responses high-signal and low-filler.
- **Methodology:** Follow the working rules in `AGENTS.md`; for review-shaped tasks, lead with file:line citations and severity-ordered findings.

## Working Rules Overrides

- None. Follow **AGENTS.md** strictly.

> Self-description (interests, working style, etc.) belongs in this agent's
> registration body — `agentchute register --bio "..."` — not in the wrapper
> file. Wrappers are read by peers, and peers MUST NOT route work by declared
> capability (§7 item 3 / §12). Anything that reads like a capability
> advertisement here pre-authorizes the routing it would forbid in the spec.
