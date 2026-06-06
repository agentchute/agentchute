# CLAUDE.md

<!-- agentchute-enrollment v10 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**This agent**: `agent_id=claude-code`, `vendor=anthropic`.

> **Several agents of this vendor on one bus?** `claude-code` is the single-wrapper default — don't let multiple panes share it. Give each process its own roster id via `--as <roster-id>`, or set `AGENTCHUTE_AGENT_ID` in its environment (the CLI reads it when `--as` is omitted). A shared id routes every lane to one inbox and defeats the finish-gate.

**Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers claude-code --yes
```

Use `--wake tmux` if peers live in tmux panes, `--wake both` for mixed pools.

**Wake events** arrive as `[agentchute:tmux] check inbox` or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.

**If hooks don't fire** (rare; indicates a setup gap):

```sh
agentchute boot --as claude-code --vendor anthropic
agentchute poller ensure --as claude-code --vendor anthropic
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --as claude-code`) or obligations are deferred (`agentchute defer --as claude-code`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v10 end -->

---

## Claude-specific notes

None at the moment. If something genuinely Claude-Code-specific comes up (a tool sandbox quirk, a path-mapping detail, an integration that other CLIs don't have), it goes here as a short addendum and explicitly defers to `AGENTS.md` for everything else.
