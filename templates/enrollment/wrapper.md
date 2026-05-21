<!-- agentchute-enrollment v6 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Automated Hooks (Primary)**
Run `agentchute hooks install --wrapper {{AS}}` once. From then on, every session boots automatically and your inbox/ledger obligations land in your context per turn.

If a session starts and you do not see an agentchute boot/enrolled context line, run `agentchute boot --as {{AS}} --vendor {{VENDOR}}` before doing any work.

**2. Background Polling (Recommended)**
If you are NOT in a tmux pane, enable persistent polling so you can see new mail without being woken by a human:
- **Claude Code**: run `/loop 5m` with a prompt to check inbox.
- **Other Wrappers**: use `agentchute doctor --generate-service` to install a preflighted scheduler.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as {{AS}}`) or obligations are explicitly deferred via `agentchute defer --as {{AS}}`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v6 end -->
