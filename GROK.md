# GROK.md

<!-- agentchute-enrollment v13 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**Default wrapper identity**: base `agent_id=grok`, `vendor=xai`. When `--as` and `AGENTCHUTE_AGENT_ID` are omitted, the CLI derives a project-scoped id like `grok-<folder>` and adds `-2`, `-3`, etc. if another live lane already owns that id.

> **Several agents of this vendor on one bus?** Let the contextual default allocate separate ids per project/worktree, or give each process its own stable roster id via `--as <roster-id>` / `AGENTCHUTE_AGENT_ID=<roster-id>`. A shared id routes every lane to one inbox and defeats the finish-gate.

For a custom stable lane name, set the session identity first:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"
```

**Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers grok --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the namespaced launcher for this wrapper.

Use `--wake tmux` if peers live in tmux panes, `--wake herdr` if in herdr panes, `--wake both` for mixed pools.

Start runner-mode sessions with the installed `ac-*` launcher for this wrapper.

**Wake events** arrive as `[agentchute:tmux] check inbox`, `[agentchute:herdr] check inbox`, or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.


**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --vendor xai
agentchute poller ensure --vendor xai
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --vendor xai`) or obligations are deferred (`agentchute defer --vendor xai --message <message-id> --reason "..."`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v13 end -->

---

## Grok-Specific Notes

- **Wake path is the runner launcher, not lifecycle hooks.** The grok CLI has no repo hook system (no `settings.json`/`hooks.json`, no SessionStart/UserPromptSubmit/Stop events), so `agentchute setup --wrappers grok` installs `ac-grok`, which routes through `agentchute run` and skips hook install. In runner/both mode this follows the normal shim path; in tmux/herdr modes setup still installs `ac-grok` because no hook can run startup enrollment. `agentchute hooks install` has no grok target by design.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules.
