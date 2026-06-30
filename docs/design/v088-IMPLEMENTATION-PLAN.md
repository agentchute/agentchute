# v0.8.8 — implementation plan (claude + codex, converged)

**Mandate (Alex):** three work items, autonomous build. Process: claude+codex draft this plan → **4-way team review of the plan** → claude+codex implement with a **review at each gate** → **4-way final review** → **claude tags & releases v0.8.8** (full release, like v0.8.0). Stall fallback: proceed solo + self-review, never block.

**Base:** `feat/v0.8.8-dispatcher-clean-install` from `origin/main` 7c56491 (= v0.8.0), clean worktree. main is protected → squash PR → tag.

**Decisions locked:** endpoint = release. #2 = fully REMOVE generated `ac-*` shims this release → single `ac` dispatcher. Enrollment templates → **v16**.

**Decisions claude+codex converged (team to confirm):**
- **Invocation: `ac run <wrapper>` canonical** (e.g. `ac run claude`, `ac run codex`); `ac check`/`ac send`/`ac doctor` for commands. Rationale: this release already does full shim removal + destructive clean — keep parser magic minimal. Implicit `ac <wrapper>` only as a secondary sugar IF the bounded parser stays strict; both leans say start canonical-only.
- **Global flags before the subcommand:** `ac --as reviewer run codex …` (pick one grammar, test hard).

**Crown-jewel NO-TOUCH:** do not modify the PTY submit path in `run.go` — constants `bracketedPasteStart`/`bracketedPasteEnd`/`codexEnhancedEnter`, `injectPrompt`, `promptInjectionBytes`, and the PTY write/termios/SIGWINCH path. The dispatcher changes only *how we invoke* `run`, never how the runner submits `[agentchute:run] check inbox`. The 3 exact-byte submit tests guard this.

**Critical environment findings (codex scan):**
- `command -v ac` today = `/usr/sbin/ac` (system accounting). The dispatcher installs at `$shim_dir/ac` and **must NOT touch `/usr/sbin/ac`**; setup must ensure `$shim_dir` precedes `/usr/sbin` on PATH, and `doctor`/`setup` must report which `ac` resolves (and warn "new shell / PATH" if a non-owned `ac` wins).
- Existing `setup --reset --wipe-state --dry-run` **mis-reports live runner PIDs as "ambiguous … cmdline did not match this pool"** though `ps` clearly matches `--control-repo`/`--loop-dir`. That matching path is buggy and must be fixed before clean-install automation relies on it (Gate 3).
- Stale binary backups present: `~/.local/bin/agentchute.pre-hidden-poller-fix-20260616T213600Z`, `~/.local/bin/agentchute.pre-wakefix.bak`, `~/code/agentchute/agentchute.stale-dev-jun19.bak`.

---

## Gates

### Gate 0 — plan + 4-way review (this doc)
Plan committed; reviewed by the team before any code. Adopt edits, then start Gate 1.

### Gate 1 — `wrapperSpec` + `ac` dispatcher parser (additive, no setup/install changes)
- `shimSpec` → `wrapperSpec` (canonical wrapper key, aliases, vendor, agent id, executable candidates incl. `agy`). Still load-bearing for `ac run`, setup detection, vendor inference, real-wrapper resolution.
- Add the `ac` dispatcher command path. Canonical `ac run <wrapper> [args…]`; `ac <agentchute-subcommand>` routes normal CLI commands.
- **Bounded parser:** known command ⇒ command; known wrapper alias ⇒ run; unknown ⇒ error with suggestions; **no arbitrary-PATH-executable inference; command wins collisions.** Global flags before subcommand.
- Tests: command-wins, wrapper-alias-resolves, unknown→suggestions, no PATH inference, vendor/candidate resolution, command/wrapper collision.
- **Risk:** parser ambiguity / future namespace collisions. Compile-green, additive.

### Gate 2 — dispatcher install + full `ac-*` removal
- setup installs ONE `$shim_dir/ac` dispatcher (stops generating per-wrapper scripts).
- Remove setup-owned `ac-*` only via the `isAgentchuteShim` marker (`shims exec --name` + `AGENTCHUTE_BIN=`); leave any user-owned same-name file. Update setup state schema (e.g. `dispatcher_installed`, schema/version bump) so `ShimsInstalled` no longer implies per-wrapper shims.
- Collision guard: refuse a non-agentchute `$shim_dir/ac`; never touch `/usr/sbin/ac`. doctor reports the resolved `ac` + PATH order. Drop the runner-child false-positive warnings (the pid-8848 `unenrolled_presence`/`launch_provenance` over-warn).
- install.sh dry-run/fresh plan text: "launcher shims" → "dispatcher".
- **Writes-before-reset invariant:** dispatcher/hooks/templates written FIRST, destructive removal LAST — never strand a pool with no launch path.
- Tests: install/setup idempotency, old `ac-*` cleanup, non-agentchute `ac` refusal, PATH-shadow report, wrapper detection still skips the dispatcher dir.
- **Risk:** `/usr/sbin/ac` collision; stranding users if cleanup precedes dispatcher landing.

### Gate 3 — clean-remains audit planner + guarded destructive cleanup (HIGHEST RISK)
- **Pure audit planner first** (no mutation), then apply. Feeds `install.sh --fresh` / `setup --reset --wipe-state` — never an unguarded home sweep.
- **Remove classes (owned + allowlisted-root + dry-run + confirm):** stale agentchute binary backups (basename `agentchute.pre-*` / `agentchute.*.bak` / classified repo-local `dev`), under `install_dir`/configured control repo only — regular file, no symlink, current-user, never arbitrary `*agentchute*`; old owned `ac-*`; exact-match live/orphan `agentchute run`/`poller run` processes for THIS control-repo/loop-dir (current user; refuse ambiguous, print pid/cmdline); loop runtime + legacy `_msg-*` residue (inbox/archive/malformed/live/agents-live/state-except-setup.json/loop logs+sockets+pids); stale temp socket/pid dirs if owned; `.rehumanlabs/loop` if sentinel passes; stale setup-state schema migration.
- **Leave / report-only:** wrapper caches & sessions (`~/.gemini/*`, `~/.grok/*`, `.claude/projects/*`), herdr sessions, proposal/release packages, OTHER control repos' loops, shell-profile backups, hook `.bak` files, unknown `.*/loop` (manual report).
- Fix the buggy process/cmdline matcher (the ambiguous-PID false negative) as part of this.
- `type -a agentchute` PATH-shadow detection: warn if an earlier PATH entry resolves to a different agentchute than installed.
- **Guards:** dry-run always; apply needs `--fresh --yes` or interactive confirm; regular-file/no-symlink/current-user/allowlisted-root; **fail closed on ambiguous** process/path; refuse a live bus.
- Tests: no mutation in dry-run; refuses live bus; exact orphan match; refuses ambiguous; removes only owned shims/binaries; preserves wrapper caches + other repos.
- **Risk:** highest — a wrong delete or wrong SIGTERM is worse than stale remains. Gets the most hostile review.

### Gate 4 — README upgrade box + docs/template v16 sweep
- README: clean-upgrade box immediately below the new-install curl line. For ≤0.7.x: stop agents → fresh install → verify `ac` → setup/re-enroll → restart with `ac run <wrapper>` → doctor/status. Draft:
  > **Upgrading from 0.7.x or earlier?** 0.8 is a breaking redesign (pull-only; new on-disk format). Stop your agents, then do a clean upgrade:
  > ```sh
  > curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh -s -- --fresh --yes
  > agentchute setup --wake runner --wrappers all --yes
  > ```
  > Verify with `ac doctor`, then restart each agent: `ac run claude`, `ac run codex`, … See [CHANGELOG](CHANGELOG.md).
- Enrollment blocks/templates → **v16**; rewrite AGENTS/CLAUDE/CODEX/GEMINI/GROK.md + all user-facing docs/examples/web/spec from `ac-*` → `ac run <wrapper>`. Scrub residual stale prose (optional wake poke/watchdog/tmux-era).
- Tests: template drift tests; `rg 'ac-'` allowlist only historical blog posts / explicit removal notes.

### Gate 5 — integration proof in a scratch repo
- Build the branch binary. Scratch install/setup dry-run then apply (stub wrappers if possible).
- Assert: `agentchute version`; `command -v ac` = `$shim_dir/ac` (system `/usr/sbin/ac` only later in `type -a ac`); no `ac-*` in shim dir; setup state = dispatcher installed; old state wiped; `ac run <stub>` launches through the runner + registers.
- Live dogfood ONLY after agents stopped; never `--fresh` while agents are live.

### Gate 6 — final verification + 4-way + release
- `gofmt -l .` (clean), `go vet ./...`, `go test ./... -race`, `go build ./...`, `sh tests/install_test.sh`, `shellcheck -s sh install.sh tests/install_test.sh`, `(cd conformance && go test ./...)`, `git diff --check`, drift + submit-byte tests.
- 4-way final review → claude squash-merges to main, tags `v0.8.8`, pushes → goreleaser, verifies the release.

---

## Sequencing
Gate 0 (review) → 1 (parser, additive) → 2 (install/removal, writes-before-reset) → 3 (clean, highest risk) → 4 (docs/v16) → 5 (scratch integration) → 6 (verify+release). One PR to main; gated commits; combined-tree CI green before final.

## Open items for the 4-way plan review
1. Confirm `ac run <wrapper>` canonical (claude+codex lean yes); implicit `ac <wrapper>` — ship or defer?
2. Full `ac-*` removal in a 0.8.8 *patch* — Alex chose full-remove; codex's earlier one-release-compat concern: still object, or accept?
3. Clean-all destructive scope: anything to move from auto-remove → warn-only (esp. binaries/paths outside our install dir)?
4. Bundle `run`→`serve` now (`ac serve <wrapper>`) or defer? (Lean: defer; keep `run`.)
5. Global-flags grammar: `ac --as X run codex` confirmed?
