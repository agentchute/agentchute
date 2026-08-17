# SSH Hub — Design (implementation-ready)

Status: DESIGN — for review, then merge-by-merge execution (§11, M1–M6).
Brief: [`proposal/ssh-hub/BRIEF.md`](BRIEF.md). Consensus inputs:
`.agentchute/loop/scratch-synthesis-2026-08-14-next-ref-impl.md` ("FINAL CONSENSUS").
All code citations are `file:line @ 1244ae4` (repo HEAD at design time).
OpenSSH facts cited below were verified against `sshd(8)` and `ssh_config(5)`
(man.openbsd.org, fetched 2026-08-14); Tailscale SSH facts against
tailscale.com/kb/1193 (fetched 2026-08-14).

---

## 1. Goals / non-goals

### Goals

1. **One authoritative pool host (the hub).** The hub is *today's pool*,
   unchanged: plain files under `.agentchute/loop/`, every CI-tested invariant
   (flock, `link()`-no-clobber, rename claim, serve lease + fencing token)
   executing on the hub's kernel exactly as it does now. The network moves the
   **operation** to where the state lives; it never moves, syncs, or replicates
   the state.
2. **Remote lanes via a long-lived channel.** A remote machine's `agentchute
   serve` drives a persistent SSH session (the *channel*) whose hub-side peer
   process owns the lane's serve lease, lease renewal, registration heartbeat,
   and inbox poll. Never one-shot interpolated `ssh host agentchute <argv>`
   shell text.
3. **Forced-command per-key identity pinning, day one.** One authorized key =
   one agent id. The hub-side entry point is a forced command
   (`command="…",restrict`); the wire protocol carries **no actor field at
   all** — the actor *is* the pinned identity, so a `--as`/`--from` mismatch is
   structurally impossible past the handshake (§5).
4. **Deployment seamlessness above all** (Alex's stated priority): hub setup is
   zero commands; joining a machine is one command in the common case (§7.2,
   one paste when an admin sits in the middle); every
   plausible end-user failure has a named, actionable error (§7.5); existing
   single-host pools keep working untouched and a hub pool is adoptable
   incrementally (the hub *is* the existing pool; remotes join it).
5. **Release gates** (consensus, non-negotiable): hard connect/read/heartbeat
   timeouts everywhere (§4.6); a dropped channel fences and stops the local
   child (§6.4); explicit protocol/version handshake (§4.3); **never**
   auto-replay `send` after an ambiguous disconnect — fail closed and report
   (§4.5.3).
6. **Operation seam first.** The first implementation PR extracts structured
   operations from `cmdSend`/`cmdCheck`/`cmdAck`/the lease lifecycle
   (§3); the filesystem remains the only backend; both the in-process (local)
   and SSH (remote) paths call the same seam.
7. **Zero new Go dependencies.** The client shells out to the system `ssh`
   binary (§4.2, with the exact invocation); the hub side is the existing
   binary reading stdin/stdout. `ssh-keygen` is shelled out for join-time key
   generation. No `golang.org/x/crypto/ssh`, no new module requirements.

### Non-goals (the vetoes, restated as binding)

- **Federation** (per-host pools + remote-delivery verb): stays a later
  extension. Not v1.
- **SFTP-as-filesystem / any network-mounted loop dir** as the multi-host
  story: the hub replaces it.
- **Loop-dir sync/mirroring** (Syncthing/rsync/Dropbox/git/brokers): fails
  claim exclusivity; permanently rejected.
- **Unrestricted keys**: every hub key ships with `restrict` + forced command;
  there is no documented "trusted key with a shell" mode.
- **Unbounded SSH waits**: every network wait in this design has a numeric
  deadline (§4.6).
- **Auto-replay of `send`** after an ambiguous outcome: never (§4.5.3).
- **S3 / HTTP backends**: phase-2+ behind the same seam; not designed here
  beyond the seam being shaped so they slot in.
- **Resuming the SAME child across a dropped channel**: never — the child's
  fencing token is baked into its environment at launch
  (`internal/cli/serve.go:502-530 @ 1244ae4`) and cannot be swapped
  mid-session, so a dropped channel always fences and stops the running child
  (§6.4). What IS in scope — and consensus-compatible, because the veto
  targets resuming a fenced child, not supervision — is the **full-lane
  relaunch** (§6.7, default-on for remote lanes; `--relaunch=false` opts
  out): fresh channel, fresh lease, fresh token, fresh child.

---

## 2. Architecture overview

Three processes on the remote machine, one per-connection process on the hub,
and the pool state exactly where it is today.

```
REMOTE MACHINE (laptop)                      HUB (today's pool host)
┌──────────────────────────────────────┐     ┌──────────────────────────────────────┐
│ agentchute serve (runner, remote     │     │ sshd                                 │
│ mode)                                │     │   │ authorized_keys:                 │
│  ├─ child wrapper (claude/codex/…)   │     │   │  restrict,command="agentchute    │
│  │    PTY, cue injection — all local │     │   │  hub session --agent codex …"    │
│  │    env: AGENTCHUTE_SERVE_TOKEN    │     │   ▼                                  │
│  │         (minted hub-side)         │     │ agentchute hub session (per conn)    │
│  └─ CHANNEL: one dedicated `ssh`     │◄───►│  ├─ owns the serve lease (its OWN    │
│     child process, framed NDJSON     │ SSH │  │  hub pid is in serve.claim)       │
│     hello → lease-acquire → register │     │  ├─ tick: RenewLease + Heartbeat +   │
│     → tick every 5 s → lease-release │     │  │  throttled sweep + inbox counts   │
│                                      │     │  └─ calls the operation seam         │
│ child's CLI calls (send/check/ack/   │     │         │                            │
│ gate/pending/boot): ONE-SHOT framed  │◄───►│         ▼                            │
│ sessions over `ssh` with             │ SSH │ internal/op (operation seam)         │
│ ControlMaster=auto mux               │     │         │                            │
│                                      │     │         ▼                            │
│ LOCAL-ONLY state (per-hub dir §7.4): │     │ internal/loop — unchanged invariants │
│  guard.latch, runner.json,           │     │ .agentchute/loop/{inbox,agents,      │
│  runner.log, spool/, mux sockets,    │     │   archive,malformed,state}/          │
│  known_hosts, keys                   │     │ (flock, link-no-clobber, rename      │
└──────────────────────────────────────┘     │  claim, lease + fencing token)       │
                                             └──────────────────────────────────────┘
```

Load-bearing choices, each detailed later:

- **The hub session process is the lane's presence on the hub.** It acquires
  the serve lease with `loop.AcquireServeLease`
  (`internal/loop/lease.go:150 @ 1244ae4`); `serve.claim.pid` is the *hub-side
  session pid*, so the existing same-host pid-proof reclaim rule
  (`internal/loop/lease.go:242-244 @ 1244ae4`) works again for remote lanes —
  no cross-host freshness-only reclaim, no lease-timing changes. When the SSH
  connection dies, sshd tears the session process down, the session's exit
  path releases the lease (token-checked, `internal/loop/lease.go:345
  @ 1244ae4`), and relaunch is immediate.
- **Hub clock everywhere.** Timestamps (`MintSendStamp`,
  `internal/loop/floor.go:111 @ 1244ae4`), `last_seen` heartbeats
  (`internal/loop/registration.go:192 @ 1244ae4`), and lease freshness are all
  minted on the hub. A remote lane's clock skew has **zero** protocol effect —
  strictly stronger than today's NTP-loose assumption (AGENTCHUTE.md §5.4).
- **Local things stay local.** The PTY, cue injection, idle detection, guard
  latch (§6.6), runner state, and the send spool never cross the wire.
- **Two connection shapes, one wire protocol.** The channel (long-lived,
  dedicated TCP connection, owns the lease lifecycle) and one-shot operation
  sessions (short, multiplexed over a ControlMaster socket). Both speak the
  identical framed protocol (§4); the hub session process cannot tell them
  apart until the first request arrives (`lease-acquire` marks a channel).

---

## 3. Operation seam (`internal/op`)

Merge M1 (§11) extracts a package `internal/op` whose functions are the **only** way
`send`/`check`/`ack`/`serve`/`boot`/`gate`/`pending`/`status` mutate or read
pool state. The CLI keeps parsing/rendering; `internal/op` owns
validate-and-mutate; `internal/loop` keeps the primitives, unchanged. The hub
session process (§4) is a thin dispatcher from wire frames to these same
functions. No generic low-level Store rewrite: the seam is at *operation*
granularity, the filesystem is the only backend, and `internal/loop` signatures
do not change.

Conventions:

- **Actor context (C1).** Actor-scoped ops take an explicit, **non-wire**
  context: `op.Context{ActorID string}` — signature
  `(*loop.Config, op.Context, Request) (Response, error)`. Neither
  `loop.Config` nor the request structs carry identity (`loop.Config` has no
  identity field, `internal/loop/config.go:34-53 @ 1244ae4`), so the actor
  must arrive as its own argument, never smuggled through config or global
  state. The local CLI builds it from `resolveAgentID`
  (`internal/cli/identity.go:13-38 @ 1244ae4`); the hub session builds it
  once, from the forced command's pinned `--agent`, and reuses it for every
  dispatch. The wire structs stay actor-free (§5.3). Every actor-scoped op
  is tested through **both** constructors (§10.1).
- **Wire shape.** Requests/responses are plain JSON-taggable structs — they
  are also the wire payloads (§4.4); errors are typed sentinels mapped 1:1
  to wire error codes (§4.4.2). Lines that today go straight to a stream
  become `NoteEvent`s on the typed event stream (next bullet) so the remote
  client can render them in order — at **both** levels, where the level is
  the stream (`warn` → stderr, `info` → stdout; §4.3): `warn` for the stderr
  warnings (`check.go` alone has five distinct stderr warning sites, e.g.
  `internal/cli/check.go:141-151 @ 1244ae4`), `info` for the stdout status
  lines produced inside the stream rather than at its end
  (`check.go:201,207,262 @ 1244ae4`).
- **Streaming producers — ONE ordered, typed event stream (C4, D2).** Ops
  whose results are unbounded — `Claim`, `Ack`, `Pending` — do not
  materialize result slices *of any kind*, side channels included. Each
  takes a single typed emitter:
  `emit func(op.Event) error`, where `op.Event` is a tagged union with
  exactly one non-nil arm:
  `{Message *MessageEvent, Note *NoteEvent, Owed *OwedEvent, Ack
  *AckItemEvent}`. Events are emitted **in production order** — a
  quarantine warning lands between the messages it actually occurred
  between, an expired-owed entry where the code produced it — so ordering
  survives the wire and nothing buffers until return (an earlier draft
  returned `Notes[]`/`ExpiredOwed[]` on the summary, which both re-created
  unbounded buffering and lost every note on a mid-stream failure).
  Terminal summaries carry **counts and truncation metadata only** — no
  slices. The hub dispatcher's `emit` writes one wire frame per event
  through the single codec writer and releases it; peak hub memory is one
  event (≤4 MiB body), never N×4 MiB for a `check --limit 0` over a full
  inbox. The local CLI's `emit` renders directly — today's per-message
  display loop, so local behavior is unchanged by construction. An `emit`
  error (connection gone) aborts the op after the current event; for `Claim`
  the already-claimed residue is redelivered by the next check (§4.5.2), for
  `Ack` the archive commit already happened per item and re-acking is
  idempotent (`internal/loop/inbox.go:269-292 @ 1244ae4`).

### 3.1 `op.Send`

- **Input**: `SendReq{To string, Content []byte, Ask bool, ReplyBy
  time.Duration, ServeToken string}` — `Content` is the fully composed message
  (frontmatter + body). Composition (`loop.ComposeMessage`,
  `internal/loop/message.go:20 @ 1244ae4`; `applyAskHeading`,
  `internal/cli/send.go:483 @ 1244ae4`; `applyReplyRequiredFrontmatter`,
  `internal/cli/send.go:500 @ 1244ae4`) stays client-side/CLI-side so the hub
  never rewrites bodies. **There is no `From` field** — the actor is
  `op.Context.ActorID` (locally from `resolveAgentID`, remotely the pinned
  key id; §3 conventions, §5.3). This is the B4 pattern the codebase already
  teaches: delete the second source of a fact instead of equality-checking
  it (`internal/loop/seq.go:283-291 @ 1244ae4`).
- **Wraps**: sender-enrollment stat (`internal/cli/send.go:141-148
  @ 1244ae4`), lock-free recipient preflight `loop.CheckRecipientReachability`
  (`internal/loop/seq.go:226 @ 1244ae4`), `loop.SendTsMessageWithCommit`
  (`internal/loop/floor.go:160 @ 1244ae4`) = `MintSendStamp`
  (`floor.go:111`) + `DeliverUnderRecipientLock`
  (`internal/loop/seq.go:307 @ 1244ae4`, incl. the fresh-suffix EEXIST retry,
  `seq.go:334-350`), and — when `Ask` — `loop.RecordOwed`
  (`internal/loop/owed.go:202 @ 1244ae4`). The asker's `.owed` ledger lives
  hub-side (`state/<id>/owed.json` on the hub), so gate/check/clean all see
  one ledger.
- **Output**: `SendResp{Filename, Ref string, Committed bool, DurabilityNote
  string, OwedNote string}` — `Ref` is `TsID.RefString()`
  (`internal/loop/tsid.go:76 @ 1244ae4`). `Committed && DurabilityNote !=
  ""` is the linked-but-sync-failed partial success
  (`internal/loop/seq.go:179-181 @ 1244ae4`), which the CLI already renders
  as "delivered … Do NOT resend" (`internal/cli/send.go:269 @ 1244ae4`).
  `OwedNote` is the asker-side reply-obligation bookkeeping failure after
  delivery (`json:"owed_note"`). `DurabilityNote` and `OwedNote` are
  **independent, always-present strings** — `""` when clean, never omitted.
  **The operation error is nil once delivery commits.** `err != nil` means
  unambiguously "not delivered"; a non-empty `OwedNote` is not a delivery
  failure and must not drive resend.
- **Errors**: `ErrNotRegistered` (sender — one sentinel and one wire code, but
  the sender-path *text* stays with this call site; §7.5 carries both texts),
  `loop.ErrRecipientUnknown`
  (`seq.go:191`), `loop.ErrRecipientUnreadable` (`seq.go:206`), stale
  (preflight, C29b) vs racing (`*loop.ErrRecipientStale` post-preflight,
  C29c — classification stays in `internal/cli/send.go:397-416 @ 1244ae4`,
  which becomes the renderer for the seam's typed result), `loop.ErrFenced`
  (`internal/loop/lease.go:51 @ 1244ae4`), collision exhaustion
  (`seq.go:350`). The spool-on-failure path (`preserveSendBody`,
  `internal/cli/send.go:418-458 @ 1244ae4`) stays CLI-side: the spool is
  sender-local state and must survive precisely when the hub is unreachable.

### 3.2 `op.Claim` (the state half of `check`)

- **Signature**: `op.Claim(cfg, ctx, ClaimReq{Limit int, NoArchive bool},
  emit func(op.Event) error) (ClaimSummary, error)` — the typed event
  stream of the §3 conventions (C4/D2): each claimed/redelivered message is
  claimed, read, emitted as a `MessageEvent`, and released one at a time;
  quarantine warnings are `NoteEvent`s in place; expired obligations are
  `OwedEvent`s where the loop reaches them. Nothing accumulates a slice
  with N bodies (or N notes) in memory.
- **Wraps**, in `cmdCheck`'s exact order (`internal/cli/check.go:125-283
  @ 1244ae4`): `loop.ListInboxMessagesWithSkipped`
  (`internal/loop/inbox.go:121 @ 1244ae4`), §11 filename quarantine
  `loop.QuarantineInboxFile` (`internal/loop/inbox.go:58 @ 1244ae4`), claimed
  residue `loop.ListClaimedMessages` (`internal/loop/inbox.go:388
  @ 1244ae4`), per-message `loop.ReadFileLimit` at `MaxInboxMessageBytes`
  (`internal/loop/registration.go:34,19 @ 1244ae4`), frontmatter validation +
  quarantine `loop.ValidateMessageFrontmatter`
  (`internal/loop/message.go:105 @ 1244ae4`), `loop.ClaimMessage`
  (`internal/loop/inbox.go:328 @ 1244ae4`), the asker-side owed discharge
  `loop.ClearOwed` keyed by `in_reply_to` (`internal/cli/check.go:321-331`,
  `internal/loop/owed.go:241 @ 1244ae4`), and the expired-obligation report
  `loop.LoadOwedLedger`/`ExpiredOwed` (`internal/loop/owed.go:149,273
  @ 1244ae4`).
- **Output**: emitted events — `MessageEvent{Filename, Sender, Stamp
  string, Redelivered, ReplyRequired bool, ReplyRef string, Body []byte}`,
  `NoteEvent{Level, Msg string}` (`Level` ∈ {`warn`,`info`}, routed per
  §4.3 — `check`'s three in-stream stdout status lines are `info` notes, so
  their POSITION survives, not just their text), and `OwedEvent` carrying
  the **full**
  `loop.OwedEntry` fields — `{To, From string, Seq uint64, Stamp, Suffix
  string, By, RecordedAt time.Time, Ref string}` (`Ref` = precomputed
  `Key().RefString()` convenience; the rest byte-for-byte what the ledger
  holds, `internal/loop/owed.go:114-122 @ 1244ae4` — an earlier
  `{Ref, ExpiredAgo}` shape could not reproduce `pending --json`'s full
  `Owed []loop.OwedEntry` output, E4; expired-ness is derived client-side
  from `By` + the §4.3 hub-time offset) — then `ClaimSummary{Claimed,
  Redelivered, Quarantined, OwedExpired int}` (counts only, D2). **Display stays client-side**: the
  STALE age banner
  (`printConsumedBody`, `internal/cli/check.go:348-364 @ 1244ae4`), control-
  byte sanitization (`sanitizeControlBytes`, `check.go:374 @ 1244ae4`), and
  the reply-ref print (`check.go:393-408`) render the seam's data. The guard
  latch is **not** part of this op — it is local (§6.6), and the CLIENT
  EMITTER arms it on the **first MessageEvent of any kind, before rendering
  it** (E1) — redelivered residue included, and in both the normal and
  `--no-archive` paths. That matches the local loop, whose `setLatch`
  closure (`internal/cli/check.go:169-175 @ 1244ae4`, via
  `maybeSetGuardLatch`, `check.go:292`) is called at **three** sites, all
  before their display: `check.go:185` (non-empty `.claimed` residue, armed
  once before the redelivery loop), `check.go:243` (`--no-archive`
  display-in-place), and `check.go:256` (after `ClaimMessage`, before
  `displayConsumed`). All three must be covered — an emitter armed only for
  freshly claimed mail leaves the redelivery path unarmed, which is exactly
  the E1 failure this rule exists to prevent. Arming after the op returns is
  likewise wrong: a disconnect after one displayed message must leave the
  latch armed over the claimed residue.
- **Errors**: `ErrNotRegistered` (agent-path text, §7.5), `loop.ErrInboxMissing`
  (`internal/loop/inbox.go:111 @ 1244ae4`), read/claim I/O errors.

### 3.3 `op.Ack`

- **Signature**: `op.Ack(cfg, ctx, AckReq{}, emit func(op.Event) error)
  (AckSummary, error)` — typed event stream (§3 conventions, C4/D2): each
  archived message is emitted as an `AckItemEvent` as it commits.
- **Wraps**: `archiveAllClaimed`'s body moves from `internal/cli/ack.go:164
  @ 1244ae4` into `internal/op` (`loop.ListClaimedMessages` +
  `loop.ArchiveMessage`, `internal/loop/inbox.go:257 @ 1244ae4`, idempotent
  EEXIST/SameFile handling included) reshaped to emit per item, then the
  read-only finish-gate evaluation `finishGateClear`
  (`internal/cli/gate.go:262 @ 1244ae4`, itself `evaluateGate`,
  `gate.go:129`).
- **Output**: emitted `AckItemEvent{Filename, ArchivePath string}` events,
  then `AckSummary{Acked int, GateClear bool, BlockReasons []string}` — the
  same information `ackResult` carries today (`internal/cli/ack.go:204
  @ 1244ae4`; `BlockReasons` is the one inline list kept on a summary — a
  fixed-small set of gate reason strings, §4.4.3). The unconditional-commit contract and the `--quiet`/exit-2
  semantics (`ack.go:20-55`) stay in the CLI. The guarded-session `ack`
  denial (`ack.go:105-109`) stays CLI-side too — it reads the **local** latch.

### 3.4 `op.AcquireLease` / `op.Tick` / `op.ReleaseLease`

- **AcquireLease**: `LeaseReq{}` → `LeaseResp{Token string}`. Wraps
  `loop.AcquireServeLease` (`internal/loop/lease.go:150 @ 1244ae4`) exactly as
  `refuseLiveRunnerCollision` does (`internal/cli/serve.go:571 @ 1244ae4`).
  Errors: `loop.ErrLeaseHeld` (`lease.go:46`) with the holder's
  `{host,pid,age}` attached for the error message (read via
  `loop.ReadServeClaim`, `lease.go:297`).
- **Tick**: `TickReq{}` → `TickResp{Pending, Skipped int, Swept []string,
  Warnings []string}`.
  Wraps, in `pollOnce`'s order (`internal/cli/serve.go:609-677 @ 1244ae4`):
  `loop.RenewLease` (`lease.go:317`), `loop.HeartbeatRegistration`
  (`internal/loop/registration.go:192 @ 1244ae4`) using the same
  `heartbeatTemplate` fields (`serve.go:554`) — the template is **not** in
  the (empty) `TickReq`: `HeartbeatRegistration` hard-errors without a
  validating template (Vendor, ControlRepo, …, `registration.go:192,217-242
  @ 1244ae4`), and the hub session only knows id + pool, so the template is
  the `op.Register` payload the channel supplies at startup (§6.1), cached by
  the session for the lane's lifetime exactly as `regTemplate` is in the
  local runner (`serve.go:237 @ 1244ae4`); a `tick` arriving on a channel
  before `register` is refused with `E_ORDER` — then
  `loop.SweepStaleRegistrations`
  (`internal/loop/sweep.go:60 @ 1244ae4`) throttled to once per 10 min
  (mirror of `sweepInterval`, `serve.go:51`), and the pending-mail count
  (`ListInboxMessagesWithSkipped` counts, the seam version of
  `hasPendingInboxMail`, `serve.go:757 @ 1244ae4`). Error: `loop.ErrFenced` —
  terminal for the session, and the **only** hard error a tick returns.
  **`Warnings` carries the non-fatal step failures**, in production order, one
  entry each: today's runner logs a failed non-fenced lease renew
  (`serve.go:622`), a failed heartbeat (`serve.go:631`), and a failed sweep
  (`serve.go:641`) and continues past all three — so on the wire they cannot
  become errors without changing behavior. Each entry is the exact string
  `r.logf` receives today minus its trailing newline (`agentchute serve: renew
  serve lease: <err>` / `heartbeat registration: <err>` / `sweep stale
  registrations: <err>`), and the client re-logs it verbatim, so the runner log
  stays byte-identical. The sweep throttle still advances on failure
  (`serve.go:643`).
- **ReleaseLease**: wraps `loop.ReleaseLease` (`lease.go:345`; `ErrFenced` is
  a no-op there by design).

(There is deliberately **no `poll` op** — an earlier draft had a count-only
tick subset for the runner's pre-injection re-check; cut because the
re-check can always use the last tick's counts, ≤5 s stale, §6.5.)

### 3.5 `op.Register`

- **Input** (C2/D1 — the wire contract must preserve `registerOpts`'s full
  semantics, `internal/cli/register.go:35-44 @ 1244ae4`):
  `RegisterReq{Vendor *string, Host string, Bio *string, WorkingRepos
  []string, Announce bool, Sweep bool, ServeToken string}`. Three deliberate,
  deliberately different field shapes:
  - **`Host` is a plain string, CLIENT-resolved (D1a)** — not a presence
    pointer, because the local semantics it must preserve are not
    "preserve": with no `--host` flag, `performRegister` calls
    `os.Hostname()` and overwrites (`register.go:80-87 @ 1244ae4`). A nil
    pointer resolved hub-side would record the HUB's hostname for every
    remote self-check — wrong machine. So the CLIENT resolves it before
    framing: flag value when provided (explicit empty stays empty),
    otherwise the remote machine's own `os.Hostname()`. The hub session maps
    it to `HostProvided:true` unconditionally, so `performRegister`'s
    hostname substitution simply never runs hub-side. Test: omitted host
    after a machine move records the NEW remote hostname (§10.3).
  - **`Vendor` is a presence pointer, HUB-resolved when nil (D1b)** — bare
    hook invocations (`turn-end`/`self-check`, env-identity-only per C26)
    carry no `--vendor`, and the client-side fallback `resolveAgentVendor`
    reads the registration through cfg (`internal/cli/identity.go:72-82
    @ 1244ae4`) — which on a remote lane is the mail-free SHADOW, so a
    custom id's vendor (one the canonical-prefix table,
    `identity.go:57-70`, can't name — the live roster's "sonnet" is the
    recorded example) would never resolve and every step-0 repair would
    fail. So: non-nil = explicit, wins; nil ⇒ the HUB resolves it from the
    pinned actor's existing registration row in the hub pool, then the
    canonical-id fallback, and only then fails with the existing
    missing-vendor error. Test: omitted vendor against an existing
    custom-id row succeeds (§10.3).
    **Client call sites, normative (S2)** — "the client sends nil" is not
    automatic: `resolveAgentVendor` is called unconditionally at four sites
    today, each of which would resolve against the shadow and put a WRONG
    non-nil value on the wire (or refuse outright) before the hub ever gets
    a chance. When `cfg.Remote != nil`, all four **skip local resolution
    entirely** and send `Vendor: nil` unless `--vendor` was given
    explicitly: `cmdServe` (`internal/cli/serve.go:152 @ 1244ae4`),
    `cmdRegister` (`internal/cli/register.go:259 @ 1244ae4`), `cmdBoot`
    (`internal/cli/boot.go:86 @ 1244ae4`), and `selfRepairRegistration`
    (`internal/cli/self_check.go:126 @ 1244ae4`, the step-0 repair
    `turn-end` drives). `resolveAgentVendor` itself is unchanged — the
    branch is at the call sites, because only they know whether this is a
    local or a wire register. Consequently `cmdServe`'s hard
    missing-vendor refusal (`serve.go:153-155 @ 1244ae4`, `missing --vendor
    (recommended values: …)`) is **suppressed for remote lanes**: an empty
    vendor there is now the legitimate "let the hub resolve it" case, and
    keeping the refusal would make every bare `agentchute serve <wrapper>`
    on a custom id fail before it dialed. The `ValidateAgentID(opts.Vendor)`
    check (`serve.go:156-158`) likewise runs only on a non-empty explicit
    value. A vendor the hub cannot resolve either still fails — with the
    hub's own missing-vendor error, one layer later and with the roster it
    needs (§10.3, register-field-semantics row).
  - **`Bio` stays a presence pointer** — here "nil = keep, non-nil = set
    (empty clears)" IS the local behavior (`BioProvided`,
    `register.go:147-149,226-233 @ 1244ae4`); without it every remote
    re-register would clobber a hand-set bio.
  - **`ServeToken`**: `registrationLiveElsewhere` refuses a register when a
    FRESH serve claim exists and the caller's token is empty or mismatched
    (`register.go:126-128,189-195 @ 1244ae4`). A remote lane's own
    `self-check`/`turn-end` one-shots run while that lane's channel holds a
    fresh claim — with no token on the wire they would be refused as "live
    elsewhere" by their own lease. So: the channel dispatch injects the
    session's held token into its startup `register`; one-shot register-
    bearing ops send the child's inherited `AGENTCHUTE_SERVE_TOKEN`. Tested
    under a live lease (§10.3).
  - **`Sweep` is a plain bool discriminator, set by `cmdBoot` alone** — the
    sweep this op wraps is **boot's**, not every registrant's. In the shipped
    code `loop.SweepStaleRegistrations` runs in exactly two places: `cmdBoot`,
    right after its own registration (`internal/cli/boot.go:99-101
    @ 1244ae4`, C11's "register-self-first, THEN sweep peers"), and the
    runner's 10-minute-throttled tick (`serve.go:640 @ 1244ae4`), which is
    `op.Tick`'s (§3.4) and not this op's. A `register` op that always swept
    would add a pool-wide sweep to `register`, `self-check`, `turn-end`'s
    step-0 repair and `serve`'s startup registration — four commands that do
    not sweep today; one that never swept would strand remote `boot` with no
    hub-side trigger at all, since a remote lane's own filesystem sweep would
    walk the mail-free shadow (§6.8). So the request carries the fact: `true`
    only from `cmdBoot`, and the hub runs the sweep after the registration
    write, in boot's order. Every other caller sends `false`, including the
    channel's mandatory startup `register` (§6.1) — that lane's sweeping is
    the tick's, whose first pass is due immediately, so sweeping here too
    would double it. A sweep failure is a warning, never an error (boot's own
    rule): it appends `sweep stale registrations: <err>` to `Warnings` below,
    which is the exact string `cmdBoot` formats today (`boot.go:100`).
- **Wraps**: the registration write path `performRegister` (used by
  `registerRunner`, `internal/cli/serve.go:532-545 @ 1244ae4`, and by
  `cmdBoot`, `internal/cli/boot.go:25 @ 1244ae4`), — when `Sweep` —
  `loop.SweepStaleRegistrations` (`internal/loop/sweep.go:60 @ 1244ae4`,
  boot's post-register trigger), and — when `Announce` —
  `loop.AnnounceEnrollment`
  (`internal/loop/message.go:56 @ 1244ae4`). `Host` records the **remote
  machine's** hostname (display truth: where the wrapper runs);
  `serve.claim.Host` records the hub (lease truth: where the live process
  is). Turn-start `self-check` is `RegisterReq{Announce:false}` with the
  inherited token. On a channel, `register` is a mandatory startup step
  between `lease-acquire` and child start (§6.1): besides registering, it
  hands the session its tick heartbeat template (§3.4).
- **Output**: `RegisterResp{Announce *AnnounceView, Pending int, Reg
  RegistrationView, InboxDir string, Refreshed, ExistingFound bool,
  ResolvedHost string, Warnings []string}`. The first two are the op's own
  facts (announce fan-out, post-register inbox depth); the
  **last six are forced, not optional** — they are exactly what today's
  `registerResult` hands its callers (`internal/cli/register.go:54-61
  @ 1244ae4`), under the same names and semantics. An op returning less
  cannot reproduce what the CLI already renders, and M1's "existing tests
  pass unmodified" done-when would be unsatisfiable. `Announce` is forced by
  the same rule one layer out — `cmdRegister` renders an announce *result*,
  not a count:
  - **`Announce`** — `nil` unless the request set `Announce`; otherwise
    `AnnounceView{Sent, Total int, Warnings []string}`, a JSON-tagged mirror
    of `loop.AnnounceResult` (`internal/loop/message.go:37-41 @ 1244ae4`)
    under that struct's own field names and semantics ("how many peers were
    candidates, how many got the message in their inbox, and any per-peer
    delivery (inbox-write) warnings", `message.go:33-36`). A single `AnnouncedTo int`
    carries one of the three facts the CLI already prints: each per-peer warning
    (`register.go:285-287 @ 1244ae4`), then either `no peers to announce to`
    when `Total == 0` or `sent to %d of %d peer(s)` from `Sent` AND `Total`
    (`register.go:288-292 @ 1244ae4`). A mirror rather than
    `loop.AnnounceResult` itself for the `RegistrationView` reason below: no
    JSON tags, and `internal/loop` does not change in M1 (§3). In M1 the
    `performRegister` adapter passes `Announce:false` and `cmdRegister` keeps
    calling `loop.AnnounceEnrollment` itself (`register.go:280-294
    @ 1244ae4`), so local rendering is unchanged by construction; a remote
    lane cannot do that — its fan-out must reach the HUB's pool, not the
    mail-free shadow — so it sets `Announce:true` and renders these three
    fields alone. The announce's own hard failure (`AnnounceEnrollment`
    returned an error, `register.go:282-284 @ 1244ae4`) is **not** a fourth
    field: it leaves `Announce` nil and rides in `Warnings` as `announce
    failed: <err>`, which the client prints through the same `warning: %s`
    loop (`register.go:276-278 @ 1244ae4`) — the same bytes, and in the same
    stderr position, because `performRegister` returns no warnings of its own
    today (`register.go:180-186 @ 1244ae4`).
  - **`Reg`** — `cmdBoot` reads `Reg.LastSeen` to detect inherited mail
    (`internal/cli/boot.go:111 @ 1244ae4`), `cmdSelfCheck` renders it as
    `last_seen` (`internal/cli/self_check.go:80 @ 1244ae4`), and
    `register_test.go` asserts `Reg.{AgentID,LastSeen,Body}`
    (`internal/cli/register_test.go:347,376,379 @ 1244ae4`). Its type is
    `RegistrationView`: a JSON-tagged mirror of `loop.Registration`'s eight
    fields (`internal/loop/registration.go:58-68 @ 1244ae4`) keyed by the
    frontmatter's own names (`agent_id`, `v`, `vendor`, `control_repo`,
    `working_repos`, `host`, `last_seen`, `body`). A mirror is required, not
    preferred: `loop.Registration` carries no JSON tags and `internal/loop`
    does not change in M1 (§3). `op.Status` does **not** reuse this view — its
    rows carry a narrower, status-specific set (§3.6), because a `status` row
    renders no body and no vendor and a pool-visible listing must carry only
    what it prints. On the wire, `body` rides as the `register-ok` trailer
    rather than inside the control line (§4.4.3).
  - **`InboxDir`** — `cmdBoot` runs its side-effect-free inbox peek through it
    (`boot.go:104 @ 1244ae4`), `cmdRegister` prints it (`register.go:274
    @ 1244ae4`), and `register_test.go:144-145 @ 1244ae4` asserts it. Remote
    lanes need the HUB's path here (that is where the mail is), which is
    exactly what a hub-side op returns.
  - **`Refreshed`** — the AGENTCHUTE.md §5 wire semantic: true on every
    successful registration write, fresh or merge (`register.go:183
    @ 1244ae4`); `cmdBoot` serializes it as `refreshed` (`boot.go:137
    @ 1244ae4`).
  - **`ExistingFound`** — the pre-write existence fact (`register.go:184
    @ 1244ae4`), deliberately NOT a second spelling of `Refreshed` (which is
    unconditionally true): it alone picks boot's "Refreshed" vs "Registered"
    verb (`boot.go:137-138,199-201 @ 1244ae4`) and
    `register_test.go:344,373 @ 1244ae4` asserts it directly. It is also the
    **only** spelling of the fresh-enrollment fact: an earlier draft carried
    a `Created bool` beside it, cut because `Created` is exactly
    `!ExistingFound` in every returned response — `publishRegistrationOnce`
    sets `existingFound` from the pre-write read (`register.go:112,120-125
    @ 1244ae4`) and takes the exclusive-create arm on precisely
    `!existingFound` (`:159-171`), the EEXIST re-read re-enters that same
    read-then-write arm (`:90-97`), and any other read error returns no
    response at all (`:123-124`). `emitBootText` already derives the
    "Registered" verb that way (`boot.go:200 @ 1244ae4`), and today's
    `registerResult` has no such field (`register.go:54-61`), so nothing
    reads one.
  - **`ResolvedHost`** — the post-merge host actually written, printed by
    `cmdRegister` (`register.go:270 @ 1244ae4`), `cmdBoot` (`boot.go:142
    @ 1244ae4`) and `cmdSelfCheck` (`self_check.go:79 @ 1244ae4`). It is the
    only place the D1a client-resolved `Host` becomes observable, so it is
    also what the §10.3 "machine move records the NEW remote hostname" row
    asserts against.
  - **`Warnings`** — printed by `cmdRegister` (`register.go:276-278
    @ 1244ae4`) and appended to by `cmdBoot`, which adds its post-register
    sweep failure before rendering (`boot.go:99-101,143 @ 1244ae4`). Empty
    from today's `performRegister`, and it stays a response field precisely
    because the `Sweep` arm runs hub-side: a sweep failure on a remote lane
    has no other way home — and neither does the announce failure the bullet
    above routes here. In local mode `cmdBoot` keeps appending its own entry
    exactly as today; in remote mode the hub appends the identical string and
    `cmdBoot` renders it through the same `bootStatus.Warnings` field
    (`boot.go:143`), so the two arms print the same bytes and neither sweeps
    twice.

### 3.6 `op.Status`, `op.Gate`, `op.Pending`, `op.CleanOwed`

- **Status**: `StatusReq{}` → `StatusResp{Agents []StatusAgent, Truncated
  bool, Now time.Time}`, plus a `NoteEvent` emitter (§3 conventions). Wraps
  `loop.ReadRegistrationsLenient`
  (`internal/loop/registration.go:522 @ 1244ae4`) + per-agent inbox depth +
  `loop.ReadServeClaim`/`ClaimIsStale` (`lease.go:297,308`). Read-only.
  - **The row is a status-specific view, not `RegistrationView`**:
    `StatusAgent{AgentID string, LastSeen time.Time, Host string,
    ProtocolVersion int, InboxDepth int, Status string}` — JSON `agent_id`,
    `last_seen`, `host`, `v`, `inbox_depth`, `status`, the first four keyed
    exactly as `RegistrationView` keys them. That is precisely what
    `printStatus` renders — AGENT / STATUS / INBOX / LAST_SEEN / AGE / HOST /
    PROTO plus the protocol-warning block
    (`internal/cli/status.go:97-139,242-247 @ 1244ae4`) — and nothing else.
    `Vendor`, `ControlRepo`, `WorkingRepos` and `Body` are excluded because no
    column shows them (the header's `vendor:` line is `cfg.Vendor`,
    `status.go:100`), and `Body` in particular is a bio bounded at 1 MiB
    (`internal/loop/registration.go:18 @ 1244ae4`) that cannot ride a 64 KiB
    control line (§4.4.3). The serve claim is excluded for the same reason
    `ServeClaim.ServeToken` (`lease.go:59`) is: `status` is pool-visible to
    every member, and the claim's only rendered consequence is the `Status`
    label. `InboxDepth` and `Status` are HUB-derived — a remote client can
    neither stat the hub's inbox dirs nor read its serve claims.
  - **`Now`** is the hub's evaluation instant: the AGE column and the STATUS
    label must come off one clock, and on a remote lane that clock is the
    hub's (same reason `hello-ok` carries `hub_time`, §4.3).
  - **The op never truncates.** `Agents` carries every row, sorted by
    `loop.RegistrationsByAgentID` (`status.go:111`), and `Truncated` is
    always `false` coming out of the op. Both framing limits — the 64 KiB
    line budget and the 64-row cap — are applied by the WIRE producer when
    it frames `status-ok`, which is also the only writer of `truncated:true`
    (§4.4.3). Capping inside the op would silently drop rows in local mode,
    where there is no line to overflow and no renderer that reports a
    truncation. (The trailing notice the remote client prints when
    `truncated` is true is pinned verbatim in §4.4.3.)
  - **The header is local config, not row data — and on a remote lane it
    prints the hub's pointer, not the shadow's.** `printStatus`'s three
    header lines come from `cfg`, never from any row
    (`internal/cli/status.go:98-100 @ 1244ae4`), so the row shape above
    settles nothing about them. The rule, normative for the merge that
    switches the renderer (§11 M4):
    - `control_repo:` prints the canonical `ssh://` URL carried on
      `cfg.Remote` — **not** `cfg.ControlRepo`, which on a remote lane is
      merely the nearest local ancestor holding `AGENTCHUTE.md` and exists
      for local concerns like hook refresh (§6.8 rule 2). The rows below it
      are the hub's; a header naming a local directory that holds none of
      them is the §6.8 rule 5 failure one command later. The existing
      `(via <origin>)` suffix is kept and stays truthful — the URL is what
      the flag/env/pointer arm actually resolved.
    - `loop_dir:` keeps printing the **local shadow**, with one parenthetical
      line under it marking it as the shadow. **Its exact text is pinned**
      (PLAN WI-4.5 states the identical literal; a byte-exact assertion
      cannot be written against a paraphrase) — printed immediately under the
      `loop_dir:` line and ahead of `vendor:`, with **two leading spaces** and
      no trailing space, only when `cfg.Remote != nil`:

      `  (local shadow: this process's own loop dir, not the hub's)`

      Modelled on the marker line already in this same header block,
      `  (shadowed pointer: %s)` (`internal/cli/status.go:101-103 @
      1244ae4`), for its two-space indent and parenthesised `label: value`
      form, and on `  (pull-only: senders deliver to your inbox; you poll it
      yourself)` (`internal/cli/boot.go:204 @ 1244ae4`) for a label followed
      by prose rather than by a path. It genuinely is this process's
      loop dir — guard latch, `runner.json`, the send spool (§4.5.3, §6.8
      rule 3) all live there — and the hub's loop dir is on another
      filesystem and rides on no frame (`hello-ok` carries `pool`, the pool
      path, not a loop dir). Printing the shadow unmarked is what misleads;
      inventing a wire field to print a remote path the operator cannot open
      is not the fix.
    - `vendor:` is unchanged and needs no rule: `cfg.Vendor` is the loop
      dotdir namespace, and vendor namespacing is gone — auto-discovery
      resolves `<controlRepo>/.agentchute/loop`
      (`fixedNamespace`, `internal/loop/config.go:19-25,348-357 @ 1244ae4`)
      and §6.8 rule 3 deliberately gives the shadow the same
      `.agentchute/loop` shape, so both modes print the same string.
    - `ShadowedPointers` is unchanged: it is a diagnostic about *local*
      pointer files that lost discovery, which is a local fact in both modes.
  - **The lenient-read warnings stream as `note` frames**, `level:"warn"`,
    one per malformed row, in `ReadRegistrationsLenient`'s own order and
    before the response — never as a response array. They are genuinely
    unbounded (one per malformed `*.md` under the agents dir,
    `registration.go:522-549 @ 1244ae4`; malformed files are not
    registrations, so the pool-scale assumption does not bound them), which
    is §4.4.3's rule exactly. The `warn`→stderr routing (§4.3) reproduces
    today's `warning: %s` lines in today's position, ahead of the table
    (`status.go:67-70 @ 1244ae4`).
- **Gate**: `GateReq{Phase string, RequireConfirm, AckStaleReg bool}` →
  wraps `evaluateGate` (`internal/cli/gate.go:129 @ 1244ae4`); response
  carries the same fields `gateStatus` renders. The request is exactly
  `evaluateGate`'s parameters minus `cfg`/`agentID`/`now` (`cfg` is the op's
  first argument, `agentID` is `Context.ActorID`, `now` is hub-minted, §2).
  The two booleans are real shipped flags — `gate --require-confirm` and
  `gate --ack-stale-reg` (`internal/cli/gate.go:55-56,97 @ 1244ae4`) — and
  they change the verdict, so a `{Phase}`-only request would silently drop
  them on every remote gate: `--require-confirm` would stop refusing on
  warn-level conditions, and `--ack-stale-reg` would stop acknowledging a
  stale registration. They are request fields.
- **Pending**: typed event stream (§3 conventions, C4/D2) over the read
  half of `cmdPending` (`internal/cli/pending.go:23 @ 1244ae4`):
  `MessageEvent`s for unread mail (metadata always; `Body` populated only
  when the request sets `ShowBody` — today's `--show-body` reads full
  bodies, `pending.go:136-144 @ 1244ae4`, so the wire must carry them as
  §4.4.1 body trailers under the same 4 MiB cap, never inside control
  JSON), `OwedEvent`s for every outstanding obligation (today's output
  emits each one in full-entry form, `pending.go:269-287 @ 1244ae4` — a
  control-JSON array would re-create the 64 KiB failure for a large
  ledger), then `PendingSummary{Unread, Owed, Malformed int, NeedsBoot
  bool}`. `NeedsBoot` is HUB-derived (E4): it comes from the actor's
  registration-row stat and `ErrInboxMissing` on the hub pool
  (`pending.go:77-97 @ 1244ae4`) — facts the remote shadow cannot supply —
  and the CLIENT keeps today's rendering semantics on top of it: the boot
  hint (`needsBootMessage`, `pending.go:182`) and the `--fail-if-any`
  exit-2 rule "unread mail OR needs boot" (`pending.go:167-169`).
  Read-only.
- **CleanOwed**: wraps the `clean --owed` prune so the hint `check` prints
  (`internal/cli/check.go:279-281 @ 1244ae4`) works verbatim from a remote
  lane.

(There is deliberately **no `ping` op** — cut; its payload folded into
`hello-ok` (`pool`, `writable`, `hub_bin`, `hub_time`, §4.3), which every
session already performs. `doctor` and `hub join` probe by opening a
one-shot session, reading `hello-ok`, and closing. The `writable` field is a
create/remove of one temp file under the pool's `state/`, computed per
session.)

Local mode calls these functions in-process (zero behavior change — M1 is a
pure refactor gated on the existing CLI test suite). Remote mode marshals the
same request/response structs as wire frames (§4.4): the seam **is** the wire
schema.

---

## 4. Wire protocol

### 4.1 Name and carriage

- Protocol name: **`agentchute-hub`**, version **1** (integer). The name
  appears in the client's SSH exec request, the `hello` frame, and error
  text.
- Carriage: **forced-command pseudo-subsystem**, not an sshd `Subsystem`
  directive. The client requests exec of the literal string `agentchute-hub`;
  the hub's `authorized_keys` forced command ignores it and runs
  `agentchute hub session …` (verified `sshd(8)`: *"command= … applies to
  shell, command or subsystem execution"*, with the client's request preserved
  in `SSH_ORIGINAL_COMMAND`). **Rejected alternative**: a real
  `Subsystem agentchute-hub` directive in `sshd_config` — it requires root,
  an sshd reload, and a second install step on the hub, and a forced command
  overrides it anyway; it would add a whole "subsystem not found" error class
  for zero benefit. One carriage path only.

### 4.2 Client transport: exact `ssh` invocation

The client execs the system `ssh` via `exec.Command` (argv array — no shell,
no interpolation). Justification for shelling out (consensus item 6): zero Go
dependencies; inherits the user's entire SSH ecosystem (`~/.ssh/config` Host
aliases, ProxyJump, hardware keys, agents) for free; OpenSSH's keepalive and
multiplexing are two decades more battle-tested than anything we would write.
Argv-injection risk is closed structurally: URL components are validated
against a strict grammar at join time (§7.4 — user/host must match
`[A-Za-z0-9._-]+` and must not begin with `-`; port is numeric; path is
absolute), and every variable value is a distinct argv element.

**Channel** (long-lived; one per remote `serve`):

```
ssh -T \
  -o BatchMode=yes \
  -o ConnectTimeout=5 \
  -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
  -o StrictHostKeyChecking=accept-new \
  -o UserKnownHostsFile=<hubdir>/known_hosts \
  -o IdentitiesOnly=yes -i <hubdir>/keys/<agent-id>_ed25519 \
  -o ClearAllForwardings=yes \
  -o ControlMaster=no -o ControlPath=none \
  -o LogLevel=ERROR \
  [-p <port>] [<user>@]<host> agentchute-hub
```

**One-shot** (per CLI command): identical except
`-o ServerAliveInterval=15 -o ServerAliveCountMax=2` and

```
  -o ControlMaster=auto -o ControlPath=<hubdir>/mux/%C -o ControlPersist=60s
```

Rationale for the split (each verified against `ssh_config(5)`):

- `BatchMode=yes`: disables every interactive prompt — a hook- or runner-
  invoked ssh must never hang on a passphrase (failure surfaces as
  `E_UNAUTHORIZED`, §7.5).
- `ConnectTimeout=5`: hub-down is detected in ≤5 s at every entry point.
- `ServerAliveInterval=5 -o ServerAliveCountMax=2` on the channel: OpenSSH
  itself terminates the session ~10–15 s after the peer goes silent
  (verified: on reaching the threshold *"ssh will disconnect from the server,
  terminating the session"*) — the transport-level dead-hub detector that
  backs up the tick deadline (§4.6). One-shots use 15 s ×2: they are
  short-lived and the mux master must not be churned by aggressive probing.
- `ControlMaster=no` on the channel, **deliberately**: the channel *is* the
  lane's liveness signal, so it must own a dedicated TCP connection — its
  health must never be entangled with a shared mux socket that one-shot
  traffic also uses. One-shots use `ControlMaster=auto` + `ControlPersist=60s`
  so a turn's `check`/`send`/`ack` burst pays one authentication, then
  ~tens-of-ms per op on a tailnet; `auto` degrades gracefully (falls back to a
  fresh connection if the socket is stale/absent — verified semantics).
- `StrictHostKeyChecking=accept-new` + a **per-hub** `known_hosts`: first
  connect records the hub's key silently (no prompt — BatchMode-safe);
  a *changed* key hard-fails (verified: accept-new *"will not permit
  connections to hosts with changed host keys"*) → `E_HOSTKEY_CHANGED`
  (§5.6, §7.5). The user's global known_hosts is never touched.
- `IdentitiesOnly=yes -i <pinned key>`: exactly one key is offered — an agent
  with five keys loaded must not accidentally authenticate as a different
  pinned identity (key = identity in this design).
- `ClearAllForwardings=yes`, `-T`: belt (client) and suspenders (server-side
  `restrict`): no forwarding, no PTY.
- `LogLevel=ERROR`: ssh's own stderr is passed through to the operator only
  when the CLI reports a transport error; suppress routine banner noise.

**`ControlPath` length rule (normative).** A ControlPath is a Unix domain
socket, so its expanded path is capped by `sun_path` — ~104 bytes on macOS,
108 on Linux — and `<hubdir>/mux/%C` under a deep `$HOME` can silently exceed
it, breaking multiplexing with an opaque ssh error. The codebase already
solves exactly this for the runner socket and this design reuses that shape
rather than inventing a second one (`Config.RunnerSocketPath`,
`internal/loop/config.go:168-193 @ 1244ae4`):

- **Threshold, the shipped one.** The builder refuses the preferred path when
  `len(muxDir) + 1 + 64 >= 100`, where `100` is the literal
  `RunnerSocketPath` already uses (`config.go:170` — one number in the
  codebase, not two) and `64` is a deliberately conservative upper bound for
  `%C`'s expansion, which the client cannot compute itself. Over-triggering
  costs one extra authentication; under-triggering costs a broken mux.
- **Preferred**: `muxDir = <hubdir>/mux` (§7.4).
- **Fallback when it does not fit**: a per-user temp dir
  `<tempRoot>/agentchute-hub-<uid>/<hub-id>`, trying `os.TempDir()` then
  `/tmp` and taking the first that passes the same budget (macOS's `$TMPDIR`
  is itself a long `/var/folders/…` path, so `/tmp` is a real second
  candidate). Created and checked through the shipped owned-0700 discipline —
  `EnsureRunnerSocketDir` → `ensureOwnedRunnerSocketDir`
  (`internal/loop/config.go:200-206`,
  `internal/loop/runner_socketdir_unix.go:24-49 @ 1244ae4`: `MkdirAll` 0700,
  symlink reject, uid-ownership reject, `Chmod` 0700), generalized to a
  caller-supplied path rather than copied. The uid suffix plus the ownership
  check is what keeps a shared `/tmp` from being squattable.
- **If neither fits (or the temp root is unusable)**: **disable multiplexing
  for this hub** — one-shots run `-o ControlMaster=no -o ControlPath=none`,
  as the channel already does — and emit exactly one `warn` note naming both
  attempted paths. Correctness is unaffected; only the per-op authentication
  cost is. Never a hard refusal: an unusually deep `$HOME` must not make the
  CLI unusable.
- The **channel** is unaffected — it is `ControlMaster=no
  -o ControlPath=none` by design (above).

### 4.3 Framing and handshake

**Framing: newline-delimited JSON control frames + raw body trailers.**

- A frame is one UTF-8 JSON object on one line, LF-terminated. Max frame line:
  **64 KiB** (`E_TOO_LARGE` past it).
- A frame that carries a payload declares `"body_len": N` (bytes); exactly N
  raw bytes follow the frame's LF, then the next frame begins. Max body:
  **4 MiB** = `loop.MaxInboxMessageBytes`
  (`internal/loop/registration.go:19 @ 1244ae4`). Bodies are byte-exact
  message content — no base64, no re-encoding, no argv re-parsing anywhere.
- Client requests carry a client-assigned monotonically increasing integer
  `"id"`; responses echo it as `"re"`. **One request in flight per session,
  strictly serial** — concurrency comes from opening more one-shot sessions
  over the mux, never from interleaving frames. (Simplest correct thing; a
  future v2 could relax it behind the version handshake.)
- Unknown JSON fields are ignored (mirror of AGENTCHUTE.md §6.5). An unknown
  `"t"` gets `{"t":"error","code":"E_UNSUPPORTED",…}` and the session stays
  up. A *framing* violation (non-JSON line, body_len mismatch, oversize) gets
  an error frame and the session closes — there is no resynchronization.
  Violation-closes-session applies to **received** frames only: §4.4.3's
  producer rules guarantee the hub never composes an oversized line, so a
  session can never be killed by the size of its own response — in
  particular never *after* a commit (an `ack` that archived 200 messages
  must not die reporting them).
- On the channel, all client frames are written by a single goroutine
  (§6.5) — one-request-in-flight is structural, not just a rule.
- Hub→client `note` frames (the wire form of §3's `NoteEvent`) may precede a
  response. **`level` is one of exactly two values, and the level IS the
  stream**: `warn` → the client's **stderr**, rendered `warning: <msg>`;
  `info` → the client's **stdout**, rendered `<msg>` with no prefix. `msg`
  never carries its own level prefix — the renderer adds it, so the local
  path and the wire cannot drift. A third level is a spec change (M2), never
  an implementer's choice.

  ```
  H: {"t":"note","re":3,"level":"warn","msg":"quarantined junk.txt (malformed §6.1 filename) -> …"}
  H: {"t":"note","re":3,"level":"info","msg":"(reached limit of 5; 12 more pending)"}
  ```

  Both arms are load-bearing. `warn` carries today's stderr warnings (e.g.
  quarantine, `internal/cli/check.go:144 @ 1244ae4`). `info` carries the
  three **stdout** status lines `check` produces *inside* the stream rather
  than at its end — the empty-inbox line, before the claim loop
  (`check.go:201 @ 1244ae4`); the limit line, between two message renders
  (`check.go:207`); and the CLAIMED-not-yet-archived line, after the claim
  loop but before the owed/expired section (`check.go:262`,
  whose text begins with its own `note: ` wording, which is emitted
  byte-for-byte and is not a level prefix). Re-deriving those from the
  terminal `check-ok` summary would print them after the `owed-item` events
  the op emits before them — a silent reordering inside a merge whose whole
  claim is zero behavior change. Only `info`'s stdout routing keeps them on
  the one ordered stream (D2), in the position they occupy today.

**Handshake** (mandatory first exchange on every session, both shapes):

```
C: {"t":"hello","id":1,"proto":"agentchute-hub","v":1,"min_v":1,"agent":"codex","bin":"1.7.0"}
H: {"t":"hello-ok","re":1,"v":1,"agent":"codex","pool":"/home/alex/code/agentchute",
    "pool12":"9c4e12ab77f0","writable":true,"hub_bin":"1.7.0",
    "hub_time":"2026-08-14T21:05:03.123456Z"}
```

- Version selection: hub computes `use = min(hub_max, client v)`; if
  `use < client min_v` or `use < hub_min` → `{"t":"error","code":"E_VERSION",
  "msg":"hub speaks agentchute-hub v1; client requires ≥2"}` and close. v1 is
  the only version at ship; the negotiation exists so v2 can ship without a
  flag day. Fleet rule stated in the error text: **the hub upgrades first.**
- Identity check: `hello.agent` is the client's resolved id
  (`resolveAgentID`, `internal/cli/identity.go:13 @ 1244ae4`); the hub
  compares it to the key's pinned `--agent`. Mismatch →
  `{"t":"error","code":"E_IDENTITY","msg":"key is authorized as \"codex\"; you
  are acting as \"grok\""}` and close. Past this point **no frame carries an
  actor field** — every op executes as the pinned id (§5.3).
- Pool check (D3): `hello-ok` reports the pool's hub-side identity —
  `pool` (the normalized path, for display) and `pool12` (read from the
  pool's own `state/pool.id` at session start, after the session validated
  it against the forced command's `--pool-id` — §5.1, F1; never an argv
  echo, so a key line whose `--pool` alone was edited dies hub-side at
  startup). At join time the client records both in `config.json`; on
  every later session it hard-fails unless `hello-ok.pool12` equals the
  RECORDED value — the **client-emitted arm** of `E_POOL_MISMATCH`
  (§4.4.2, §7.5); the hub emits the same code at session start for the
  complementary case (§5.1). The raw URL path is authorize
  input and display text only, never an identity: comparing against URL
  text would false-alarm on any symlinked spelling of the same pool. The
  chain is still closed end-to-end: join's URL path feeds `hub authorize`,
  authorize canonicalizes and pins `--pool` into the forced command,
  `hello-ok` echoes what the forced command actually serves, and the
  recorded `pool12` detects a key line later edited toward a different
  pool.
- `hub_time`: the client computes `offset = hub_time − local_now` once per
  session and applies it to *display-only* age math (the C18 stale banner,
  `internal/cli/check.go:349 @ 1244ae4`), so a skewed laptop clock cannot
  mislabel mail age. Protocol state never uses the client clock.

### 4.4 Message types

Complete v1 vocabulary. Client→hub requests: `hello`, `send`, `check`,
`ack`, `register`, `status`, `gate`, `pending`, `clean-owed`,
`lease-acquire`, `tick`, `lease-release`. Hub→client: `hello-ok`,
`send-ok`, `msg`, `owed-item`, `check-ok`, `ack-item`, `ack-ok`,
`register-ok`, `status-ok`, `gate-ok`, `pending-ok`, `clean-owed-ok`,
`lease-ok`, `tick-ok`, `release-ok`, `note`, `error`. The event-stream
frames map 1:1 onto §3's `op.Event` arms: `msg` = MessageEvent (used by
`check` AND `pending` — a pending `msg` omits `body_len` unless the request
set `show_body`, in which case the body follows as a normal ≤4 MiB
trailer), `note` = NoteEvent, `owed-item` = OwedEvent, `ack-item` =
AckItemEvent; all stream interleaved in production order (D2). (`ping`/
`poll`/`pending-item` from earlier drafts are cut: `ping`'s payload lives in
`hello-ok`, §3.6; the pre-injection re-check uses last-tick counts, §6.5;
pending reuses `msg`.)

#### 4.4.1 Examples (normative shapes)

`send` — body flows in as the trailer:

```
C: {"t":"send","id":2,"to":"claude-code","ask":true,"reply_by_s":3600,
    "serve_token":"9f2c…32hex","body_len":184}
   <184 raw bytes: the composed message, frontmatter + body>
H: {"t":"send-ok","re":2,
    "filename":"20260814T210503123456Z_from-codex_r4b1d….md",
    "ref":"to-claude-code_from-codex_20260814T210503123456Z_r4b1d…",
    "committed":true,"durability_note":"","owed_note":""}
```

`committed` is **mandatory on every `send-ok`** and is the field the
never-replay rule (§4.5.3) is written against: `committed:true` means the
recipient-side `link()` succeeded, so the message IS delivered and must
never be resent — including the `committed:true` + non-empty
`durability_note` partial success (linked, dir-sync failed,
`internal/loop/seq.go:179-181 @ 1244ae4`), which the CLI renders as
"delivered … Do NOT resend" (`internal/cli/send.go:269 @ 1244ae4`). It
mirrors `op.SendResp.Committed` (§3.1) 1:1; a `send-ok` without it is a
malformed response, not a defaulted `false`.

`durability_note` and `owed_note` are **mandatory on every `send-ok`**,
each a string, always present — `""` when that arm is clean; omission of
either field is a malformed response, not a defaulted empty (the same
rule `tick-ok.warnings` follows). They are independent: both may be
non-empty on the same send. A non-empty `owed_note` is not a delivery
failure; nothing may treat it as grounds to resend. A remote send
terminates as `send-ok` or `error`: `error` means nothing was delivered;
`send-ok` means delivery committed. An owed-record failure cannot ride
as `error`. This matches AGENTCHUTE.md §13 and the shipped M1 binary;
DESIGN is aligned (the earlier §13-wins carve-out is closed).

`serve_token` is present only when the sender runs under a serve lease (the
child got it via `AGENTCHUTE_SERVE_TOKEN`, `internal/cli/serve.go:520
@ 1244ae4`); the hub passes it into `MintSendStamp`/`DeliverUnderRecipientLock`
for the in-lock fence checks (`internal/loop/floor.go:127-131`,
`internal/loop/seq.go:328-332 @ 1244ae4`). Empty/absent = intentionally
unfenced (a human's ad-hoc send), same as today.

`check` — claimed bytes flow back as one `msg` frame + trailer per item,
terminated by `check-ok`:

```
C: {"t":"check","id":3,"limit":0,"no_archive":false}
H: {"t":"msg","re":3,"filename":"20260814T…_from-claude-code_r….md",
    "sender":"claude-code","stamp":"20260814T205900000000Z",
    "redelivered":true,"reply_required":false,"reply_ref":"","body_len":812}
   <812 raw bytes>
H: {"t":"note","re":3,"level":"warn","msg":"quarantined junk.txt (malformed §6.1 filename) -> …"}
H: {"t":"note","re":3,"level":"info","msg":"note: messages CLAIMED (at-least-once), not yet archived. …"}
H: {"t":"owed-item","re":3,"to":"grok","from":"codex",
    "stamp":"20260814T190211000042Z","suffix":"4b1d…32hex",
    "by":"2026-08-14T19:32:11Z","recorded_at":"2026-08-14T19:02:11Z",
    "ref":"to-grok_from-codex_20260814T190211000042Z_r4b1d…"}
H: {"t":"check-ok","re":3,"claimed":1,"redelivered":1,"quarantined":1,"owed_expired":1}
```

(`quarantined` is a **count**; the per-file details travel in the preceding
`note` frames — §4.4.3. The two `note` frames show both levels and why order
matters: the `warn` one goes to stderr, the `info` one to stdout, and the
`info` line must land *before* the `owed-item` events, which is exactly the
position a summary-derived renderer would lose — §4.3.)

`ack` — results stream one frame per archived message, then the terminal
frame carries counts:

```
C: {"t":"ack","id":4}
H: {"t":"ack-item","re":4,"filename":"20260814T…_from-claude-code_r….md",
    "archive_path":"…/archive/2026-08-14T21-06-11Z_to-codex_…"}
H: {"t":"ack-ok","re":4,"acked":1,"gate_clear":false,
    "block_reasons":["unread inbox mail: 1"]}
```

`register` — the §3.5 structs verbatim, with `reg.body` as the trailer (its
own one-shot session, so the ids restart after its `hello`):

```
C: {"t":"register","id":2,"host":"tiny","working_repos":["/home/alex/code/dash"],
    "announce":true,"serve_token":"9f2c…32hex"}
H: {"t":"register-ok","re":2,
    "announce":{"sent":2,"total":3,"warnings":["send to grok: …"]},
    "pending":0,
    "reg":{"agent_id":"codex-tiny","v":3,"vendor":"openai",
           "control_repo":"/home/alex/code/agentchute",
           "working_repos":["/home/alex/code/dash"],"host":"tiny",
           "last_seen":"2026-08-14T21:05:03.123456Z"},
    "inbox_dir":"/home/alex/code/agentchute/.agentchute/loop/inbox/codex-tiny",
    "refreshed":true,"existing_found":true,"resolved_host":"tiny",
    "warnings":[],"body_len":142}
   <142 raw bytes: reg.body>
```

`announce` is the §3.5 `AnnounceView` verbatim — `sent`, `total`, `warnings`,
all three, because the client renders all three — and is present only when the
request set `announce`; `null` otherwise, including when the hub-side announce
failed outright (that text rides in the top-level `warnings` as `announce
failed: …`, §3.5). `vendor` is absent from the request here (`Vendor:nil` ⇒
hub-resolved, §3.5/D1b), as is `bio` (`Bio:nil` ⇒ keep). Both `warnings` lists
are always present — `[]` when clean, never omitted — the same rule
`send-ok.durability_note` and `tick-ok.warnings` follow.

Lease lifecycle (channel only):

```
C: {"t":"lease-acquire","id":5}
H: {"t":"lease-ok","re":5,"token":"9f2c…32hex"}
     — or —
H: {"t":"error","re":5,"code":"E_LEASE_HELD",
    "msg":"serve lease for codex is held (host=hub pid=48122, fresh 2s ago)"}

C: {"t":"tick","id":6}
H: {"t":"tick-ok","re":6,"pending":3,"skipped":0,"swept":[],"warnings":[]}
     — with a non-fatal step failure —
H: {"t":"tick-ok","re":6,"pending":3,"skipped":0,"swept":[],
    "warnings":["agentchute serve: heartbeat registration: no space left on device"]}
     — after a reclaim/fence —
H: {"t":"error","re":6,"code":"E_FENCED","msg":"serve lease was reclaimed; stop"}   (hub closes)

C: {"t":"lease-release","id":7}
H: {"t":"release-ok","re":7}
```

`warnings` is `[]string` and, like `durability_note` on `send-ok`, is
**always present** — `[]` when the tick was clean, never omitted — so a
missing field is a malformed response rather than a defaulted empty. It
mirrors `op.TickResp.Warnings` (§3.4) 1:1: the fenced case is the tick's only
hard error, and every other step failure (non-fenced lease renew, heartbeat,
sweep) rides back here in production order with the exact text today's runner
logs, which the client re-logs verbatim. It is a **fixed-small** list — at
most one entry per tick step — so it rides inline like `block_reasons`
(§4.4.3), never as `note` frames.

`register`, `status`, `gate`, `pending`, `clean-owed` carry/return the §3
request/response structs as their JSON fields verbatim (the seam structs are
the schema; one source of truth), with two exceptions, both of them framing
concessions to the 64 KiB line cap (§4.4.3):

1. `register-ok` sends `reg.body` as the frame's trailer rather than inline,
   because a bio is capped at 1 MiB and the control line at 64 KiB.
2. `status-ok` may send FEWER rows than `op.Status` returned: the producer
   appends rows only while the encoded control line stays within 64 KiB
   **and** the count stays within 64, and sets `"truncated":true` when
   either budget excluded a row (§4.4.3). The op itself never truncates and
   always reports `truncated:false` (§3.6), so this flag is a property of
   the framing, not of the read — which is also why the budgets are
   invisible in local mode.

Actor context never appears: it is the session's pinned identity
(§3 conventions, §5.3).

#### 4.4.2 Error frame and code registry

`{"t":"error","re":N,"code":"E_…","msg":"<human text>","retriable":false}`

**Pinned amendment (#152 item 2): the error path carries `claimed_held`.**
A top-level optional boolean on the terminal `error` frame, encoded only
as `true` and omitted otherwise, set when `Claim` returns an error with
`ClaimSummary.Redelivered > 0`. M3 frames it; M4 arms the local latch on
`true`. **`check-ok.redelivered` is unchanged** — `check-ok` is only
emitted on a nil error, where residue found equals residue delivered.
**A `note` frame is not sufficient** — arming a latch must never depend
on parsing display text. Covered by **W6**, not W1.

| code | meaning | maps from |
|---|---|---|
| `E_VERSION` | protocol version mismatch | handshake |
| `E_IDENTITY` | hello.agent ≠ pinned key id | handshake |
| `E_POOL_NOT_FOUND` | forced command's `--pool` invalid hub-side | session start |
| `E_NOT_REGISTERED` | actor has no registration row — **two exact texts, selected client-side by call site** (§7.5) | `internal/cli/send.go:145`, `check.go:120`, `status.go:62 @ 1244ae4` |
| `E_RECIPIENT_UNKNOWN` | no row for `to` | `loop.ErrRecipientUnknown`, `seq.go:191` |
| `E_RECIPIENT_UNREADABLE` | row exists, unparseable | `loop.ErrRecipientUnreadable`, `seq.go:206` |
| `E_RECIPIENT_STALE` | preflight stale (C29b) | `seq.go:240` |
| `E_RECIPIENT_RACING` | fresh-then-stale under lock (C29c) | `*loop.ErrRecipientStale`, `seq.go:253` |
| `E_FENCED` | serve token check failed | `loop.ErrFenced`, `lease.go:51` |
| `E_LEASE_HELD` | fresh lease owns the id | `loop.ErrLeaseHeld`, `lease.go:46` |
| `E_HUB_IO` | hub filesystem error (ENOSPC, EACCES…) | any loop I/O error |
| `E_MALFORMED_FRAME` | framing violation (session closes) | codec |
| `E_TOO_LARGE` | frame >64 KiB or body >4 MiB | codec |
| `E_UNSUPPORTED` | unknown `t` (session survives) | codec |
| `E_ORDER` | request out of order (e.g. `tick` before `register` on a channel; lease ops mid-one-shot); session survives | session dispatcher |
| `E_POOL_ID_INVALID` | the pool's `state/pool.id` fails the J1 contract (not a regular 0600 non-symlink file, or content ≠ `[0-9a-f]{12}` + LF); session refuses at startup, before any op | session start (§5.1) |
| `E_POOL_MISMATCH` (hub arm) | the pool's `state/pool.id` is **absent**, or present-and-valid but ≠ the forced command's `--pool-id`; session refuses at startup, before any op | session start (§5.1) |

**`E_POOL_MISMATCH` is emitted by BOTH sides (F9/X1), deliberately one code
with two emitters.** They are the two halves of one fact — "this key is not
serving the pool it is supposed to serve" — and an operator who sees the
code gets the same §7.5 remedy either way, so splitting it into two codes
would buy a distinction without a difference:

- **Hub-emitted, at session start** (registry row above, §5.1): `hub
  session` re-reads `state/pool.id` from the ACTUAL `--pool` it was handed
  and refuses when that value is absent or ≠ `--pool-id`. This catches a
  key line whose `--pool` alone was edited — the case a verbatim argv echo
  would make invisible. It travels as a normal `error` frame before any op,
  and the session closes. (A `pool.id` that is *present but malformed* is
  the distinct `E_POOL_ID_INVALID` above — malformed state, not a mismatch.)
- **Client-emitted, after `hello-ok`** (§4.3, D3): the client compares
  `hello-ok.pool12` against the value RECORDED in `config.json` at join and
  fails closed on inequality. This catches a key line re-pointed
  *consistently* (`--pool` and `--pool-id` moved together) at some other
  validly-authorized pool — which the hub arm passes by construction.

The two arms are ordered and non-overlapping: the hub arm runs before
`hello-ok` exists, the client arm only on a `hello-ok` the hub arm already
let through. §7.5 carries a distinct exact message for each.

Client-side only (never on the wire): `E_CONNECT`, `E_UNAUTHORIZED`,
`E_HOSTKEY_CHANGED`, `E_CHANNEL_LOST`, `E_SEND_UNKNOWN`, `E_HELLO_TIMEOUT`,
`E_HUB_NO_BINARY` (ssh remote exit 127 mapped immediately — no 10 s wait),
`E_NOT_JOINED`, `E_NO_SSH` — produced by the
transport/discovery layer from ssh exit codes/stderr, deadline expiry, and
the §7.4 join-state check; user-facing text in §7.5. (`E_POOL_MISMATCH` is
NOT in this list — it is the both-sides code above.)

#### 4.4.3 Producer rules (aggregate responses)

Unbounded lists never ride inside one control frame — a 64 KiB line cap must
never become a semantic failure, least of all after a commit:

- `check` and `pending` message events stream as `msg` frames (pending
  bodies only under `show_body`, as trailers); `ack` results stream as
  `ack-item` frames; owed entries stream as `owed-item` frames; notes — at
  BOTH levels, `warn` and `info` (§4.3) — as `note` frames; all interleaved
  in production order (D2). The terminal
  `*-ok` frame carries **counts** (plus truncation metadata) for those
  streams, never arrays.
- Streaming reaches all the way down (C4): the §3 seam's emitter APIs
  produce items one at a time, and the dispatcher's emitter writes each
  frame through the single codec writer before the next item is read — the
  hub never materializes an unbounded `[]Item` (peak buffering is one ≤4 MiB
  body). Tested with many max-size messages and with a connection failure
  after the first emitted item (§10.2, §10.3).
- Quarantine details travel as per-item `note` frames;
  `check-ok.quarantined` is a count.
- A pending `msg` frame carries `body_len` (and a trailer) only when the
  request set `show_body`; bodies obey the same 4 MiB cap as every trailer
  (§4.3) — pending bodies never ride inside control JSON (D2).
- `register-ok` carries its `reg` (the `RegistrationView` of §3.5) in the
  control line **except `body`**, which rides as that frame's trailer. A
  registration body is a free-form bio bounded by `loop.MaxRegistrationBytes`
  (1 MiB, `internal/loop/registration.go:18 @ 1244ae4`) — inside the 4 MiB
  trailer cap but far outside the 64 KiB line cap, so inlining it would let a
  long bio turn a response that *already wrote the registration* into a
  framing violation. `status-ok`'s rows carry no body at all — its row view is
  status-specific (§3.6), because `status` renders
  AGENT/STATUS/INBOX/LAST_SEEN/AGE/HOST/PROTO and never the body
  (`internal/cli/status.go:97-139 @ 1244ae4`). That drops the one 1 MiB
  field; it does **not** make the row a bounded shape — see the next bullet.
- **`status-ok`: TWO budgets, both enforced against the ENCODED response.**
  A row count alone does not bound a line, because the row is not a bounded
  shape. `Host` is free-form: `ReadRegistration` copies the frontmatter
  `host` through verbatim under only the 1 MiB whole-file cap
  (`internal/loop/registration.go:72,88 @ 1244ae4`), `Registration.Validate`
  imposes no length bound on it (`registration.go:217-241 @ 1244ae4`), and
  `register --host` is an unconstrained string flag
  (`internal/cli/register.go:206 @ 1244ae4`) — so ONE otherwise-valid
  registration with, say, a 70 KiB host blows the line cap before row count
  matters. `AgentID` has no length bound either (`agentIDPattern =
  [a-z0-9][a-z0-9-]*`, `internal/loop/inbox.go:40 @ 1244ae4`); only the
  filesystem's `NAME_MAX` on `<id>.md` bounds it in practice. The producer is
  therefore written to depend on **no** field being bounded. Normatively, when
  it frames `status-ok` it appends rows from the op's sorted slice **in
  order**, keeping a row only while BOTH hold:
  1. **wire budget** — the complete control line, measured as encoded bytes
     including every non-row field (`t`, `re`, `now`, `truncated`), the
     `agents` array's own punctuation and the terminating LF, stays ≤ 64 KiB;
  2. **row cap** — the kept-row count stays ≤ 64 (the pool-scale assumption,
     AGENTCHUTE.md §2, 2–~10 agents: a several-hundred-row table is a bug
     report, not a listing).

  The first row that fails either check **ends the append** — every later row
  is dropped too, so the emitted list is always a PREFIX of the op's sort
  order. (Skip-and-continue is rejected: it would omit a middle row while
  showing later ones, and `truncated` cannot say which.) `"truncated":true`
  is written whenever ANY row was excluded, by **either** budget — it is not
  a row-cap flag. Measure the budget with `"truncated":false` encoded: `true`
  is one byte shorter, so a frame that fit while the flag was false still
  fits once it flips, and the producer needs no second pass.

  **The non-row fields count against the budget**, deliberately. The
  normative rule below is that the hub must never emit a control line over
  64 KiB; a rows-only budget would balance its own books and still emit an
  over-cap line — the exact defect the budget exists to prevent. The
  bookkeeping cost is one integer.

  **If the FIRST row alone does not fit**, `agents` is `[]` and `truncated`
  is `true`. That is a valid response, not an error: `status` is read-only
  and pool-wide, and refusing the whole listing because one row is
  pathological would lose every other agent's row. The client's truncation
  notice (§11 M4) names the wire limit as well as the row cap, so the
  operator is not told "64 rows" when the cause was one 70 KiB host.

  **That notice's exact text is pinned** (PLAN WI-4.5 states the identical
  literal; two compliant implementations must not print two different lines,
  and the byte-exact test rows in §10.3 assert these bytes) — one
  unindented line on **stdout**, emitted only when `truncated` is true, as
  the LAST line of `status` output (after the table and after any
  `PROTOCOL WARNINGS:` block), preceded by one blank line:

  `note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.`

  Modelled on the shipped trailing notice at
  `internal/cli/check.go:262 @ 1244ae4` — the lowercase `note: ` prefix, no
  indent, one sentence, a semicolon before the consequence. It names both
  budgets, states that rows were withheld, and states the PREFIX rule (the
  missing rows are the TAIL of the agent-id sort, never an arbitrary
  subset). It states no total: `status-ok` carries only the kept rows and
  the flag, so "N of M" would have to be invented.

  **Rejected alternative — streaming status rows as individual frames.** It
  would keep an oversized row visible remotely, at the cost of a new frame
  type, a second terminal-count contract, and a renderer that must buffer the
  whole listing anyway to compute the tabwriter's column widths — so unlike
  `msg`/`owed-item`, whose items are individually unbounded *and* individually
  useful, streaming buys no memory bound here. It would also still not render
  a 70 KiB host into a HOST column. The only row the wire budget drops that
  the row cap would not is one that cannot be displayed as a table cell
  anyway. The root fix for that row is a length bound on `host` at the
  registration layer — shipped-code scope, deliberately outside this
  proposal, and the hub must not depend on it landing.

  Both budgets are applied when the response is FRAMED, never inside
  `op.Status`, which returns every row with `truncated:false` (§3.6): a rule
  that exists to keep one line under 64 KiB has no business dropping rows on a
  path that has no line, and `printStatus` reports no truncation, so an in-op cap would
  lose rows silently in local mode. (`expired_owed` no longer needs a cap —
  owed entries stream as `owed-item` frames, D2. The `status` lenient-read
  warnings are likewise NOT a `status-ok` field: they are unbounded — one per
  malformed file, not per agent — and stream as `warn` `note` frames, §3.6.)
  `block_reasons` (gate reason strings), `tick-ok.warnings` (at most one entry
  per tick step, §3.4) and `register-ok.announce.warnings` (at most one entry
  per peer, bounded by the same pool-scale assumption as the row cap above)
  are fixed-small sets and ride inline.
- Normative: the hub MUST NOT emit a control line over 64 KiB; a response
  that would exceed the cap is a hub implementation bug, not a session-fatal
  wire event (§4.3). Every aggregate response above satisfies this
  **structurally**, not by assumption: the unbounded lists stream, the one
  unbounded scalar (`reg.body`) rides as a trailer, and `status-ok` — the
  only response that composes an inline list from unbounded strings —
  measures the encoded line as it appends. No producer is permitted to reason
  "this field is small in practice".

### 4.5 Semantics of the three delicate flows

#### 4.5.1 Send bodies in

The client composes the full message locally (so `--body-file` reads, ASK
heading, and `reply_required` splicing behave identically to
`internal/cli/send.go:174-224 @ 1244ae4`), then transmits it once as the
`send` trailer. The hub-side `op.Send` performs mint + deliver under hub
locks. Honesty note on the local-mode guarantee "a piped body is untouched
on every preflight failure" (`send.go:135-137 @ 1244ae4`): it survives only
**partially** on the wire path. Stdin/`--body-file` are read after the hello
succeeds, so connect/auth/version/identity/pool failures leave a piped body
untouched — but the hub's own preflights (sender enrollment, recipient
freshness) can only run after the body has necessarily been read and
transmitted. The compensation is the spool: every hub-side preflight failure
spools the body locally with the retry command (§4.5.3); nothing is lost,
but "stdin untouched" is a local-mode property and this design says so.
(For calibration: local mode itself already reads `--body-file` before its
preflights, `send.go:114-125 @ 1244ae4` — the wire path's read-after-hello
ordering is the stricter of the two.)

#### 4.5.2 Check-claimed bytes back

Claiming happens hub-side (rename into `.claimed/`,
`internal/loop/inbox.go:328 @ 1244ae4`) **before** the bytes stream back. If
the connection dies mid-stream, the mail is claimed-but-undisplayed — which is
exactly the crash-window the two-phase design already covers: the next `check`
lists `.claimed` residue and re-displays it with the REDELIVERED banner
(`internal/cli/check.go:180-193 @ 1244ae4`). At-least-once for the work,
unchanged.

#### 4.5.3 Ambiguous `send` outcome — fail closed, never replay

The **ambiguity window** opens when the first byte of the `send` frame is
handed to the ssh child's stdin, and closes when `send-ok` or an `error` frame
for that `id` is read. Failure semantics:

- **Before the window** (connect, hello, preflight error frame): the send
  provably did not happen. The CLI spools the body
  (`writeSendSpool`, `internal/cli/send.go:427 @ 1244ae4`) and prints the
  retry command. In remote mode the spool directory is
  `~/.agentchute/hub/<hub-id>/spool/` (§7.4) — deliberately **outside** the
  shadow loop tree, because the shipped `--body-file` guard refuses any path
  that resolves inside `<LoopDir>/state/` (`rejectLoopStateBodyFile`,
  `internal/cli/send.go:600-633 @ 1244ae4`): a spool under the shadow state
  dir would make the printed retry command refuse its own file. The remote
  retry command is `--body-file <spool>` (one inert command; works while
  guard-latched), not local mode's `< <spool>` stdin redirection
  (`sendRetryCommand`, `send.go:465-477 @ 1244ae4`). Exit 1. Retrying is
  safe and the text says so.
- **Inside the window** (channel drops, ssh exits, response deadline expires
  with no frame): the outcome is **unknown** — the hub may or may not have
  linked the message. The CLI spools the body, exits 1 with `E_SEND_UNKNOWN`
  (§7.5 text), and **never retries automatically**. There is no
  delivery-side dedup to hide behind (AGENTCHUTE.md §6.2: at-most-once, no
  idempotency key); a blind replay would be a duplicate message. The text
  tells the operator how to resolve the ambiguity (ask the recipient /
  `agentchute status` shows their inbox depth) before deciding to resend.
- A `send-ok` with non-empty `durability_note` is the existing
  linked-but-dir-sync-failed partial success: report, do not resend
  (`internal/cli/send.go:269 @ 1244ae4` text reused).

### 4.6 Timeouts, deadlines, cadence (all values final)

| timer | value | enforced where |
|---|---|---|
| TCP connect | 5 s | client, `-o ConnectTimeout=5` |
| hello → hello-ok | 10 s | client (kills ssh child on expiry); hub kills session if no `hello` within 10 s of start |
| one-shot response deadline | 30 s per request | client (covers a 4 MiB body on a slow link) |
| channel tick interval | 5 s | client serve loop (parity with `defaultRunnerIntervalSeconds`, `internal/cli/serve.go:23 @ 1244ae4`) |
| tick response deadline | 10 s | client; expiry ⇒ kill ssh child ⇒ fence path (§6.4) |
| transport dead-peer kill | ~10–15 s | ssh itself, `ServerAliveInterval=5 ×2` (channel) |
| hub session read deadline | 20 s (channel, = 3 missed ticks + margin); 30 s idle (one-shot) | hub, `os.Stdin.SetReadDeadline` |
| hub session write deadline | 30 s per response | hub |
| one-shot session max lifetime | 10 min | hub (safety net) |
| mux master linger | 60 s | `-o ControlPersist=60s` |
| serve lease timeout | **10 s — unchanged** | hub, `leaseTimeout`, `internal/loop/lease.go:34 @ 1244ae4` |
| heartbeat / registration refresh | every tick (5 s) — unchanged cadence | hub session |
| sweep throttle | 10 min — unchanged | hub session (mirror of `serve.go:51`) |
| lease reclaim protection | stale ≥10 s **and** (hub-pid dead **or** the claim's recorded `boot_ref` differs from this host's current one, §6.9) | `lease.go:242-244` |

Why the lease numbers don't change: the hub session's own pid sits in
`serve.claim`, so a lane whose ticks are delayed but whose session process is
alive is protected by the same-host pid-proof — network jitter alone can never
lose a lease. Only a genuinely dead connection (session process gone) opens
reclaim, and the session's exit path releases the lease immediately anyway
(§6.4), making the 10 s window moot in the common case.

---

## 5. Identity & security

### 5.1 authorized_keys template (exact line)

One line per (key, agent id), written by `agentchute hub authorize` (§7.3):

```
restrict,command="/usr/local/bin/agentchute hub session --agent codex --pool /home/alex/code/agentchute --pool-id 9c4e12ab77f0" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI… agentchute:codex:9c4e12ab77f0
```

- `restrict` (verified `sshd(8)`): disables port/agent/X11 forwarding, PTY
  allocation, and `~/.ssh/rc` execution. Future-proof default-deny.
- `command="…"` (verified): runs for shell, exec, **and subsystem** requests —
  the client's requested command (`agentchute-hub`) is ignored and preserved
  in `SSH_ORIGINAL_COMMAND`.
- The binary path is absolute (resolved via `os.Executable()` at authorize
  time) — forced commands run with a minimal env; PATH must not be trusted.
- `--pool` is the hub pool's absolute control-repo path, baked in — no
  discovery ambiguity, no client influence over which pool is touched.
- **Shell-safety decision (one rule everywhere): reject, never encode.**
  The forced command runs through the hub user's login shell AND through
  authorized_keys' own quoted-string layer, and the auto-authorize path adds
  a third interpreter (the interactive ssh's remote shell, §7.2). Writing a
  correct encoder for that three-layer stack — spaces, `$()`, backticks,
  `;`, `'`, `"`, `\` — is exactly the kind of quoting machinery this project
  deletes. Instead every value is validated against a grammar that makes all
  three layers inert, or refused with a remediation:
  - pool path and binary path: absolute, matching `^[A-Za-z0-9._/+-]+$`
    (no whitespace, no quotes, no shell metacharacters, no backslash) —
    else the broadened `E_POOL_PATH_UNSAFE` refusal (§7.5);
  - agent id: the existing slug (`ValidateAgentID`,
    `internal/loop/inbox.go:494-504 @ 1244ae4`);
  - the public key (the one value that inherently contains spaces): composed
    of an exactly-validated key type (`^[a-z0-9-]+$`), base64 blob
    (`^[A-Za-z0-9+/=]+$`), and the marker comment (grammar below) — and it
    is the **only** value ever wrapped in single quotes on the interactive
    auto-authorize command line, where its charset is inert by construction.
  The rejected alternative — an opaque binding id in authorized_keys with an
  agent/pool mapping in a separate 0600 file — was considered and declined:
  it would reintroduce a second mapping store that can drift from the key
  line (§5.2's whole point is that the line IS the mapping), for a benefit
  (exotic paths with spaces) the refusal text already remediates ("move or
  symlink the pool to a plain path"). The full metacharacter set is tested
  as refusals (§10.3).
- The trailing comment `agentchute:<id>:<pool12>` is the machine-readable
  marker `hub authorize --list/--revoke/--replace-key` operates on.
  Per-(id, pool), not per-id (G-M2): one hub account may serve several
  control repos, and a per-id marker would make authorize/--list/--revoke
  collide across them. **`<pool12>` is anchored by a durable identity file
  INSIDE the pool** (F1 — one small file, superseding round-4's SameFile
  scan with something simpler and edit-proof): `hub authorize` reads
  `<pool>/.agentchute/loop/state/pool.id` — one line, the pool's `pool12` —
  and reuses it as the marker when present; only a pool with no `pool.id`
  yet mints a fresh `pool12 = hex(sha256(normalized))[:12]` (normalized =
  `EvalSymlinks(Clean(abs(--pool)))`, killing trailing-slash/symlink
  spellings) and writes the file — by **exclusive create** (`O_EXCL` /
  link-no-clobber, the codebase's existing first-writer-wins idiom,
  `loop.WriteRegistrationExclusive`, `internal/loop/registration.go:133-169
  @ 1244ae4`), never a plain atomic replace: two concurrent first
  authorizes reaching the pool through different spellings (case alias,
  bind mount) could otherwise both see "no file", mint different hashes,
  and each replace the other — one line instantly inconsistent (H2). The
  loser of the exclusive create re-reads and adopts the winner's value.
  **Validation contract (J1)** — pool.id is reused AND interpolated into
  the forced-command line, so it is never trusted on read: it must be
  exactly one regular, non-symlink, 0600 file whose entire content matches
  `^[0-9a-f]{12}\n$`, read through the bounded no-follow reader
  (`loop.ReadFileLimit` — `O_NOFOLLOW` + fstat-on-the-open-fd, the shipped
  idiom, `internal/loop/registration.go:34-48 @ 1244ae4`) with a 64-byte
  cap. Both consumers validate BEFORE any comparison or interpolation:
  `hub authorize` (the fresh-read path AND the H2 loser-re-read path) and
  `hub session` at startup. Anything else — oversized, symlinked, embedded
  newline/quote/`$()`/whitespace, wrong length — is refused with the named
  error `E_POOL_ID_INVALID` (§7.5): authorize writes NOTHING to
  authorized_keys, the session refuses before any op. Only a value that
  matched the 12-lowercase-hex grammar can ever reach the key line, which
  is inert in the authorized_keys, shell, and JSON layers alike — the
  reject-not-encode rule (above) applied to our own state file, not just
  operator input.
  `pool.id` is also **preserved non-runtime scaffold** against the shipped
  wipe (H1): `setup --reset --wipe-state` currently deletes every `state/`
  entry except `setup.json` and its post-wipe rescan flags any other
  survivor (`wipeStateCategory`, `internal/cli/setup_wipe.go:295-316
  @ 1244ae4`; `rescanWipeLeftovers`, `setup_wipe.go:519-536`) — M5 adds
  `pool.id` to the preserved list in the wipe plan, the rescan, and the
  dry-run/preserved output alike (spec'd in M2, §9.1), because wiping it
  would remint a new identity on the next authorize and silently invalidate
  every existing key binding for that pool. Because the file travels
  WITH the pool directory, every spelling that reaches the pool — symlink,
  macOS case alias, bind mount, even a wholesale pool move — reads the same
  identity with no filesystem-identity comparison needed at all. Path text
  is still never identity (the round-3/4 lesson stands; the file simply
  anchors identity more durably than any stat comparison — cf. the
  SameFile-over-path-strings precedent, `internal/cli/send.go:590-599
  @ 1244ae4`). `hub authorize` writes the marker AND bakes the same value
  into the forced command as `--pool-id` (template above). **The session
  never echoes argv** (F1 — a verbatim `--pool-id` echo would make an edit
  of `--pool` alone invisible): at startup `hub session` re-reads `pool.id`
  from the ACTUAL `--pool` it was handed and refuses with an
  `E_POOL_MISMATCH` error frame, before any op, when that value is absent
  or ≠ `--pool-id` — this is the **hub-emitted arm** of that code, a wire
  `error` frame registered in §4.4.2 alongside its client-emitted twin
  (F9/X1: one code, two emitters, two exact texts in §7.5), not a
  client-only condition; `hello-ok.pool12` always carries the value READ
  FROM THE POOL. The CLIENT still records pool/pool12 at join and compares
  thereafter (§4.3) — the second layer, catching a key line re-pointed
  *consistently* (`--pool` and `--pool-id` together) at some other
  validly-authorized pool. Duplicate-id detection (§7.5) stays keyed on
  `(id, pool12)`; all spellings of one pool collapse to one marker (§10.3:
  realpath / symlink / trailing-slash / macOS case-alias spellings, the
  `--pool`-only edit, duplicate refusal, `--list`, `--revoke`).
- `hub authorize` validates before writing: the `--pool` path must exist and
  contain `AGENTCHUTE.md` + `.agentchute/loop/` (a typo'd join URL dies
  loudly here, on the hub, before any key lands — §7.1), and the resolved
  binary path must be executable.
- `hub authorize` must run **as the SSH login user** — the account whose
  `~/.ssh/authorized_keys` sshd consults for the join URL's `user@`. On a
  macOS hub, Remote Login must be enabled for that user.
- `hub session` builds its `loop.Config` **directly** from `--pool`
  (`ControlRepo=<pool>`, `LoopDir=<pool>/.agentchute/loop`) and never calls
  the discovery cascade: under Debian-family sshd the forced command runs
  via the login shell with the hub user's rc files sourced, so a stray
  `AGENTCHUTE_CONTROL_REPO`/`AGENTCHUTE_LOOP_DIR` in the hub user's
  environment would otherwise outrank the pinned pool
  (`internal/loop/config.go:218-225,299-329 @ 1244ae4`).

### 5.2 Key→id mapping storage

**The authorized_keys line is the mapping** — single source of truth, no
side database, nothing to drift (subtract-default; also why the
opaque-binding-id alternative was declined, §5.1). `hub authorize` manages
lines exclusively by the `agentchute:<id>:<pool12>` marker (§5.1 — scoped
per (id, pool) so one hub account can serve several control repos, G-M2):
`--list` prints and health-checks them, `--revoke <id>` (resolving pool12
from the current pool) deletes them, and a second `authorize --agent <id>`
for the **same pool** with a *different* key is **refused** (duplicate-id
join protection, §8 row 12) unless `--replace-key` is given (the
moved-laptop case; also what `hub join --rotate-key` drives, §7.2). The same
id under a different pool is a different marker — no collision. The one
adjacent file, `state/pool.id` (§5.1, F1), is not a mapping store — it is
the pool's own name tag, living inside the pool the way `AGENTCHUTE.md`
already marks a control repo; key↔id↔pool mapping still lives only in the
authorized_keys lines.

### 5.3 `--as`/`--from` enforcement point

Two layers, then structural absence:

1. **Handshake** (the enforcement point): hub compares `hello.agent` to the
   forced command's `--agent`; mismatch → `E_IDENTITY`, close. Mismatches are
   *rejected, never silently rewritten* — a mis-set `AGENTCHUTE_AGENT_ID`
   must surface, not be masked.
2. **Structure**: past hello, no frame carries an actor field. `send` has no
   `from`; `check`/`ack`/`register`/`gate` have no `as`. The hub session
   builds one `op.Context{ActorID: <pinned id>}` at startup and passes it to
   every `internal/op` dispatch (§3 conventions — the actor is an explicit
   seam argument, never config or global state). There is no request a
   client can compose that acts as someone else — the B4 lesson applied to
   the wire (`internal/loop/seq.go:283-291 @ 1244ae4`).
3. Client-side, the CLI still resolves and validates identity exactly as
   today (`resolveAgentID`, `internal/cli/identity.go:13 @ 1244ae4`) and
   refuses locally before dialing when `--from`/`--as` disagrees with the
   joined identity for that hub (cheap fast-fail; the handshake remains the
   authority).

### 5.4 What a compromised remote key can and cannot do

**Can** (blast radius = exactly one lane):

- Act fully *as that one agent id*: send as it (to any recipient), read/claim/
  ack its inbox, hold its serve lease, refresh its registration, read the
  pool roster (`status` is read-only and pool-visible by design — same as any
  pool member today).
- Poison peers with malicious message *content* — the §15 prompt-injection
  posture (bodies are untrusted data) is unchanged and remains the control.

**Cannot**:

- Get a shell, run arbitrary commands, or allocate a PTY (forced command +
  `restrict` — the client's exec request is discarded).
- Forward ports / tunnel into the hub's network (`restrict` +
  client-side `ClearAllForwardings`).
- Act as any other agent id (§5.3), read another agent's `state/` or claim
  another agent's mail (no wire op exposes them; ops are actor-scoped).
- Tamper with pool state outside the protocol (no filesystem access is
  exposed — the vetoed SFTP surface does not exist).

**Honest boundary**: the session process runs as the hub's UNIX user, so this
containment is enforced by sshd's forced command plus the `agentchute`
binary's own dispatcher — a vulnerability in `hub session` parsing would be a
real escape. That is why the codec is deliberately dumb (NDJSON + fixed-length
trailers, no argv parsing, no shell anywhere hub-side). Compared to today's
baseline — where any process on the shared host can impersonate anyone by
writing files (AGENTCHUTE.md §15 cooperative trust) — per-key pinning is a
strict *strengthening*: remote sender identity becomes unforgeable at the
transport layer.

### 5.5 Threat table

| threat | control | residual |
|---|---|---|
| stolen laptop / leaked private key | one-lane blast radius (§5.4); `hub authorize --revoke <id>`; key files 0600 under `~/.agentchute/hub/…/keys/` | revoke applies at next auth (§8 row 10); operator kills a live session for immediate cut |
| MITM on first connect | `accept-new` TOFU into a per-hub known_hosts; documented out-of-band fingerprint check for the paranoid (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the hub) | first connect is TOFU by explicit choice — the seamlessness/security trade is documented, not hidden |
| MITM after first connect / hub swapped | changed host key → hard refusal `E_HOSTKEY_CHANGED` | none |
| malicious/compromised peer *content* | unchanged §15 posture: bodies are untrusted data; wrapper-enforced human confirmation for scope expansion | unchanged |
| client impersonating another agent | forced-command pinning + handshake + actor-free wire (§5.3) | hub-binary parsing bugs (mitigated: dumb codec, fuzz tests §10.2) |
| hub host compromise | out of scope — identical to today (the pool lives there) | full pool compromise, as today |
| replay of captured traffic | SSH transport (fresh session keys); no app-layer replay surface (ids are per-session, ops are not signed requests) | none |
| DoS: connection flooding by an authorized key | sshd's own `MaxStartups`/`MaxSessions`; one-shot session cap 10 min; per-request deadlines | an *authorized* key can busy the pool — same trust level as a pool member today |
| unauthorized network peer | no key → sshd refuses at auth; recommend tailnet/firewall so port 22 isn't public (§7.7) | standard sshd exposure, standard mitigation |

### 5.6 Host-key verification story (first-connect UX)

- `hub join` performs the first connect; `accept-new` records the hub key
  into `<hubdir>/known_hosts` and join prints the fingerprint it recorded:
  `hub key recorded: ED25519 SHA256:xxxx… (verify out-of-band if this pool
  crosses trust boundaries)`. No prompt, no hang — BatchMode-safe.
- Any later change → every command fails with `E_HOSTKEY_CHANGED` (§7.5) and
  the two-line remediation (confirm with the hub operator; then
  `agentchute hub join --reset-hostkey`). No silent re-trust, ever.

---

## 6. Lease & liveness

### 6.1 Acquisition over the channel

Remote `serve` startup order (derived from local `runWrapper`, with the
deliberate registration reordering noted at step 3 —
`internal/cli/serve.go:363-475 @ 1244ae4`, with the lease moved to the far
end of the channel):

1. Spawn the channel ssh child (§4.2); `hello`/`hello-ok` within 10 s.
2. `lease-acquire` → hub session calls `loop.AcquireServeLease`
   (`lease.go:150`) — link-no-clobber under `withAgentLock(id)`, stale-reclaim
   CAS, pid-proof — all on the hub, unchanged. `serve.claim` now records
   `{host: <hub>, pid: <session pid>, serve_token, …}`.
3. `register` (`RegisterReq{Vendor, Host: <remote hostname>, WorkingRepos,
   Announce:false}`; the channel dispatch injects its held serve token,
   §3.5) → the hub session executes `op.Register` **and caches the payload
   as its tick heartbeat template** (§3.4). Ordering note (C10): the LOCAL
   runner actually starts the child first and registers after
   (`runWrapper`: lease `serve.go:374`, child start `serve.go:384`,
   `registerRunner` `serve.go:393 @ 1244ae4`) — remote
   register-**before**-child is a deliberate strengthening (a remote lane is
   never a live child without a registration row), not parity; M1's
   zero-behavior-change tests assert the local order **unchanged**. A
   channel that skips this step gets `E_ORDER` on its first tick.
4. `lease-ok{token}` (from step 2) → serve exports
   `AGENTCHUTE_SERVE_TOKEN=<token>` into the child wrapper's env
   (`runnerChildEnv`, `serve.go:502-530`) per the §6.8 env contract, and only
   then starts the child. A fenced lane's own sends fail closed exactly as
   today.
5. On `E_LEASE_HELD`: refuse to start, naming the holder
   (`runner for codex is already active (serve lease held by hub pid 48122,
   fresh)`) — the same fail-closed refusal as `refuseLiveRunnerCollision`
   (`serve.go:571-580`).

### 6.2 Renewal — the tick

Every 5 s the serve loop sends `tick`; the hub session runs `RenewLease` +
`HeartbeatRegistration` + throttled sweep + inbox count (§3.4) and answers
`tick-ok{pending,skipped,swept,warnings}`. The counts feed the existing
wake-cue predicate unchanged (`pollOnce`'s re-cue logic,
`internal/cli/serve.go:659-668 @ 1244ae4`); `warnings` is re-logged verbatim
into the local runner log, so a hub-side heartbeat or sweep failure reads
exactly as it does on a local lane (§3.4). `E_FENCED` on a tick = this lane was reclaimed → client runs the
exact fence path that exists today (`serve.go:616-623`): buffer the fatal
message, `requestShutdown(SIGTERM)` the child, exit.

### 6.3 Fencing token flow

```
AcquireServeLease (hub)  ──lease-ok──▶  serve (remote)  ──env──▶  child wrapper
        │                                                            │
        │                                   one-shot send frame: serve_token
        ▼                                                            │
serve.claim on hub  ◀──VerifyFence (in-lock, floor.go:127 / seq.go:328)──┘
```

The token is minted hub-side, travels to the remote once, and returns inside
every fenced op. All verification happens hub-side inside the same
`withAgentLock` critical sections as today — the TOCTOU analysis in
`MintSendStamp`'s doc comment (`internal/loop/floor.go:91-104 @ 1244ae4`)
holds verbatim because none of that code moves.

### 6.4 Channel drop — both sides

**Client side** (any of: ssh child exits; tick response deadline (10 s)
expires; ServerAlive kills the transport):

1. Stop the poll/inject loops (`stopLoops`, `serve.go:955`).
2. Fence the child: `requestShutdown(SIGTERM)` (`serve.go:937-953`) — the
   wrapper gets SIGTERM, then SIGKILL after the existing 2.3 s escalation.
3. Then, by mode (§6.7): by default (remote lanes relaunch unless
   `--relaunch=false`), enter the supervised relaunch loop — fresh channel,
   fresh lease/token, fresh child, never the old child (its env token is
   dead, and env is immutable post-launch — fence-then-fresh-child is
   structural, not policy). With `--relaunch=false`: exit with
   `E_CHANNEL_LOST` (§7.5), whose text echoes the lane's own launch
   invocation. Manual relaunch always works, and §6.4-hub makes it
   immediate.

**Hub side** (any of: stdin EOF because sshd tore down; read deadline (20 s)
expires on a silent channel; SIGTERM/SIGHUP):

1. `ReleaseLease` (`lease.go:345`) — token-checked, so if the lane was
   *already* reclaimed this is a safe `ErrFenced` no-op that never deletes the
   new owner's claim.
2. Exit. Because release happens on **every** exit path, a relaunched serve
   acquires instantly instead of waiting out the 10 s staleness + pid-proof —
   and the early release is safe precisely because of the fence: a half-open
   client that still believes it holds the lease can only reach the pool
   through ops carrying the old token, which now fail `VerifyFence`
   (absent claim ⇒ `ErrFenced`, `lease.go:279-281 @ 1244ae4`).

Worst case (hub session wedged in read, connection half-open hub-side): the
20 s read deadline bounds it; a relaunch during that window sees
`E_LEASE_HELD` for ≤20 s (stale-but-pid-alive protection, `lease.go:242-244`),
then succeeds. Documented in §7.5's `E_LEASE_HELD` text.

### 6.5 Interaction with the runner, wake cues, and the injection re-check

The runner's PTY supervision is untouched: cue text, submit sequences,
idle/injection windows, recue interval (`serve.go:44,710-843 @ 1244ae4`) all
operate on local observations. Two substitutions:

- `hasPendingInboxMail` (`serve.go:757`) becomes "last tick's counts" —
  at most 5 s stale, same cadence as the local poll.
- `injectIfPending`'s immediately-before-injecting re-check (M4,
  `serve.go:740-750`) reads the **last tick's counts** (≤5 s stale) — there
  is no `poll` op (cut, §3.4): a dedicated wire round-trip bought at most
  5 s of freshness at the cost of a second channel producer. The
  consequence — a cue can fire into an inbox the agent drained within the
  last 5 s — is the fail-open direction the local listing-error path already
  chose (`serve.go:754-763 @ 1244ae4`): a spurious cue is acceptable, a
  suppressed real one is not, and the next tick corrects the count.
- **Single channel writer**: with `poll` gone, the tick loop is the ONLY
  goroutine that ever writes to the channel — the §4.3 serial-frames rule
  holds by construction (nothing else has a reference to the ssh child's
  stdin). Two goroutines interleaving bytes on one pipe would corrupt
  framing (`E_MALFORMED_FRAME`) and spuriously fence the child; the design
  now makes that structurally impossible rather than merely forbidden.

### 6.6 Guard latch stays local-cached (mandated; specified)

The guard latch **never crosses the wire**. In remote mode, `loop.Discover`
returns a Config whose `LoopDir` is the **local shadow loop dir**
(`~/.agentchute/hub/<hub-id>/.agentchute/loop/`, §6.8/§7.4), so every
existing latch accessor —
`cfg.GuardLatchPath` (`internal/loop/config.go:158 @ 1244ae4`),
`loop.SetGuardLatch`/`ReadGuardLatch`/`ClearGuardLatch`
(`internal/loop/guard.go:41,72,118 @ 1244ae4`), the PreToolUse evaluation
(`evaluateGuardDecision`, `internal/cli/guard.go:216 @ 1244ae4`), and the
`check`/`ack` self-denials (`internal/cli/check.go:101-105`,
`internal/cli/ack.go:105-109`) — reads and writes a local file with **zero
network I/O**, unchanged code. The session key remains the serve token
(`resolveGuardSession`, `internal/cli/guard.go:109 @ 1244ae4`), which the
remote serve exports as the local one does. Arming point (E1): the remote
CLI's event emitter sets the latch on the FIRST message event it receives —
redelivered residue included — **before** rendering it, the same
before-display discipline the local loop applies at all three of its arming
sites (`setLatch()` at `internal/cli/check.go:185,243,256 @ 1244ae4`;
redelivery, `--no-archive`, and claim respectively — §3.2) — never after the
wire op
returns, so a channel/one-shot disconnect mid-stream (mail already claimed
hub-side, partially displayed) still leaves the latch armed over the
residue. Same session key, same clear path.

Ordering in remote `turn-end` (mirrors the local ordered handler,
`turn_end.go:15-39 @ 1244ae4`): (0) **best-effort** wire `register` — the
step-0 registration self-repair, carrying the inherited serve token (§3.5);
a failure here (hub briefly unreachable, vendor unresolvable) is reported
and **does not abort** steps 1–3, exactly matching the local best-effort
contract (`turn_end.go:110-120 @ 1244ae4`: a repair failure must never
prevent committing this session's own claimed mail); (1) wire `ack` op
commits hub-side; (2) local `ClearGuardLatch`; (3) wire `gate` op for the
finish verdict. The latch clears only after the commit is confirmed; if the
hub is unreachable at step 1, `turn-end` fails, the latch stays armed, and
the claimed mail stays in the hub's `.claimed/` — the existing
wedge-then-redeliver semantics
(AGENTCHUTE.md §15 mixed-hook-trust recovery) apply with "hub unreachable" as
just another reason turn-end didn't complete. Note also: `ssh` is *not* on
the guard's deny list (deliberately cut in the livelock fix,
`internal/cli/guard.go:45-58 @ 1244ae4`), and the transport runs inside the
`agentchute` binary anyway — the guard inspects tool command text, so remote
transport adds no new guard interactions.

### 6.7 Supervised lane relaunch (`serve --relaunch`)

Laptops sleep and hubs reboot — both kill the channel in **daily normal
use**, and "a human retypes `ac serve` per lane per event" is not seamless.
The consensus veto targets resuming the SAME child across a drop (impossible
anyway, §6.4); a supervised **full-lane relaunch** is compatible and is
designed in, **default-on for remote lanes** (G-B3: lid-close is a daily
event, and an opt-in flag would leave the written quickstart path producing
dead lanes; a recovery default that only ever launches a *fresh* fenced-off
lane is safe to default):

- **Surface**: one boolean flag, `--relaunch` — **default true when
  `Remote != nil`**, `--relaunch=false` to opt out; passing it to a local
  serve is an error (local lanes have no channel). No `--relaunch-args` and
  no interactive prompt (both cut — C9/G-m3: an opt-out default needs no
  prompt, and a string flag reparsed into argv is exactly the shell-text
  re-parsing this design bans elsewhere).
- **Trigger set**: transport-loss only — ssh child exit, tick deadline,
  `E_CONNECT` during a relaunch attempt. `E_FENCED` gets exactly **one**
  relaunch attempt: that is the hub-update case (§8 row 24 — `update`
  invalidates every lease, `internal/loop/lease.go:371-376 @ 1244ae4`, and a
  fresh lane under the new binary is precisely what is wanted); if the
  attempt sees `E_LEASE_HELD`, a live rival owns the id — stop for real.
  Never relaunches on `E_IDENTITY` / `E_VERSION` / `E_POOL_MISMATCH` /
  `E_HOSTKEY_CHANGED` (config/security errors need a human).
- **Loop**: fence + stop the old child fully (§6.4 steps 1–2, unchanged),
  then repeat the §6.1 startup sequence — fresh channel, `hello`,
  `lease-acquire`, `register`, fresh token, fresh child process — with
  backoff 1 s, 2 s, 4 s … capped at 60 s, ±20 % jitter, unbounded attempts
  (the overnight-sleep case is the point), one status line per attempt to
  the terminal and the runner log.
- **Wrapper argv**: a relaunch starts the wrapper with its **original argv,
  verbatim** (`opts.WrapperArgs` held as `[]string` from the first launch,
  `internal/cli/serve.go:117 @ 1244ae4`) — argv-exact, no reparsing, no
  per-vendor resume args in v1 (cut with `--relaunch-args`; a vendor
  session-resume flag can arrive later as a repeatable `--relaunch-arg`
  once verified against that vendor's docs, if ever wanted).
- **Guard/lease interplay**: the fresh serve token is a new guard session
  key, so a latch armed by the dead session reads as foreign/inert
  (`internal/loop/guard.go:20-24 @ 1244ae4`) and claimed residue redelivers
  on the first `check` — the existing crash-recovery semantics, reached the
  existing way. Hub-side the dead session already released the lease (§6.4);
  after a hub **reboot**, the surviving claim becomes reclaimable once it is
  stale (≥10 s past its last renew — long past by the time sshd is back) via
  the boot-ref pid-proof (§6.9); the freshness check still runs first
  (C8), so "reclaimable" is never claimed for a fresh row.
- **Hub reboot, N lanes**: every remote serve (relaunch is the default)
  retries independently; when sshd returns, each lane relaunches within one
  backoff interval (≤60 s worst case) with zero human action on any machine.
  This is the designed recovery for §8 rows 22–24.

### 6.8 Child environment & discovery contract (normative)

The local runner exports `AGENTCHUTE_CONTROL_REPO=<cfg.ControlRepo>` and
`AGENTCHUTE_LOOP_DIR=<cfg.LoopDir>` verbatim to the child
(`internal/cli/serve.go:512-514 @ 1244ae4`), and env outranks the pointer
file in discovery (`internal/loop/config.go:218-225 @ 1244ae4`). Exporting
local paths from a REMOTE serve would therefore silently re-point every
child CLI call at a dead local/shadow pool — and letting Discover hard-error
instead would be worse: the PreToolUse guard fails **open** on any Discover
error (`internal/cli/guard.go:191-204 @ 1244ae4`), which would leave the
guard latch permanently inert on every remote lane. The contract:

1. Remote serve exports `AGENTCHUTE_CONTROL_REPO=<canonical ssh:// URL>` and
   exports **no** `AGENTCHUTE_LOOP_DIR` (stripping an inherited one — the
   same hygiene `runnerChildEnv` already applies to `AGENTCHUTE_GUARD`,
   `serve.go:502-511 @ 1244ae4`).
2. Discover's control-repo arms (flag, env, pointer) all branch on the
   `ssh://` prefix **before** `validateExplicitControlRepo`'s local-directory
   checks (`config.go:264-273 @ 1244ae4`). The ssh arm is pure local
   derivation — parse the URL, compute `<hub-id>`, return
   `Config{Remote: …, LoopDir: <shadow>, ControlRepo: <nearest ancestor of
   cwd containing AGENTCHUTE.md, else cwd — used only for local concerns
   like hook refresh, serve.go:159>}` — no network, so `guard` and every
   hook path resolve instantly and correctly even fully offline.
3. The shadow loop dir is `~/.agentchute/hub/<hub-id>/.agentchute/loop/` —
   note the literal `.agentchute` segment: the existing invariant that a
   loop dir is named `loop` under a dotdir parent (`vendorFromLoopDir`,
   `config.go:348-357 @ 1244ae4`) holds for the shadow too, so nothing that
   validates loop-dir shape needs a special case.
4. An explicit `AGENTCHUTE_LOOP_DIR`/`--loop-dir` combined with an `ssh://`
   control repo is a hard error — one authority for where local state lives.
5. **The launchers forward the URL, never the derived local paths (B4 —
   BLOCKER).** Rules 1–4 govern the child *wrapper*'s environment; they do
   NOT govern how `serve` itself is launched, and today both launcher paths
   re-exec `agentchute serve` with the DERIVED config rather than the
   locator that produced it: `buildDispatchRunArgs` emits
   `--control-repo <cfg.ControlRepo> --loop-dir <cfg.LoopDir>`
   (`internal/cli/dispatch.go:247-258 @ 1244ae4`, called at
   `dispatch.go:241`) and the legacy shim path emits the same pair
   (`cmdShimsExec`, `internal/cli/shims.go:300-310`, inside
   `shims.go:259-312 @ 1244ae4`). Note what that second launcher is: the
   single `ac` dispatcher is the only launcher installed today
   (`installDispatcher`, `shims.go:208-231 @ 1244ae4`), and install/setup
   **deletes** the generated per-wrapper `ac-*` shims that reach
   `cmdShimsExec` (`removeLegacyWrapperShims`, `shims.go:233-257`) — so that
   path is reachable only from a shim surviving an older install. It is live
   code carrying the same defect, so it stays in scope, and the §10.3 row has
   to CONSTRUCT such a shim to reach it. Under rule 2 a remote
   `Config` has `ControlRepo` = the LOCAL repo root and `LoopDir` = the
   shadow — both perfectly valid local paths. So the re-exec'd child hits
   the **flag arm** of the discovery cascade, which outranks the pointer
   (`internal/loop/config.go:209-216` precedes the pointer arm at
   `config.go:228-246 @ 1244ae4`) and passes `validateExplicitControlRepo`
   (`config.go:264-273`) because the local root really does contain
   `AGENTCHUTE.md`. Result: `Remote` is nil, the lane runs **LOCAL against
   the mail-free shadow** — no hub, no mail, no lease on the hub, and **no
   error anywhere**. Every `ac serve <wrapper>` and every surviving legacy
   `ac-*` shim in a joined checkout silently de-remotes itself. Normative fix, in both
   launchers: when the discovered config is remote, forward
   `--control-repo` with the canonical `ssh://` URL carried on
   `cfg.Remote` — not `cfg.ControlRepo` — and
   **omit `--loop-dir` entirely** — the child then re-derives the shadow
   through the same ssh arm its parent used, one authority, no second
   spelling. Forwarding the URL *and* the shadow would additionally trip
   rule 4's hard error, which is the same rule saying the same thing. A
   caller-supplied `--loop-dir` peeled off the dispatcher's global flags
   (`extractGlobalFlag`, `dispatch.go:220-221`) against a remote config is
   that same rule-4 refusal, raised in the dispatcher before the exec.
   `internal/cli/dispatch.go` and `internal/cli/shims.go` are therefore
   **in scope for M4** (§11), which is where the `ssh://` discovery arm
   lands: the arm and its launchers must ship together, because either one
   without the other produces a silently local lane.
6. Integration tests pin the contract (§10.3): a hook-context `guard` call
   in a joined checkout resolves the shadow latch with networking blocked; a
   child `send` under remote serve lands in the hub pool; and `ac serve` /
   a purpose-built legacy `ac-*` shim in a joined checkout produces a child
   whose discovered config has `Remote != nil` (the B4 row).

### 6.9 Lease pid-proof after a hub reboot (targeted `lease.go` change)

`serve.claim` survives a hub reboot, and reclaim requires stale ≥10 s
**and** a same-host pid-proof failure (`internal/loop/lease.go:236-244
@ 1244ae4`). After a reboot, OS pid reuse can make the recorded pid read
"alive" again — wedging the id behind `ErrLeaseHeld` until a human deletes
the claim file, with no self-heal. `lease.go` already documents pid-only
liveness as a known limitation ("vulnerable to OS PID REUSE",
`lease.go:236-241`). The fix is scoped to that one decision point and
flagged in the spec merge M2 (§9.1).

**Recorded boot reference, compared for EQUALITY — never wall-clock
ordering (B6).** An earlier draft of this section derived the host's boot
*time* and treated a claim whose `StartedAt` predates it as provably dead.
That rule is unsafe and is withdrawn: every portable boot-time source is
computed as `wall_now − uptime`, so a wall-clock step moves the derived
boot time by exactly the size of the step (`/proc/stat`'s `btime` is
recomputed from the stepped calendar; Darwin's `kern.boottime` is
re-anchored by the kernel on calendar adjustment for the same reason). A
laptop or hub that boots with a wrong RTC and is then stepped FORWARD by
NTP therefore lands in the worst case simultaneously: the live lane's
`LastSeen` suddenly reads hours stale (so branch (c)'s freshness refusal no
longer protects it), and its `StartedAt` suddenly reads pre-boot — so the
ordering rule would declare a LIVE lane dead and steal its lease, when
branch (d)'s `existing.Host == host && pidAlive(...)` (`lease.go:242-244
@ 1244ae4`) was the only thing still protecting it. Stealing a live lane's
lease is strictly worse than the wedge this section exists to fix.

The replacement records the boot reference **in the claim, at acquire**,
and compares refs for equality:

- **New field**: `ServeClaim` gains a `BootRef` string, JSON-tagged
  `boot_ref,omitempty` (the struct at `internal/loop/lease.go:55-62
  @ 1244ae4`), populated by `AcquireServeLease` when it builds the claim
  (`lease.go:159-167`).
- **Sources** (both are per-boot identifiers with **no wall-clock
  component**, so a clock step cannot move them):
  - Linux: the contents of `/proc/sys/kernel/random/boot_id` — a UUID the
    kernel mints at boot.
  - Darwin: the `kern.bootsessionuuid` sysctl — likewise a per-boot UUID
    (`golang.org/x/sys/unix.Sysctl`; `golang.org/x/sys` is already a
    direct requirement, `go.mod`, so this adds no module — or an exec'd
    `sysctl -n kern.bootsessionuuid`, same value). Deliberately **not**
    `kern.boottime`: that is the wall-clock-derived value the paragraph
    above rules out.
  - Anything else (other OS, unreadable file/sysctl, permission denied):
    the empty string.
  - **Both sources are HOST-scoped, and the scope of this fix is exactly
    that.** They change on a host **reboot** and on nothing else: a
    container or VM guest restart, a service restart, or a pid-namespace
    re-create on a host that did not reboot leaves the ref identical, so
    such a claim takes the unchanged stale+pid-alive refusal and behaves
    exactly as today — no better, no worse. This section fixes the
    post-reboot wedge; it does not solve pid reuse in general, and the
    §7.5 manual remediation remains the answer everywhere else. The M2
    spec text must carry this clause (§9.1).
- **Reclaim rule, branch (d) only**: the stale-claim pid-proof still
  refuses (`ErrLeaseHeld`) when `existing.Host == host && pidAlive(pid)`,
  EXCEPT when the claim carries a boot ref, this host reports one, and the
  two **differ** — in which case the recorded process belongs to a previous
  boot of this host, is provably dead whatever the recycled pid says, and
  the claim falls through to branch (e)'s reclaim CAS. Equality/inequality
  only; the refs are never ordered, compared for age, or parsed.
- **Absent ref ⇒ exactly today's behavior**, which is also the upgrade
  path: a claim written by a pre-upgrade binary has no `boot_ref`, so it
  takes the unchanged stale+pid-alive refusal and the §7.5 manual
  remediation. The same holds when the current host's ref is unreadable.
  No migration step, no fixup pass: the first `AcquireServeLease` under the
  new binary writes the field, and every claim is rewritten on every
  acquire anyway.

**Field preservation and mixed-version safety — verified, not assumed.**
`RenewLease` round-trips the claim through the `ServeClaim` struct
(`readClaim` → mutate `LastSeen` → `marshalClaim` → `atomicWriteFile`,
`lease.go:317-340 @ 1244ae4`), so a field that is part of the struct
survives every heartbeat, while a JSON key that is NOT part of the struct
would be silently dropped on the first renew. `boot_ref` must therefore be a real
struct field (it is, above) and must never be smuggled as an ad-hoc extra
key. In the other direction, `readClaim` uses a plain `json.Unmarshal`
with no `DisallowUnknownFields` (`lease.go:101-114 @ 1244ae4`), so an OLD
binary reading a NEW claim ignores `boot_ref` and behaves exactly as today
— an old and a new binary can share a pool through the upgrade window
without either failing closed. Note precisely what that tolerance is and is
not: an unknown key **parses**, it does not **persist** — an old binary that
renews a new claim re-marshals its own struct and drops `boot_ref` entirely.
That is safe by the absent-ref rule above (the claim simply reverts to
today's stale+pid-alive behavior) and self-heals at the next
`AcquireServeLease` under the new binary; it is also why the field must be
part of the struct rather than an ad-hoc key, and why §10.3's mixed-version
row asserts parse-tolerance and key-dropping separately, never "unknown keys
survive".

**Ordering (C8), unchanged and restated exactly**: `AcquireServeLease`
refuses a claim that is not yet stale BEFORE any pid or boot reasoning
(`lease.go:230-232` precedes the pid branch at `lease.go:242-244
@ 1244ae4`). The boot-ref corroboration lives strictly inside the
stale-claim pid-proof branch (d), under the same `withAgentLock(id)`. So a
pre-boot claim whose `LastSeen` still reads fresh (a sub-10 s reboot
window) is still refused until the 10 s staleness floor passes; nothing
here is "immediately" reclaimable, and the §10.3 fixture makes its
`LastSeen` explicitly STALE so the test exercises the branch that actually
changed.

Net effect on the clock-step case that killed the previous rule: the
stepped clock still makes the live lane's `LastSeen` read stale, so the
claim still reaches branch (d) — and there the pid is alive and the boot
refs are EQUAL (no reboot happened; a clock step is not a boot), so the
lease is refused to the would-be reclaimer and the live lane keeps it.
Protection is strictly stronger than today's and never weaker: boot_ref can
only ever turn a refusal into a reclaim when a *different boot* is proven,
never the reverse. This also fixes the same (rarer) wedge for local pools
after a host reboot; it is the only change this design makes inside
`internal/loop`.

---

## 7. End-user experience

### 7.1 Quickstart — hub operator

The hub is your existing pool. **Setup: zero commands.** In the common case
(§7.2) even authorization is driven from the joining machine and the hub
operator runs nothing at all. When a joiner lacks SSH access and sends you
the paste line instead, run it:

```
$ agentchute hub authorize --agent codex-tiny --pool /home/alex/code/agentchute \
    --key "ssh-ed25519 AAAAC3Nz… agentchute:codex-tiny"
authorized: codex-tiny -> pool /home/alex/code/agentchute (canonical; marker agentchute:codex-tiny:9c4e12ab77f0)
key ed25519 SHA256:Yk3n… — 1 line appended to ~/.ssh/authorized_keys
```

`authorize` refuses to write a bad line: the pool path must resolve to a
real pool (contains `AGENTCHUTE.md` + `.agentchute/loop/`) and the binary
path must be executable (§5.1) — a typo'd join URL dies here, loudly, not as
a remote's mystery failure later. It must be run as the SSH login user
(the account whose `~/.ssh/authorized_keys` sshd reads); on a macOS hub,
enable Remote Login for that user.

First-time hub prerequisites (almost always already true — three lines,
once per hub):

```
1. sshd is running and the joining user can SSH in  (macOS: enable Remote Login)
2. agentchute is installed at a stable absolute path (e.g. /usr/local/bin/agentchute)
3. that's it — the pool itself needs nothing
```

`hub authorize` also enforces the permissions sshd silently requires: it
chmods `~/.ssh/authorized_keys` to 0600 and `~/.ssh` to 0700 on write
(sshd's StrictModes *ignores* the file on group/world-writable paths with no
error to the joiner — the failure would otherwise surface only as a remote's
inexplicable `E_UNAUTHORIZED`), and `--list`/hub-side `doctor` FAIL on wrong
permissions. `agentchute hub authorize --list` prints every
`agentchute:`-marked line with a **PASS/FAIL health verdict** (binary exists
and is executable; pool resolves; perms correct) so a broken line is visible
on the hub, not just as a remote's `E_UNAUTHORIZED`. `--revoke <agent>`
removes that pool's line and — when that agent's
`serve.claim` in the pinned pool is fresh with a live pid — prints the
`kill <pid>` command for an immediate cut (sshd re-checks keys only at the
next authentication, §8 row 10).

### 7.2 Quickstart — joining machine

Prerequisites: `agentchute` installed (same installer as the hub) and a
working checkout of the repo cloned. The join URL's path is the pool's
**absolute path on the hub** — run `pwd` there if unsure.

```
$ cd ~/code/agentchute        # your working checkout of the same repo
$ agentchute hub join ssh://alex@hub.tail1234.ts.net/home/alex/code/agentchute --name codex
generated key: ~/.agentchute/hub/3fa8c21b90de/keys/codex-tiny_ed25519.v1
hub key recorded: ED25519 SHA256:xxxx…
authorizing via your own SSH access… ok
joined: codex-tiny (local name: codex) @ ssh://alex@hub.tail1234.ts.net/home/alex/code/agentchute (agentchute-hub v1, rtt 18ms)
wrote pointer: .agentchute-control-repo (ignored via .git/info/exclude)
$ agentchute serve codex      # local name — resolves to codex-tiny; `ac serve codex` after a new shell
```

**Naming: local names for the operator, hostname-suffixed pool ids
(Alex-directed, post-SHIP delta).** With `--as` omitted, join derives the
pool-wide id as `<local-name>-<hostname>`:

- `<local-name>` is `--name`'s value, canonicalized to the wrapper's agent
  id (`claude` → `claude-code` — the same canonicalization `ac serve`
  applies: `wrapperForToken`, `internal/cli/shims.go:51-64 @ 1244ae4`,
  called from `internal/cli/dispatch.go:132-137`); `<hostname>` is the
  first DNS label of the machine's `os.Hostname()`.
- **`--name` MUST name a known wrapper token; a non-wrapper `--name` is
  REFUSED at join, before keygen or authorize (S4).** The rule is forced by
  both launch paths, neither of which can be taught a local alias for a
  non-wrapper: `ac serve <token>` resolves the token through
  `wrapperForToken` and hard-refuses an unknown one before any identity
  work happens (`dispatch.go:132-137 @ 1244ae4`), and direct `agentchute
  serve <token>` treats the positional as the **wrapper command to exec**,
  not as a name (`opts.WrapperArgs = fs.Args()`, then
  `wrapperSpecForName(filepath.Base(WrapperArgs[0]))`, `serve.go:117-129
  @ 1244ae4`) — so `--name work` would mint `work-tiny`, record
  `names["work"]`, authorize a key for it, and then have no launch form at
  all: `ac serve work` says "unknown wrapper", and `agentchute serve work`
  tries to exec a binary called `work`. Accepting it produces an
  unlaunchable lane whose failure appears long after join reported success.
  The rejected alternative — teaching the dispatcher the `names` map —
  was declined: it would put a per-hub, per-machine map into the argv
  parser that runs *before* discovery, make `ac` behave differently in
  different directories, and still leave the direct-`serve` path broken,
  since there the positional is genuinely a command. Refusal text in §7.5;
  an arbitrary pool id remains available through `--as`, whose lanes launch
  with an explicit wrapper (`ac --as work-tiny serve claude`).
- Each component is sanitized to the id grammar `[a-z0-9][a-z0-9-]*`
  (`agentIDPattern`, `internal/loop/inbox.go:40 @ 1244ae4`, enforced by
  `ValidateAgentID`, `inbox.go:496-504`) — **exactly**: lowercase; every
  character outside `[a-z0-9-]` replaced with `-`; runs of `-` collapsed to
  one; leading and trailing `-` trimmed; an empty result is an error.
  (`Alexs-MacBook.local` → label `Alexs-MacBook` → `alexs-macbook`;
  `tiny` → `tiny` → id `codex-tiny`.)
- At **join time**, `--as` remains the explicit override, minted **verbatim**
  — never suffixed. (Runtime identity resolution is a separate rule, below:
  there, a value that happens to be a recorded local name maps to its
  joined id.)
- The mapping is minted **once, at join**, and recorded in `config.json`'s
  `names` map (`{"codex":"codex-tiny"}`, §7.4); a later hostname change
  never re-derives it — the recorded id wins.
- **Runtime resolution (the rule that actually has to survive the real
  launch path).** The real path is NOT "no id set": enrollment exports
  `AGENTCHUTE_AGENT_ID=codex` as standing practice, and the dispatcher
  injects `--as <wrapper-canonical>` when nothing else is set
  (`ensureDispatchIdentity`, `internal/cli/dispatch.go:269-278 @ 1244ae4`)
  — so a naive "explicit values win verbatim" rule would hello as `codex`
  against a key pinned to `codex-tiny` and hand every joined lane
  `E_IDENTITY` on its first launch. The rule, applied at the single
  identity choke point (`resolveAgentID`, `internal/cli/identity.go:13
  @ 1244ae4`) whenever the discovered config is remote: take the candidate
  id from `--as` / `AGENTCHUTE_AGENT_ID` / the wrapper default as today,
  then **if the candidate is a LOCAL NAME present in the `names` map,
  resolve it to the joined id; treat it as a pool id only when it is not
  in the map or already equals the joined id.** The lookup lives **inside
  `resolveAgentID` itself** (`identity.go:13-38`), which — verified — BOTH
  launch paths reach: direct `agentchute serve` calls it after discovery
  (`cmdServe`, `internal/cli/serve.go:135,145 @ 1244ae4` — a path that
  never touches the dispatcher), and `ac serve` merely injects `--as
  <canonical>` (`ensureDispatchIdentity`, `dispatch.go:269-278`) before
  exec-ing that same serve, so the injected value is resolved identically.
  The wrapper-default candidate needs one explicit wire-up on the DIRECT
  path (round 10 blocker): today `cmdServe` computes the wrapper's
  canonical id but keeps it only in `launchedWrapper`
  (`serve.go:122-129 @ 1244ae4`), and `resolveAgentIDRaw` has exactly two
  sources — flag, then env, then error (`identity.go:27-38`) — so a bare
  `agentchute serve codex` in a fresh shell with env unset (precisely the
  two-command quickstart above: `hub join` cannot export into its parent
  shell) would have NO candidate at all. Fix: direct `cmdServe` supplies
  `launchedWrapper` as the fallback candidate when both flag and env are
  absent; that candidate then resolves through the `names` map like any
  other.
  Every other consumer — the one-shot ops, hook invocations, the spool
  path — already resolves through the same function. One exception is
  named rather than assumed: `guard`'s hook evaluation resolves its id
  inline today (`evaluateGuardInvocation`, `internal/cli/guard.go:169-189
  @ 1244ae4`, deliberately not via `resolveAgentID`); M4 routes that
  lookup through the same map so the latch path can never disagree with
  the lane's resolved id. `doctor` and `hub join` warn when `AGENTCHUTE_AGENT_ID`
  is set and resolves to something other than what it names (the same
  pattern as the existing `AGENTCHUTE_CONTROL_REPO`-vs-pointer mismatch
  warning, §7.6). **No send-side peer aliasing, explicitly**: the map
  resolves SELF-identity on this machine only — `--to` is never mapped,
  and peers everywhere address the full `codex-tiny`.
- **`agentchute identity`** in a joined checkout lists this machine's
  local-name → joined-id map and what the current env/flags resolve to
  (a `config.json` read; no network, no new command — `identity` is
  already the id-resolution command).

Rationale (recorded per Alex's direction): the operator thinks in LOCAL
names — "codex on this laptop" — so the machine-qualified pool id should be
automatic, never typed; the hostname suffix makes pool-wide uniqueness
automatic as machines join; and it makes message traffic **human-legible**
— every inbox filename, roster row, and archive entry then carries the
origin machine in the sender slug
(`20260815T101503123456Z_from-codex-tiny_r….md`), so an operator reading
the pool sees who talks to whom, and from where, with no lookup.

Edge cases: two machines sharing a hostname mint the same auto id — the
duplicate-id refusal at authorize catches it, and its text now names the
hostname-collision case with the `--as` remedy (§7.5). An explicit `--as`
on one machine colliding with an auto-derived name elsewhere is the same
normal refusal. A hostname change after join changes nothing (minted once,
above).

**Local-name/pool-id shadowing is REFUSED at join, before keygen or
authorize (round 10).** The two namespaces share one spelling space on a
machine, and the runtime resolver gives local names precedence — so a pool
id that equals a recorded local name would be unselectable here (`--as
codex` would remap to `codex-tiny` forever). Both join orders are refused
with the existing mapping named and the way out:

- `names["codex"]="codex-tiny"` exists, then `hub join <url> --as codex`:
  `hub join: "codex" is already this machine's local name for
  "codex-tiny" — a pool id "codex" would be unselectable here (runtime
  --as codex resolves to codex-tiny). Pick a different --as, or use the
  existing codex lane.`
- joined as pool id `codex` (verbatim `--as`), then `hub join <url>
  --name codex` on the same machine:
  `hub join: this machine already joined as pool id "codex"; recording
  "codex" as a local name for "codex-tiny" would shadow it (runtime --as
  codex would stop selecting it). Pick a different --name, or --as an
  explicit id.`

**One command on the joining machine** in the common case: §7.1 already
presupposes the joining human can SSH to the hub, so `hub join` performs the
authorize step itself — an interactive
`ssh alex@hub.tail1234.ts.net agentchute hub authorize --agent codex-tiny
--pool /home/alex/code/agentchute --key '<pubkey>'` under the operator's
normal credentials (agent or password prompt allowed — this is a human-run
command, not BatchMode). Remote arguments are single-quoted per argument;
every value is already validated by grammars that exclude `'` (§5.1, §7.4).
When that ssh fails (no direct access; a different admin owns the hub), join
falls back to printing the complete ready-to-paste authorize line and exits
0; after the hub operator runs it, re-running `agentchute hub join`
(idempotent) completes and verifies the join. So: one command and zero
pastes normally; one paste when an admin sits in the middle.

Key handling is **normatively idempotent** (G-B1 — §7.3 recommends bare
re-runs, so a re-run must never invalidate the live key): if
`keys/<id>_ed25519` already exists, join **reuses it** — never a second
keygen, never a surprise `--replace-key`. Generation happens only when the
file is absent, with this exact argv (G-B2 — the default interactive
passphrase prompt would hang a scripted join, and a passphrase-protected key
would fail every BatchMode operation as `E_UNAUTHORIZED`):

```
ssh-keygen -q -t ed25519 -N "" -C agentchute:<id> -f ~/.agentchute/hub/<hub-id>/keys/<id>_ed25519.v1
```

followed by creating the active-pointer symlink `keys/<id>_ed25519 -> <id>_ed25519.v1`
(the stable path §4.2's `-i` uses; versioning + the symlink are the
rotation substrate, D4b/F3). A crash in the gap between those two steps is
covered by the version-state recovery's no-pointer branch (H3a, below):
a rerun NEVER re-runs `ssh-keygen` onto an existing versioned path —
`-q` still prompts "Overwrite?" on an existing file, which hangs a
scripted join — it adopts or retires the orphan instead.

(The key's own comment carries only the client-known `agentchute:<id>`; the
authoritative `agentchute:<id>:<pool12>` marker is computed and written by
`hub authorize` on the hub — the client never derives pool identity from
URL text, D3.)

Join additionally refuses a pre-existing passphrase-protected key (probe:
`ssh-keygen -y -P "" -f <key>` must succeed) with the remediation "remove
the passphrase or rotate". Tested: join-twice ⇒ same pubkey, exactly one
authorized_keys line (§10.3).

**Per-hub-dir mutual exclusion (G2).** `hub join`, `hub join --rotate-key`,
and the migration below all read-modify-write the same
`~/.agentchute/hub/<hub-id>/` tree (config, key versions, the active
symlink, the whole directory in migration's case), and two of them
interleaving would produce exactly the half-states the recovery narration
is supposed to be the LAST line of defense against — two concurrent joins
both minting `v<N+1>`, a rotate promoting a symlink a migration is copying,
a migration deleting a dir a rotate is writing into. All three therefore
run inside an exclusive advisory lock, taken before any probe and held to
the end of the run:

- **Lock path**: `~/.agentchute/hub/.locks/<hub-id>.lock` — a sibling
  directory, deliberately NOT a file inside `<hub-id>/`, because migration
  renames and deletes the very directory it is protecting; a lock file that
  travels (or vanishes) with its subject stops excluding anyone the moment
  it matters, while every waiter's `flock` on the unlinked inode keeps
  succeeding. `.locks/` is created 0700 on demand.
- **Idiom**: the shipped one, unchanged — `syscall.Flock(fd,
  LOCK_EX|LOCK_NB)` in a deadline-bounded poll loop, i.e. `withAgentLock`'s
  mechanism (`internal/loop/filelock_unix.go:46-67 @ 1244ae4`;
  `agentLockTimeout` 5 s at `filelock_unix.go:20`,
  `agentLockRetryInterval` 25 ms at `:23`), generalized to a
  caller-supplied lock path rather than an agent state dir. Same
  non-reentrancy discipline: one acquisition per run, never nested. The
  wait stays SHORT and deliberately does not cover a slow run: an
  interactive auto-authorize can sit on a password prompt for minutes, and
  a second operator should be told to come back, not blocked silently.
- **Timeout ⇒ a named refusal**, never a silent wait: `hub join: another
  agentchute hub join/rotate is already running for this hub (lock
  ~/.agentchute/hub/.locks/3fa8c21b90de.lock). Wait for it to finish and
  re-run.`
- **Migration holds TWO locks** (old hub-id and new hub-id). To make
  deadlock structurally impossible they are acquired in ascending
  lexicographic hub-id order, always — never "old then new".
- The lock is machine-local and says nothing about lanes: a live lane is
  still refused by the liveness check below, which is a separate, stronger
  gate.

**Same-hub URL migration (D4a).** A join whose URL hashes to a hub-id with
no local dir is NOT automatically a new hub — the same hub reached through a
different spelling (ssh-config alias, MagicDNS vs IP) must never mint a
second key and then die on the duplicate-id refusal. Before any keygen,
join: (1) connects and obtains the host-key fingerprint (available
pre-auth); (2) scans `~/.agentchute/hub/` for dirs whose recorded
fingerprint matches — considering **only** entries whose name is exactly 12
lowercase hex characters, so scratch (`<id>.partial`) and infrastructure
(`.locks/`) can never be mistaken for a joined hub (G1, G2); (3) for each
match, attempts a hello using that dir's
existing key for this `--as` id — if it succeeds and `hello-ok.pool12`
equals that dir's recorded pool12, this IS the same hub: join **migrates**.
**Migration REFUSES while any lane of the old hub dir is live (E2)** — a
running remote serve's child carries `AGENTCHUTE_CONTROL_REPO=<old URL>` in
its immutable environment (§6.8), so moving the state dir out from under it
would send every child op to `E_NOT_JOINED` and strand its latch/spool in a
dir nothing resolves anymore; no reconciliation can fix a live child whose
env cannot change, so the lane must relaunch regardless — refusing first
merely makes that explicit instead of letting it wedge. (The rejected
alternative — a durable old-hub-id alias dir kept until reconciliation —
would leave two names for one hub's latch/spool/cache/mux state, exactly
the double-source drift this design deletes everywhere else.) Liveness
check is local and uses the codebase's existing **runner attribution**
discipline, not a bare pid probe (F2 — a live NUMBER from a stale
`runner.json` can be an unrelated process after OS pid reuse): for each id
in the old dir's `joined_as`, read its shadow `runner.json`
(`loop.RunnerStatePath`, `internal/loop/config.go:149-151 @ 1244ae4`),
require the recorded host to be this host and the pid alive, then read the
process's command line and attribute it — the shape `stopSetupRunner` and
`scanWipeLiveSignals` already use (`internal/cli/setup_reset.go:196-224`,
`internal/cli/setup_wipe.go:553-570 @ 1244ae4`: state file binds pid→id,
cmdline proves the pool, ambiguity fails closed).

**The attribution predicate is REMOTE-SPECIFIC and must not reuse
`setupCommandMatchesPool` unchanged (B5).** That function's pool proof is
an EXACT filesystem-path match of argv's `--control-repo`/`--loop-dir`
against `cfg.ControlRepo`/`cfg.LoopDir`, via `setupPathsEquivalent`
(`internal/cli/setup_reset.go:308-344`, path comparison at
`setup_reset.go:402-415 @ 1244ae4`). In remote mode neither string can
match: the argv carries the canonical `ssh://` URL (§6.8 rule 5), the
config's `ControlRepo` is the local repo root, and its `LoopDir` is the
shadow — and `setupPathsEquivalent` applies `filepath` semantics that are
meaningless on a URL anyway. Reusing it verbatim would send EVERY live
lane, in the normal case, down the "alive but unattributed" branch: the
good refusal below (stop the lane, then re-run) would never fire, and every
ordinary migration would instead print the pid-reuse text with its
"remove runner.json" advice, which for a genuinely live lane is both wrong
and destructive. The predicate, stated in full — for old hub dir `H` with
recorded canonical URL `U` (from `H/config.json`) and shadow loop dir `S`:

1. **Subcommand match, unchanged**: the cmdline must read as an
   `agentchute serve` (including the pre-v0.9.1 `agentchute run` alias
   kept for upgrade cleanup, `setup_reset.go:315-329 @ 1244ae4`). Anything
   else — any non-agentchute process — is NOT this lane; the pid is alive
   and unexplained ⇒ **ambiguous**.
2. **Pool proof**, on the argv values extracted by `setupCommandFlagValue`
   (which already stops at `--`, so a wrapper's own flags can never be
   mistaken for ours, `setup_reset.go:352-369 @ 1244ae4`):
   - argv has `--control-repo`: canonicalize it with §7.4's **URL**
     canonicalizer (lowercase host, port elided at 22, no trailing slash)
     and require equality with `U` ⇒ **attributed**. A different `ssh://`
     URL, or any local path, is a serve for some other pool ⇒
     **ambiguous** (a recycled pid over a stale `runner.json`).
   - else argv has `--loop-dir`: require `setupPathsEquivalent` against
     `S` ⇒ **attributed**; a mismatch ⇒ **ambiguous**. This arm exists
     only for lanes launched by a pre-§6.8 binary or by hand — after §6.8
     rule 5 no launcher emits `--loop-dir` for a remote lane at all.
   - else (**neither flag** — the direct `agentchute serve <wrapper>` form
     the quickstart itself prints, which resolves through the pointer file
     and therefore carries no locator on its argv) ⇒ **attributed**. The
     pid→(id, hub) binding here is the *location* of the `runner.json`
     that named this pid: it lives under `H`'s own shadow state dir, and
     nothing but a serve for `H` writes there. The residual this arm
     accepts is a recycled pid belonging to a DIFFERENT flagless
     `agentchute serve`; the consequence is that migration refuses when it
     could have proceeded, which is the fail-closed direction this whole
     check is built to prefer. It is deliberately not folded into
     "ambiguous", because in the normal case it is not ambiguous at all —
     it is the ordinary live lane, and it deserves the ordinary message.

Note the interlock with §6.8 rule 5: the URL arm only works because the
launchers forward the URL. The two changes ship together (M4/M5) and a
review of either must check the other. Three outcomes:

- **Attributed live lane** ⇒ refuse:
  `hub join: lane "codex" is still running against the old URL (serve pid
  41210). Stop that session first (Ctrl-C in its terminal, or end the
  serve process from its own supervisor), then re-run this join; the lane
  relaunches under the new URL afterwards.` — never a `kill` command.
- **Pid alive but cmdline does NOT attribute** ⇒ fail closed, ambiguous:
  `hub join: pid 41210 is alive but is recorded as lane "codex"'s runner
  while its command line does not match this hub — possibly OS pid reuse
  over a stale runner.json. Refusing to migrate. Inspect with ps -p 41210;
  if it is unrelated, remove <old-hubdir>/.agentchute/loop/state/codex/
  runner.json and re-run this join.` — inspection guidance only, no kill
  suggestion for a process that may be anything.
- **Pid dead or no runner.json** ⇒ no live lane; proceed.

With no live lane, migration proceeds. **The completion boundary is a
single `rename()`, not "durably complete" (G1).** An earlier draft said a
crash mid-migration "is healed by re-running join, which finds either dir
usable" — but a half-copied `<new-id>/` is *not* usable, and worse, it
matches the fingerprint scan exactly as well as the intact old dir does, so
a re-run cannot tell which one is authoritative. The sequence is therefore
pinned, and no step is optional:

1. Copy key material + config (latch/spool residue included) into
   `~/.agentchute/hub/<new-id>.partial/`. `mux/` is deliberately NOT
   copied — a ControlPath socket is bound to a path that no longer exists
   after the move.
2. Write the new `config.json` (recorded URL updated, `joined_as`/`names`/
   `pool`/`pool12` carried over) inside `.partial`, then fsync every
   written file and fsync the `.partial` directory itself.
3. `rename("<new-id>.partial", "<new-id>")` — **this is the commit
   point**, atomic on every supported filesystem. Fsync the parent
   `~/.agentchute/hub/` afterwards.
4. Rewrite the checkout's pointer file to the new URL.
5. Delete the old hub dir.

**The fingerprint scan only ever considers directories whose name is
exactly 12 lowercase hex characters**, so `<new-id>.partial` (and
`.locks/`) is never a scan candidate and can never be mistaken for a
joined hub. Recovery is then deterministic at every crash point: before
step 3 the only complete dir is the old one, and the leftover `.partial`
is removed and redone from scratch on the next run (it is scratch, never
adopted); after step 3 both dirs are complete and consistent, and the
next run — which resolves the *new* URL to `<new-id>`, finds a complete
config there recording that URL, and so never reaches the fingerprint scan
at all — takes the ordinary joined-verify path, which sweeps for exactly
this leftover (a sibling hub dir with the SAME recorded host-key
fingerprint but a stale recorded URL) and finishes steps 4–5. Migration is
thus idempotent from any interruption, with exactly one authoritative dir
at every instant.

**Mux masters outlive the old dir by design; reap them (G4).** A one-shot
op's `ControlPersist=60s` master (§4.2) can still be running against
`<old-id>/mux/%C` when the dir is deleted. Step 5 first sends a
best-effort `ssh -O exit` for the old URL (one call, failure ignored); an
unreaped master is harmless — it holds an unlinked socket inode, exits at
its own ControlPersist deadline, and no new one-shot can ever attach to it
because the ControlPath it would look for no longer exists.

A fingerprint match with a different pool12 is a different pool on
the same host — a normal fresh join (G-M2 markers keep them apart). No
match ⇒ genuinely new hub ⇒ fresh join. Tested: same-hub-alias rejoin ⇒
zero new keys, one authorized_keys line; alias rejoin under a LIVE latched
lane ⇒ refused, state untouched, and after stopping the lane the rejoin
migrates with latch/spool residue intact (§10.3).

**Staged key rotation (D4b/F3): versioned keypairs + one atomic active
pointer.** Keys are versioned files — `keys/<id>_ed25519.v<N>` (+`.pub`) —
and `keys/<id>_ed25519` is a SYMLINK to the active version. That symlink is
the stable path §4.2's `-i` already names (ssh follows it), so no
invocation changes; and every local state transition below is ONE atomic
step, so no rename boundary needs its own recovery narration (round-4's
active→prev / staged→active / marker-rewrite triple had crash interleavings
that contradicted its own recovery rule — F3). There is no `rotate.json`
either: **a version file newer than the symlink's target IS the staged
state** — the filesystem is the marker.

**Version-file grammar (G3) — normative, because "newer" and "the current
pubkey" are both load-bearing:**

- **`.v<N>` is parsed as a NUMBER, and ordering is numeric.** `<N>` must
  match `^[1-9][0-9]*$`; comparison is by integer value, so `v10` is newer
  than `v2`. (String ordering would silently promote `v2` over `v10` on
  the eleventh rotation of a long-lived lane — a stale key adopted as
  "newest", with the real active key pruned as "residue".) Minting always
  uses `max(existing N) + 1`.
- **A non-numeric suffix is NOT a version file and is never guessed at.**
  Any `keys/<id>_ed25519.v<S>` whose `<S>` fails that pattern (`v01`,
  `v2b`, `vold`, `v`) is excluded from the version scan and is neither
  adopted as newest nor pruned as residue; join/rotate refuses the run and
  names the file — `hub join: keys/codex-tiny_ed25519.vold is not a valid
  key version (expected .v<N>, N a positive decimal integer). Move it out
  of the keys directory and re-run.` Silently deleting an operator's
  hand-made file, or silently trusting it as a key, are both worse than
  stopping.
- **The scan's other exclusions**: `*.pub` (the public half of a version)
  and `*.invalid.*` (retired files, below) are never version candidates.
- **`.invalid.<ts>` is `<original-file-name>.invalid.<stamp>`**, where
  `<stamp>` is the codebase's existing UTC filename stamp — `YYYYMMDDT`
  `HHMMSS` + 6 digits of microseconds + `Z`, e.g.
  `codex-tiny_ed25519.v3.invalid.20260815T101503123456Z` — the same
  spelling message filenames use (`tsStampLayout`,
  `internal/loop/tsid.go:22,51 @ 1244ae4`). Microsecond resolution makes
  repeated retirements within one second collide-free; the `.pub` half is
  retired alongside its private half, under the same stamp. Retired files
  are inert forever: nothing scans them, nothing prunes them, and the
  operator can delete them at leisure.
- **There is deliberately NO `.pub` symlink; the current pubkey is
  DERIVED.** `keys/<id>_ed25519` is the one and only pointer, and the
  public half of whatever it names is that target's path plus `.pub`:
  resolve the symlink (`readlink`) → `<id>_ed25519.v<N>` → the current
  pubkey is `keys/<id>_ed25519.v<N>.pub` (which `ssh-keygen -f` wrote next
  to the private key by construction). This is what keeps promotion ONE
  atomic step — a second symlink would need a second `rename()` and
  reintroduce exactly the multi-boundary crash interleavings F3 deleted —
  and it makes a `.pub` that disagrees with the active key structurally
  impossible. Both paste lines that need a "current pubkey" resolve it
  this way: `E_UNAUTHORIZED` (§7.5) prints the pubkey of the **active**
  target, and the rotation-recovery branches print the pubkey of the
  version that branch selected (the STAGED `v<N+1>.pub` while resuming a
  rotation, the ACTIVE `v<N>.pub` in the post-promotion residue branch).

`hub join --rotate-key`:

1. Mint `v<N+1>` directly at its versioned path (keygen, then dir fsync).
2. Remote replace: `hub authorize --replace-key` with the v<N+1> pubkey —
   **only** via the operator's own unrestricted SSH access (interactive
   auto-authorize) or the printed paste line, the same two paths as join's
   authorize. Never over the agent's pinned key: its forced command can
   only ever run `hub session` (§5.1), so "authorize over the old key" is
   structurally impossible (E3). Idempotent.
3. Promote: swap the symlink to v<N+1> — write a temp symlink, one
   `rename()` over the active path.
4. Verify: hello with the (new) active key.
5. Cleanup: delete the v<N> files — retained until this point.

Recovery (E3/F3/H3 — executable from ANY crash point without assuming a
credential still works). Every join/rotate run classifies the version state
and handles all three abnormal shapes, in this order:

- **Version files with no pointer symlink (H3a — the first-join gap).**
  Probe the newest version with the passphrase-free check
  (`ssh-keygen -y -P ""`); if it passes, ADOPT it: create the initial
  symlink to it and continue the normal flow (the key may or may not be
  authorized yet — the ordinary join probe decides what happens next). If
  the probe fails (corrupt or passphrase-protected), move the file aside
  (`.invalid.<ts>` rename) and mint fresh. `ssh-keygen` is never re-run
  onto an existing path.
- **A version NEWER than the symlink's target (staged rotation).** Probe
  hello with that staged key FIRST. Success proves the remote replacement
  landed — promote (if not already) then fall through to the residue
  branch below, never re-driving step 2 over a now-revoked key. Staged-key
  failure: if the active key still answers hello, resume at step 2 through
  the operator path; if neither answers, print the complete paste line
  with the staged pubkey for the hub operator — no self-rotation op exists
  or is needed.
- **Older versions beside the active target (H3b — post-promotion
  residue, otherwise indistinguishable from a completed rotation).**
  VERIFY first, prune second: probe hello with the ACTIVE key; only on
  success delete the older version files. On ANY failure, prune nothing —
  the older versions stay as evidence and fallback — and the response then
  splits by error class (J2): **only the auth-refusal class
  (`E_UNAUTHORIZED`) means the key itself needs authorizing**, so only
  there does recovery print the paste line for the ACTIVE pubkey
  (finishing the intended rotation — late revoke or half-landed replace
  both converge on it). Every other failure class says nothing about the
  key and gets its own existing §7.5 remediation, propagated unchanged:
  `E_CONNECT` (hub down — retry later; pruning simply waits for a later
  successful verify), `E_HOSTKEY_CHANGED` (security stop — never overlaid
  with rotation advice), `E_VERSION`, `E_IDENTITY`, `E_POOL_MISMATCH`,
  `E_HUB_NO_BINARY` likewise. A reauthorize hint on a mere network failure
  would send an operator to the hub to "fix" a key that was never broken.

A leftover temp symlink (crash mid-step-3, before the rename) is inert and
removed on the next run; the real symlink at that point still names v<N>,
so the staged branch applies unchanged. Tested with failure injection at
EVERY transition — between first keygen and the initial symlink (orphan
adoption; and the probe-fail retire path), after mint, before and after
the remote replace, mid-promote (temp symlink present, rename not yet),
after promote before verify (asserting verify-then-prune runs and old
versions survive an active-key hello failure), after verify before cleanup
(asserting the residue branch completes the prune) — the post-replace
cases asserting recovery succeeds with the old key already revoked
(§10.3).

Join also **proves the lane can actually work** before declaring success
(G-M5): the verification hello checks `hello-ok.writable` (§3.6), and a
false verdict fails the join naming the three things to look at —
`joined but the hub session cannot WRITE the pool: check that user "alex"
owns /home/alex/code/agentchute (ls -ld), that the .agentchute/loop tree is
writable by it, and that the pool was not authorized against another user's
checkout.` — instead of letting the first real `send` die as `E_HUB_IO`.

Join is **probe-before-pointer** (§7.4): a typo'd URL never poisons the
checkout. Re-running join with a different URL first runs the same-hub
migration check above; only a URL that probes as a genuinely different hub
replaces the pointer as a fresh join, printing
`pointer replaced: <old> -> <new>`. Un-join = delete
`.agentchute-control-repo` (and, whenever no lane still uses that hub, the
`~/.agentchute/hub/<hub-id>/` dir can be removed too).

`hub join` also installs the `ac` dispatcher/shims if absent (reusing
setup's shim machinery; the PATH change needs a new shell, which join
reminds you of), so the joining machine never needs `agentchute setup`;
wrapper hooks are refreshed by `serve` at launch as today
(`internal/cli/serve.go:159 @ 1244ae4`). After join, **every existing
command works unchanged** — `send`, `check`, `ack`, `boot`, `status`,
`gate`, `pending`, `doctor` — the remoteness lives entirely in discovery.

Incremental adoption is trivially safe: a fresh clone contains a tracked
`.agentchute/loop/agents/README.md`, so the cwd-walk would find a
plausible-looking empty local pool — but the pointer file outranks the cwd
walk in the discovery cascade (flag > env > pointer > walk,
`internal/loop/config.go:208-258 @ 1244ae4`), so a joined checkout always
resolves to the hub. One thing DOES outrank the pointer: a set
`AGENTCHUTE_CONTROL_REPO` env var (`config.go:218-225`) — env leakage has
bitten this project before, so `hub join` and `doctor` both warn loudly
whenever that variable is set and disagrees with the pointer.

### 7.3 New command surface (subtract-default audit)

**One new command family (three subcommands), one new `serve` flag, zero
new flags on any other existing command:**

| addition | justification |
|---|---|
| `agentchute hub join <url> (--name <local> \| --as <id>)` (flags: `--reset-hostkey`, `--rotate-key`) | the entire remote-side join: keygen (idempotent — an existing key is reused; rotation only via the explicit `--rotate-key`, §7.2), auto-authorize, probe, pointer, shims. `--name` mints the hostname-suffixed pool id (§7.2 naming) and must be a known wrapper token (S4 — it is also the launch name); `--as` is the verbatim override. A bare re-run verifies, so a separate `--verify` flag was cut as redundant. Folding join into `setup` would entangle pool-mutating setup with machine-local join and double both help texts. |
| `agentchute hub authorize --agent <id> --pool <abs> --key "<pubkey>"` (flags: `--list`, `--revoke <id>`, `--replace-key`) | the entire hub-side story; writes and validates the forced-command line so no human ever hand-edits authorized_keys grammar; `--list` doubles as the hub-side health audit (§7.1). |
| `agentchute hub session --agent <id> --pool <path>` | the forced-command entry point. Never typed by a human; exists because a forced command must be a real command. Hidden from `ac` help. |
| `agentchute serve --relaunch` (boolean; **default true for remote lanes**, `--relaunch=false` to opt out; error on local lanes) | supervised full-lane relaunch (§6.7) — removes the one recurring manual-recovery event (laptop sleep, hub reboot/update) this design would otherwise create daily. `--relaunch-args` was cut from v1 (C9/G-m3). |
| `ssh://` form of the existing control-repo locator (pointer file / `AGENTCHUTE_CONTROL_REPO` / `--control-repo`) | a grammar extension to an existing knob, not a new knob. |

Explicitly **not** added: `hub ping` (folded into `doctor` and a bare
re-join), `hub status` (plain `status` works), any per-command
`--remote`/`--hub` flags (remoteness is discovered, never spelled per-call),
any hub-side daemon (sshd is the daemon), any config beyond the pointer +
`config.json`. Subtractions delivered alongside: joining machines no longer
run `setup` at all, and the spec merge M2 (§9) retires the network-mounted-loop-dir
guidance as the multi-host story in favor of the hub.

### 7.4 Config / pointer-file format

Pointer file (`.agentchute-control-repo`, existing discovery step 3,
`internal/loop/pointer.go:14 @ 1244ae4`) — one line, now also accepting:

```
ssh://[user@]host[:port]/absolute/path/to/control-repo
```

Grammar: `user`,`host` match `[A-Za-z0-9._-]+` and must not start with `-`
(argv-option-injection guard); `host` may be a `~/.ssh/config` Host alias
(ProxyJump etc. work for free); `port` 1–65535, omitted = ssh default; path
absolute. The same form is accepted by `AGENTCHUTE_CONTROL_REPO` and
`--control-repo`. Canonical form for hashing: lowercase host, port elided when
22, no trailing slash; `hub-id = hex(sha256(canonical))[:12]`.

Local per-hub state (never in the repo, survives multiple checkouts):

```
~/.agentchute/hub/.locks/<hub-id>.lock     join/rotate/migrate exclusion (§7.2, G2)
                                           — a SIBLING of the dirs it guards,
                                           since migration renames/deletes them
~/.agentchute/hub/<hub-id>/
  config.json          {"url":"ssh://alex@hub…/home/alex/code/agentchute",
                        "joined_as":["codex-tiny"],"names":{"codex":"codex-tiny"},
                        "pool":"/home/alex/code/agentchute","pool12":"9c4e12ab77f0"}
                       (pool identity RECORDED from hello-ok at join — hub-side
                        canonical, never derived from URL text; §4.3, D3)
  known_hosts          per-hub TOFU store (§5.6)
  keys/<agent>_ed25519.v<N>[.pub]   versioned keypairs (D4b/F3; N numeric, §7.2 G3)
  keys/<agent>_ed25519              SYMLINK -> the active version (the -i path;
                                    the ONLY pointer — the current pubkey is
                                    derived as <target>.pub, never a 2nd symlink)
  mux/                 ControlPath sockets (%C) — the PREFERRED location; a
                       path too long for sun_path falls back to an owned 0700
                       per-user temp dir, else mux is disabled (§4.2 rule)
  spool/               preserved send bodies (§4.5.3) — deliberately OUTSIDE
                       the shadow loop tree so the --body-file retry command
                       is never refused by the state-tree guard
                       (send.go:600-633)
  .agentchute/loop/    LOCAL SHADOW loop dir: state/<agent>/{guard.latch,
                       runner.json, runner.log} — dotdir parent kept so the
                       loop-dir shape invariant holds (§6.8) — never mail
```

Remote-mode `Discover` follows the §6.8 contract: every control-repo arm
branches on the `ssh://` prefix and returns `Config{Remote:
&RemoteConfig{…}, LoopDir: <shadow>, ControlRepo: <local repo root>}` by
pure local derivation.
Everything that reads local state through `cfg.AgentStateDir`
(`internal/loop/config.go:142 @ 1244ae4`) — guard latch, runner state —
lands in the shadow automatically, with no per-callsite changes.

**Who picks the transport — the dependency direction, pinned (B2).** An
earlier draft said "`internal/op` picks the transport on `Remote != nil`",
which cannot be built: `internal/op` owns the request/response structs, the
wire codec package must import them to marshal them, and the ssh client
must import both — so an `op` that reached back for a transport would close
the cycle `op → hubclient → hubwire → op`. The direction is one-way and
normative:

```
internal/loop  ←  internal/op  ←  internal/hubwire  ←  internal/hubclient  ←  internal/cli
```

- `internal/op` is a **leaf over `internal/loop`**. It executes operations
  against a local filesystem pool and knows nothing about transports,
  sessions, sockets, or `Remote`. It MUST NOT import `hubwire`,
  `hubclient`, or `internal/cli` (§7.4's companion rule to B1's
  `op`-must-not-import-`cli`), and a dependency-direction test asserts it.
- `internal/hubwire` imports `internal/op` for the payload structs (the
  seam structs ARE the wire schema, §3) and adds framing, codes, and the
  handshake.
- `internal/hubclient` imports both, and owns the `ssh` invocation, the
  channel, and the one-shot sessions.
- **`internal/cli` selects.** It is the only layer that has both the
  discovered `Config` and every implementation in scope: on `cfg.Remote ==
  nil` it calls `internal/op` in-process; otherwise it drives
  `internal/hubclient`. Equivalently — and this is the permitted
  refinement, not a different design — `internal/op` may DECLARE a
  `Transport` interface over its own request/response types, with the
  in-process implementation living in `op` and the ssh implementation in
  `hubclient`; the CLI still constructs and injects it. Either way the
  arrow from an implementation to `op` points inward, never out.
- `hub session` (hub-side) sits beside the CLI in the same direction: it
  imports `hubwire` + `op` and dispatches frames to seam calls.

Pointer lifecycle (all normative):

- **Probe before pointer.** `hub join` writes `.agentchute-control-repo`
  only after a probe proves the hub is real: a successful hello, or an SSH
  auth refusal (which proves host + sshd + reachability; only the key is
  pending). Connect/DNS/host-key failures write **nothing** — a typo'd URL
  never poisons the checkout.
- **Never committable by accident.** Join appends the pointer filename to
  `.git/info/exclude` (machine-local; no repo file is mutated, so nothing
  shows up in `git status` or a PR). If the pointer is already *tracked* —
  someone committed one previously — join warns loudly instead of hiding it.
- **Unjoined machines refuse cleanly — including the hub itself (C3).** An
  earlier draft refused whenever the URL's *path* resolved to a local pool;
  that falsely rejects the common valid case of a laptop and hub sharing the
  same absolute layout (same username, same checkout path), and no
  offline-safe host-identity proof exists (DNS in the discovery path would
  break the §6.8 guard contract). The refusal is instead keyed on **join
  state**, which is purely local and machine-specific: when the `ssh://` arm
  activates but `~/.agentchute/hub/<hub-id>/config.json` does not exist —
  true on a hub (or any third machine) that pulled a committed pointer, and
  never true on a machine that actually joined — discovery refuses with
  `E_NOT_JOINED` (§7.5), whose text covers both "this is the hub; delete the
  committed pointer" and "this machine should join; run hub join". A
  genuinely-joined machine with an identical path is untouched (its config
  exists), and the guard cannot be wedged by this: an unjoined machine has
  no serve session, so `resolveGuardSession` is already "" there
  (`internal/cli/guard.go:109-115 @ 1244ae4`). Both cases are tested
  (§10.3): identical-paths-two-hosts joins and operates; a true self-URL
  committed-pointer pull gets `E_NOT_JOINED`.
- **Env still wins.** A set `AGENTCHUTE_CONTROL_REPO` outranks the pointer
  (`config.go:218-225 @ 1244ae4`); `hub join` and `doctor` warn when it is
  set and disagrees (§7.2, §7.6).

### 7.5 Error-message catalog (exact text)

Transport / client-side:

| code | exact message |
|---|---|
| `E_CONNECT` | `hub: cannot reach hub.tail1234.ts.net:22 (connect failed after 5s). Check network/VPN/tailnet, then retry; `agentchute doctor` runs this same probe. (If this machine should no longer be joined to this hub, delete .agentchute-control-repo.)` |
| `E_UNAUTHORIZED` | `hub: hub refused this key for alex@hub.tail1234.ts.net. Either it was never authorized or it was revoked. Run this ON THE HUB, then retry here:`<br>`  agentchute hub authorize --agent codex --pool /home/alex/code/agentchute --key "ssh-ed25519 AAAAC3Nz… agentchute:codex"`<br>(the client composes the complete ready-to-paste line from its own pubkey and the joined URL; authorize canonicalizes the pool and writes the authoritative `agentchute:codex:<pool12>` marker itself, D3) |
| `E_HOSTKEY_CHANGED` | `hub: HOST KEY CHANGED for hub.tail1234.ts.net — refusing to connect. If the hub was reinstalled, confirm with its operator, then run: agentchute hub join --reset-hostkey. If not, treat this as a possible interception.` |
| `E_HUB_NO_BINARY` | `hub: connected, but the hub could not run agentchute (remote exit 127 — command not found at /usr/local/bin/agentchute). Reinstall agentchute on the hub, or re-authorize this key so its line points at the current binary path.` (mapped immediately from the ssh child's exit status — never burns the 10 s hello timeout) |
| `E_HELLO_TIMEOUT` | `hub: connected but no protocol answer in 10s. The hub-side agentchute may be hung or broken; on the hub run: agentchute doctor` |
| `E_VERSION` | `hub: hub speaks agentchute-hub v1; this CLI needs v2. The hub always upgrades first — run `agentchute update` on the hub, then retry. This machine is not misconfigured and re-running `agentchute hub join` will NOT fix it: wait for the hub upgrade.` (P8 — the operator instinct on any hub error is to re-join; here that burns a key rotation and changes nothing, so the text forecloses it) |
| `E_IDENTITY` | `hub: this key is authorized as "codex" but you are acting as "grok". Fix --as/AGENTCHUTE_AGENT_ID, or join this machine as grok: agentchute hub join <url> --as grok` |
| `E_POOL_MISMATCH` (client arm, §4.4.2) | `hub: this key now serves pool /home/alex/other-pool (id 41d2…) on the hub, but this machine joined pool id 9c4e12ab77f0 (/home/alex/code/agentchute). The key line was re-pointed or the hub moved the pool. Re-join if the move is intended (agentchute hub join <url> --as codex), or re-authorize the key with the right --pool on the hub.` (compared against the pool id RECORDED at join, never raw URL text — a symlinked spelling of the same pool never trips this, D3) |
| `E_NOT_JOINED` | `hub: .agentchute-control-repo points at ssh://alex@hub…/home/alex/code/agentchute, but this machine never joined that hub (no ~/.agentchute/hub/3fa8c21b90de/config.json). If this machine IS the hub, a joined machine probably committed its pointer file — delete .agentchute-control-repo here (and `git rm` it if tracked). If this machine should be joined, run: agentchute hub join <that-url> --as <id>` |
| `E_CHANNEL_LOST` | `hub: channel to the hub was lost; the wrapper was stopped (fenced). Relaunch with: <the exact command line this serve was started with, echoed from its own argv — never a hard-coded example>. (This lane was started with --relaunch=false; the default relaunches automatically, §6.7.)` |
| `E_NO_SSH` | `hub: the `ssh` binary was not found on this machine. Install the OpenSSH client (macOS: preinstalled — check PATH; Debian/Ubuntu: apt install openssh-client), then retry.` |
| `E_SEND_UNKNOWN` | `hub: connection lost after the send was transmitted — DELIVERY UNKNOWN. Do NOT resend blindly: a copy may already be in claude-code's inbox (check `agentchute status`, or ask them). Body preserved at <spool-path>; if you confirm it did not arrive, retry with: agentchute send --to claude-code --body-file <spool-path>` |

Hub-reported (wire `error` frames, rendered with context):

| code | exact message |
|---|---|
| `E_LEASE_HELD` (fresh claim) | `runner for codex is already active (serve lease held by hub pid 48122, fresh 2s ago). A second machine serving the same id must pick a distinct --as; if a connection just dropped, this clears within ~20s.` |
| `E_LEASE_HELD` (stale claim, pid reads alive — §6.9; reachable when the claim carries no `boot_ref` (written by a pre-upgrade binary), when this host's boot ref is unreadable, or when the two refs MATCH — i.e. same boot, so a genuinely frozen-but-alive holder is protected, exactly as today) | `runner for codex looks DEAD (lease stale 3h) but hub pid 48122 still reads alive on the same boot as this claim — either that process is frozen but real, or this is OS pid reuse within one boot. On the hub: inspect `ps -p 48122`; if it is unrelated, delete /home/alex/code/agentchute/.agentchute/loop/state/codex/serve.claim and relaunch.` |
| `E_POOL_MISMATCH` (hub arm, §4.4.2 / §5.1) | `hub: this key is authorized for pool id 9c4e12ab77f0, but /home/alex/code/agentchute on the hub reports pool id 41d2c8ab0917 (or has no state/pool.id at all). The authorized_keys line's --pool was edited without its --pool-id, or the pool directory was replaced. On the hub, re-run: agentchute hub authorize --agent codex --replace-key --pool <the pool this key should serve> --key "<key>".` (refused at session start, before any op — the client never sees a hello-ok) |
| `E_FENCED` | `serve: this lane was fenced out (lease reclaimed — likely a newer serve for codex, or a hub update). Restart this lane: ac serve codex` (identical framing to today's `serve.go:618 @ 1244ae4`; under `--relaunch` this triggers the single automatic relaunch attempt, §6.7) |
| `E_POOL_NOT_FOUND` | `hub: the authorized pool path /home/alex/code/agentchute no longer resolves on the hub. The hub operator should re-run hub authorize from the pool's current location (agentchute hub authorize --agent codex --replace-key --pool <new-path> --key "<key>").` |
| `E_NOT_REGISTERED` | (unchanged texts — **two of them**, one per caller path; the code travels on the wire and the CLIENT renders the text its own call site renders today, so "sender" never becomes "agent" on a remote lane)<br>**sender path** — `send` (`internal/cli/send.go:145 @ 1244ae4`): `sender "codex" is not registered. Run `agentchute boot --as codex --vendor <vendor>` first (AGENTCHUTE.md §5.3)`<br>**agent path** — `check` (`internal/cli/check.go:120`) and `status` (`internal/cli/status.go:62 @ 1244ae4`), byte-identical to each other: `agent "codex" is not registered. Run `agentchute boot --as codex --vendor <vendor>` first (AGENTCHUTE.md §5.3)`<br>The two differ only in their first word; an earlier draft of this catalog listed the agent-path text alone, which would have made a remote `send` print "agent" where a local `send` prints "sender" — a behavior change inside a "zero behavior change" refactor. |
| `E_RECIPIENT_*` | the four C29 texts, verbatim from `internal/cli/send.go:347-416 @ 1244ae4` (unknown / stale / racing / unreadable) — single-sourced client-side, as the seam mandates (`seq.go:250-252`). |
| `E_HUB_IO` | `hub: the hub could not write pool state: <os error, e.g. "no space left on device">. This is a hub-side problem; nothing was partially delivered unless the message text says otherwise.` |
| `E_ORDER` | `hub: protocol order violation (tick before register on this channel). This is a client bug, not an operator problem — please report it.` |

CLI-surface refusals (not wire codes; exact text):

| surface | exact message |
|---|---|
| `hub authorize`, duplicate id | `hub authorize: "codex-tiny" already has an authorized key (SHA256:Yk3n…, added 2026-08-01). One key = one agent id. If this machine REPLACES the old one, re-run with --replace-key. If both machines should run, join the new one under its own id — ids are cheap, and a shared id would collide on the serve lease anyway. (Auto-derived names collide when two machines share a hostname; pick an explicit id on one of them: agentchute hub join <url> --as codex-tiny2.)` |
| `hub authorize`, unsafe pool/binary path (`E_POOL_PATH_UNSAFE`, §5.1) | `hub authorize: pool path contains characters outside the safe set [A-Za-z0-9._/+-] (spaces, quotes, and shell metacharacters are refused rather than escaped): "/home/al ex/pool". Move or symlink the pool to a plain path and re-run.` |
| `hub authorize`, malformed pool identity (`E_POOL_ID_INVALID`, §5.1 J1) | `hub authorize: /home/alex/code/agentchute/.agentchute/loop/state/pool.id is not a valid pool identity (must be a regular 0600 file containing exactly 12 lowercase hex characters). Nothing was written to authorized_keys. Inspect the file; if it is corrupt, delete it and re-run authorize to mint a fresh identity (existing key lines for this pool will then need re-authorizing).` |
| `turn-end`, hub unreachable (§8 row 21) | `turn-end: could not reach the hub to commit claimed mail (connect failed after 5s). Nothing is lost: the claim is held on the hub and the guard latch stays armed; turn-end retries at the next turn boundary. If this persists, check the network and run agentchute doctor.` |
| `hub join`, non-wrapper `--name` (§7.2, S4) | `hub join: --name work does not name a wrapper this machine can launch (known: claude, codex, gemini, grok). --name is the LOCAL name you launch the lane with (ac serve <name>), so it must be a wrapper token — a lane named "work" would have no launch form at all. For an arbitrary pool id, use --as instead and launch with an explicit wrapper: agentchute hub join <url> --as work-tiny, then ac --as work-tiny serve claude.` (refused before keygen and before any authorize call) |
| `hub join`/`--rotate-key`, hub dir busy (§7.2, G2) | `hub join: another agentchute hub join/rotate is already running for this hub (lock ~/.agentchute/hub/.locks/3fa8c21b90de.lock). Wait for it to finish and re-run.` (bounded wait, then this refusal — never a silent block) |
| `hub join`/`--rotate-key`, unparseable key version (§7.2, G3) | `hub join: keys/codex-tiny_ed25519.vold is not a valid key version (expected .v<N>, N a positive decimal integer). Move it out of the keys directory and re-run.` (never adopted as newest, never pruned as residue) |
| bare `agentchute hub join` | `usage: agentchute hub join ssh://[user@]host[:port]/abs/path/to/pool (--name <local-name> \| --as <agent-id>)`<br>`  --name mints the pool id <local-name>-<hostname> (e.g. --name codex on host tiny -> codex-tiny) and must be a known wrapper token; --as uses your id verbatim.`<br>`  The path is the pool's absolute path ON THE HUB (run `pwd` there).` |

Hook-context degradation rule (seamlessness invariant): commands that run
from hooks (`pending`, `self-check`, `guard`, `boot` context emitters) must
**never wedge the wrapper on network failure** — `guard` is fully local
(§6.6); `pending`/`self-check` print one stderr warning
(`hub unreachable; skipping (will retry next event)`) and exit 0. `turn-end`
is the exception by design: it reports the failure (text above) and leaves
the latch armed (§6.6). Worst-case added delay while the hub is down is
bounded by a **30 s negative cache** (G-M3): any `E_CONNECT` writes
`~/.agentchute/hub/<hub-id>/hub-down.json` `{"last_econnect":"<T>"}`; until
`T+30s`, hook-context commands — `pending`, `self-check`, **and hook-mode
`boot`** (`--context-only` / `--codex-hook SessionStart`,
`internal/cli/boot.go:15-40 @ 1244ae4`; D6 — SessionStart dials too, so
omitting it meant two 5 s stalls per offline session start) — skip the dial
entirely and take the same warn-and-exit-0 path, and any successful
connection deletes the file. Interactive `boot` bypasses the cache like
every other human-typed command. So a fully offline turn pays ConnectTimeout
(5 s) at most once per 30 s across all hooks, not once per invocation.
`turn-end` deliberately ignores the cache (it is the commit path —
correctness over latency), as does any human-typed command (a fresh attempt
is what the human is asking for: `send`, `check`, `doctor` always dial).

### 7.6 `doctor` integration

`agentchute doctor` in a joined checkout adds one check group, `hub`,
alongside the existing checks (`runDoctorChecks`,
`internal/cli/doctor.go:197 @ 1244ae4`):

```
hub: config ok (ssh://alex@hub…/home/alex/code/agentchute, joined as codex-tiny [local name: codex])
hub: key ok (~/.agentchute/hub/3fa8…/keys/codex-tiny_ed25519 -> .v1, 0600)
hub: connect ok (rtt 18ms, host key ED25519 SHA256:xxxx…)
hub: identity ok (key pinned to codex-tiny; protocol agentchute-hub v1; hub binary 1.7.0)
hub: pool ok (writable; hub time offset +0.4s)
```

Each line has a FAIL form that names the §7.5 error and its remediation
verbatim. The probe is one one-shot session's `hello`/`hello-ok` (the `ping`
op was cut into it, §3.6), total budget 10 s. Two further checks:

- On joined machines, `doctor` (like `hub join`) warns loudly when
  `AGENTCHUTE_CONTROL_REPO` is set in the environment and disagrees with the
  pointer file — env outranks the pointer (`config.go:218-225 @ 1244ae4`)
  and env leakage has bitten this project before. `doctor` also reports the
  negative-cache state (`hub-down.json` age, §7.5) so "why are my hooks
  quiet" has a one-line answer, and FAILs with `E_NO_SSH` when the local
  `ssh` binary is missing.
- The `AGENTCHUTE_AGENT_ID` counterpart of that warning — the one §7.2's
  resolver rule points at — is specified here (round 10; it existed only
  by reference before). Emitted by `doctor` and `hub join` whenever the
  env var is set and names a mapped local name, exact text:
  `warning: AGENTCHUTE_AGENT_ID=codex is a local name on this machine;
  every command resolves it to "codex-tiny" (this hub's names map). Unset
  it, or export the full id, if that is not what you want.`
- Run **on the hub**, `doctor` performs the authorize-line audit (the same
  validation as `hub authorize --list`): every `agentchute:`-marked
  authorized_keys line gets PASS/FAIL for "binary exists and is executable"
  and "pool resolves to a real pool" — surfacing hub-side the breakage that
  otherwise appears only as a remote's opaque
  `E_UNAUTHORIZED`/`E_HUB_NO_BINARY`.

### 7.7 Tailscale recipe (documented recipe — never a dependency)

The blessed deployment shape, shipped as a README/docs section:

1. Hub and every remote on one tailnet (`tailscale up`). Port 22 never faces
   the public internet; Tailscale ACLs say which machines may reach the hub's
   22 at all.
2. Join with the MagicDNS name:
   `agentchute hub join ssh://alex@hub.tail1234.ts.net/home/alex/code/agentchute --name codex`.
3. Everything else is the standard flow — **standard sshd + authorized_keys
   on top of the tailnet**, keys and forced commands exactly as in §5.

**Do not use `tailscale ssh` for the hub** (explicit, verified): Tailscale SSH
performs its own authentication and *does not read `~/.ssh/authorized_keys`*
("Your SSH configuration and keys files will not be modified"; Tailscale's own
docs flag machines that use authorized_keys to limit commands as a poor fit) —
so forced-command identity pinning, the day-one security requirement, would
silently not exist. The recipe uses Tailscale as the network layer only, which
already delivers its whole value here: no public exposure, stable names, no
firewall wrangling. Key management stays (that's the cost of keeping pinning);
`hub join` reduces it to the one join command per machine (§7.2 — zero
pastes when you can SSH to the hub yourself).

---

## 8. Failure-mode matrix

| # | fault | detected by | behavior | user-visible message |
|---|---|---|---|---|
| 1 | hub down at op start | ConnectTimeout 5 s | op fails; `send` spools body (safe-retry class) | `E_CONNECT` |
| 2 | hub dies under a live channel | ServerAlive (≤15 s) or tick deadline (10 s) | child fenced + stopped; then §6.7: relaunch loop (default) or exit (`--relaunch=false`) | relaunch status lines / `E_CHANNEL_LOST` |
| 3 | TCP half-open, client side | same as 2 | same as 2 | `E_CHANNEL_LOST` |
| 4 | TCP half-open, hub side | hub read deadline 20 s | session exits, releases lease; relaunch may see `E_LEASE_HELD` ≤20 s | `E_LEASE_HELD` (with the "clears within ~20s" line) |
| 5 | ssh helper process crashes | serve's child-wait | same as 2 (fence + stop + exit) | `E_CHANNEL_LOST` |
| 6 | wrapper (child) crashes | serve's `done` channel (`serve.go:469`) | serve sends `lease-release`, exits; hub releases | normal exit + wrapper error, as today |
| 7 | whole serve process crashes | ssh child gets stdin EOF → exits → hub session EOF | hub releases lease; child wrapper dies with the PTY | none (next launch is clean) |
| 8 | version skew (a remote updated before the hub) | handshake | fail closed before any op; the remote WAITS for the hub upgrade — re-running `hub join` is explicitly not the fix (P8) | `E_VERSION` ("hub upgrades first"; text names the do-not-re-join rule) |
| 9 | clock skew (either side) | n/a — hub clock owns all protocol state (§2); display ages corrected via `hub_time` offset | no protocol effect; spool/local log stamps may skew (cosmetic) | none |
| 9b | hub wall clock STEPPED forward under a live lane (wrong RTC at boot, then an NTP step) | the live lane's `LastSeen` suddenly reads stale, so a would-be acquirer reaches the stale-claim branch | lease is **NOT stolen**: branch (d)'s same-host pid-proof holds, and the claim's `boot_ref` equals this host's (a clock step is not a boot), so the boot-ref rule never fires — equality, never ordering (§6.9, B6) | `E_LEASE_HELD` to the would-be acquirer; the live lane is unaffected |
| 10 | key revoked while a session is up | sshd checks keys at auth only | live session survives until its connection ends; no *new* session can start | doctor/status show the still-live lease; docs: kill the `hub session --agent <id>` pid for immediate cut |
| 11 | disk full on hub | any loop write fails | op fails loudly; a live channel's heartbeat/sweep failures ride back as `tick-ok.warnings` (§3.4) and are re-logged into the runner log, never fatal; delivery never partial (link is atomic) | `E_HUB_IO` with the ENOSPC text |
| 12 | duplicate id join (2nd machine, same id) | `hub authorize` refuses a 2nd key for a marked id; if forced via `--replace-key`, the old machine's next auth fails; two live serves collide on the lease | fail closed at authorize time, else at lease time | duplicate-id refusal text (§7.5, spells the `codex-laptop` fork) / `E_LEASE_HELD` |
| 13 | disconnect after `check` claimed, before display | client sees dead channel/deadline | mail sits in hub `.claimed/`; next `check` REDELIVERS with banner (`check.go:180-193`); local latch set on that next check | `E_CHANNEL_LOST`, then normal REDELIVERED flow |
| 14 | disconnect after `send` transmitted, before `send-ok` | response deadline / EOF inside the ambiguity window (§4.5.3) | fail closed, spool, never auto-replay | `E_SEND_UNKNOWN` |
| 15 | lease reclaimed mid-session (zombie resume) | next `tick` → `E_FENCED`; next fenced one-shot `send` → `E_FENCED` | fence path: stop child, exit | `E_FENCED` |
| 16 | malformed/oversized frame (either direction) | codec | error frame, session closes (no resync) | `E_MALFORMED_FRAME` / `E_TOO_LARGE` |
| 17 | hub pool path gone/moved | session start validation of `--pool` | error frame then exit | `E_POOL_NOT_FOUND` |
| 18 | hub host key rotated | ssh + per-hub known_hosts | hard refusal until explicit reset | `E_HOSTKEY_CHANGED` |
| 19 | authorized_keys line hand-mangled | sshd auth failure | as key-revoked | `E_UNAUTHORIZED` |
| 20 | hub unreachable during a hook (`pending`/`self-check`) | ConnectTimeout | warn to stderr, exit 0 — never wedge the wrapper (§7.5) | one stderr warning line |
| 21 | hub unreachable during `turn-end` | ConnectTimeout | commit not performed; latch stays armed; retried next turn-end | exact text in §7.5 ("could not reach the hub to commit claimed mail …") |
| 22 | remote laptop sleeps, later wakes | on wake, ServerAlive/tick deadline kill the dead channel ≤15 s | child fenced + stopped during sleep-detection; the lane relaunches within one backoff step of waking (relaunch default-on); hub side released at its 20 s read deadline back when the laptop slept | relaunch status line; with `--relaunch=false`, `E_CHANNEL_LOST` |
| 23 | hub reboots (N remote lanes) | all channels drop at reboot; sshd returns minutes later | every remote serve (relaunch default-on) retries with capped backoff (≤60 s interval) and relaunches when sshd answers — zero human action on any machine; pre-reboot `serve.claim` files are long stale by then and reclaimable because their recorded `boot_ref` differs from the rebooted host's (§6.9; freshness floor still applies first, C8) | relaunch status lines; with `--relaunch=false`, N × `E_CHANNEL_LOST` |
| 24 | hub `agentchute update`/`setup` invalidates all serve leases (`lease.go:371-376 @ 1244ae4`) | every channel's next `tick` → `E_FENCED` | fleet-wide fence by design (the update forcing function); lanes perform their single `E_FENCED` relaunch attempt (default-on) and come back under the new binary; note: a live hub session keeps executing its already-open old binary inode until it exits | `E_FENCED` text |
| 25 | `<hubdir>/mux/%C` too long for `sun_path` (deep `$HOME`; ~104 B macOS / 108 B linux) | the ControlPath length budget in the ssh-invocation builder (§4.2), before ssh is spawned | one-shots move to the owned-0700 per-user temp mux dir; if that does not fit either, multiplexing is disabled for this hub (one extra authentication per op, correctness unaffected). Never a refusal; the channel never multiplexes anyway | none in the fallback arm; exactly one `warn` note naming both attempted paths in the mux-disabled arm |

---

## 9. Compatibility & spec delta

Ordering (C6, decided): **one tag, at the end of M6, numbered v1.6.0.**
There is no standalone post-M1 tag. Both halves of C6 — no released spec
ahead of a released binary, no released binary carrying an unspecified
transport — are boundary conditions on *released artifacts*; a single tag
after M6 satisfies both. **Ordering constrains tags, not publication.**
Publication happens at merge-to-main (the site auto-deploys from `main`).
What actually keeps the published spec from describing a transport no
released binary has is that published spec/conformance URLs are pinned to
the latest GitHub release (issue #154, PR #156) — not an interim tag.
Working rule 1 (spec before code) still holds: M2 merges before any hub
code. M1 is already on `main` (`7d08654`); M2 is already on `main`
(`1431657`).

### 9.1 AGENTCHUTE.md amendments (exact sections)

- **A normative "Hub wire & lifecycle" section (C7)** — not amendment notes
  but binding spec text, added by M2 to AGENTCHUTE.md (or a normative
  `HUB.md` incorporated by reference from it): the frame grammar and the
  64 KiB / 4 MiB limits (§4.3), the message vocabulary and per-session
  serial ordering (§4.4), the version handshake + hub-upgrades-first policy
  (§4.3), the never-auto-replay rule for ambiguous sends (§4.5.3) **and the
  mandatory `send-ok.committed` flag it is written against** (§4.4.1, F11 —
  the spec copy of that example carries `"committed":true`; a `send-ok`
  without the field is malformed, not a defaulted `false`) **and the
  always-present `tick-ok.warnings` list** (§3.4/§4.4.1 — `[]` when the tick
  was clean; the fenced case is the tick's only hard error, every other step
  failure is a warning entry) **and `note.level`'s two-value vocabulary with
  its pinned routing** (§4.3 — exactly `warn`/`info`; `warn` → stderr with a
  rendered `warning: ` prefix, `info` → stdout with none; the `info` arm is
  not decorative, it is what keeps `check`'s in-stream status lines in
  position), the
  disconnect-after-claim redelivery rule (§4.5.2), remote turn-end ordering
  (§6.6), the timeout/size table (§4.6), the per-key identity-pinning
  model (§5), and **the error-code registry with each code's emitter side**
  (§4.4.2, F9/X1 — hub-emitted, client-emitted, or both; `E_POOL_MISMATCH`
  is the one code with two emitters and two exact texts, and the spec must
  say so rather than classifying it as client-only). After M2, when this
  proposal and that spec text disagree, the spec wins — the same supremacy
  rule the conformance suite already has.
- **Enrollment surface (G-M4)**: AGENTS.md and the wrapper enrollment
  templates (CLAUDE.md / CODEX.md / GEMINI.md / GROK.md + `examples/`) gain
  four lane-facing rules: remoteness is *discovered* (commands are
  identical on hub and remote; never a `--hub`/`--remote` spelling); NEVER
  re-send after `E_SEND_UNKNOWN` without confirming non-delivery first; run
  `agentchute doctor` immediately after a join and after any hub move; and
  **on `E_VERSION`, WAIT for the hub to upgrade — never "fix" it by
  re-running `hub join`** (P8: the hub upgrades first, §4.3; a remote that
  updated ahead of its hub is correctly configured and simply early, and
  re-joining rotates a key for no reason). The same line belongs on the
  release checklist for any tag that bumps the wire version.
- **`setup --wipe-state` surface (H1)**: the layout/§3 amendment names
  `state/pool.id` as preserved, non-runtime scaffold — alongside
  `setup.json` — in the wipe plan, the post-wipe rescan, and the
  dry-run/preserved output (today's wipe deletes every other `state/`
  entry and its rescan flags survivors, `setup_wipe.go:295-316,519-536
  @ 1244ae4`; wiping the pool's identity would silently invalidate every
  key binding). Spec text in M2; behavior lands in M5 with `authorize`.

- **§1 Reference implementation**: add the hub as the reference *transport*
  for multi-host pools: "Transport: … or, for a remote lane, framed
  operations over SSH to the pool host (the hub); the substrate is unchanged."
- **§2 Scope / Tested targets**: add "hub pool (one pool host + SSH remote
  lanes)" as a supported, CI-tested configuration; rewrite the cross-host
  network-mount paragraph to point new multi-host deployments at the hub
  (mount-based cross-host remains tolerated-but-unverified compatibility, one
  notch further deprecated).
- **§4 Discovery**: document the `ssh://` locator form in the §4.1 cascade
  arms (flag/env/pointer).
- **§5.4 Serve lease**: one paragraph — for a remote lane, the lease holder
  is the hub-side session process; pid-proof is therefore same-host again;
  channel loss releases the lease token-checked. Additionally the same-host
  pid-proof gains **boot-reference corroboration** (§6.9, B6): the claim
  records a per-boot identifier (`boot_ref`) at acquire, and a stale claim
  whose recorded ref DIFFERS from the host's current one is provably dead
  whatever its pid says. Spec text must state the three properties that
  bound it — the comparison is **equality only, never ordering**; the
  reference has **no wall-clock component** (so a clock step cannot forge a
  difference); and the reference is **host-scoped**, changing on a host
  reboot and on nothing else, so a container/VM/service restart on an
  unrebooted host is unchanged from today's behavior and pid reuse is NOT
  solved generally (§6.9) — and that an absent ref means unchanged,
  pre-existing behavior. This is the one behavioral amendment to existing lease
  semantics, called out explicitly since it also affects local pools. (An
  earlier draft of this design proposed a `StartedAt`-predates-boot-time
  rule; it is withdrawn as unsafe — see §6.9 — and must not reach the
  spec.)
- **§8 Wake/supervision**: note that for a remote lane the runner's poll is
  the tick over the channel; injection is unchanged and local.
- **§12 Non-goals**: amend "No non-filesystem transport in the reference
  CLI" → "No non-filesystem **state substrate** in the reference CLI; the
  hub's SSH channel forwards *operations* to the one filesystem pool and is
  part of the reference implementation. Sync/replication of loop state
  remains excluded." Also drop "no handshake" phrasing in favor of "no
  *dynamic capability* negotiation beyond the versioned hub handshake".
- **§15 Security**: add the per-key pinning model (§5 here, condensed): remote
  sender identity is transport-enforced; cooperative trust still governs
  co-tenants on the hub itself; bodies remain untrusted data.

### 9.2 EXTENSIONS.md

Keeps: the substrate sketches (queue, object store, HTTP endpoint, git),
bridges, and the "not extension space" list — all still true and still
non-shipping. Changes: the HTTP sketch gains one line ("planned phase-2 behind
`internal/op` — see the hub design"), and any wording implying every
non-filesystem *transport* is out of the reference CLI is aligned with the
amended §12.

### 9.3 Conformance-suite additions

`conformance.Binding` (`conformance/binding.go:52 @ 1244ae4`) has **no
lease/fencing surface** — passing it alone was already flagged insufficient by
the consensus. Additions:

- New vector set **L (lease/fencing)**, driven in-process against the seam:
  `L1` a second acquire while fresh fails closed; `L2` a fenced holder's
  mint/heartbeat writes nothing (zombie-resume); `L3` release is token-checked
  (never deletes a successor's claim); `L4` stale + pid-dead reclaim succeeds
  and old token is dead. These encode `lease.go`'s existing behavior as
  executable spec for *any* future backend (S3's CAS story must pass L2 —
  the veto's "executable proof" hook).
- New vector set **W (wire)**, driven against a fake transport (§10.2):
  `W1` disconnect-after-claim redelivers; `W2` disconnect-after-send is
  reported unknown and not replayed; `W3` reclaim-during-send fails fenced;
  `W4` identity mismatch closes at handshake; `W5` version mismatch closes at
  handshake; `W6` unreadable claimed residue reports `claimed_held: true`
  (and the client arms its latch with no `msg` frames). W1 is
  disconnect-after-claim: the session dies and **no terminal `error` frame
  is written**, so it does not cover a hub that forgets `claimed_held`.
  W6's path is `Claim` returning an error with
  `ClaimSummary.Redelivered > 0` — residue exists but its bytes cannot be
  read. Setup is `chmod 000` on a `.claimed` residue file, the same probe
  that proved the M1 latch bug in two worktrees on both platforms.

Timing (C7): these vectors are **not** a post-release merge. The L set and
the fake-transport W set land inside M3 (with the code they test); the
sshd-backed W runs land inside M6 — all of it **gating** the v1.6.0 tag,
never after it (§11).

---

## 10. Test plan

### 10.1 Operation-seam tests (M1; in-process, no network)

Port the behavioral assertions of the existing CLI tests to the seam:
send preflight ordering (A5), C29 classification, fresh-suffix collision
retry (C4), claim/ack idempotence, redelivery, quarantine, owed
record/discharge, lease acquire/renew/release/fence, heartbeat self-heal,
sweep back-off. Plus, per the reshaped seam: every actor-scoped op driven
through **both** `op.Context` constructors (CLI-resolved and pinned-id,
§3 conventions — C1), and the streaming emitters (`Claim`/`Ack`/`Pending`)
asserted to hold at most one item in memory and to surface an emit-error
abort cleanly (C4). Plus a byte-for-byte stdout/stderr golden for the two
note levels' routing (§4.3): the three `info` status lines land on **stdout**
in their production positions — the limit line between two message renders,
the CLAIMED line before the expired-obligation lines — and `warn` notes on
**stderr** with the `warning: ` prefix the renderer (never `Msg`) supplies.
Position, not merely presence, is the assertion: a renderer driven from the
terminal summary passes a `Contains` check and still reorders the output.
Success criterion for the merge: the **existing** CLI
test files (`send_test.go`, `check_*.go`, `ack_test.go`,
`consume_boundary_test.go`, `b1_convergence_test.go`, …) pass unmodified —
the refactor is invisible, including the local child-then-register serve
startup order (§6.1, C10).

### 10.2 Framing tests with a fake transport (M3)

The codec reads/writes an `io.ReadWriter`; tests drive it over `net.Pipe`:
round-trip every frame type; body trailers byte-exact at 0 B, 1 B, 4 MiB, and
4 MiB+1 (`E_TOO_LARGE`); truncated frame/trailer at every boundary; unknown
`t`; unknown fields ignored; oversize line; interleaved `note` frames at both
levels, with **frame-level production order** asserted (rendered
`warn`→stderr / `info`→stdout is M4 / WI-4.5 — production remote rendering
first lands there; a test-only renderer would prove no production behavior);
a `register-ok` whose `reg.body` exceeds 64 KiB round-tripping through the
trailer (§4.4.3); the **`status-ok` two-budget rows** (§4.4.3) — 64 small rows
in ⇒ 64 out, `truncated:false`; 65 in ⇒ the first 64 in sort order,
`truncated:true`; **one valid registration whose `host` alone exceeds 64 KiB**
⇒ that row is excluded even though the pool holds far fewer than 64 agents,
`truncated:true`, and the emitted line is ≤ 64 KiB when measured whole; the
same oversized row placed FIRST in sort order ⇒ `agents:[]` with
`truncated:true` and every later row dropped (prefix, never skip-and-continue);
and a pool sized so the encoded line lands exactly at the budget boundary
(largest frame that fits ⇒ `truncated:false`; one byte more ⇒ the last row
drops), asserted on the **encoded line length including the LF**, so a
rows-only budget fails the row;
handshake version matrix (v1/v1, v1/v2 both directions); a fuzz target on the
frame parser (`go test -fuzz`, checked in as a regular seed-corpus test —
the hub-side parser is the §5.4 attack surface); typed-event-stream tests
(C4/D2): a `check` over many maximum-size (4 MiB) messages with peak-memory
assertion; a `check` producing many interleaved `note`/`owed-item` frames
with order preserved end-to-end; a `pending` with many owed entries and one
4 MiB `show_body` body as a trailer; transport failure injected after the
first emitted item AND after a mid-stream `note` (claim durable, no partial
frame, clean abort, no note silently lost before the failure point).

### 10.3 Real-sshd integration matrix (M6)

Harness: generate throwaway host + client keys in a temp dir; start
`/usr/sbin/sshd -D -f <generated sshd_config>` on a random high port with
`AuthorizedKeysFile` pointing at a generated file containing the §5.1 forced-
command line (absolute path to the freshly built test binary); pool in a temp
dir. Build-tagged `//go:build sshd_integration`, gated on
`AGENTCHUTE_SSHD_TEST=1` (env-strip discipline per the destructive-test rule —
the harness refuses to run if `AGENTCHUTE_LOOP_DIR`/`AGENTCHUTE_CONTROL_REPO`
point at a real pool).

Matrix (each row asserts both sides' end state):

| test | method |
|---|---|
| join→authorize→verify happy path | drive `hub join`/`hub authorize` against the harness |
| identity mismatch | hello with wrong `agent` → `E_IDENTITY`, no op executed |
| version mismatch | client forced to v2-min → `E_VERSION` |
| disconnect-after-claim | SIGKILL the client ssh after `msg` frames start; assert `.claimed/` residue; next check redelivers |
| disconnect-after-send | SIGKILL between body flush and `send-ok` (test seam in the client transport); assert spool + `E_SEND_UNKNOWN`; assert at most one inbox file |
| reclaim-during-send | acquire lease as A, release/reacquire as A′ (new token), then one-shot send with A's token → `E_FENCED`, nothing linked |
| channel drop fences child | kill sshd mid-channel; assert wrapper (a script child) got SIGTERM within 15 s; assert hub lease released |
| half-open hub side | SIGSTOP the client; assert hub session exits at the 20 s read deadline and releases |
| lease-held refusal | two serves same id; second gets `E_LEASE_HELD` |
| host-key change | swap host key; assert `E_HOSTKEY_CHANGED` refusal |
| mux reuse | 3 sequential one-shots; assert 1 sshd auth log entry (ControlMaster hit) |
| ControlPath length rule (§4.2) | drive the invocation builder with an **injected** hub-dir length (deterministic — no real deep `$HOME`, no OS dependence; no CI runner has a home deep enough to trigger this naturally): within budget ⇒ `-o ControlPath=<hubdir>/mux/%C`; over budget ⇒ the owned-0700 per-user temp path (assert the dir is 0700 and uid-owned, and that a symlinked or foreign-owned temp dir is refused, `runner_socketdir_unix.go:24-49`); over budget with every temp root also over budget/unwritable ⇒ `-o ControlMaster=no -o ControlPath=none` plus exactly one `warn` note naming both attempted paths, and the op still succeeds. Re-run the mux-reuse row through the fallback arm (still 1 auth entry) and the disabled arm (3 auth entries) |
| child env contract (§6.8) | launch remote serve in the harness; assert child env carries `AGENTCHUTE_CONTROL_REPO=<ssh URL>` and no `AGENTCHUTE_LOOP_DIR`; with networking blocked, run `guard --pre-tool-use` in hook context and assert it resolves the SHADOW latch (denies while armed — never fail-open); child `send` lands in the hub pool |
| supervised relaunch — default path (§6.7, D5) | launch a remote lane with a BARE `agentchute serve` (no flag — this is the load-bearing default); kill sshd; restart sshd; assert the lane re-acquires with a NEW token and NEW child pid, exactly one lane instance, old child SIGTERM'd first. An implementation that kept relaunch opt-in fails this row |
| relaunch opted out (`--relaunch=false`) | same drop; assert the old child is stopped, NO new child appears, and serve exits with `E_CHANNEL_LOST` echoing its own argv |
| hub-reboot pid reuse (§6.9, C8, B6) | fabricate a `serve.claim` with an explicitly **stale** `LastSeen`, a `boot_ref` DIFFERENT from the injected current one, and a live decoy pid → assert acquire reclaims; same claim with a MATCHING `boot_ref` → assert `ErrLeaseHeld` (a frozen-but-alive same-boot holder keeps its id); with `boot_ref` ABSENT (a pre-upgrade claim) → assert `ErrLeaseHeld` (today's behavior preserved = the upgrade path); with the host's current ref unreadable → assert `ErrLeaseHeld`; with a FRESH `LastSeen` and a differing ref → assert `ErrLeaseHeld` (the freshness refusal still precedes the pid/boot branch) |
| clock step does not steal a live lease (§6.9, B6 — the row the withdrawn rule failed) | hold a live lease, then step the injected wall clock FORWARD by hours so the claim's `LastSeen` reads stale while the process stays alive and the boot ref is unchanged → assert a competing `AcquireServeLease` gets `ErrLeaseHeld` and the claim file is byte-unchanged (same `serve_token`), i.e. the live lane keeps its lease; assert the same for a step BACKWARD |
| `boot_ref` survives the heartbeat (§6.9, B6) | acquire, assert `boot_ref` is present in `serve.claim`, run `RenewLease` repeatedly, assert `boot_ref` is still present and identical after each renew (guards the `readClaim`→`marshalClaim` round-trip, `lease.go:317-340`) — it survives **because it is a real struct field**, which is the property under test. Separately assert the mixed-version property, which is TOLERANCE and not preservation: a claim JSON carrying an unknown extra key still unmarshals cleanly and every known field is read correctly (`readClaim`'s plain `json.Unmarshal`, no `DisallowUnknownFields`, `lease.go:101-114`) — and assert the other half explicitly, that the unknown key is **DROPPED** by the first `RenewLease`, so no test and no caller may treat an off-struct key as durable |
| launcher preserves remoteness (§6.8 rule 5, B4) | in a joined checkout, launch through BOTH launcher paths and capture the re-exec'd `agentchute serve` argv. (1) The single `ac` dispatcher (`ac serve <wrapper>`) — the only launcher setup installs today. (2) `cmdShimsExec` (`shims.go:259-312 @ 1244ae4`), the LEGACY per-wrapper `ac-*` path: setup deletes those shims (`removeLegacyWrapperShims`, `shims.go:233-257`), so the test must CONSTRUCT one (or call `cmdShimsExec` directly) — a row that exercises only the dispatcher passes green while `shims.go:304-305` is still broken. For each: assert it carries `--control-repo ssh://…` (the canonical URL, not the local repo root) and NO `--loop-dir` at all, and that the child's discovered config has `Remote != nil` and `LoopDir` = the shadow. A launcher that forwards `cfg.ControlRepo`/`cfg.LoopDir` fails this row — today's code would pass discovery, run LOCAL against the shadow, and report no error, so the assertion must be on `Remote != nil`, never merely on "the command succeeded". Also assert an explicit `--loop-dir` passed to `ac` in a joined checkout is REFUSED (§6.8 rule 4) rather than forwarded |
| live-lease self-check/turn-end (C2) | with a channel holding a fresh lease, run the child's one-shot `register` (self-check) and `turn-end` carrying the inherited token → accepted; with a missing/wrong token → refused as live-elsewhere (`register.go:189-195` semantics preserved over the wire) |
| register field semantics (C2/D1) | wire register with `Bio:nil` preserves an existing bio; `Bio:&""` clears it; omitted host after a simulated machine move records the NEW remote hostname (never the hub's); omitted vendor (`Vendor:nil`) against an existing custom-id hub row resolves that row's vendor — every step-0 repair succeeds bare; `Announce:true` against a pool with one undeliverable peer renders the per-peer warning AND `sent to N of M peer(s)` byte-identically to a local `register --announce`, and a hub-side announce failure prints `warning: announce failed: …` with the register itself still exiting 0; `Sweep` is `true` on `boot` alone — a remote `boot` removes a stale lease-dead row from the HUB pool and leaves the shadow untouched (the local sweep is skipped, never duplicated), a hub-side sweep failure surfaces as `sweep stale registrations: …` in boot's warnings with boot still succeeding, and remote `register`/`self-check`/`turn-end` send `sweep:false` and remove nothing |
| remote vendor resolution skipped client-side (S2) | in a joined checkout with a custom pool id whose vendor no canonical-prefix rule can name, drive each of the four sites bare (no `--vendor`) — `agentchute serve <wrapper>`, `register`, hook-mode `boot`, and `turn-end`'s step-0 self-check → assert each sends `Vendor:nil` on the wire (never a value resolved from the mail-free shadow) and each succeeds; **assert the rendered OUTPUT carries the hub-RESOLVED vendor, not the empty request value** — `register`'s `  vendor:        <resolved>` (text; the command has no `--json`), `boot`'s text `(<resolved>)` **and** `boot --json`'s `"vendor":"<resolved>"`, `self-check`'s text **and** `self-check --json`'s `"vendor":"<resolved>"`, each byte-identical to the same pool driven locally (a renderer printing the pre-wire `opts.Vendor` prints empty here and fails this row); assert `cmdServe` does NOT emit its `missing --vendor` refusal (`serve.go:153-155`) on a remote lane; assert an explicit `--vendor` still wins and is still validated |
| remote `status` renders the HUB, header included (§3.6) | in a joined checkout, run `agentchute status` remotely against a hub pool of three agents and assert: the table's STATUS/INBOX columns and the AGE column come from the response (`StatusAgent.Status`, `.InboxDepth`, `StatusResp.Now`) and are byte-identical to the same pool driven locally with the clock pinned — a client that re-derives them locally renders `-`/`0` and fails; the header's `control_repo:` line prints the canonical `ssh://` URL (never the local repo root, never the shadow), `loop_dir:` prints the shadow WITH its marker line — asserted **byte-exact** against §3.6's pinned `  (local shadow: this process's own loop dir, not the hub's)`, two leading spaces included — `vendor:` matches the local run's string; the lenient-read `warning: …` lines land on stderr AHEAD of the table. Then plant a registration whose `host` alone exceeds 64 KiB and re-run. **The fixture pins where that row sorts: the oversized `host` belongs to the lexicographically LAST of the three agent ids** (e.g. `alpha`/`bravo`/`zulu`, huge host on `zulu`) — §4.4.3's rule emits a PREFIX of the sort order, so an oversized row sorting first or in the middle drops every row behind it and "every other agent still renders" would be false (that first-row case is the §10.2 producer row asserting `agents:[]`). With it sorting last: the listing still renders the other two agents, and the trailing truncation line is asserted **byte-exact** against §4.4.3's pinned `note: listing truncated by the hub at the first row that would exceed 64 rows or a 64 KiB response; later agent ids are missing.` (a notice naming only "64 rows" fails the row, since this pool has three) |
| durable pool identity (D3/D9/E5/F1) | authorize the same pool via its realpath, a symlink spelling, a trailing-slash spelling, and — on the macOS runner — a CASE-alias spelling (`/Users/...` vs `/users/...`) → ONE marker (`pool.id` reuse), duplicate-id refusal fires across all spellings, `--list` shows one line, `--revoke` removes it |
| `--pool`-only key-line edit (F1; `E_POOL_MISMATCH` **hub arm**, F9) | after a completed join, edit ONLY the forced command's `--pool` to another directory (leaving `--pool-id` intact) → session start reads that directory's `pool.id` and refuses → `E_POOL_MISMATCH` **error frame from the hub**, before any op and before `hello-ok`; assert the client never sees a `hello-ok` and renders the hub-arm text (§7.5). Repeat with the new directory having NO `state/pool.id` at all → same code, same arm |
| mid-stream disconnect arms the latch (E1) | kill the connection after the first `msg` frame of a multi-message `check` → assert the LOCAL guard latch is armed, the displayed message's claim persists hub-side, and the undisplayed remainder redelivers on the next check. Repeat with the first frame being a **REDELIVERED** `msg` (hub-side `.claimed` residue, no fresh mail) → assert the latch is armed before that render too — the remote counterpart of the local `check.go:185` arming site (§3.2), and the one an emitter that only watches freshly claimed mail would miss |
| same-hub alias rejoin (D4a) | re-join through a different Host alias for the same hub/pool → migration path: zero new keys, one authorized_keys line, pointer updated |
| migration attribution, normal lane (B5) | launch a remote lane **exactly as the quickstart launches one** — `ac serve codex` in the joined checkout, and separately the direct `agentchute serve codex` form the quickstart prints — then attempt an alias rejoin: assert BOTH are attributed as live lanes and produce the **"still running against the old URL … stop that session first"** refusal, never the ambiguous-pid text; assert state is untouched; stop the lane and assert the rejoin then migrates. A predicate that exact-matched argv `--control-repo`/`--loop-dir` against `cfg` fails this row in the normal case |
| migration vs pid reuse (F2, B5) | plant a stale `runner.json` whose pid is alive but belongs to (a) a non-agentchute process and (b) an `agentchute serve --control-repo ssh://<a-different-hub>/…` → alias rejoin fails CLOSED with the ambiguous-pid text in both (no kill suggestion, state untouched); after removing the stale file, the rejoin migrates |
| migration completion is atomic (G1) | inject a crash BEFORE the `rename()` → assert `<new-id>.partial` exists, `<new-id>` does not, the old dir is intact and still authoritative, and a re-run discards the `.partial` and completes; inject a crash AFTER the rename but before the old dir is deleted → assert a re-run finishes steps 4–5 (pointer rewritten, old dir gone) with no second migration; assert the fingerprint scan never returns `<new-id>.partial` or `.locks/` (names that are not 12 lowercase hex) |
| join/rotate/migrate mutual exclusion (G2) | run two `hub join` invocations for the same hub concurrently → exactly one proceeds, the other exits with the lock-busy refusal, and the key/version state is identical to a single run; assert the lock file lives at `~/.agentchute/hub/.locks/<hub-id>.lock` and SURVIVES a migration that deletes the old hub dir; assert migration takes both locks in ascending hub-id order (no deadlock under a reversed-pair concurrent run) |
| key-version grammar (G3) | `.v2` vs `.v10` present → `.v10` is treated as newest (numeric, not lexicographic); a `.vold`/`.v01`/`.v2b` file → run refuses with the invalid-version text and neither adopts nor deletes it; a retired file is named `<file>.invalid.<YYYYMMDDTHHMMSSuuuuuuZ>` with its `.pub` retired under the same stamp, and two retirements within one second do not collide; the "current pubkey" every paste line prints is `readlink(keys/<id>_ed25519) + ".pub"` and there is NO `keys/<id>_ed25519.pub` symlink |
| mux reaped across migration (G4) | run a one-shot to leave a live ControlPersist master, then migrate → assert the old dir is deleted, a best-effort `ssh -O exit` was issued, and a subsequent one-shot opens a fresh master under the NEW hub dir (never attaches to the orphan) |
| staged rotation crash recovery (D4b/F3/H3) | inject failure at EVERY transition — between first keygen and the initial symlink (rerun adopts the orphan after the passphrase-free probe, never re-runs keygen onto it; a probe-failing orphan is retired `.invalid.<ts>` and reminted), after mint, before the remote replace, after the remote replace, mid-promote (temp symlink present, rename not done), after promote before verify (recovery verifies the ACTIVE key via hello THEN prunes; on any active-hello failure old versions survive, with the J2 class split below), after verify before cleanup (residue branch completes the prune) → re-running join/rotate converges from each; post-replace cases run with the old key already revoked |
| wipe preserves pool identity (H1) | run `setup --reset --wipe-state` on a pool with `state/pool.id` → pool.id listed as Preserved in the dry-run and the wipe, survives, and the post-wipe rescan does NOT flag it; next session start still validates against `--pool-id` |
| pool.id validation (J1) | plant each malformed `pool.id` in turn — oversized (>64 B), a symlink, embedded newline, quote, `` `$()` ``, whitespace, wrong length/charset → `hub authorize` (fresh-read AND loser-re-read paths) refuses `E_POOL_ID_INVALID` and authorized_keys is byte-identical before/after; `hub session` startup refuses the same before any op |
| residue remediation by error class (J2) | post-promotion residue with the active key deauthorized → `E_UNAUTHORIZED` path prints the active-key paste line; same residue with the hub DOWN → `E_CONNECT` remediation only, no reauthorize hint; same residue with a rotated host key → `E_HOSTKEY_CHANGED` security stop only; all three preserve every version file |
| concurrent alias first-authorize (H2) | two `hub authorize` runs racing on the same never-identified pool via different spellings → exactly one `pool.id` (exclusive create; loser adopts the winner's value), both key lines carry the SAME marker |
| default naming (operator-directed) | `hub join --name codex` on two hosts (`tiny`, `alexs-macbook`) → distinct ids `codex-tiny` / `codex-alexs-macbook`, each machine's `agentchute identity` lists its own local-name map, `ac serve codex` resolves the joined id on each; same `--name` on two hosts SHARING a hostname → duplicate-id refusal with the hostname-collision `--as` hint; sanitization cases (`Alexs-MacBook.local`, unicode/underscore chars, empty-after-sanitize error) |
| non-wrapper `--name` refused (S4) | `hub join <url> --name work` → refused with the §7.5 text BEFORE keygen and before any authorize call; assert no key file, no authorized_keys line, no `config.json` change. Then the supported form: `hub join <url> --as work-tiny` succeeds, and `ac --as work-tiny serve claude` launches and hellos as `work-tiny`. Also assert the negative that motivates the rule: with a hypothetically recorded `names["work"]`, `ac serve work` exits on `wrapperForToken`'s unknown-wrapper refusal (`dispatch.go:132-137`) and `agentchute serve work` tries to exec a wrapper command named `work` (`serve.go:117-129`) — neither path can ever launch that lane |
| resolver, direct launch with env UNSET (round 10 — the quickstart's own path) | after `hub join --name codex`, in a fresh shell with NO `AGENTCHUTE_AGENT_ID`, run bare `agentchute serve codex` → `launchedWrapper` supplies the candidate, the `names` map resolves it → lane hellos clean as `codex-tiny` (this case must NOT export env — the earlier row false-greened by doing so) |
| resolver with env exported (round 9/9b) | same join; launch BOTH ways — direct `agentchute serve codex` AND `ac serve codex` — each with `AGENTCHUTE_AGENT_ID=codex` exported (the enrollment default) → resolved to `codex-tiny` in both; guard latch, spool, and hello all use the resolved id; a candidate NOT in the map (`codex-tiny2`) passes verbatim; `send --to codex` is never remapped (no peer aliasing) |
| hostname-change invariant (round 9b) | join as `codex-tiny`, then change the injected hostname (`tiny` → `renamed`), re-run `hub join`, `agentchute identity`, and `serve` → id, `names` map, and key files all remain `codex-tiny`; no newly derived id, key, or authorized_keys line appears |
| name/pool-id shadowing refused, both orders (round 10) | (a) after `hub join --name codex` (⇒ `names["codex"]="codex-tiny"`), `hub join <url> --as codex` → refused before keygen/authorize with the mapping named; (b) after a verbatim `hub join --as codex`, `hub join <url> --name codex` on the same machine → refused likewise; in both cases no key, no authorize call, no config change |
| env local-name warning (round 10) | with `AGENTCHUTE_AGENT_ID=codex` exported and `names["codex"]="codex-tiny"`, `doctor` and `hub join` each emit the §7.6 warning naming the resolution; with the env var set to an unmapped id, no warning |
| negative cache covers SessionStart (D6) | with the hub down, hook-mode `boot` then `pending` inside 30 s ⇒ exactly one dial (one 5 s stall), both exit 0 |
| join idempotence (G-B1) | run `hub join` twice → same pubkey fingerprint, exactly one authorized_keys line; `--rotate-key` → new key, old line replaced |
| shell-safety refusals (C5) | authorize/join with values containing each metacharacter class (space, `'`, `"`, `` ` ``, `$()`, `;`, `\`, newline) in pool/binary paths → refused, never written; quoted-pubkey path round-trips |
| same-path two-hosts vs true self-URL (C3) | harness "hub" and "remote" sharing one absolute pool path → join + ops work; a pointer to a hub-id with no local join config → `E_NOT_JOINED` |
| `E_FENCED` single relaunch attempt | invalidate the lease under a bare (default-relaunch) lane → assert exactly one relaunch attempt succeeds; repeat with a rival holding the lease → assert the lane stops on `E_LEASE_HELD` |
| pool mismatch, consistent re-point (§4.3, D3/F1; `E_POOL_MISMATCH` **client arm**, F9) | after a completed join, re-point the key line's `--pool` AND `--pool-id` together at another validly-authorized pool → session start passes (its pool.id matches) and a `hello-ok` IS returned, but the CLIENT fails closed on `hello-ok.pool12` ≠ the RECORDED pool12, rendering the client-arm text (§7.5), no op executed; a symlinked spelling of the joined pool does NOT trip either arm |
| authorize validation (§7.1) | authorize with a non-pool path / non-executable binary path → loud refusal; `--list` shows FAIL for a hand-broken line |

### 10.4 CI wiring

New workflow job `hub-integration` on `ubuntu-latest` **and** `macos-latest`
(both ship sshd; macOS uses `/usr/sbin/sshd` with a user-owned config — no
system Remote Login needed since we run our own sshd on a high port).
Runs 10.3 + 10.2; the seam/unit tests join the normal test job. The existing
`gh pr checks`-before-SHIP gate applies (embedded-asset coupling rule).

---

## 11. Implementation plan (6 ordered merges)

Collapsed from an earlier 10-PR plan (G-m7): the process weight shrinks, the
code does not — every deliverable of the old plan appears below, and the
conformance vectors moved INSIDE the merges they test (C7).

| merge | contents | ~LOC (incl. tests) | reviewer gate | tag |
|---|---|---|---|---|
| M1 | **Operation seam** `internal/op`: extract §3 ops (op.Context + emitter shapes from day one); CLI calls seam in-process; zero behavior change — the existing CLI test files pass unmodified. `op` is a LEAF over `internal/loop` and imports neither `internal/cli` nor any transport package — the §7.4 dependency direction (B2), with a direction test landing here | 1,500 | deep-review (design) + codex | — (merged; no standalone tag) |
| M2 | **Spec**: the normative Hub wire & lifecycle section + §9.1 amendments + §9.2 EXTENSIONS.md + enrollment-surface deltas (G-M4). Prose only | 350 | deep-review + codex (spec PRs always) | — |
| M3 | **Wire codec + `hub session` + conformance**: frames/codes/handshake + fake-transport & fuzz tests (§10.2); forced-command entry, op dispatch, pinning, deadlines, streaming emitters; the §6.9 boot-**ref** pid-proof in `lease.go` (`ServeClaim.BootRef` + the two per-boot-UUID sources + the equality rule in branch (d)); **L vectors + fake-transport W vectors** | 2,450 | grok review; opus-xhigh deep pass on §4.4.3 producer rules (+ security riding) | — |
| M4 | **Client transport + remote discovery**: `ssh://` grammar, §6.8 env/discovery contract **including rule 5's launcher fix in `internal/cli/dispatch.go` + `internal/cli/shims.go`** (forward the URL, omit `--loop-dir` — B4; ships with the discovery arm, never after it), shadow dir, ssh invocation builder, one-shot ops (send/check/ack/status/gate/pending/boot/clean-owed) with the four remote `Vendor:nil` call sites (S2), spool + ambiguity handling, hook degradation + 30 s negative cache | 1,350 | grok review; opus-xhigh deep pass on §6.8 + resolver | — |
| M5 | **Remote serve channel + join UX**: lease lifecycle over the channel, tick, single-writer, fence-on-drop both sides, default-on relaunch (§6.7); `hub join`/`hub authorize`/doctor group/error catalog — the §7 UX exactly (auto-authorize, probe-before-pointer, key idempotence, `--list` validation, env warnings) | 2,400 | grok review; opus-xhigh deep pass on §7.2 key lifecycle | — |
| M6 | **Real-sshd integration matrix + CI (§10.3–10.4) + sshd-backed W vector runs + docs** (quickstarts, Tailscale recipe, README pointer) | 1,150 | grok review + opus-xhigh vectors-only deep pass | **v1.6.0** after green on ubuntu + macos |

Total ≈ 9.2k LOC — larger than the synthesis's 300–600-line naive-hub sketch
because the consensus itself priced the seam extraction at 1.5–3k and added
the lifecycle/pinning/test release gates; this is the honest cost of "zero
issues, very seamless". Order constraints: **no tag until after M6** (C6
constrains released artifacts; publication is merge; #154/#156 pin the
published spec to the latest release); M2 before any hub code (Working
rule 1); M3→M4→M5 sequential (each consumes the previous layer); M6
gates the **v1.6.0** tag — no tag while any conformance vector or the
sshd matrix is red. Reviewer-gate names: **codex implements M3–M6**;
**grok reviews** every remaining merge; **opus-xhigh** one named deep
pass per merge (see PLAN.md §4). Codex is not the second gate on merges
it implements.

---

## 12. Open questions

Deliberately near-empty; everything the brief listed as "choose and specify"
is decided above. Two items are recorded as *resolved-with-rationale* rather
than open:

1. **Offline hook behavior** — RESOLVED in v1 by the 30 s negative cache
   (§7.5, G-M3): at most one 5 s ConnectTimeout per 30 s across all hooks
   while the hub is down. The richer idea — caching pending *content*
   (last-tick counts/entries) for offline display — stays deliberately
   unbuilt (subtract-default: don't build a cache nobody has missed).
2. **Windows remotes / Windows hubs** — out of scope, matching the spec's
   existing tested-targets posture (AGENTCHUTE.md §2: native Windows is out
   of scope for `serve`); the hub requires a Unix host for the same reasons
   the pool already does (flock/PTY semantics).

No other open questions.
