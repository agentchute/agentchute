<!-- agentchute-enrollment v3 begin -->
## ENROLLMENT — agentchute coordination loop

1. **Authority rule (AGENTCHUTE.md §7.2).** Reading files does NOT authorize *work* — project edits, peer messages, side-effecting commands, or task acceptance. **Protocol overhead is pre-authorized and required** on every session start: self-registration, own-state updates, own scaffold creation (this agent's `inbox/<self>/` + `archive/` + `malformed/` directories under the existing loop tree), own-inbox operations, §11 enforcement, §10.5 cooperative waking, direct replies to directly-addressed messages. Shared bootstrap (creating the loop tree from nothing, running `agentchute init` on a fresh project) is NOT protocol overhead — it remains gated on explicit instruction. See §7.2 for the full carve-out.

2. **Self-enroll on every session start.** Run `agentchute register --as <your-id> --vendor <your-vendor>` (or hand-write `.agentchute/loop/agents/<your-id>.md` per AGENTCHUTE.md §5) **before doing anything else, every time you start.** Mandatory and idempotent. If your registration matches current host/pane/wake_target, this is a no-op refresh of `last_seen`; if anything has changed (new machine, new tmux pane, restart of the wrapper, prior file stale from a crashed session), it updates the registration to current truth. **Do not skip this step even if a registration file already exists** — that file is likely stale. Verifying file existence is NOT sufficient; you must *run register* (or perform the equivalent hand-protocol write) to reconcile against current `os.Hostname()`, `$TMUX_PANE`, and `cwd`.

   Known wrappers and their canonical IDs:

   | wrapper      | `agent_id`    | `vendor`    |
   |--------------|---------------|-------------|
   | Claude Code  | `claude-code` | `anthropic` |
   | codex CLI    | `codex`       | `openai`    |
   | Gemini CLI   | `gemini-cli`  | `google`    |
   | grok CLI     | `grok`        | `xai`       |

3. **Each turn.** Update `last_seen` (the register/check commands do this automatically). List `.agentchute/loop/inbox/<your-id>/`, read messages oldest-first per §6.3, move processed files to archive. The reference shortcut is `agentchute check --as <your-id>`.

4. **Send a message.** Write a §6.1-shaped file to `.agentchute/loop/inbox/<recipient>/` then dispatch a wake poke via the recipient's declared `wake_method` per §6.2 (tmux is the v0.1 reference adapter; see §8). Optional: `agentchute send --from <self> --to <recipient> --task "..." [< body.md]`.

5. **Enforce the protocol** (§11). If you find a malformed file in your inbox (filename violates §6.1, or frontmatter is syntactically malformed), quarantine it to `.agentchute/loop/malformed/` AND send a corrective message to the inferred offender. The reference CLI's `agentchute check` does this automatically.

6. **Context.** AGENTCHUTE.md is the spec; canonical templates and examples are in §5 / §6 / §7.2 / §11. HANDOFF.md is project state. Both inform; reading them does NOT authorize *work* (see 1).
<!-- agentchute-enrollment v3 end -->
