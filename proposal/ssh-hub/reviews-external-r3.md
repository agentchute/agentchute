# External review round 3 — DESIGN.md @ sha256 b438482a… (2026-08-15)

codex: **FIX** — 2 blockers + 3 majors, all in the round-3 delta. grok's SHIP
stands. Reviser: address all five, log dispositions below.

- **E1 (BLOCKER, D2 §3.2)** Lines 262-264 arm the guard latch only after
  Claim RETURNS, but current check arms inside the loop before each display
  (check.go:243/256). Disconnect after one emitted msg ⇒ displayed/claimed
  mail with NO local latch. Fix: client emitter arms on the FIRST msg,
  before rendering, both normal and --no-archive paths; test mid-stream
  failure leaves the latch armed.
- **E2 (BLOCKER, D4a §6.8/§7.2)** Alias migration moves the state dir to the
  new hub-id and removes the old (1355-1367) while a live child's
  AGENTCHUTE_CONTROL_REPO still carries the OLD URL (1202-1213) → child ops
  E_NOT_JOINED, guard discovery can fail open; the pointer rewrite can't
  override child env. Fix: refuse migration while the old runner is live,
  OR retain a durable old-hub-id alias/state dir until that lane is
  stopped/relaunched and latch/spool state reconciled. Test: live latched
  lane + alias rejoin.
- **E3 (MAJOR, D4b §7.2)** Staged-rotation recovery not executable: the old
  agent key is forced to `hub session` so it can NEVER run
  `hub authorize --replace-key`; and after a crash post-replacement the old
  key is revoked yet stage:"staged" resumes over it (1380-1391). Fix:
  recovery FIRST tries hello with the staged key — success proves remote
  replacement, permit promotion; failure routes through unrestricted
  operator SSH/paste (or a specified authenticated self-rotation op). Test
  post-replace/pre-promotion crash without assuming another credential.
- **E4 (MAJOR, D2 §3.6)** Pending fidelity: OwedEvent{Ref,ExpiredAgo}
  (253-256) can't reproduce the full OwedEntry JSON
  (to/from/seq|stamp/suffix/by/recorded_at, pending.go:269-287); summary
  386-395 omits NeedsBoot/boot-hint semantics (pending.go:71-95,
  enforced_enrollment_test.go:158-193) which the remote shadow can't derive
  locally. Fix: owed events carry the full entry fields; terminal result
  gains NeedsBoot; client keeps boot hint/--fail-if-any behavior.
- **E5 (MAJOR, D3 §5.1)** EvalSymlinks(Clean(abs)) is not a filesystem
  identity: Unix EvalSymlinks preserves non-symlink component spelling —
  macOS APFS case aliases (/Users vs /users) hash differently though
  os.SameFile is true; Linux bind mounts split too. Fix: durable pool
  identity, or compare candidate pool against EXISTING marked pools with
  os.SameFile and reuse the matching marker. Add macOS case-alias test next
  to the symlink/trailing-slash row.

## Revision log (round 4)

- **E1** FIXED §3.2 + §6.6 + §10.3: the client emitter arms the local guard
  latch on the FIRST MessageEvent, BEFORE rendering, in both normal and
  `--no-archive` paths — matching the local per-message
  `setLatch()`-before-display (verified `check.go:243,256`), never after
  the op returns; new §10.3 row: disconnect after the first `msg` frame ⇒
  latch armed, claim persists, remainder redelivers.
- **E2** FIXED §7.2 — chose the REFUSE option: migration refuses while any
  lane of the old hub dir is live (local check: `joined_as` ×
  `runner.json` pid probe, `config.go:149-151`), with an exact refusal text
  naming the serve pid and the stop-then-rejoin procedure. Justification
  recorded in-text: the live child's env is immutable (§6.8) so no
  reconciliation can fix it — the lane must relaunch anyway; a durable
  alias dir would keep two names for one hub's latch/spool/cache state,
  the double-source drift this design deletes everywhere else. §10.3:
  live-latched-lane rejoin refused; post-stop rejoin migrates with
  latch/spool residue intact.
- **E3** FIXED §7.2: step 2 corrected — remote replace runs ONLY via
  operator SSH/paste (the pinned agent key structurally cannot run
  `authorize`, its forced command is `hub session`); recovery now probes
  hello with the STAGED key FIRST: success ⇒ replacement landed ⇒ promote +
  verify (never re-drives step 2 over a revoked key); staged-fail +
  old-key-works ⇒ resume step 2 via operator path; neither ⇒ print the
  paste line. No new self-rotation op (the existing authorize paths cover
  every branch — spec'd in one paragraph). §10.3 post-replace crash case
  asserts recovery with the old key already revoked.
- **E4** FIXED §3.2 + §3.6 + §4.4.1: `OwedEvent` now carries the full
  `loop.OwedEntry` fields (To/From/Seq/Stamp/Suffix/By/RecordedAt + Ref
  convenience; verified `owed.go:114-122`, `pending.go:269-287`);
  `PendingSummary` gains hub-derived `NeedsBoot` (verified
  `pending.go:77-97` — registration stat + ErrInboxMissing are hub facts);
  client keeps the boot hint (`pending.go:182`) and `--fail-if-any` exit-2
  semantics (`pending.go:167-169`); `owed-item` wire example updated to
  full fields.
- **E5** FIXED §5.1 + §4.3 + §10.3 — chose the SameFile-with-marker-reuse
  option: normalization (`EvalSymlinks(Clean(abs))`) is only a
  pre-processing step; identity continuity is `os.SameFile` against every
  existing marked line's `--pool`, reusing the matching marker; a fresh
  `pool12` is minted only when nothing matches (precedent cited:
  `send.go:590-599`'s directory-identity-over-path-strings rationale).
  `hub authorize` additionally bakes `--pool-id <pool12>` into the forced
  command so `hello-ok.pool12` is echoed verbatim — session and marker can
  never disagree. §10.3 row gains the macOS case-alias spelling.

Rejections: none.
