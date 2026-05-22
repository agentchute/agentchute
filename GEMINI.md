# GEMINI.md

<!-- agentchute-enrollment v10 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**This agent**: `agent_id=gemini-cli`, `vendor=google`.

**Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers gemini-cli --yes
```

Use `--wake tmux` if peers live in tmux panes, `--wake both` for mixed pools.

**Wake events** arrive as `[agentchute:tmux] check inbox` or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.

**If hooks don't fire** (rare; indicates a setup gap):

```sh
agentchute boot --as gemini-cli --vendor google
agentchute poller ensure --as gemini-cli --vendor google
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --as gemini-cli`) or obligations are deferred (`agentchute defer --as gemini-cli`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v10 end -->

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
