# CODEX.md

<!-- agentchute-enrollment v4 begin -->
## ENROLLMENT — agentchute coordination loop

**STOP: before any user work, run:**

```
agentchute boot --as codex --vendor openai
```

This protocol overhead is **pre-authorized and required**. Do not declare consensus, sign off on another agent's proposal, tag a release, ask the user to proceed, commit, or report final completion until your inbox is empty or explicitly deferred via `agentchute defer`.

**If hooks are configured** (`.claude/settings.json` / `.codex/hooks.json` / `.gemini/settings.json`), boot runs automatically at session start. If not, run it yourself before the first user prompt.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v4 end -->

---

## Codex-Specific Notes

- Default posture: review first. Identify bugs, scope creep, behavioral regressions, missing tests, and unclear spec/code mismatches before drafting.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Keep patches narrow and use the standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Do not reach into the chorus-protocol sibling repo from this repo (see HANDOFF.md for context). agentchute is independent.
- Use `.rehumanlabs/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules; codex's review posture (concise, file:line cited, severity-ordered findings) flows from the rules there.
