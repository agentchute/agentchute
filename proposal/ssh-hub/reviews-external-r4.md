# External review round 4 — DESIGN.md @ sha256 4d932e22… (2026-08-15)

codex: **FIX** — 1 blocker + 2 majors, all in the round-4 delta.

- **F1 (BLOCKER, E5 §4.3/§5.1)** hello-ok.pool12 blindly echoes the
  forced-command `--pool-id` (568-580, 893-896) — so editing only `--pool`
  to point at another pool leaves the echoed id unchanged and
  E_POOL_MISMATCH never fires, contradicting §4.3's edited-line detection
  and the test at 1910. Fix: session start re-derives/validates the marker
  from the actual normalized `--pool` (SameFile against the recorded
  identity), or a durable identity stored IN the pool validated against
  `--pool-id`. Add the exact `--pool`-only edit test.
- **F2 (MAJOR, E2 §7.2)** Migration treats a live numeric pid from stale
  runner.json as proof and prints `kill <pid>` (1409-1414) — pid reuse can
  target an unrelated process. Existing precedent requires command
  attribution and refuses ambiguity (setup_reset.go:207-217,
  setup_wipe.go:560-570). Fix: same attribution/start-time proof; on
  alive-but-unattributed pid fail closed with inspection guidance, never a
  kill command. Test: stale runner.json whose pid was reused.
- **F3 (MAJOR, E3 §7.2)** Local promotion not crash-idempotent: step 3 does
  active→prev, staged→active, then marker rewrite (1440-1441); a crash
  between renames leaves stage:"staged" with staged absent / active already
  new, contradicting staged-key-first recovery (1445-1456). Fix: specify
  recovery for EVERY rename boundary, or versioned keypairs + one atomic
  active pointer; inject failures at each local transition, not only around
  the remote replace.

## Revision log (round 5)

- **F1** FIXED §5.1/§4.3/§5.2/§7.4-layout/§10.3 — chose the
  durable-identity-in-the-pool option (it stays one small file):
  `<pool>/.agentchute/loop/state/pool.id`, one line, written atomically
  0600 by `hub authorize` at first mint, reused on every later authorize
  (this also replaces round-4's SameFile scan — the file travels with the
  directory, so symlink/case-alias/bind-mount/moved-pool spellings all
  read one identity with no stat comparison). `hub session` no longer
  echoes argv: at startup it re-reads `pool.id` from the actual `--pool`
  and refuses with `E_POOL_MISMATCH` when absent or ≠ `--pool-id`;
  `hello-ok.pool12` always carries the pool-read value. New explicit §10.3
  row: `--pool`-only edit ⇒ hub-side refusal before any op; the
  consistent-re-point (both flags) case remains the client-side
  recorded-identity layer. §5.2 notes pool.id is a name tag in the pool,
  not a mapping store.
- **F2** FIXED §7.2 + §10.3 — migration liveness mirrors the existing
  attribution pattern (verified `setup_reset.go:196-224`,
  `setup_wipe.go:553-570`): runner.json binds pid→id, host must match, pid
  alive, AND the process command line must attribute as an `agentchute
  serve` for this hub. Attributed ⇒ refusal naming the session with
  stop-then-rejoin guidance and NO kill command; alive-but-unattributed ⇒
  fail closed with the ambiguous-pid text (`ps -p` inspection + stale
  runner.json removal guidance), never a kill suggestion; dead/no-file ⇒
  proceed. New §10.3 row: stale runner.json with a reused pid ⇒ refused
  closed, migrates after the stale file is removed.
- **F3** FIXED §7.2 (+§7.4 layout, keygen argv) — chose versioned keypairs
  + one atomic active pointer: `keys/<id>_ed25519.v<N>` files with
  `keys/<id>_ed25519` as a SYMLINK to the active version (the §4.2 `-i`
  path unchanged — ssh follows it); promotion is a single temp-symlink +
  `rename()`; `rotate.json` deleted — a version newer than the symlink's
  target IS the staged state; first-join keygen mints `.v1` + symlink.
  Recovery (staged-key-first hello, E3) now covers every crash point
  because no multi-rename boundary exists; a mid-promote temp symlink is
  inert and cleaned. §10.3 row expanded to failure injection at all six
  transitions, post-replace cases with the old key revoked.

Rejections: none. Out-of-delta touches, flagged: F3 required the §7.4
layout block's keys lines and the §7.2 keygen argv (`-f …v1` + symlink
note) to match the versioned scheme; F1 required the §5.2 single-source
clause and the §7.4 layout unchanged otherwise — all direct consequences
of the named findings.
