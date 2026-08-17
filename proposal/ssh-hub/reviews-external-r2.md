# External review round 2 — DESIGN.md @ sha256 a1819217… (2026-08-15)

grok: **SHIP** (all dispositions verified; laptop walk survives re-join,
keygen, lid-close) with 2 nits. codex: **FIX** — 5 majors + 2 minors, all in
the delta. codex confirms all other round-1 dispositions internally
consistent (actor context, boot-proof ordering, reject-not-encode, release
order, normative spec section, cuts, no-auto-replay, spool, child env).

## codex findings (code-verified @ 1244ae4). Verdict: FIX

- **D1 (MAJOR) §3.5 register semantics still wrong for host/vendor.**
  (A) Host: current HostProvided=false does NOT preserve — performRegister
  calls os.Hostname() and overwrites (register.go:72-87); mapping Host:nil
  to "preserve" changes local behavior, and hub-side nil resolves the HUB's
  hostname for a remote self-check. Fix: Host is CLIENT-resolved (omitted
  flag ⇒ remote client's os.Hostname; explicit empty stays empty) — not a
  preserve pointer. (B) Vendor: bare hooks carry no vendor; remote client's
  registration reads hit the mail-free shadow, so resolveAgentVendor
  (identity.go:72-82) can't recover a custom id's vendor → every step-0
  repair fails. Fix: Vendor presence-sensitive; omitted vendor resolved
  HUB-SIDE from the pinned actor's existing hub registration, then
  canonical-id fallback. Tests: omitted host after machine move; omitted
  vendor on existing custom-id row.
- **D2 (MAJOR) §3 streaming — unbounded side channels + pending --show-body
  unspecified.** ClaimSummary still carries ExpiredOwed[]/Notes[]; Claim has
  only a ClaimedItem emitter so quarantine notes buffer until return (order
  changes; notes lost on mid-stream failure). Pending: current code emits
  every owed entry and supports --show-body (4 MiB bodies,
  pending.go:127-150,269-288); wire has no owed-item stream or pending-item
  body trailer — control-JSON recreates the 64 KiB failure; omission breaks
  "works unchanged". Fix: ordered event stream (message/note/owed emitters
  or one typed emitter); terminal summaries = counts + truncated metadata
  only; pending-item body trailers with the 4 MiB cap. Tests: many notes,
  many owed, one 4 MiB pending body, emit-failure after a note/item.
- **D3 (MAJOR) §5.1-5.2 pool12 = sha256(abs path) is not a pool identity.**
  Real path vs symlink path hash differently → two (id,pool12) markers, two
  keys, same underlying loop; duplicate-id and revoke/list defeated. Lexical
  cleaning alone insufficient (also grok nit 2: trailing slash). Fix: derive
  marker HUB-SIDE from canonical resolved identity (Clean + EvalSymlinks),
  authorize writes/returns the marker; client never derives identity from
  raw URL text. Tests: realpath-vs-symlink authorize, duplicate refusal,
  list, revoke.
- **D4 (MAJOR) §7.2 key lifecycle unsafe for URL alias + failed rotation.**
  (A) Re-join via a different SSH host alias for the same hub/path → new
  hub-id → new key → duplicate-id rejection, contradicting "different URL
  replaces the pointer". Fix: same-hub migration defined explicitly —
  verified host-key fingerprint + canonical pool identity. (B) --rotate-key
  has no staging/commit order → self-lock on either side of the boundary.
  Fix: crash-recoverable staged rotation (fsynced staged key → remote
  replace → durable local promotion + recovery marker; old key retained
  until safe). Tests: failure injection before/after remote replace;
  same-hub-alias rejoin.
- **D5 (MAJOR) §10.3 relaunch gate tests the opt-in path.** Integration row
  launches "a --relaunch lane" but the load-bearing change is BARE
  `agentchute serve` relaunching by default; an implementation keeping the
  old false default passes. Fix: primary case flagless, assert fresh
  token/pid after reconnect; separate --relaunch=false case asserting
  old-child stop and no new child.
- **D6 (MINOR) §7.5 negative cache omits the SessionStart boot hook.**
  boot --context-only (boot.go:15-24) dials outside the cache participant
  list → two 5 s stalls per offline session. Fix: hook-mode boot joins the
  cache; interactive boot bypasses like other human commands. Test:
  SessionStart boot then pending inside 30 s ⇒ one dial.
- **D7 (MINOR) stale plan labels + parity claim.** §3/§10 still say
  PR-1/PR-2/PR-7 (plan is M1-M6); §6.1 opens "mirrors local runWrapper"
  before correctly explaining it doesn't. Fix: relabel; opener becomes
  "derived from local runWrapper, with the deliberate registration
  reordering below."

## grok nits (SHIP already given; apply, no re-gate needed)

- **D8** §1 non-goals still says "opt-in full-lane relaunch" — flip to match
  default-on §6.7.
- **D9** pool12 trailing-slash canonicalization — subsumed by D3's
  hub-side canonical identity.

## Revision log (round 3)

- **D1** FIXED §3.5: `Host` is a plain string, CLIENT-resolved (flag value
  or the remote's own `os.Hostname()`; hub maps to `HostProvided:true` so
  `register.go:80-87`'s hub-hostname substitution never runs) — verified
  there is no local host-preserve semantic to mirror; `Vendor` is a
  presence pointer, resolved HUB-SIDE when nil from the pinned actor's
  existing hub registration then the canonical-id fallback (client-side
  `resolveAgentVendor`, `identity.go:72-82`, reads the mail-free shadow and
  cannot); `Bio` keeps pointer-presence (that one matches `BioProvided`).
  §10.3 row: machine-move host test + custom-id bare-vendor test.
- **D2** FIXED §3 conventions + §3.2/§3.3/§3.6 + §4.4/§4.4.1/§4.4.3: one
  typed emitter `emit func(op.Event)` (Message/Note/Owed/AckItem arms),
  events interleaved in production order, terminal summaries counts +
  truncation metadata only (`ClaimSummary` slices deleted); wire gains
  `owed-item`, drops `pending-item` (pending reuses `msg`); pending
  `show_body` bodies are ≤4 MiB trailers, never control JSON; owed entries
  stream (cap removed). §10.2 tests: many notes, many owed, one 4 MiB
  pending body, emit-failure after an item AND after a note.
- **D3** FIXED §5.1/§5.2/§4.3/§7.2/§7.4/§7.5: `pool12` derived HUB-SIDE
  from `EvalSymlinks(Clean(abs(--pool)))`; authorize computes, writes,
  prints the marker; keygen comment is client-known `agentchute:<id>` only;
  hello-ok reports canonical `pool` + `pool12`; the client RECORDS them at
  join and compares later sessions against the record — never derives
  identity from URL text; `E_POOL_MISMATCH` reworded accordingly. §10.3:
  realpath/symlink/trailing-slash → one marker, duplicate refusal, list,
  revoke.
- **D4** FIXED §7.2: (a) same-hub URL migration — pre-keygen host-key
  fingerprint scan across existing hub dirs, then hello-with-existing-key +
  recorded-pool12 equality ⇒ migrate (reuse key, move dir, rewrite pointer;
  crash-idempotent); fingerprint-match/different-pool12 ⇒ fresh join;
  "different URL replaces the pointer" now routes through this check.
  (b) staged `--rotate-key` with exact commit order: staged keypair+fsync →
  `rotate.json{stage:"staged"}` → remote `--replace-key` (idempotent) →
  local promotion (`.prev` retained) + `stage:"promoted"` → verify hello →
  delete marker+`.prev`; recovery resumes from the recorded stage. §10.3:
  failure injection before/after remote replace; alias rejoin.
- **D5** FIXED §10.3: primary relaunch row is a BARE `agentchute serve`
  (flagless) asserting fresh token + fresh child pid — an opt-in
  implementation fails it; separate `--relaunch=false` row asserts
  old-child stop, no new child, argv-echoing `E_CHANNEL_LOST`; the
  `E_FENCED` row de-flagged too.
- **D6** FIXED §7.5: hook-mode `boot` (`--context-only` /
  `--codex-hook SessionStart`, boot.go:15-40) joins the 30 s negative
  cache; interactive boot bypasses like other human commands. §10.3 row:
  SessionStart boot then pending inside 30 s ⇒ one dial.
- **D7** FIXED: all PR-* labels in §3/§6.1/§6.9/§7.3/§10 relabeled to M*
  (plus the doc header); §6.1 opener now "derived from local runWrapper,
  with the deliberate registration reordering noted at step 3".
- **D8** FIXED §1: non-goal bullet now says default-on for remote lanes
  (`--relaunch=false` opts out), matching §6.7.
- **D9** FIXED — subsumed by D3 (Clean+EvalSymlinks kills the
  trailing-slash split).

Rejections: none. Out-of-delta touches, flagged: D2 required small
consistency edits in §3 conventions' "Wire shape" bullet (Notes[] →
NoteEvent stream) and the §4.4.1 check example; D3 required the §7.1
authorize output example and §7.4 config.json layout line to show the
hub-returned marker/recorded identity — all direct consequences of the
named findings, no independent design changes.
