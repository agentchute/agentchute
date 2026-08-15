# Internal critic reports — DESIGN.md round 1 (2026-08-14)

Two independent claude-side critics reviewed `DESIGN.md` (authored against HEAD
1244ae4). Both returned NEEDS-WORK. Findings below are verbatim-condensed; the
reviser must address every BLOCKER and MAJOR, and record a disposition
(FIXED §x / REJECTED because …) per finding in the Revision log at the end.

## Critic A — end-user seamlessness. Verdict: NEEDS-WORK

- **A-B1 (BLOCKER)** Laptop sleep/wake and hub reboot kill remote lanes daily;
  design never says "sleep" or "reboot"; recovery is a human retyping
  `ac serve` per machine per event. Consensus only vetoes resuming the SAME
  child on a dropped channel — a supervised full-lane relaunch (fence old
  child → fresh channel/lease/token/child, vendor resume args where the
  wrapper supports them) is consensus-compatible. Fix: matrix rows
  "remote sleeps/wakes" + "hub reboots (N lanes)"; opt-in relaunch
  (`--relaunch` or prompt on E_CHANNEL_LOST); if deferred, price it out loud.
- **A-M1** `hub authorize` paste runs from $HOME; pool resolution unspecified;
  URL path vs forced-command pool never reconciled → silent wrong-pool join.
  Fix: URL path authoritative end-to-end; authorize line includes `--pool`;
  authorize fails loudly on non-pool path; client hard-errors on
  hello-ok.pool != url.path (E_POOL_MISMATCH).
- **A-M2** Join can be ONE command, zero cross-machine pastes, in the common
  case: §7.1 already presupposes ordinary SSH access, so `hub join` should
  attempt `ssh user@hub agentchute hub authorize ...` itself (interactive
  auth), falling back to print-and-paste. Record why if rejected.
- **A-M3** `.agentchute-control-repo` not gitignored → `git add -A` on laptop
  commits it; hub pulling it redirects hub discovery to ssh-to-itself. Fix:
  join writes pointer AND ensures ignore entry; hub refuses a pointer whose
  URL resolves to its own pool.
- **A-M4** Typo'd URL poisons checkout: pointer written before probing;
  re-join/undo semantics unspecified; stale shadow dirs accumulate. Fix:
  probe before writing pointer; re-join replaces pointer with notice;
  E_CONNECT mentions un-join.
- **A-M5** No hub-side health surface for broken authorized_keys lines. Fix:
  `hub authorize --list` validates each line (binary exists/executable, pool
  resolves) PASS/FAIL; hub-side doctor runs same check.
- **A-M6** Error-catalog gaps: duplicate-id authorize refusal text (row 12
  promises it, absent — spell the codex/codex-laptop fork), E_POOL_PATH_
  UNQUOTABLE text, row-21 turn-end-failure text naming the hub, bare
  `hub join` with no args.
- **A-m1** §7.2 quickstart fails verbatim on fresh laptop: missing
  install+clone prereq line; `ac` shim not on PATH in same shell — use
  `agentchute serve` or print "open a new shell".
- **A-m2** E_UNAUTHORIZED should embed the complete ready-to-paste authorize
  line (client has its own pubkey), not send user to another command.
- **A-m3** Cut `hub join --verify` (redundant once A-m2 lands; bare re-join
  verifies).
- **A-m4** Map fast ssh exec-failure (127) to immediate "hub binary not
  found at <path>" instead of burning the 10s hello timeout.
- **A-m5** `--revoke` should detect a live session via serve.claim pid and
  print the kill line.
- **A-m6** "Two commands per side" claim is off by 2× as designed (4 actions
  + paste); land A-M2 or reword.
- **A-n1** Pool-path inconsistent across examples (agentchute vs
  coordination) — one story-wide path. **A-n2** "5 s worst-case hook delay"
  is per invocation, not per turn. **A-n3** authorize must run as the ssh
  login user; macOS hub needs Remote Login — one clause each. **A-n4** note
  in §7.2 the URL path is the pool's absolute path on the hub (`pwd` there).

## Critic B — adversarial correctness (verified against code @ 1244ae4). Verdict: NEEDS-WORK

- **B-B1 (BLOCKER)** Child env/discovery contract unspecified. Real serve
  exports AGENTCHUTE_CONTROL_REPO + AGENTCHUTE_LOOP_DIR verbatim
  (serve.go:512-514); env outranks pointer (config.go:208-258). With §7.4's
  config: (1) CONTROL_REPO=local-repo-root resolves LOCAL → children mail a
  dead shadow/local pool silently; (2) LOOP_DIR=shadow can't resolve —
  vendorFromLoopDir requires dotdir parent (config.go:331-357); (3) guard
  fails OPEN on Discover error (guard.go:195-204) → latch permanently inert
  on every remote lane. Fix (normative subsection): serve exports
  CONTROL_REPO=<canonical ssh:// URL>, never exports LOOP_DIR; Discover's
  ssh:// arm derives the shadow LoopDir itself (or shadow path gains a
  dotdir segment); integration test: hook-context guard resolves shadow
  latch, child send reaches hub.
- **B-M1** E_SEND_UNKNOWN retry command as printed is REFUSED by the shipped
  --body-file state-tree guard (send.go:600-632 rejects any path under
  <LoopDir>/state/; spool lives there per §7.4). Fix: move spool to
  ~/.agentchute/hub/<hub-id>/spool/ (simpler) or carve out spool subdir.
- **B-M2** tick heartbeat has no registration-template source: TickReq{} is
  empty but HeartbeatRegistration needs a validating template (Vendor,
  ControlRepo…, registration.go:192-240); hub session knows only id+pool.
  Fix: add `register` to channel startup (after lease-acquire, before child,
  mirroring serve.go:374-393); session caches it as tick template; tick
  refused before it arrives.
- **B-M3** 64 KiB control-line cap vs legitimately large aggregate responses
  (ack-ok acked[] for 200+ messages, check-ok quarantined[], pending-ok,
  status-ok) — and oversize is spec'd session-fatal, after ack already
  committed hub-side. Fix: stream aggregates as per-item frames (msg
  pattern) or bound + truncated-count and exempt hub-composed responses
  from violation-closes-session.
- **B-M4** Hub reboot/SIGKILL: serve.claim survives; reclaim needs stale ≥10s
  AND pidAlive false; OS pid reuse after reboot → ErrLeaseHeld forever until
  manual delete (lease.go:236-244). Matrix has no hub-reboot row;
  E_LEASE_HELD "clears within ~20s" is false here. Fix: add matrix rows;
  branch remediation text on claim age; optionally StartedAt < hub boot
  time ⇒ dead (flag as spec decision touching lease.go).
- **B-m1** Two channel producers (tick loop + inject loop's poll) can
  interleave frames → E_MALFORMED_FRAME → spurious child fence. Specify
  single channel-writer; contended poll falls back to last-tick counts.
- **B-m2** §4.5.1 "piped body untouched on preflight failure" overstated on
  the wire path (stdin drained before hub verdict; spool compensates) —
  reword honestly; note code reads --body-file BEFORE preflights
  (send.go:114-125), design defers to after hello.
- **B-m3** Hub `agentchute update` invalidates ALL serve leases
  (lease.go:371-373) → fleet-wide E_FENCED + relaunch; name it in the
  matrix; note live sessions keep executing the old binary inode.
- **B-m4** Stale AGENTCHUTE_CONTROL_REPO env silently defeats the join
  pointer (env outranks, config.go:218-225; env-leak incidents are in this
  project's history). hub join + doctor warn when env disagrees with
  pointer.
- **B-m5** hub session must build Config directly from --pool, both arms —
  hub user's env (bashrc sourced under Debian sshd) still outranks
  otherwise (config.go:299-329).
- **B-n1** Sanitize SSH_ORIGINAL_COMMAND control bytes before logging.
  **B-n2** Latch arms at first claim locally (check.go:168-193), not
  "exactly" as today remotely — drop "exactly".

### Verified-sound (do NOT churn these)
Hub-side pid lease reclaim; token-checked early release; in-lock fence
checks (floor.go:127-131, seq.go:328-332); disconnect-after-claim
redelivery (inbox.go:328-381); no-auto-replay semantics (no interleaving
double-sends or loses body); ack/turn-end crash recovery (turn_end.go:
120-163); disk-full behavior; version handshake all skew directions;
actor-free wire identity incl. owed/floor/gate; restrict/forced-command +
accept-new + ControlMaster facts; 4 MiB cap = MaxInboxMessageBytes (wire
stricter than local today — a plus); conformance lease-gap response;
consensus compliance incl. all vetoes (one-shot mux'd FRAMED sessions do
not violate the one-shot veto — it targets shell-text re-parsing).

## Revision log (round 1)

- **A-B1** FIXED §6.7 (new) + §1 non-goal reworded + §6.4 step 3 + §8 rows
  22–24 + §7.3 tally + §10.3 relaunch tests + §11 PR-5 (+250 LOC). Supervised
  full-lane relaunch: `serve --relaunch` (+`--relaunch-args`), TTY prompt
  fallback, backoff 1→60 s ±20 % jitter unbounded, transport-loss trigger set,
  single attempt on `E_FENCED` (hub-update case), never on
  identity/version/pool/hostkey errors; hub-reboot N-lane recovery designed.
- **A-M1** FIXED §4.3 (pool check bullet, `E_POOL_MISMATCH`), §5.1 (authorize
  validates pool + `--pool` mandatory in line), §7.1, §7.5 texts, §10.3 rows.
  URL path authoritative end-to-end.
- **A-M2** FIXED §7.2: join auto-runs authorize over the operator's own
  interactive ssh (per-argument single-quoting, values `'`-free by grammar),
  falls back to print-and-paste; §7.1/§1 claims updated.
- **A-M3** FIXED §7.4 pointer lifecycle: `.git/info/exclude` entry (warn if
  pointer already tracked) + `E_SELF_POINTER` refusal when the ssh arm's URL
  path resolves to a local pool (the committed-pointer-on-hub accident).
- **A-M4** FIXED §7.4 (probe-before-pointer: hello-ok or auth-refusal writes;
  connect/DNS/hostkey failures write nothing) + §7.2 (re-join replaces with
  notice; un-join documented) + `E_CONNECT` un-join hint.
- **A-M5** FIXED §7.1 (`--list` PASS/FAIL validation) + §7.6 (hub-side doctor
  runs the same audit) + §10.3 authorize-validation row.
- **A-M6** FIXED §7.5: duplicate-id refusal text (spells `codex-laptop`),
  unquotable-pool text, exact turn-end-unreachable text (row 21 now cites
  it), bare `hub join` usage text.
- **A-m1** FIXED §7.2 (prereq line; "open a new shell" note on `ac`).
- **A-m2** FIXED `E_UNAUTHORIZED` embeds the complete ready-to-paste line.
- **A-m3** FIXED — `--verify` cut (§7.3, §3.6); bare re-join verifies.
- **A-m4** FIXED — `E_HUB_NO_BINARY` from ssh exit 127, no 10 s wait (§4.4.2,
  §7.5).
- **A-m5** FIXED §7.1 — `--revoke` reads `serve.claim`, prints `kill <pid>`.
- **A-m6** FIXED — claims reworded everywhere ("one command … one paste when
  an admin sits in the middle"): §1, §7.1, §7.2.
- **A-n1** FIXED — `/home/alex/code/agentchute` unified across §4.3, §5.1,
  §7.5, §7.6, §7.7.
- **A-n2** FIXED — "per hook invocation" wording in §7.5 and §12.
- **A-n3** FIXED §5.1 + §7.1 (login user; macOS Remote Login).
- **A-n4** FIXED §7.2 ("absolute path on the hub — run `pwd` there").
- **B-B1** FIXED §6.8 (new, normative): serve exports
  `AGENTCHUTE_CONTROL_REPO=<ssh URL>`, never `AGENTCHUTE_LOOP_DIR` (strips
  inherited, mirroring the AGENTCHUTE_GUARD hygiene); ssh:// arm branches
  before `validateExplicitControlRepo`, derives the shadow locally (offline-
  safe, so guard can never fail open from remoteness); shadow moved to
  `~/.agentchute/hub/<hub-id>/.agentchute/loop/` so the dotdir-parent loop-dir
  invariant holds; explicit `--loop-dir`+ssh:// is a hard error; §10.3 gains
  the child-env/guard-shadow/child-send integration row. §7.4 updated.
- **B-M1** FIXED — spool relocated to `~/.agentchute/hub/<hub-id>/spool/`
  (outside the shadow loop tree; `rejectLoopStateBodyFile` verified at
  send.go:600-633); §4.5.3 + §7.4 specify it and the `--body-file` retry.
- **B-M2** FIXED — `register` is a mandatory channel-startup step between
  `lease-acquire` and child start (§6.1 step 3, §3.4, §3.5); session caches
  it as the tick heartbeat template; `tick` before `register` → `E_ORDER`
  (new code, §4.4.2).
- **B-M3** FIXED §4.4.3 (new producer rules): `ack-item`/`pending-item`
  streaming frames (vocab + example updated), quarantined→count,
  status-ok/expired_owed caps with `truncated`, and violation-closes-session
  scoped to received frames only (§4.3) — a post-commit response can never
  kill the session.
- **B-M4** FIXED §6.9 (new): boot-time corroboration of the same-host
  pid-proof (StartedAt < boot ⇒ dead; /proc/stat btime, sysctl kern.boottime;
  unreadable ⇒ unchanged fail-closed), flagged as the one internal/loop
  change and added to the §9.1 spec delta; §8 rows 23–24; `E_LEASE_HELD`
  text branched fresh-vs-stale-claim in §7.5; §10.3 pid-reuse row; §11 PR-3.
- **B-m1** FIXED §6.5 single-channel-writer bullet (poll goroutine owns the
  pipe; inject's poll answered from last-tick counts when a tick is in
  flight) + §4.3 note.
- **B-m2** FIXED §4.5.1 honesty rewrite (partial survival, spool
  compensation, and the send.go:114-125 calibration note).
- **B-m3** FIXED §8 row 24 + §6.7 trigger set (single `E_FENCED` relaunch
  attempt) + old-binary-inode note.
- **B-m4** FIXED §7.2 + §7.4 + §7.6: join and doctor warn on
  env-vs-pointer disagreement.
- **B-m5** FIXED §5.1: hub session builds Config directly from `--pool`,
  both arms; discovery cascade never runs hub-side.
- **B-n1** FIXED §5.1: SSH_ORIGINAL_COMMAND logged only after C0/C1 strip.
- **B-n2** FIXED §6.6: "exactly" dropped; local-vs-remote arm timing stated.

Rejections: none — every finding was accepted. (A-M2's security note is
recorded in §7.2: the auto-authorize ssh uses the human's own interactive
credentials, deliberately not BatchMode and not the pinned key.)
