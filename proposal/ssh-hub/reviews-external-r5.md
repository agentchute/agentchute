# External review round 5 — DESIGN.md @ sha256 1959c113… (2026-08-15)

codex: **FIX** — 1 blocker + 2 majors, all in the round-5 delta.

- **H1 (BLOCKER, F1 §5.1)** `state/pool.id` is not durable against the
  shipped wipe: `setup --reset --wipe-state` deletes every state/ entry
  except setup.json (setup_wipe.go:295-315) and the post-wipe rescan
  rejects other survivors (setup_wipe.go:527-533) → next session
  E_POOL_MISMATCH until re-authorize; a pool move + remint changes the id
  and invalidates every binding. Fix: pool.id becomes preserved non-runtime
  scaffold in the wipe plan, rescan, dry-run/output, and tests (or an
  equivalently preserved location).
- **H2 (MAJOR, F1 §5.1)** First mint described only as atomic write
  (878-885), not create-once: two concurrent authorizes via case/bind
  aliases can both see no file, compute different hashes, both replace →
  one authorized_keys line instantly inconsistent. Fix: exclusive-create
  mint (loser re-reads) or one lock spanning pool-id creation +
  authorized_keys mutation; concurrent alias-first-authorize test.
- **H3 (MAJOR, F3 §7.2)** Version state machine misses two crash states:
  (a) first-join crash after keygen mints .v1 but before the initial
  symlink (1384-1390) — rerun sees no stable key, re-runs ssh-keygen onto
  existing .v1, which can prompt/hang; (b) post-promotion crashes before
  verify/cleanup (1481-1498) leave old-version residue indistinguishable
  from completed rotation, and the recovery branch never verifies-then-
  cleans. Fix: recovery for version-files-with-no-pointer (validate/create
  initial pointer) and older-versions-beside-active (verify active before
  pruning); failure injection between first keygen and initial symlink;
  the promised post-promote/cleanup assertions.

## Revision log (round 6)

- **H1** FIXED §5.1 + §9.1 + §10.3: `state/pool.id` declared preserved
  non-runtime scaffold alongside `setup.json` in the wipe plan, the
  post-wipe rescan, and the dry-run/preserved output (verified the shipped
  behavior at `setup_wipe.go:295-316` — only setup.json survives — and
  `setup_wipe.go:519-536` — rescan flags other survivors); named in the
  §9.1 spec delta (M2 text, behavior lands in M5 with authorize); §10.3
  row asserts survive + not-flagged + post-wipe session validation.
- **H2** FIXED §5.1 + §10.3 — took the exclusive-create option: first mint
  is `O_EXCL`/link-no-clobber (the codebase's first-writer-wins idiom,
  `registration.go:133-169`), loser re-reads and adopts the winner's
  value; concurrent alias-first-authorize test row (one pool.id, both
  lines share one marker).
- **H3** FIXED §7.2 + §10.3: (a) no-pointer branch — a rerun never re-runs
  `ssh-keygen` onto an existing versioned path (`-q` still prompts
  Overwrite? and hangs scripts); it adopts the orphan after the
  passphrase-free `ssh-keygen -y -P ""` probe or retires it
  (`.invalid.<ts>`) and remints; the keygen note points at this branch.
  (b) residue branch — older-versions-beside-active now verifies the
  ACTIVE key via hello BEFORE pruning; on active-hello failure nothing is
  pruned and the active-key paste line prints (converging remediation for
  both late-revoke and half-landed replace). Injection rows added for the
  first-keygen→symlink gap and the promised post-promote/cleanup
  assertions.

Rejections: none.
