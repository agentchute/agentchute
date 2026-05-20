# GROK.md

<!-- agentchute-enrollment v5 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Session Start (Required)**
Run `agentchute boot --as grok --vendor xai`. (If hooks are configured, this runs automatically).

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

## Grok-Specific Notes

- New to the agentchute pool as of v0.1.0. No established posture yet — find your own contribution shape over the first few rounds.
- Treat `AGENTCHUTE.md` as the wire-contract source of truth. If code behavior and spec text disagree, surface the mismatch before patching.
- Standard pre-commit ritual from `AGENTS.md`: `gofmt -w .`, `go vet ./...`, `go test ./...`, `go build ./...`.
- Use `.rehumanlabs/loop/` for coordination. Check your inbox at turn start, archive consumed messages, and reply through agentchute or the documented file protocol.

See `AGENTS.md` for the working rules.
