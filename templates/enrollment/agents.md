<!-- agentchute-enrollment v19 begin -->
## ENROLLMENT — agentchute coordination loop

**1. Setup / Startup Path**
Run `agentchute setup` once per control repo. `runner` is the only supported wake path: coordination is pull-only, so senders write your inbox and never poke you, and the runner polls your own inbox to wake you. (`--wake all`/`both` are deprecated aliases for `runner`; `tmux`/`herdr` adapters were removed.) The canonical post-install step is:

```sh
agentchute setup --wake runner --wrappers all --yes
```

> **Note**: A new shell session (or manually sourcing your profile) is required for the PATH changes to take effect. Setup adds the shim directory to PATH and installs the single `ac` dispatcher (`ac serve <wrapper>`; `run` is a deprecated alias for `serve`, removed in v0.10.0).

Start sessions with `ac serve <wrapper>` from a control repo (`run` is a deprecated alias for `serve`, removed in v0.10.0). The dispatcher routes through `agentchute serve`, which registers you, acquires a serve lease (id-uniqueness + fencing token), refreshes `last_seen` and your `.live` presence, polls your OWN inbox, exports your resolved id as `AGENTCHUTE_AGENT_ID` into the wrapper, and injects `[agentchute] check inbox` when mail arrives. The runner publishes no wake target — peers never poke it (pull-only). Hookless wrappers such as Grok still need the dispatcher for startup because they have no lifecycle hook that can run `boot`; `ac serve <wrapper>` enrolls them. Treat the bracketed prefix as machine metadata: the injection is only a CUE — you must actually RUN `agentchute check --as "$AGENTCHUTE_AGENT_ID"` to claim mail (then `ack` to commit); the runner does NOT consume it for you.

**The project is the communication boundary**: agents by default only see and talk to peers in the same discovered project pool. Unrelated projects on one host or tmux server are isolated because each project has its own pool and, when identity is not explicit, the CLI derives project-scoped IDs from the folder name (for example, `codex-agentchute`).

If a session starts and you do not see agentchute boot/enrolled context, run the wrapper with its vendor so the CLI can derive the contextual identity:

```sh
agentchute serve --vendor <vendor> -- <wrapper>
```

As a manual fallback, pin your identity ONCE and then enroll under it before doing any work:

```sh
export AGENTCHUTE_AGENT_ID="$(agentchute identity --vendor <vendor>)"   # or a named roster id
agentchute boot --as "$AGENTCHUTE_AGENT_ID" --vendor <vendor>
agentchute poller ensure --as "$AGENTCHUTE_AGENT_ID" --vendor <vendor>
agentchute check --as "$AGENTCHUTE_AGENT_ID"
```

If a first `check` says you are not registered, do this fallback immediately instead of stopping. Capture the id with `identity` (or pick a roster id) BEFORE `boot`, because once a live registration reserves the base id a later bare resolve returns a different `-N` suffix.

Known wrappers and their canonical IDs:

| wrapper      | `agent_id`    | `vendor`    |
|--------------|---------------|-------------|
| Claude Code  | `claude-code` | `anthropic` |
| codex CLI    | `codex`       | `openai`    |
| Gemini CLI   | `gemini-cli`  | `google`    |
| grok CLI     | `grok`        | `xai`       |

The IDs above are wrapper bases. With no explicit identity, the reference CLI derives `<base>-<folder>` and reserves live conflicts with `-2`, `-3`, etc. **When several agents of one vendor share a bus** (e.g. `claude-l1`/`claude-l2`/`merger` all on the `claude-code` wrapper), each process must still enroll under its own id. Use contextual defaults for ordinary project/worktree lanes; use `AGENTCHUTE_AGENT_ID=<roster-id>` or `--as <roster-id>` for named lanes.

**Identity precedence** (the reference CLI resolves your `agent_id` in this exact order, first match wins):

1. `--as <id>` flag
2. `AGENTCHUTE_AGENT_ID` env var
3. contextual default → `<canonical-base>-<folder-slug>`, suffixed `-2`, `-3`, … past live conflicts

(Pull-only registrations carry no wake target, so there is no longer a herdr/tmux pane to map back to a prior registration — id comes from `--as` / `$AGENTCHUTE_AGENT_ID` or the contextual default.)

**Pin it once.** Resolve your id ONE time at startup and reuse the SAME id on every command. The `ac` dispatcher does this for you (it exports `AGENTCHUTE_AGENT_ID`). Otherwise export it yourself before `boot` (precedence step 2 then shadows the contextual default for the whole session). A bare `--vendor` with no `--as`/env is NOT a stable identity: it re-derives the contextual default (step 3) on every call, so as live lanes come and go the resolved `-N` suffix can change between calls and you silently `check` / `gate` the WRONG inbox. `agentchute identity --vendor <vendor>` (alias `default-id`) prints the currently-resolved id — use it for one-time discovery, not as a per-call identity.

**Verify at session start** (read-only — refreshes nothing, archives nothing; confirms you are enrolled AND present via a fresh `.live`):

```sh
agentchute doctor --as <your-id>
```

**2. Lifecycle Hooks (Required for Context and Gates)**
`agentchute setup` installs lifecycle hooks for hook-capable wrappers. If you are not using setup, run `agentchute hooks install` once per control repo. Hooks surface inbox context per turn and block finish while unread mail remains. Hookless wrappers rely on the `ac` dispatcher (`ac serve <wrapper>`) for startup enrollment.

**3. Recipient Polling Fallback**
Senders only deliver to your inbox (pull-only; nobody pokes you). If you are not launched through `agentchute serve`, keep recipient polling alive so your `.live` presence stays fresh:
- **Runner default**: `agentchute serve --vendor <vendor> -- <wrapper>` polls your own inbox, keeps `.live` fresh, and injects the `check inbox` cue.
- **Hook-managed fallback**: `agentchute poller ensure --as <id> --vendor <vendor>` starts/verifies heartbeat-only `poller run` and writes `state/<agent_id>/poller.json` + `.live`; it does not launch wrappers or consume mail unless explicitly run with `--launch`.
- **Native loops**: if your wrapper has a recurring task feature, it may replace `poller run` only if it keeps a fresh heartbeat.

**4. In-Session Catchup**
If hooks are configured, you will catch new mail mid-turn via `gate --before continue`. Consumption is two-phase: `agentchute check` CLAIMS each message (moves it to `inbox/<id>/.claimed/`) and displays it — it does NOT archive; `agentchute ack` commits (archives) the claimed mail. A crash between `check` and `ack` re-delivers (at-least-once), so handlers must be idempotent. You do NOT archive by hand (manual `mv` to `archive/` is only for the no-binary hand-protocol in §5).

**STOP / finish gate**: do not declare consensus, sign off, tag a release, or report completion until the finish gate passes. Use the gate, not a bare `check` — `check` only claims mail, while the gate is the read-only STOP verdict (unread/malformed mail, unregistered self):

```sh
agentchute gate --before finish --as <your-id>
```

The gate (read-only) blocks `finish` on unread direct mail or an unregistered self; it does NOT check `.live` at `finish`/`continue` (a stale/absent `.live` blocks only the `commit`/`release` gates). Reply obligations are asker-owned only: outstanding/expired `.owed` obligations surface as non-blocking warnings, and a `reply_required` message never blocks the recipient. Clear the gate by consuming mail with `agentchute check --as <your-id>` (then `ack`); reply to any message that needs one with `agentchute send --reply-to <ref>`.

Hand-protocol path (no binary): see [`AGENTCHUTE.md`](AGENTCHUTE.md) §5.
<!-- agentchute-enrollment v19 end -->
