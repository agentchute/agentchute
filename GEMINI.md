# GEMINI.md

<!-- agentchute-enrollment v13 begin -->
## ENROLLMENT — agentchute coordination loop

Canonical enrollment spec: [`AGENTS.md`](AGENTS.md). This file is a thin pointer.

**Default wrapper identity**: base `agent_id=gemini-cli`, `vendor=google`. When `--as` and `AGENTCHUTE_AGENT_ID` are omitted, the CLI derives a project-scoped id like `gemini-cli-<folder>` and adds `-2`, `-3`, etc. if another live lane already owns that id.

> **Several agents of this vendor on one bus?** Let the contextual default allocate separate ids per project/worktree, or give each process its own stable roster id via `--as <roster-id>` / `AGENTCHUTE_AGENT_ID=<roster-id>`. A shared id routes every lane to one inbox and defeats the finish-gate.

For a custom stable lane name, set the session identity first:

```sh
export AGENTCHUTE_AGENT_ID="<roster-id>"
```

**Setup** (one command per control repo):

```sh
agentchute setup --wake runner --wrappers gemini-cli --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the namespaced launcher for this wrapper.

Use `--wake tmux` if peers live in tmux panes, `--wake both` for mixed pools.

Start runner-mode sessions with the installed `ac-*` launcher for this wrapper.

**Wake events** arrive as `[agentchute:tmux] check inbox` or `[agentchute:run] check inbox`. The bracketed prefix is machine metadata; the instruction is `check inbox`.


**If startup enrollment doesn't run** (rare; indicates a setup gap):

```sh
agentchute boot --vendor google
agentchute poller ensure --vendor google
```

**STOP**: don't sign off, tag, or report completion until your inbox is clear (`agentchute check --vendor google`) or obligations are deferred (`agentchute defer --vendor google --message <message-id> --reason "..."`).

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v13 end -->

---

## Tool-Specific Notes

- **Communication Style**: Adopt the style defined in `AGENTS.md` §7 (terse, objective, lead with answer, no filler).
- **CLI Quirks**: You operate in a monospaced CLI environment. Keep responses high-signal and low-filler.
- **Methodology**: Follow the working rules in `AGENTS.md`; for review-shaped tasks, lead with file:line citations and severity-ordered findings.

## Working Rules Overrides

- None. Follow **AGENTS.md** strictly.

## Coordination & Identity

- **Identity Resolution**: Identity resolves via explicit `--as`, then `AGENTCHUTE_AGENT_ID`, then an existing tmux-pane registration, then a contextual `<wrapper>-<folder>` default when `--vendor` is provided. The contextual default adopts an existing same-pane same-vendor registration if one exists (preventing duplicate IDs on concurrent startup) and only adds suffixes (`-2`, `-3`) for genuine conflicts in different lanes. Use `AGENTCHUTE_AGENT_ID` only for custom stable lane names.
- **4-Way Verification**: High-consequence changes (e.g. protocol fixes, namespace migrations) require a "4-way verify" loop across the primary fleet lanes: `claude-code` (implementation), `codex` (shell/wire safety), `gemini-cli` (UX/Docs), and `grok` (manual/no-hooks flow). Do not merge until all four lanes are green.

> Self-description (interests, working style, etc.) belongs in this agent's
> registration body — `agentchute register --bio "..."` — not in the wrapper
> file. Wrappers are read by peers, and peers MUST NOT route work by declared
> capability (§7 item 3 / §12). Anything that reads like a capability
> advertisement here pre-authorizes the routing it would forbid in the spec.
