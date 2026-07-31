# update-fix-v2: `agentchute update` must never finish green with a broken hook

Status: ACCEPTED (codex decision, ref `20260731T183110119815Z_from-codex`, 2026-07-31) — implemented in the PR that carries this document.
Authorization: Alex ("running `agentchute update` is breaking agentchute, and that is not acceptable — everyone expects it to work"); coordinate the fix, review with the team, merge to main; no tag, no release.

## Problem

`docs/decisions/agentchute-v150-cutover-incident-and-fix.md` fixed *detection*: doctor now BLOCKs on a hook file invoking an unknown subcommand, and `update` prints a warning when a resync will replay `setup --wrappers none` while hook files exist on disk. Detection is not the contract. A warning scrolling by during an unattended upgrade does not keep the fleet working — the v1.5.0 cutover outage happened with a human watching and still took ~4 minutes to notice and fix by hand.

## The invariant

After `agentchute update` completes successfully, every installed hook file at a known `hookWrappers` path works with the new binary (passes the hook subcommand validation shipped in the v1.5.0 cutover fix). If update cannot make that true, it must say so loudly and exit non-zero — never finish green with broken hooks.

## Design frame (supersedes part of the v1.5.0 cutover fix's decision B)

That decision treated hook refresh as a wrapper-**membership** question — operator intent, un-inferable from an empty recorded wrapper list. That conflated two separate things:

- **Membership** (shims, enrollment blocks, which wrappers the pool manages): stays explicit, `--wrappers` only. Unchanged.
- **Compatibility** (an existing hook file at an agentchute-owned path must keep working after a binary swap): an invariant, not an intent question. The file's existence on disk *is* the recorded intent. Refreshing it creates nothing, removes nothing, and changes no membership; a `.bak` preserves whatever was there before (the existing `installOneHook` force semantics already do this — the fix reuses them, not new mechanism).

## Decision

1. **Refresh-existing, in the setup-resync path only** (`internal/cli/hooks.go: refreshHookCompatibility`, called from `internal/cli/setup.go: applySetup`, Phase 2.5 — after wrapper-membership hook install/removal, before the destructive runtime reset and before "setup complete"). Refreshes every hook file that already exists at a `hookWrappers` Dest, regardless of the recorded wrapper list. Existence-preserving no-create mode: a `--wrappers none` pool with no hook files on disk is a no-op. `update.go` is deliberately not touched a second time — one compatibility phase, not a duplicated repair/verification.
2. **Verify-before-green** (`internal/cli/hooks.go: verifyHookCompatibility`, called immediately after refresh, same placement). Reuses doctor's token scan (`hookBodyUnknownSubcommands`, factored out of `checkHookContentSanity` in `internal/cli/doctor.go` so the two checks can never drift on what counts as "broken") over every currently-installed hook file. Any unknown-subcommand hit is actionable and returns a non-zero error naming the file, the token, and the fix command — no rollback of the binary swap (not attempted; the old updater already propagates setup failure the same way). Because refresh always writes the current canonical template first, a naturally-occurring verification failure here would itself be a packaging bug; `applySetupVerifyHookCompatibility` is a package var so tests can inject one without waiting for that bug to exist.
3. **`--no-resync` stays untouched-and-silent with respect to hooks.** It waives the hook-compatibility postcondition entirely (bootstrap-honest: no new-binary code runs on that path) — same as before this fix.
4. **The wrappers-null warning is now membership-only.** Renamed `hookRefreshSkipWarning` → `wrappersUnrecordedWarning` (`internal/cli/update.go`); it no longer claims hooks will not refresh, and no longer prescribes `hooks install` as the safety repair — that repair is now automatic. It still fires when a resync's recorded wrapper list is empty while hook files exist, because the pool's membership bookkeeping being out of date is still worth surfacing.
5. **This document** supersedes decision item 2 of `docs/decisions/agentchute-v150-cutover-incident-and-fix.md` (marked there with a pointer back here).

On a verification failure, `applySetup` returns before Phase 3 (the destructive runtime reset): running supervisors are left un-fenced rather than forced to restart straight into a hook that would fail on its very next invocation. `agentchute update`'s existing resync-failure handling (`cmdUpdate`, `internal/cli/update.go`) already treats any non-zero `setup` exit as a failed resync — it prints the manual fix command and returns a non-zero error without ever reaching the final restart-success line — so no changes were needed there.

Out of scope: auto-repairing `setup.json`'s `wrappers: null` (unchanged from the prior decision — operator re-records at the next planned restart); any change to `--wrappers none`'s membership semantics; protocol or wire changes; new subcommands.

## Test plan

- `internal/cli/hooks_test.go`: `refreshHookCompatibility`/`verifyHookCompatibility` as pure functions — stale existing file refreshed with an exact `.bak`, missing destination stays missing, already-current file gets no backup, unknown-subcommand detection and its clearance after refresh.
- `internal/cli/setup_test.go`, exercising the real `cmdSetup` entrypoint: the three invariants above under a genuine `--wrappers none` resync; a stale hook file for a wrapper outside the run's recorded membership (never dropped, never selected) still refreshed — compatibility is independent of membership; a forced verification failure (injected via `applySetupVerifyHookCompatibility`) exits non-zero, prints neither "setup complete" nor reaches the destructive reset, and names the fix command.
- `internal/cli/update_test.go`: `TestUpdate_NoResyncSkipsSetupReSync` extended to assert hook bytes are byte-identical and no backup exists after a `--no-resync` update; `wrappersUnrecordedWarning`'s wording asserted to no longer claim hooks are unsafe or prescribe `hooks install`.
- Existing hook_content_sanity / hooks-install suites hold unchanged after the `hookBodyUnknownSubcommands` extraction (behavior-preserving refactor).

## Rollout

PR to main; review by codex (deep, final gate, independent full-suite run) + grok (read-only cross-check against the live incident, bus verdict); claude-code verifies first-hand (CI, diff scope vs the E3 freeze, independent suite run) and squash-merges after both verdicts land and CI is green.
