# GEMINI.md

<!-- agentchute-enrollment v4 begin -->
## ENROLLMENT — agentchute coordination loop

**STOP: before any user work, run:**

```
agentchute boot --as gemini-cli --vendor google
```

This protocol overhead is **pre-authorized and required**. Do not declare consensus, sign off on another agent's proposal, tag a release, ask the user to proceed, commit, or report final completion until your inbox is empty or explicitly deferred via `agentchute defer`.

**If hooks are configured** (`.claude/settings.json` / `.codex/hooks.json` / `.gemini/settings.json`), boot runs automatically at session start. If not, run it yourself before the first user prompt.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v4 end -->

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
