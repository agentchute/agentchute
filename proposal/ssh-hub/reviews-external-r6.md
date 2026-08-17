# External review round 6 — DESIGN.md @ sha256 b3ae0db4… (2026-08-15)

codex: **FIX** — 1 blocker + 1 major, both in the round-6 delta.

- **J1 (BLOCKER, H2/F1 §5.1)** pool.id is reused and interpolated into the
  forced-command line (878-915) with no bounded parser/validation: a
  corrupt or peer-written value (whitespace, quotes, newline, shell syntax)
  escapes the inert-value grammar; a symlink lets authorize read an
  arbitrary file. Fix: pool.id is exactly one regular non-symlink 0600 file
  containing `[0-9a-f]{12}\n`, small read cap; validated on authorize
  (including the loser-re-read path) AND session startup before any
  comparison/interpolation; malformed state refused. Tests: oversized,
  symlinked, newline, quote, `$()`, wrong-length — none may alter
  authorized_keys.
- **J2 (MAJOR, H3b §7.2)** Residue branch maps EVERY active-hello failure
  to the reauthorize paste line (1524-1532), but hello can fail as
  E_CONNECT / E_HOSTKEY_CHANGED / E_VERSION / E_IDENTITY /
  E_POOL_MISMATCH — reauthorization is right only for E_UNAUTHORIZED and
  misleading otherwise. Fix: preserve all versions on every failure;
  offer/run authorization recovery only on the explicit auth-refusal
  class; propagate each other error's existing remediation. Tests:
  E_UNAUTHORIZED vs E_CONNECT/E_HOSTKEY_CHANGED residue cases.

## Revision log (round 7)

- **J1** FIXED §5.1 + §4.4.2 + §7.5 + §10.3: pool.id gets an exact
  validation contract — exactly one regular non-symlink 0600 file, content
  `^[0-9a-f]{12}\n$`, read via the bounded no-follow reader
  (`ReadFileLimit`/`openRegularNoFollow`, `registration.go:34-48`) with a
  64-byte cap; validated at `hub authorize` (fresh-read AND the H2
  loser-re-read path) and at `hub session` startup, before any comparison
  or interpolation; malformed state refused with the new named
  `E_POOL_ID_INVALID` (wire registry row + authorize CLI text — authorize
  writes nothing to authorized_keys). Only a grammar-passing value (inert
  in the authorized_keys/shell/JSON layers) can ever reach the key line.
  §10.3 row: oversized / symlink / newline / quote / `$()` / whitespace /
  wrong-length, asserting authorized_keys byte-identical, both consumer
  paths.
- **J2** FIXED §7.2 + §10.3: the residue branch preserves ALL version
  files on EVERY active-hello failure, and the remediation now splits by
  error class — the active-key reauthorize paste line only on
  `E_UNAUTHORIZED`; `E_CONNECT` / `E_HOSTKEY_CHANGED` / `E_VERSION` /
  `E_IDENTITY` / `E_POOL_MISMATCH` / `E_HUB_NO_BINARY` propagate their own
  §7.5 remediation unchanged (a reauthorize hint on a network failure
  would send the operator to fix an unbroken key; a host-key stop is never
  overlaid with rotation advice). Contrast tests added: deauthorized vs
  hub-down vs rotated-host-key residue, all preserving every version.

Rejections: none.

## Revision log (round 8) — operator-directed naming default (Alex, post-SHIP; own codex delta gate pending)

- §7.2: new Naming block — `hub join --name <local>` mints the pool id
  `<local-name>-<hostname>` (`--as` stays the verbatim, never-suffixed
  override): local name canonicalized via the wrapper-token table
  (`wrapperForToken`, dispatch.go:132-137) when applicable; hostname = the
  first DNS label of `os.Hostname()`; exact sanitization to the id grammar
  (`agentIDPattern` verified at inbox.go:40; ValidateAgentID
  inbox.go:496-504): lowercase → non-`[a-z0-9-]` → `-` → collapse runs →
  trim edges → empty = error. Minted ONCE at join, recorded in
  `config.json.names` (§7.4); hostname changes never re-derive; `serve`
  resolves the same default via `ensureDispatchIdentity`
  (dispatch.go:269-278) consulting the names map, so `ac serve codex` ⇒
  `codex-tiny` with nothing retyped; `agentchute identity` lists the
  machine's local-name → joined-id map (no new command).
- Rationale recorded verbatim in-doc, both halves: operators think in local
  names / hostname suffix = automatic pool-wide uniqueness; and
  hostname-suffixed ids make message traffic human-legible — every inbox
  filename, roster row, and archive entry shows the origin machine in the
  sender slug (`from-codex-tiny`), so who-talks-to-whom-from-where reads
  off the pool with no lookup (Alex addendum).
- Edge cases specified: shared hostname ⇒ duplicate-id refusal at
  authorize, its §7.5 text now carrying the hostname-collision `--as`
  hint; explicit `--as` vs auto name elsewhere ⇒ same normal refusal;
  post-join hostname change ⇒ recorded id wins.
- §7.1 + §7.2 quickstarts updated to the `--name codex` / `codex-tiny`
  flow (key paths, authorize line, join output, serve resolution); §7.3
  join row and the §7.5 bare-join usage text gained `--name`; §7.4
  config.json example gains `names`; §10.3 row added (two-host distinct
  ids; same-hostname refusal with hint; sanitization cases).

## Revision log (round 9) — grok fixes to the naming default

- RESOLVER FIXED §7.2: the round-8 "consult names only when nothing is
  set" rule was wrong for the real launch path (enrollment exports
  `AGENTCHUTE_AGENT_ID=codex`; `ensureDispatchIdentity`,
  dispatch.go:269-278, injects `--as <canonical>` — every joined lane
  would hello as `codex` against the pinned `codex-tiny` ⇒ `E_IDENTITY`
  on first launch). New rule, exactly as grok specified, applied at the
  single identity choke point (`resolveAgentID`, identity.go:13) in
  remote mode: a candidate id from `--as`/env/wrapper-default that is a
  LOCAL NAME in the `names` map resolves to the joined id; it is a pool
  id only when not in the map or already equal to the joined id. Guard
  latch/spool/hello agree by construction (choke point). `doctor` +
  `hub join` warn when env is set and resolves differently (mirror of the
  CONTROL_REPO-mismatch warning). Explicitly stated: NO send-side peer
  aliasing — `--to` is never mapped; peers address the full `codex-tiny`.
  Join-time `--as` verbatim-minting clarified as a separate rule to avoid
  reading as a contradiction.
- STALE EXAMPLES FIXED: §7.2 auto-authorize interactive ssh now
  `--agent codex-tiny`; §7.7 Tailscale recipe now `--name codex`.
- §10.3 row added: `ac serve codex` with `AGENTCHUTE_AGENT_ID=codex`
  exported resolves to `codex-tiny` and hellos clean; unmapped candidate
  passes verbatim; `--to` unmapped.

## Revision log (round 9b) — codex round-8 gate overlap

- BLOCKER (direct-serve path) CONFIRMED COVERED and made explicit:
  verified in code that `cmdServe` reaches `resolveAgentID` after
  discovery (`serve.go:135,145`) on the direct `agentchute serve` path
  that never touches the dispatcher, and that `ac serve` merely injects
  `--as <canonical>` (`ensureDispatchIdentity`, dispatch.go:269-278)
  before exec-ing that same serve — §7.2 now states the lookup lives
  INSIDE `resolveAgentID` (identity.go:13-38) and therefore covers BOTH
  launch paths. Honest addition beyond the finding: `guard` resolves its
  id inline (`evaluateGuardInvocation`, guard.go:169-189, not via
  resolveAgentID) — named in §7.2 with M4 routing it through the same
  map so the latch path cannot diverge. §10.3 resolver row now launches
  BOTH ways after `hub join --name codex`.
- MAJOR (hostname-change invariant untested) FIXED: §10.3 row added —
  join as `codex-tiny`, change the injected hostname, re-run
  join/identity/serve → id, `names` map, and key files remain
  `codex-tiny`; no newly derived id, key, or authorized_keys line.

## Revision log (round 10) — codex combined-gate findings on naming

- BLOCKER (no candidate on the direct env-unset path) FIXED §7.2 + §10.3:
  verified `cmdServe` keeps the wrapper canonical only in
  `launchedWrapper` (serve.go:122-129) and `resolveAgentIDRaw` has only
  flag/env sources (identity.go:27-38), so the two-command quickstart's
  bare `agentchute serve codex` had NO candidate; design now wires
  `launchedWrapper` in as the fallback candidate when both flag and env
  are absent, resolved through `names` like any other. The §10.3 resolver
  row was split exactly as directed: a direct-launch case with env
  explicitly UNSET (the prior row false-greened by exporting it) and a
  separate exported-env case covering both launch paths.
- MAJOR (local-name/pool-id shadowing) FIXED §7.2 + §10.3 — took the
  refuse-at-join option: a join whose id (verbatim `--as` or minted local
  name) collides with the other namespace on this machine is refused
  BEFORE keygen/authorize, with exact error texts naming the existing
  mapping and the way out, both orders; §10.3 row covers both orders and
  asserts no key/authorize/config side effects.
- MINOR (stale §7.6 + unspecified warning) FIXED: doctor sample updated to
  `codex-tiny` (config/key/pinned lines, versioned-key symlink shown); the
  `AGENTCHUTE_AGENT_ID`-disagreement warning §7.2 referenced is now
  actually specified in §7.6 with exact text, plus a §10.3 row (warning
  fires on a mapped local name, silent on an unmapped id).
