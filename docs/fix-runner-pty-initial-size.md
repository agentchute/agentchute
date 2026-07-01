# Fix report: runner starts wrapper on a 0×0 PTY → blank TUIs ("blank boxes")

- **Branch:** `fix/runner-pty-initial-size` (based on tag `v0.9.1`, commit `07c46cd`)
- **Fix commit:** `f44efe4` — `fix(runner): size child PTY from stdin before the wrapper starts`
- **Date:** 2026-07-01
- **Severity:** high (visible rendering failure in every fast-booting wrapper TUI)
- **Affected:** all wrappers launched via `agentchute serve` / `ac serve` (grok, codex, agy observed; claude usually unaffected — see "Why intermittent")

## 1. Reported symptom

grok, codex, and antigravity (`agy`) TUIs running under `ac serve` inside tmux
"not showing up correctly, sometimes as blank boxes": a pane comes up blank or
degenerate at launch, and stays that way until the pane is manually resized.
Intermittent — some launches are fine. Claude Code panes were not observed to
fail.

## 2. What was ruled out first (environment layers)

All of these were checked on the affected machine (Mac M5 Max, Ghostty 1.3.1,
tmux 3.7a, fish) and are **not** the cause:

| Layer | Check | Result |
|---|---|---|
| Locale | `locale` inside pane | clean `en_US.UTF-8` everywhere |
| Font | `ghostty +list-fonts`, `~/Library/Fonts` | Maple Mono NF installed and resolved |
| terminfo | `tput -T tmux-256color colors` with Ghostty's `TERMINFO` set | resolves via fallback to `/usr/share/terminfo` (256) |
| tmux Unicode | glyph gauntlet (box drawing, braille, NF PUA icons, emoji+VS16) through `tmux capture-pane` | byte-perfect passthrough |
| tmux build | `otool -L $(which tmux)` | linked against libutf8proc (modern widths) |
| Pane sizes at steady state | `stty size < /dev/ttysNNN` for every runner + child pty vs `tmux list-panes` | all matched (33×92 / 33×139) — resize path works once running |

The only remaining layer between tmux and the TUIs was the `agentchute serve`
PTY runner — which is where the defect is.

## 3. Root cause

At `v0.9.1`, `internal/cli/run.go` starts the wrapper like this:

```
248:  ptmx, err := runnerpty.Start(cmd)          // creack pty.Start → PTY has NO size: 0 rows × 0 cols
257:  registerRunner(...)                        // filesystem I/O, locking
266:  runnerMakeRaw(os.Stdin)                    // termios ioctls
301:  rt.saveState()                             // more filesystem I/O
327:  go rt.resizeLoop()                         // ← FIRST InheritSize happens inside this goroutine
```

`creack/pty.Start` opens the PTY pair without ever calling `TIOCSWINSZ`, so the
child's window size is **0×0** from exec until `resizeLoop`'s initial
`runnerpty.InheritSize(os.Stdin, r.ptmx)` runs (`internal/cli/run.go:632-646`
at v0.9.1). Everything between line 248 and line 327 — registration writes,
lease work, raw-mode ioctls, state save, goroutine scheduling — is the race
window, realistically tens of milliseconds.

**Failure sequence for a fast native TUI (grok, codex, agy):**

1. Child execs and its TUI runtime boots in a few ms.
2. First draw queries the terminal size (`TIOCGWINSZ`) → gets **0×0** → ratatui/Ink
   render a zero-area frame: a blank screen / degenerate "blank box".
3. The runner's `InheritSize` finally lands and the kernel raises SIGWINCH —
   but if the TUI has not yet installed its SIGWINCH/resize handler, the
   signal's default disposition is *ignore* and the resize is **lost**.
4. Nothing else ever re-triggers a size read → the pane stays blank until the
   user manually resizes it (tmux then raises a fresh SIGWINCH).

**Why intermittent:** it's a straight race. Whether the pane comes up blank
depends on whether the TUI's first size read beats the runner's first
`InheritSize`. Node-based Claude Code boots slowly (hundreds of ms) and
essentially always loses the race in the good direction, which is why it was
never observed blank; native Rust TUIs frequently win it and break.

## 4. Reproduction (before/after)

Deterministic repro — a child that reads its size immediately is guaranteed to
beat the resize goroutine:

```sh
# scratch pool
mkdir pool && cd pool && git init -q && agentchute init --yes

tmux new-session -d -s racetest -x 120 -y 30 \
  "agentchute serve --vendor anthropic --as racetest \
     --control-repo \"$PWD\" --loop-dir \"$PWD/.agentchute/loop\" \
     -- sh -c 'stty size > /tmp/race.txt; sleep 1; stty size >> /tmp/race.txt'"
sleep 3; tmux kill-session -t racetest; cat /tmp/race.txt
```

| Binary | t=0 | t=1s |
|---|---|---|
| v0.9.1 (unfixed) | **`0 0`** | `30 120` |
| `0.9.1+pty-initial-size` (fixed) | **`30 120`** | `30 120` |

The `0 0` at t=0 is the bug; the t=1s heal is `resizeLoop`'s late
`InheritSize` (which real TUIs cannot rely on, per step 3 above).

## 5. The fix

Size the child's PTY **before the child process starts**, inheriting from the
runner's own stdin.

- `internal/runner/pty/pty.go` — new `StartInheritSize(cmd, from *os.File)`:
  if `from` is non-nil and `creackpty.GetsizeFull(from)` returns a size with
  `Rows > 0 && Cols > 0`, start via `creackpty.StartWithSize(cmd, sz)`
  (creack sets the winsize on the PTY before `exec`). Otherwise fall back to
  plain `creackpty.Start` — preserves existing behavior when the runner itself
  has no terminal (piped stdin, CI) or a degenerate 0×0 size.
- `internal/cli/run.go:248` — call site changed from `runnerpty.Start(cmd)` to
  `runnerpty.StartInheritSize(cmd, os.Stdin)`.
- `resizeLoop` is untouched: its initial `InheritSize` is now redundant-but-
  harmless belt-and-braces, and its SIGWINCH loop still handles live resizes.

No behavior change for: injection, idle detection, lease/registration,
raw-mode handling, shutdown. The only new syscalls are one `TIOCGWINSZ` on
stdin and creack's `TIOCSWINSZ` on the fresh PTY before exec.

## 6. Tests

New `internal/runner/pty/pty_size_test.go`:

- `TestStartInheritSizeChildSeesInitialSize` — opens a creack PTY pair as a
  stand-in for the runner's stdin, sets it to 41×97, launches `stty size`
  under `StartInheritSize`, and asserts the child's **first** output is
  `41 97`. This test fails against the old `Start` path (child sees `0 0`);
  it was written first and observed red (compile-red, then behavioral).
- `TestStartInheritSizeFallsBackWithoutTerminal` — nil source must not error
  (fallback path).

Full suite: `go test ./...` passes (exit 0) at `f44efe4`.

⚠️ **Verifier gotcha:** run the suite with a scrubbed environment. A shell that
is itself enrolled (runner-launched agents, hook-enrolled sessions) carries
`AGENTCHUTE_AGENT_ID`, `AGENTCHUTE_CONTROL_REPO`, `AGENTCHUTE_LOOP_DIR`,
`AGENTCHUTE_RUNNER*`, `AGENTCHUTE_SERVE_TOKEN` — the CLI reads config from env,
and ~10 send/ack/consume tests fail spuriously with those set:

```sh
env -i HOME="$HOME" PATH=/usr/bin:/bin:/opt/homebrew/bin:"$HOME/go/bin" go test ./...
```

(Possible hardening follow-up: have the test helpers clear `AGENTCHUTE_*`.)

## 7. How to verify (checklist)

1. `git log --oneline v0.9.1..fix/runner-pty-initial-size` → `f44efe4` (+ this doc).
2. `env -i HOME="$HOME" PATH=... go test ./internal/runner/pty/ -v` → both new tests pass.
3. Revert the two non-test hunks (`git checkout v0.9.1 -- internal/runner/pty/pty.go internal/cli/run.go` won't compile with the test; instead stash the pty.go hunk) or simply run the §4 tmux repro against a binary built from `v0.9.1` → observe `0 0`; rebuild from the branch → observe full size at t=0.
4. Sanity: launch a real wrapper (`ac serve grok`/`codex`/`agy`) in a tmux pane repeatedly (10×); no launch should come up blank.
5. Confirm live resize still works: resize the pane; TUI reflows (SIGWINCH path unchanged).

## 8. Deployment state on this machine (Alex's M5 Max)

- Installed: `~/.local/bin/agentchute` → reports `agentchute 0.9.1+pty-initial-size`
  (built from `f44efe4` with `-X main.version=0.9.1+pty-initial-size`).
- Rollback: previous binary preserved at `~/.local/bin/agentchute.v0.9.1.bak`
  (also reproducible from the `v0.9.1` tag; that binary's provenance was
  verified via `go version -m`: revision `07c46cd`, `vcs.modified=false`).
- **Runners started before the install still run the old code in memory** —
  each `ac serve …` pane must be relaunched to pick up the fix. Until then a
  blank pane heals with any manual resize.
- The branch was created in a temporary git worktree; the commits live in the
  main repo's object store regardless. If the worktree directory has been
  cleaned up, `git worktree prune` removes the stale entry; the branch remains.

## 9. Related finding (NOT fixed here — proposed follow-up)

The runner writes its own diagnostics with `fmt.Fprintf(os.Stderr, ...)`
(inject errors, poll errors, stale-wake notices) directly to the shared tty
**while that tty is in raw mode with OPOST off**. Any such write lands on top
of the child TUI's screen (and without ONLCR, `\n` stair-steps), garbling the
display until the TUI repaints. Suggested follow-up: route runner diagnostics
to a file under the agent state dir (alongside runner state), keeping stderr
for pre-raw-mode startup errors only. This is a second, independent source of
"not showing up correctly" and deserves its own change + test.
