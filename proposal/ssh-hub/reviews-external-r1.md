# External review round 1 — DESIGN.md @ sha256 f858d5dc… (2026-08-14)

Both reviewers verified the freeze hashes and returned **FIX**. Both endorse
the architecture (one hub, seam, forced-command pinning, no-auto-replay,
channel/one-shot split) and explicitly warn against shrinking the code back
toward a naive one-shot. Reviser: address every finding, log dispositions in
the Revision log at the end.

## codex (deep backstop, code-verified at HEAD 1244ae4). Verdict: FIX

- **C1 (BLOCKER) §3/§5.3 — actor-free wire has no actor-bearing operation
  API.** Ops (`check`/`ack`/`gate`/`pending`/`register`/`clean-owed`/`send`)
  must be actor-scoped, but the sole seam signature is
  `(*loop.Config, Request)` and requests carry no actor; `loop.Config` has
  no identity (config.go:34-53; identity.go:13-38). "Hub passes its pinned
  id into every call" is unimplementable without smuggling identity through
  config/global state. Fix: explicit non-wire `op.Context{ActorID string}`
  in every actor-scoped op signature; local CLI builds it from
  `resolveAgentID`, hub session from the forced-command id; actor stays
  absent from wire structs; test all actor-scoped ops through both callers.
- **C2 (BLOCKER) §3.5/§6.6 — RegisterReq cannot preserve registration or
  guarded lifecycle semantics.** (A) child one-shot `self-check`/`turn-end`
  under a live channel lease: refresh requires ServeToken == claim
  (register.go:118-149); RegisterReq has no token → "live elsewhere".
  (B) current register distinguishes absent vs explicitly-empty
  `--bio`/`--host`; wire omits Bio/BioProvided/HostProvided.
  (C) remote turn-end spec omits current best-effort step-0 self-repair
  (turn_end.go:15-39,83-90). Fix: add serve_token + bio + field-presence
  (pointers or booleans) to the register wire contract; channel dispatch
  injects its held token, one-shots send the inherited token; spec remote
  turn-end step 0 as best-effort wire register continuing on failure; add
  live-lease self-check/turn-end tests.
- **C3 (BLOCKER) §7.4 — path-only E_SELF_POINTER rejects valid remote
  joins.** Laptop and hub sharing the same absolute pool path (same
  username/layout is common) → discovery refuses a genuinely remote URL.
  Fix: require proof the URL HOST is the local host before refusing (host
  identity + path), or drop discovery-time refusal for a non-mutating hello
  identity check; test identical-paths-two-hosts and true-self-URL.
- **C4 (MAJOR) §4.4.3 — streaming fixed the frames, not the producer.** The
  seam still materializes unbounded slices (`ClaimResp.Items` with every
  4 MiB body; check --limit 0) before the dispatcher can stream → N×4 MiB
  hub buffering, OOM risk; Ack/Pending same shape. Fix: producer APIs
  (emitter/callback/iterator + terminal counts), single codec writer
  consuming incrementally; tests: many max-size messages; connection failure
  after first emitted item.
- **C5 (MAJOR) §5.1/§7.2 — shell-argument encoding is not general.**
  Rejecting only `"`\newline doesn't protect spaces, `$()`, backticks,
  `;`, `'` in binary/pool paths (login shell runs `command="…"`); the
  auto-authorize "values are '-free" claim isn't guaranteed by the URL
  grammar. Fix: one tested shell-word encoder for every forced-command and
  interactive-ssh argument + separate authorized_keys-option-layer escaping;
  OR pin an opaque binding id in authorized_keys with the validated
  agent/pool mapping in a 0600 file. Test the full metacharacter set.
- **C6 (MAJOR) §9/§11 — release ordering publishes a false capability.**
  PR-0 (spec: "hub is a supported transport") lands before PR-1 (seam) tags
  v1.6.0 → v1.6.0 ships hub spec with no hub. Fix: release seam-only v1.6.0
  BEFORE PR-0, or tag nothing between PR-0 and complete v1.7.0.
- **C7 (MAJOR) §9 — PR-0's spec delta isn't normative enough; conformance
  gates late.** The AGENTCHUTE.md amendment list has no normative frame
  schema, ordering, no-replay rule, disconnect-after-claim rule, remote
  turn-end, timeout/size limits, or version policy — they live only in this
  proposal, violating spec-is-source-of-truth; and §9.3's L/W vectors sit in
  PR-8 AFTER v1.7.0 ships. Fix: PR-0 adds a normative hub-wire/lifecycle
  section (or normative doc incorporated by reference); move L/W vectors
  into PR-2/PR-3/PR-7 so they gate v1.7.0.
- **C8 (MINOR) §6.9 — boot-corroboration ordering ambiguous.** Fresh
  LastSeen refusal happens BEFORE the pid-proof (lease.go:205-244); a
  pre-boot claim with fresh LastSeen still holds → contradicts "immediately
  reclaimable". Fix: state+test the ordering (StartedAt<boot under lock
  before freshness refusal, or keep the 10 s floor and delete "immediately"
  everywhere); make the fixture's LastSeen explicitly fresh.
- **C9 (MINOR) §6.7 — `--relaunch-args "<string>"` has no argv grammar.**
  Fix: repeatable `--relaunch-arg` or a `--` argv tail; store []string; no
  reparsing; argv-exact tests. (Interacts with grok m3: cut from v1.)
- **C10 (MINOR) §6.1 — local-startup claim reversed.** Local runWrapper
  acquires lease, starts wrapper, THEN registers (serve.go:363-400); the
  design claims register-before-child as a mirror. Fix: state remote
  register-before-child as a deliberate strengthening, not parity; don't
  derive PR-1 zero-behavior tests from the wrong order.
- Verified sound at this revision: spool placement vs rejectLoopStateBodyFile;
  no-auto-replay; relaunch fences+stops old child fully before fresh child;
  child env exports ssh URL / strips LOOP_DIR; filename-derived sender
  identity canonical.

## grok (outside view, simplicity+ops; walked both personas). Verdict: FIX

- **G-B1 (BLOCKER) §7.2/§7.4 — re-join not specified idempotent.** Bare
  re-run (which §7.3 recommends!) re-runs ssh-keygen → new key → duplicate-id
  refusal or --replace-key evicts the live key. Happy-path self-lockout.
  Fix: normative key reuse if `keys/<id>` exists; rotation only via explicit
  `--rotate-key` (drives --replace-key); test join-twice → same pubkey, one
  authorized_keys line.
- **G-B2 (BLOCKER) §1/§7.2 — ssh-keygen argv unspecified.** Default prompts
  for a passphrase → join hangs or a passphrase key kills every BatchMode op
  as E_UNAUTHORIZED. Fix: exact argv `ssh-keygen -q -t ed25519 -N "" -f
  <hubdir>/keys/<id>_ed25519 -C agentchute:<id>`; refuse passphrase-protected
  keys (BatchMode probe).
- **G-B3 (BLOCKER) §7.2+§6.7 — relaunch is opt-in; written next step is
  still `ac serve codex`.** Daily lid-close = dead lane unless the user was
  watching a TTY prompt. Fix: remote serve (Remote != nil) defaults relaunch
  ON, `--relaunch=false` to opt out; §7.2 shows the default path.
- **G-M1 (MAJOR) §5.1 — `--pool <path>` unquoted in command="…"** — space
  in path splits argv. Fix: reject whitespace in pool/binary paths (extend
  E_POOL_PATH_UNQUOTABLE) or quote; prefer reject. (Codex C5 generalizes
  this — solve together.)
- **G-M2 (MAJOR) §5.2 — `agentchute:<id>` marker global per hub user, not
  per pool.** Two control repos on one hub account break authorize/--list/
  --revoke. Fix: marker `agentchute:<id>:<canonical-pool>` (or hub-id);
  duplicate-id keyed on (id, pool). Cheap now, expensive migration later.
- **G-M3 (MAJOR) §7.5/§12.1 — offline laptop stalls 5 s per hook
  invocation, several per turn.** Fix: 30 s negative cache in the shadow dir
  (last E_CONNECT at T → skip dial until T+30 s, same warn-and-exit-0);
  do NOT build the deferred pending cache.
- **G-M4 (MAJOR) §9 — enrollment surface missing.** AGENTS.md/enrollment
  templates must cover: remoteness is discovered; never replay
  E_SEND_UNKNOWN; doctor after join.
- **G-M5 (MAJOR) §7.2 — join never proves the hub session can WRITE the
  pool.** Auto-authorize can succeed against another user's repo / 700
  perms; first send is E_HUB_IO. Fix: post-auth probe checks Writable
  (fold into hello-ok per G-m2), join fails naming hub-user/pool-path/perms.
- **G-m1** Cut `poll` op — inject re-check always falls back to last-tick
  counts. **G-m2** Cut `ping` op — fold Writable into hello-ok. **G-m3**
  Cut `--relaunch-args`/RelaunchArgs from v1 (ships empty everywhere; see
  C9). **G-m4** §7.7 "one paste per machine" stale vs zero-paste flow.
  **G-m5** Catalog gaps: laptop ssh binary missing; authorized_keys not
  0600 (sshd silently ignores); E_CHANNEL_LOST must echo the real
  invocation, not hard-code `ac serve codex`. **G-m6** §7.2 next-command
  must be runnable in the CURRENT shell (`agentchute serve …`). **G-m7**
  Collapse the 10-PR plan to ~6 merges (2+3, 5+6, 9 with 7) — process
  weight only, do not shrink code.
- Keep (explicit): seam PR, channel/one-shot split, pinning, no-auto-replay,
  §6.9 boot-proof, probe-before-pointer, shadow/no-LOOP_DIR contract, sshd
  matrix. Hub-operator prereqs (sshd, Remote Login, stable binary path) as a
  3-line first-time block; no `hub locate` in v1.

## Revision log (round 2)

- **C1** FIXED §3 conventions (new "Actor context" rule: every actor-scoped
  op is `(*loop.Config, op.Context{ActorID}, Request)`; non-wire; CLI builds
  it from `resolveAgentID`, hub session from the pinned `--agent`), §3.1,
  §5.3, §4.4.1 note; both-constructor tests in §10.1.
- **C2** FIXED §3.5 (RegisterReq gains `ServeToken` + pointer-typed
  `Host`/`Bio` for field presence — verified against
  `register.go:35-44,80-87,126-128,147-149,189-195,226-233`; channel injects
  its held token, one-shots the inherited env token), §6.1 step 3, §6.6
  (remote turn-end step 0 = best-effort wire register continuing on failure,
  mirroring `turn_end.go:15-39,110-120`), §10.3 live-lease + field-presence
  test rows.
- **C3** FIXED §7.4: path-only refusal replaced by **join-state** refusal —
  `E_SELF_POINTER` deleted; `E_NOT_JOINED` fires only when the per-hub
  `config.json` is absent (pure local, offline-safe, guard unaffected since
  an unjoined machine has no serve session); same-abs-path-two-hosts is
  valid and tested; §7.5 text rewritten; §10.3 row.
- **C4** FIXED §3 conventions + §3.2/§3.3/§3.6: `Claim`/`Ack`/`Pending` are
  emitter-based streaming producers (summary-only returns); dispatcher's
  emit writes each frame through the single codec writer (peak hub buffering
  = one ≤4 MiB body); §4.4.3 producer rules tied to the seam; tests in
  §10.1/§10.2 (max-size fleet, failure-after-first-item).
- **C5+G-M1** FIXED §5.1 (one decision): **reject, never encode** — pool and
  binary paths must match `^[A-Za-z0-9._/+-]+$` (whitespace and all shell
  metachars refused via `E_POOL_PATH_UNSAFE`); every other value grammar-
  validated; the pubkey is the sole single-quoted element, inert by charset.
  The opaque-binding-id alternative was considered and DECLINED: it
  reintroduces a second mapping store that can drift from the key line, for
  a benefit (paths with spaces) the refusal already remediates. Full
  metacharacter refusal matrix in §10.3.
- **C6** FIXED §9 intro + §11: decision = **seam-only v1.6.0 tags BEFORE the
  spec merge**; no tag between M2 and complete v1.7.0 — the spec never
  describes a capability no release has.
- **C7** FIXED §9.1 (M2 adds a normative "Hub wire & lifecycle" spec
  section: frame grammar, limits, ordering, no-replay,
  disconnect-after-claim, remote turn-end, timeouts, version policy,
  pinning; spec-wins supremacy) + §9.3/§11 (L + fake-transport W vectors in
  M3, sshd-backed W in M6 — all gating v1.7.0, never after it).
- **C8** FIXED §6.9 (ordering stated exactly: freshness refusal at
  `lease.go:230-232` precedes the pid branch at `lease.go:242-244`,
  unchanged; boot corroboration lives inside branch (d) only), "immediately/
  at once reclaimable" deleted in §6.7/§8 row 23; §10.3 fixture now stale-
  LastSeen explicit plus a fresh-LastSeen still-held case.
- **C9+G-m3** FIXED — `--relaunch-args` and `RelaunchArgs` cut from v1
  entirely (§6.7, §7.3): relaunch reuses the original argv `[]string`
  verbatim, no reparsing; a repeatable `--relaunch-arg` is named as the
  future shape if ever wanted.
- **C10** FIXED §6.1 step 3: local order is lease → child → register
  (`serve.go:374,384,393`); remote register-before-child stated as a
  deliberate strengthening, not parity; §10.1 pins the LOCAL order
  unchanged.
- **G-B1** FIXED §7.2/§7.3: key reuse is normative on re-join (existing
  `keys/<id>` never regenerated); rotation only via explicit
  `hub join --rotate-key` (drives `--replace-key`); join-twice test in
  §10.3.
- **G-B2** FIXED §7.2: exact argv `ssh-keygen -q -t ed25519 -N "" -C
  agentchute:<id>:<pool12> -f <hubdir>/keys/<id>_ed25519`; pre-existing
  passphrase-protected keys refused via `ssh-keygen -y -P ""` probe.
- **G-B3** FIXED §6.7/§6.4/§7.3/§8 rows 2/22-24: relaunch is **default-on
  for remote lanes** (`--relaunch=false` opts out; error on local lanes);
  the TTY prompt was cut with the default flip; quickstart path is now the
  self-healing path with no flag typed.
- **G-M1** FIXED with C5 (whitespace in pool/binary paths is inside the
  rejected set; refusal preferred over quoting, as grok asked).
- **G-M2** FIXED §5.1/§5.2: marker is `agentchute:<id>:<pool12>`
  (`sha256(abs pool path)[:12]`, computable independently on both sides);
  authorize/--list/--revoke and duplicate-id detection keyed on (id, pool12).
- **G-M3** FIXED §7.5 + §12.1: 30 s negative cache
  (`~/.agentchute/hub/<hub-id>/hub-down.json`) — hook-context commands skip
  the dial until T+30 s, success clears it; `turn-end` and human-typed
  commands deliberately bypass the cache; the deferred pending-content cache
  stays unbuilt; doctor reports cache state (§7.6).
- **G-M4** FIXED §9.1: enrollment-surface bullet (AGENTS.md + wrapper
  templates: remoteness is discovered; never replay `E_SEND_UNKNOWN`
  without confirming; doctor after join/move) — lands in M2.
- **G-M5** FIXED §7.2 + §3.6/§4.3: `writable` folded into `hello-ok`
  (per-session temp-file probe); join fails on `writable:false` with the
  exact three-part remediation text (hub user / pool path / perms).
- **G-m1** FIXED — `poll` op cut (§3.4, §4.4, §6.5): pre-injection re-check
  uses last-tick counts (≤5 s stale, fail-open); single-channel-writer is
  now structural (only the tick loop writes).
- **G-m2** FIXED — `ping` op cut (§3.6): payload folded into `hello-ok`;
  doctor/join probe = one-shot hello.
- **G-m4** FIXED §7.7: "one paste" → "one join command (zero pastes with own
  SSH access)".
- **G-m5** FIXED §7.5/§7.1/§7.6: `E_NO_SSH` row; authorize chmods
  authorized_keys 0600 / ~/.ssh 0700 and --list/doctor FAIL on wrong perms
  (StrictModes silent-ignore named); `E_CHANNEL_LOST` echoes the lane's own
  launch argv, never a hard-coded example.
- **G-m6** FIXED §7.2: next command is `agentchute serve codex` (runnable in
  the current shell), with `ac serve codex` noted for after a new shell.
- **G-m7** FIXED §11: 10 PRs → **6 merges** (M1 seam/tag, M2 spec, M3
  codec+session+vectors, M4 client, M5 channel+join UX, M6 sshd matrix+docs/
  tag) — process collapsed, all code deliverables retained, ≈9.2k LOC
  unchanged.
- Hub prereqs 3-line first-time block added to §7.1 (grok keep-list item);
  no `hub locate` added.

Rejections: none of the findings; one **sub-option** declined with argument —
C5's opaque-binding-id alternative (see C5 entry above; §5.1 records the
reasoning in the design itself).
