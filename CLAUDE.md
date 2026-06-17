# CLAUDE.md

<!-- agentchute-enrollment v14 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**Default wrapper identity**: base `agent_id=claude-code`, `vendor=anthropic`. When `--as` and `AGENTCHUTE_AGENT_ID` are omitted, the CLI derives a project-scoped id like `claude-code-<folder>` and adds `-2`, `-3`, etc. if another live lane already owns that id.

> **Several agents of this vendor on one bus?** Let the contextual default allocate separate ids per project/worktree, or give each process its own stable roster id via `--as <roster-id>` / `AGENTCHUTE_AGENT_ID=<roster-id>`. A shared id routes every lane to one inbox and defeats the finish-gate.

For a custom stable lane name, set the session identity first:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"
```

**Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers claude-code --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the namespaced launcher for this wrapper.

Use `--wake runner` for the universal launcher+socket path; add `tmux` or `herdr` if peers reach you via pane send-keys (e.g. `--wake runner,tmux`).

Start runner-mode sessions with the installed `ac-*` launcher for this wrapper.

**Wake events** arrive as `[agentchute:tmux] check inbox`, `[agentchute:herdr] check inbox`, or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.


**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --vendor anthropic
agentchute poller ensure --vendor anthropic
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --vendor anthropic`) or obligations are deferred (`agentchute defer --vendor anthropic --message <message-id> --reason "..."`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v14 end -->

---

## Claude-specific notes

None at the moment. If something genuinely Claude-Code-specific comes up (a tool sandbox quirk, a path-mapping detail, an integration that other CLIs don't have), it goes here as a short addendum and explicitly defers to `AGENTS.md` for everything else.
