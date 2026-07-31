# v1.5.0 cutover incident: stale hook templates, and the fix

Status: ACCEPTED (codex decision B, ref `20260731T175038205661Z_from-codex`, 2026-07-31) — implemented in the PR that carries this document.
Authorization: Alex ("brief the team and fix everything with a full review"); merge to main in scope, tag/release out of scope.

## Incident

On 2026-07-31 the fleet updated from a Protocol v2 CLI to 1.5.0 via `agentchute update`. Immediately after, every Claude Code prompt was blocked by its UserPromptSubmit hook:

```
[${AGENTCHUTE_BIN:-agentchute} poller ensure --vendor anthropic --quiet]: agentchute: unknown command "poller"
```

The installed hook templates in the control repo (`.claude/settings.json`, `.codex/hooks.json`, `.gemini/settings.json`) still invoked `poller ensure`, a subcommand the update removed. The outage lasted ~4 minutes until an operator-side `agentchute hooks install --wrapper all --scope repo --force` rewrote all three files from the 1.5.0 embedded templates.

Timeline (UTC, from file mtimes, registration rows, and `setup.json`):

- ~17:25 — old updater swaps the binary, re-execs the new binary's `setup`.
- 17:27:55 — replayed setup writes `setup.json`; hook refresh skipped (below). Serve leases invalidated (designed); lanes relaunch on 1.5.0.
- ~17:30 — stale `poller` hooks × new binary → prompts blocked fleet-wide.
- 17:31:17 — manual `hooks install --force` rewrites all three hook files (`.bak` backups). Outage over.

## Root cause

Three layers, ordered cause → missing detection → compounding drift:

1. **The setup replay had no wrappers to replay.** The pre-v2.5 pool state never recorded a wrapper list, so `readSetupPoolState` returned `wrappers: null`, and `cmdUpdate` maps an empty list to `--wrappers none` (`internal/cli/update.go:120-124` — deliberate: empty is the valid "none" mode, indistinguishable from missing). The replayed `setup --wrappers none` therefore installed no hook templates, while the binary moved past the installed ones. The replay also re-records `wrappers` as empty, so the state **self-perpetuates**: every future `update` of this pool would skip hook refresh the same way.
2. **doctor could not see the breakage.** `hook_content_sanity` validates the forbidden `check` subcommand and that the binary reference resolves — it never validates referenced subcommands against the command registry (`commandHandlers`, `internal/cli/dispatch.go` — "the single source of truth for agentchute's subcommands"). A hook invoking a removed subcommand passes doctor cleanly.
3. **The control-repo checkout compounded the confusion.** The live checkout sat ~40 commits behind main, and main's own tracked `.claude/settings.json` was a pre-1.5 hook shape (no PreToolUse guard, Stop still running `ack` + `gate`) — so both the working tree and a fresh clone disagreed with the binary's embedded templates.

## Decision (option B)

Detection belongs in doctor; update warns but does not hard-fail; the state repair is deferred to the operator.

1. **doctor: unknown-subcommand validation (BLOCKER).** Extend `hook_content_sanity`: extract every `<binary-ref> <subcommand>` token from installed hook commands (parsed via the existing `hookCommandBody`, so `permissions.allow` entries stay out of scope) and require the token to be a registered command. Message names the wrapper file and the unknown token, and gives the fix (`agentchute hooks install --wrapper all --scope repo --force`). This turns the incident's silent failure mode into a named blocker.
2. **update: loud warning when hook refresh is skipped.** ~~When the replayed wrapper list resolves to `none` while wrapper hook files exist on disk, `update` prints a warning naming the affected wrappers, the refresh command above, and `agentchute setup --wrappers ...` to re-record the pool. No behavior change to the replay itself — a hard-fail branch inside update was considered and rejected (option C) as heavier than the incident justifies.~~ **SUPERSEDED** by `docs/decisions/agentchute-update-fix-v2.md`: a warning is detection, not the contract Alex asked for ("running `agentchute update` is breaking agentchute, and that is not acceptable"). update-fix-v2 makes hook refresh a resync-time invariant, independent of recorded wrapper membership — see that document for the design and the (renamed, reworded) membership-only note that replaces this warning.
3. **Delete the duplicate hook-path table.** `doctor.go` kept its own `hookFiles` list of the three wrapper paths, parallel to `hookWrappers` in `hooks.go`. Two hand-maintained copies of the same table are a drift hazard; doctor now derives paths from `hookWrappers`.
4. **Refresh the tracked `.claude/settings.json`.** This repo is its own control repo; its committed hook file must match the embedded template. It now does (guard + turn-end shape, `turn-end` permission included).
5. **This document** is the incident record.

Out of scope: auto-repairing `setup.json` (rerunning `setup` re-fences all live supervisors; Alex re-records wrappers at the next planned restart); any change to the replay's `none` semantics; protocol or wire changes. ~~**operator rule until then: do not run `agentchute update` on this pool**~~ **SUPERSEDED** by `docs/decisions/agentchute-update-fix-v2.md`: that blanket prohibition existed only because a `wrappers: null` resync used to skip hook refresh entirely. update-fix-v2 makes hook compatibility a resync invariant independent of recorded membership, so `agentchute update` is safe to run on this pool again. `wrappers: null` is now purely a membership-bookkeeping gap (surfaced by the renamed `wrappersUnrecordedWarning`, no longer a safety warning) — re-record it with `agentchute setup --wrappers <list>` whenever convenient, not as a precondition for updating.

## Test plan

- Fixture hook file invoking `poller ensure` → `hook_content_sanity` BLOCKER naming `poller`, plus the fix hint.
- Hyphenated subcommands (`self-check`, `turn-end`, `boot --context-only` flags) pass the token extraction.
- `permissions.allow` entry naming an unknown subcommand, hooks clean → OK (regression guard for the PR #74 false-positive class).
- Canonical installed templates still pass end-to-end (existing `TestDoctorCanonicalInstalledTemplatesPassHookContentSanity` now also exercises the new rule).
- The update warning decision is a pure helper unit-tested without network; skip/fire cases for: no hook files, hook files + `none`, hook files + recorded wrappers.
- Existing presence/drift tests hold after the table dedup.

## Rollout

PR to main; review by codex + sonnet + grok (pinned SHAs); merge after verdicts and green CI. Live-checkout switch to main and the fleet-side verification are operational steps tracked in the recovery brief (`.agentchute/loop/scratch-brief-2026-07-31-v150-recovery.md`), not in this PR.
