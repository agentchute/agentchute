# CODEX.md

<!-- agentchute-enrollment v14 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**Default wrapper identity**: base `agent_id=codex`, `vendor=openai`. When `--as` and `AGENTCHUTE_AGENT_ID` are omitted, the CLI derives a project-scoped id like `codex-<folder>` and adds `-2`, `-3`, etc. if another live lane already owns that id.

> **Several agents of this vendor on one bus?** Let the contextual default allocate separate ids per project/worktree, or give each process its own stable roster id via `--as <roster-id>` / `AGENTCHUTE_AGENT_ID=<roster-id>`. A shared id routes every lane to one inbox and defeats the finish-gate.

For a custom stable lane name, set the session identity first:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"
```

**Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers codex --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the namespaced launcher for this wrapper.

Use `--wake runner` for the universal launcher+socket path; add `tmux` or `herdr` if peers reach you via pane send-keys (e.g. `--wake runner,tmux`).

Start runner-mode sessions with the installed `ac-*` launcher for this wrapper.

**Wake events** arrive as `[agentchute:tmux] check inbox`, `[agentchute:herdr] check inbox`, or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.


**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --vendor openai
agentchute poller ensure --vendor openai
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --vendor openai`) or obligations are deferred (`agentchute defer --vendor openai --message <message-id> --reason "..."`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v14 end -->

---

## Codex-Specific Notes

- Default posture: review first. Identify bugs, scope creep, behavioral regressions, missing tests, and unclear spec/code mismatches before drafting.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Keep patches narrow and use the standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Do not reach into the chorus-protocol sibling repo from this repo (see HANDOFF.md for context). agentchute is independent.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules; codex's review posture (concise, file:line cited, severity-ordered findings) flows from the rules there.
