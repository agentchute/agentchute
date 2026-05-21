# CODEX.md

<!-- agentchute-enrollment v9 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Setup / Startup Path**
Run `agentchute setup` once per control repo. Choose `tmux` when tmux is the primary wake path, `runner` when launcher shims should route wrappers through `agentchute run`, or `both` for mixed pools. Non-interactive examples:

```sh
agentchute setup --wake runner --wrappers codex --yes
agentchute setup --wake tmux --wrappers codex --yes
agentchute setup --wake both --wrappers codex --yes
```

Start sessions with the normal wrapper command from a control repo. In runner mode, the shim routes through `agentchute run --as codex --vendor openai -- <real-wrapper>`, which registers you, refreshes `last_seen`, exposes a reachable `agentchute-run` wake socket, polls your inbox, and injects `[agentchute:run] check inbox` when mail arrives. In tmux mode, peer wakes inject `[agentchute:tmux] check inbox`. Treat the bracketed prefix as machine metadata and follow the inbox-check instruction.

If a session starts and you do not see agentchute boot/enrolled context, run `agentchute run --as codex --vendor openai -- <wrapper>` or, as a manual fallback, `agentchute boot --as codex --vendor openai` and `agentchute poller ensure --as codex --vendor openai` before doing any work.

**2. Lifecycle Hooks (Required for Context and Gates)**
`agentchute setup` installs lifecycle hooks. If you are not using setup, run `agentchute hooks install --wrapper codex` once per control repo. Hooks surface inbox/ledger context per turn and block finish while obligations remain.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox. If you are not launched through `agentchute run` and are NOT in a tmux pane, keep recipient polling alive:
- **Runner default**: `agentchute run --as codex --vendor openai -- <wrapper>` polls and exposes a reachable wake socket.
- **Hook-managed fallback**: `agentchute poller ensure --as codex --vendor openai` starts/verifies `poller run` and writes `state/codex/poller.json`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh poller heartbeat.
- **Generated services**: `agentchute doctor --generate-service` emits launchd/systemd/script schedulers that call `self-poll --heartbeat`.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check --as codex`) or obligations are explicitly deferred via `agentchute defer --as codex`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v9 end -->

---

## Codex-Specific Notes

- Default posture: review first. Identify bugs, scope creep, behavioral regressions, missing tests, and unclear spec/code mismatches before drafting.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Keep patches narrow and use the standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Do not reach into the chorus-protocol sibling repo from this repo (see HANDOFF.md for context). agentchute is independent.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules; codex's review posture (concise, file:line cited, severity-ordered findings) flows from the rules there.
