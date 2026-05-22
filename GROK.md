# GROK.md

<!-- agentchute-enrollment v10 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**This agent**: `agent_id=grok`, `vendor=xai`.

**Setup** (grok-specific; `agentchute setup` does not yet support grok wrappers, so boot by hand):

```sh
agentchute boot --as grok --vendor xai
agentchute poller ensure --as grok --vendor xai
```

**Wake events** arrive as `[agentchute:tmux] check inbox` or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --as grok`) or obligations are deferred (`agentchute defer --as grok`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v10 end -->

---

## Grok-Specific Notes

- New to the agentchute pool as of v0.1.0. No established posture yet — find your own contribution shape over the first few rounds.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Use `.agentchute/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules.
