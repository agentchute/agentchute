# CLAUDE.md

<!-- agentchute-enrollment v9 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Setup / Startup Path**
Run `agentchute setup` once per control repo. Choose `tmux` when tmux is the primary wake path, `runner` when launcher shims should route wrappers through `agentchute run`, or `both` for mixed pools. Non-interactive examples:

```sh
agentchute setup --wake runner --wrappers claude-code --yes
agentchute setup --wake tmux --wrappers claude-code --yes
agentchute setup --wake both --wrappers claude-code --yes
```

Start sessions with the normal wrapper command from a control repo. In runner mode, the shim routes through `agentchute run --as claude-code --vendor anthropic -- <real-wrapper>`, which registers you, refreshes `last_seen`, exposes a reachable `agentchute-run` wake socket, polls your inbox, and injects `[agentchute:run] check inbox` when mail arrives. In tmux mode, peer wakes inject `[agentchute:tmux] check inbox`. Treat the bracketed prefix as machine metadata and follow the inbox-check instruction.

If a session starts and you do not see agentchute boot/enrolled context, run `agentchute run --as claude-code --vendor anthropic -- <wrapper>` or, as a manual fallback, `agentchute boot --as claude-code --vendor anthropic` and `agentchute poller ensure --as claude-code --vendor anthropic` before doing any work.

**2. Lifecycle Hooks (Required for Context and Gates)**
`agentchute setup` installs lifecycle hooks. If you are not using setup, run `agentchute hooks install --wrapper claude-code` once per control repo. Hooks surface inbox/ledger context per turn and block finish while obligations remain.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox. If you are not launched through `agentchute run` and are NOT in a tmux pane, keep recipient polling alive:
- **Runner default**: `agentchute run --as claude-code --vendor anthropic -- <wrapper>` polls and exposes a reachable wake socket.
- **Hook-managed fallback**: `agentchute poller ensure --as claude-code --vendor anthropic` starts/verifies `poller run` and writes `state/claude-code/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as claude-code`) or obligations are explicitly deferred via `agentchute defer --as claude-code`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v9 end -->

---

## Claude-specific notes

None at the moment. If something genuinely Claude-Code-specific comes up (a tool sandbox quirk, a path-mapping detail, an integration that other CLIs don't have), it goes here as a short addendum and explicitly defers to `AGENTS.md` for everything else.
