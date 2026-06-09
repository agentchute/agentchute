# GROK.md

<!-- agentchute-enrollment v11 begin -->
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

Use `--wake tmux` if peers live in tmux panes, `--wake both` for mixed pools.

**Wake events** arrive as `[agentchute:tmux] check inbox` or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.

**If hooks don't fire** (rare; indicates a setup gap):

```sh
agentchute boot --vendor xai
agentchute poller ensure --vendor xai
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --vendor xai`) or obligations are deferred (`agentchute defer --vendor xai --message <message-id> --reason "..."`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v11 end -->

---

## Grok-Specific Notes

- **Wake path is the runner shim, not lifecycle hooks.** The grok CLI has no repo hook system (no `settings.json`/`hooks.json`, no SessionStart/UserPromptSubmit/Stop events), so `agentchute setup --wrappers grok` installs a launcher shim that routes `grok` through `agentchute run` and skips hook install. Use `--wake runner` (or `both`); a pure `--wake tmux` pool relies on peer pokes plus the polling baseline. `agentchute hooks install` has no grok target by design.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules.
