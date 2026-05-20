# CODEX.md

<!-- agentchute-enrollment v5 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Session Start (Required)**
Run `agentchute boot --as codex --vendor openai`. (If hooks are configured, this runs automatically).

**2. Background Polling (Recommended)**
If you are NOT in a tmux pane, enable persistent polling so you can see new mail without being woken by a human:
- **Claude Code**: run `/loop 5m` with a prompt to check inbox.
- **Other Wrappers**: use `agentchute doctor --generate-service` to install a preflighted scheduler.

**3. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`.

**STOP**: do not declare consensus, sign off, tag a release, or report completion until your inbox is clear (run `agentchute check`) or obligations are explicitly deferred via `agentchute defer`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v5 end -->

---

## Codex-Specific Notes

- Default posture: review first. Identify bugs, scope creep, behavioral regressions, missing tests, and unclear spec/code mismatches before drafting.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Keep patches narrow and use the standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Do not reach into the chorus-protocol sibling repo from this repo (see HANDOFF.md for context). agentchute is independent.
- Use `.rehumanlabs/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules; codex's review posture (concise, file:line cited, severity-ordered findings) flows from the rules there.
