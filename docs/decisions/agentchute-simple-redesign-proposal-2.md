# Proposal: agentchute, simple again — pull mailbox + pull presence + per-agent PTY supervisor

**Status:** Draft for maintainer review
**Audience:** agentchute maintainers
**Origin:** clean-slate redesign. Forget the current implementation; this is how the system looks if built from scratch against the same requirements.

---

## 1. Recommendation (read this, skip the rest if pressed)

- Three coordination concerns, all served by **pull**, never push:
  - **delivery** → inbox directory + atomic no-overwrite write
  - **reply obligation** → recipient-local gate on its own `reply_required`
  - **presence** ("who's on the team, who's active") → one `.live` file per agent, read on demand
- One OS-specific component: **`serve`**, a per-agent PTY supervisor. It launches the agent, polls that agent's inbox, injects the consume trigger, and writes the agent's `.live`. Wake, liveness, and busy/idle all fall out of the single PTY fd `serve` owns.
- **Delete** the entire push apparatus: watchdog, cooperative waking, sender-side wake, published wake state, reachability cache, status/restart/provenance fields, vendor-namespacing, the five-tier polling model.
- **Drop herdr.** It re-implemented tmux's job (external pane poke) with more parts (name resolver, handle≠name, two-step send, precedence rules, doctor probe) plus an external binary dependency — near-zero gain over tmux, and **zero** gain over `serve`, which makes external pane-poking unnecessary.

**This is primarily a correctness argument, not a simplicity one.** Push presence is not just heavy, it is *unreliable*: reachability caches go stale, the watchdog races, gates fire on phantom liveness. Parent-child supervision (`serve`) is **ground truth** — alive = the process I spawned is running. You trade guesswork for a fact; simplicity is the byproduct.

Net shape: a ~200-LOC stdlib mailbox + one PTY supervisor. Everything deleted was compensation for push-based wake.

---

## 2. Why it grew (the actual diagnosis)

The mailbox + pull core was never the problem; it is small. Growth came from one **unstated requirement**: a team needs to know who is present and active. That requirement got answered as **push** — every agent tracking every other, via a watchdog, reachability re-proving, and a registration record that accreted ~18 fields (`last_seen`, `status`, `restart_at`, `last_active`, `reachable_at`, `reachability_method/target/error`, `launched_by`, `shim_name`, `hook_event`, `wake_method/target`, reserved `wake_endpoints`).

Push presence is O(agents²) attention and forces liveness caches, reprove ticks, and cross-agent gates. **Presence is legitimate. Push is the mistake.** Make presence pull — one file per agent, read when you want the roster — and the requirement is met in one field.

herdr is the same mistake one layer down: it added weight to the *poke* path (the thing that exists only because wake was push) without removing any of the push machinery.

---

## 3. Protocol (normative surface — keep this tiny)

Reimplementable in ~30 lines of POSIX shell. Everything else in this document is reference implementation, not protocol.

**Layout**
```
.agentchute/loop/            # fixed dir, no vendor namespacing
  inbox/<id>/                # an agent's existence == this directory
    .live                    # presence (recipient-written)
  archive/                   # consumed messages
  malformed/                 # quarantined
```

**Primitives**
- **Inbox = directory named `<id>`.** Its existence is the registration. No registry file, no enrollment schema.
- **Message = file** written into `inbox/<to>/` with **atomic no-overwrite** (`O_EXCL` create, or `link()`). On collision, regenerate nonce and retry.
- **Identity + order = filename:** `<utc-microseconds>-<nonce>-<from>.md`. Oldest-first is lexical sort. **One clock: the filename. Never read mtime for ordering** (removes the two-clock skew in the current design).
- **Consume contract = decide crash semantics explicitly.** The naive "move-to-archive then act" is **not** exactly-once: a crash between archive and act silently loses the message. Pick one and write it in the spec:
  - **act-then-archive** → at-least-once; a crash re-delivers. Requires **idempotent handlers**. *Recommended for coordination.*
  - **archive-then-act** → at-most-once; a crash drops the message. Only if loss is acceptable.
  Only the recipient consumes its own inbox. (The current spec is ambiguous here too — fix both.)
- **Body = free text.** Optional frontmatter: `from`, `reply_required`, `in_reply_to`. Nothing else normative. Receivers ignore unknown fields.
- **Presence = `.live`** (see §5). Recipient-written, read by anyone building a roster.
- **No published wake state.** Senders write files; they never need to know how a recipient wakes.

**Shell proof (the whole bus):**
```sh
send() {  # send <to> <from> <body>
  ts=$(date -u +%Y%m%dT%H%M%S%6NZ); n=$(od -An -N2 -tx1 /dev/urandom|tr -d ' \n')
  f=".agentchute/loop/inbox/$1/$ts-$n-$2.md"
  ( set -C; printf '%s\n' "$3" >"$f" ) || { sleep 0.01; send "$@"; }   # set -C == no-overwrite; retry on collision
}
recv() {  # recv <me>
  for f in $(ls ".agentchute/loop/inbox/$1" 2>/dev/null | grep -v '^\.' | sort); do
    mv ".agentchute/loop/inbox/$1/$f" ".agentchute/loop/archive/"; cat ".agentchute/loop/archive/$f"
  done
}
roster() {  # who's present
  for d in .agentchute/loop/inbox/*/; do id=$(basename "$d")
    [ -f "$d/.live" ] && awk -v id="$id" 'NR==1{print id, $0}' "$d/.live"
  done
}
```

---

## 4. `serve` — the only OS-specific component

A PTY is a **kernel primitive** (`openpty`/`/dev/ptmx`), not a terminal-emulator feature. POSIX-only (WSL covers Windows) = one code path. `serve` owns the PTY of the child it spawns, so it is **indifferent to whatever runs above it** — bare shell, terminal tab, tmux pane, nested arbitrarily. No pane ids, no layout coupling, no tmux required.

**Behavior**
```
master, slave = openpty()
child = spawn(<agent-cli>, stdin=slave, stdout=slave, stderr=slave)
write_live(id, last_seen=now, busy=false)
loop ~1s (or when master readable):
    if child exited: rm .live; exit
    bytes = drain(master)                       # observe the agent's own output
    busy  = bytes_flowing(bytes)                # heuristic: output streaming = busy; quiet at prompt = idle
    if new_files(inbox/<id>/): write(master, "<recv-trigger>\n")   # nudge; agent runs its own consume
    write_live(id, last_seen=now, busy=busy)
```

- **Wake is reliable** — `serve` owns the channel, so the inject cannot miss (unlike external `tmux send-keys`, which can land mid-generation or hit a shifted pane).
- **Liveness is free** — `serve` is the agent's parent; child pid alive = present. No reachability cache, no socket probe, no reprove.
- **Busy/idle is free** — `serve` reads the agent's output stream off the master fd. Heuristic but cheap and local; honest because it observes the real stream.
- **`serve` does not consume mail** — it injects the trigger; the agent runs `recv`. Recipient-owned consumption preserved.

**Poll, don't filewatch.** A 1s poll on `inbox/<id>/` erases inotify/kqueue/event-API divergence at zero cost; handoff timescales are seconds, not microseconds.

**Constraints (state them plainly):**
- `serve` must *launch* the agent. It can only own the PTY of a child it spawned; it cannot adopt an already-running agent.
- One `serve` per agent (one PTY per agent).

---

## 5. Presence — `.live`

```
last_seen: 2026-06-29T18:00:00.000000Z
busy: true            # optional; only honest under serve (PTY). omit for tmux-tier.
```

- **Written** by the agent's supervisor each tick (`serve`), or by a native-loop agent for itself. **Write atomically** — `.live.tmp` then `rename()` over `.live` — so a reader never catches a torn file and misreads it as dead. Same discipline as messages.
- **Roster** = list `inbox/*/`, read each `.live`:
  - `.live` fresh → **active** (and `busy`/`idle` if present)
  - `.live` stale or absent → **dead/idle** — the 4-days-later case: stale `last_seen` ⇒ not on the team
- This single file replaces the entire field cluster in §2. No watchdog, no cross-agent poking — presence is read on demand.

`busy` vs `idle` is only trustworthy under `serve`, because it is a property of PTY ownership — the same property that makes the poke reliable. Do **not** derive it from tmux (`capture-pane` scraping is fragile and racy). tmux-tier agents expose `last_seen` only.

---

## 6. Agent tiers

| Tier | Wake | `.live` | Notes |
|---|---|---|---|
| **native loop** (Claude) | self-polls `recv` on its own loop | self-written | no `serve`, no external poke |
| **loopless** (codex, gemini, grok) | `serve` injects trigger | `serve`-written, incl. `busy` | the common case; `serve` mandatory — a loopless idle agent has no turn to recover a missed poke |
| **un-wrappable** | none (no automatic wake) | `last_seen` only, if a human runs it | a human triggers `recv` manually in any terminal; **unsupported** if loopless with no human |

> **No tmux poke anywhere.** A sender dialing a recipient's tmux pane is sender-push and violates the send→read separation, so tmux is **not** a wake adapter in this design. A human manually running `recv` (in tmux or any terminal) is recipient-initiated and fine, but that is a human action, not a protocol wake mechanism. tmux survives only as an environment `serve` or a human may run inside — never as a way to wake a peer.

Loopless agents **require** `serve`; tmux is not a safe substitute for them (miss = stuck-forever). This is the conclusion that decides the architecture: the agents that actually need waking can't use the portable-but-lossy path.

---

## 7. Who starts `serve` (resolving the open seam)

**Recommendation: per-agent, decentralized.** The agent's launch command *is* `serve -- <agent-cli>`. One supervisor per agent, no central runtime process, no shared authority. This matches the original pull intent end-to-end.

- A pool-level launcher is acceptable **only** as setup sugar that spawns N independent per-agent `serve` processes and then holds nothing at runtime. It must not become a broker. If it would, drop it.

---

## 8. What this deletes (mapping to the current tree)

> Conceptual mapping; I read the docs and file listing, not every `.go` line. Treat file names as targets to verify, not surgical instructions.

| Current | Fate | Why |
|---|---|---|
| `watchdog.go`, cooperative waking in `check` | **delete** | push presence; not needed under pull |
| `recipient_liveness.go`, `runner_reachable.go`, reachability fields (`reachable_at`, `reachability_*`) | **delete** | parent-child liveness from `serve` |
| `status`, `restart_at`, `last_active`, `launched_by`, `shim_name`, `hook_event` | **collapse → `.live`** {`last_seen`, optional `busy`} | one pull file replaces the cluster |
| `wake_method`/`wake_target`/`wake_endpoints` | **delete** | recipient-pull publishes no wake state |
| `poller.go`, `self_poll.go`, five-tier polling model | **collapse** | `serve` loop, or native-loop self-poll |
| herdr adapter, `herdr_state.go` | **drop** (or community example) | weight, ~zero gain over tmux; zero over `serve` |
| tmux wake adapter (`send-keys` poke), `tmux_state.go` | **drop as a wake path** | sender-push; violates send→read. tmux remains only as an environment, never a poke |
| `shims.go` / `ac-*` launcher shims | **collapse → `serve`** | `serve -- <cli>` is the single launch path |
| vendor-namespacing, `migrate.go`, the `.rehumanlabs` migration | **delete** | fixed `.agentchute/loop/` removes the ambiguity class |
| enforced-enrollment registry (`register.go` heavy path) | **collapse** | inbox dir + `.live` existence |
| `gate.go` liveness branch | **trim** | gate on own `reply_required` only |
| mtime-staleness path (§10.1c) | **delete** | one clock: filename |

Remaining core: mailbox (`send`/`recv`/`addr`), `serve`, presence (`roster`). The `creack/pty` dependency stays only inside `serve`; the bus is pure stdlib (and shell-reproducible).

---

## 9. Migration (no big bang)

1. Land `serve` as a new launcher alongside the existing runner; it already does what the runner does plus `.live` and busy/idle. Existing runner users switch by changing the launch command.
2. Add `.live` writing to `serve` and native-loop agents; add `roster` reading `.live`.
3. Flip `gate` to own-obligations-only.
4. Stop writing the reachability/status/provenance fields (readers already ignore unknowns; absence is safe).
5. Remove watchdog/cooperative-wake from the `check` path.
6. Demote herdr to example; pin the namespace; delete `migrate.go` after a release.
7. Trim the spec to §3 of this document; move everything else to a reference-CLI appendix.

Each step is independently shippable and backward-tolerant (unknown-field ignore + durable inbox).

---

## 10. Tradeoffs (candid)

- **N supervisor processes** (one `serve` per loopless agent). For 2–10 agents on one host this is strictly simpler than an awake-tracking matrix. Accept.
- **~1s wake latency** from polling. Irrelevant at handoff timescales.
- **`serve` must launch the agent** and owns one PTY per agent — cannot adopt running agents.
- **busy/idle is heuristic** (output-stream observation), not a contract.
- **POSIX-only** by choice; Windows via WSL.
- **The one real regression:** a zero-binary, hand-protocol *loopless* agent loses automatic wake. The old tmux-by-hand path let a human poke with no binary at all; under serve-only it needs `serve` or a human to trigger `recv`. This is an accepted trade, named explicitly — it is the only capability the design gives up, and it is a runtime convenience, not a protocol/Markdown loss (the agent still participates over files end-to-end).

### When this proposal is wrong (kill-criterion)

The whole design rests on one empirical claim: **for 2–10 agents on a single host, N supervisor processes are simpler and more reliable than cross-agent liveness tracking.** Test it before committing. If the real target is dozens of agents, or if multi-host is a hard requirement rather than an edge case, the conclusion flips — several deletions (filename-only clock, parent-child liveness, no distributed transport) become wrong, and the push machinery starts earning its keep. Name your scale and host model first; the rest follows from it.

---

## 11. Non-goals

- No auth, signing, or encryption (cooperative trust; only hard rule is **argv-only wake, no shell-eval**).
- No routing, coordinator, or wildcard inboxes (peers address explicitly).
- No cross-agent liveness tracking (deleted on purpose).
- No busy/idle from tmux scraping.
- No speculative wake adapters in-tree; the community model (one `Poke`-equivalent) covers them.

---

## 12. Open questions for the team

1. Stale threshold for `.live` (single value, e.g. 3× poll interval). Pick one default.
2. Is `busy` wanted in v1, or `last_seen`-only sufficient for current coordination? (Adds nothing to `serve`; only question is whether the roster consumes it.)
3. Consume contract (§3): confirm **act-then-archive + idempotent handlers** (at-least-once), or accept at-most-once.
4. **Is single-host a permanent constraint or a current convenience?** This is the load-bearing assumption (see kill-criterion). If multi-host is on the roadmap, weigh it now — several deletions are hard to reverse later.
