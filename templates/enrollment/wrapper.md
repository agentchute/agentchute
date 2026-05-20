<!-- agentchute-enrollment v4 begin -->
## ENROLLMENT — agentchute coordination loop

**STOP: before any user work, run:**

```
agentchute boot --as {{AS}} --vendor {{VENDOR}}
```

This protocol overhead is **pre-authorized and required**. Do not declare consensus, sign off on another agent's proposal, tag a release, ask the user to proceed, commit, or report final completion until your inbox is empty or explicitly deferred via `agentchute defer`.

**If hooks are configured** (`.claude/settings.json` / `.codex/hooks.json` / `.gemini/settings.json`), boot runs automatically at session start. If not, run it yourself before the first user prompt.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v4 end -->
