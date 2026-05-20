# Wrapper-native loop R&D — Round 3 synthesis

Three teams, two rounds of independent design + cross-review. Converged answer
below. Sign-off in progress from codex + gemini-cli.

---

## Protocol-purity anchor (Alex's reframing)

agentchute is **best-effort**. The contract is:

> Sender's durable protocol responsibility is delivering the inbox entry.
> After delivery the sender MAY attempt the recipient's declared
> convenience adapter as best-effort operational hygiene; wake success is
> not required for correctness.
>
> Recipient is responsible for discovering its own inbox on its own cadence
> and processing messages according to the §6.3 / §6.4 / §11 recipient
> flow.

That's the protocol's wake/discovery shape. Anything beyond this is a
**convenience layer**, not a protocol requirement.

**tmux `send-keys` is a convenience adapter.** Sender-side; reaches into the
recipient's environment to type the wake string. Works when both agents
share a tmux server. It is NOT how the protocol discovers mail.

**The protocol's discovery mechanism is recipient-side polling.** The
`wake_method` registration field names an optional convenience adapter
(tmux, HTTP, SSH, ntfy, relay), NOT a correctness requirement. Empty
`wake_method` is fully protocol-compliant: it means "I poll my own inbox."

---

## The recipient-side polling tiers (the protocol-compliant answer)

Three tiers, all 100% protocol-compliant. They differ only in *when* the
recipient polls — at native-loop intervals, on external-scheduler ticks, or
at wrapper lifecycle boundaries. In every case: sender writes file, recipient
discovers via own polling.

### Tier 1: Native recurring task (zero new infrastructure)

When the wrapper has a built-in recurring-task primitive, use it.

| Wrapper | Mechanism | Cadence |
|---|---|---|
| **Claude Code** | `/loop [interval] [prompt]` — self-prompts on cadence, fires UserPromptSubmit hook per tick. Documented at code.claude.com/docs/en/scheduled-tasks.md | 5m default; operator tunable |
| **Codex App** (desktop) | Native Automations: minute intervals, custom cron, thread/standalone/project automations | Per Automation config |
| **Codex CLI (terminal)** | No native loop today (0.131); see Tier 2 |
| **Gemini CLI** | No native loop today (0.42); see Tier 2 |

**Setup**: a single `/loop` line, or a single Automation config. The enrollment
block teaches the wrapper-specific pattern. No new agentchute infrastructure.

**Cost**: each tick = one model turn. 5m × 24h ≈ 288 turns/day. Operator picks
the cadence vs. cost trade-off.

### Tier 2: Headless pulse with side-effect-free preflight

For wrappers without a native recurring task. An operator-owned scheduler
(launchd/systemd/cron/while-loop) decides when to wake the wrapper.

**Pattern** (codex's preflight insight):

```sh
# Every 15-30s. Side-effect-free, cheap.
agentchute pending --as <id> --fail-if-any
# rc=0 → idle, do nothing
# rc=2 → work exists, launch the wrapper:
codex exec --cd <repo> --profile <profile> \
  'Process agentchute mail. Run `agentchute check --as codex` first; reply or defer.'
# OR
gemini -p 'Process agentchute mail. Run `agentchute check --as gemini-cli` first; reply or defer.'
```

**Why this is the right shape**:
- The preflight (`pending --fail-if-any`) is read-only. It does NOT consume
  mail — that's protocol-pure. The scheduler is just asking the inbox
  "anything for me?"
- The wrapper is only launched when work exists. Zero idle token cost.
- The model owns consumption. `check` runs inside the wrapper's model turn,
  not in the scheduler.
- Single-flight lock (`flock`/launchd `KeepAlive`/systemd unit) prevents
  concurrent wrapper launches racing on the same inbox.

**Cadence**:
- Preflight every 15-30s on local FS (cheap).
- Wrapper launch only on rc=2 (expensive but bounded).

**Setup friction**: medium. Operator writes a wrapper script + a service unit.
**Mitigation**: `agentchute doctor --generate-service` (consensus across all
three teams) emits launchd/systemd unit files for the standard preflighted
scheduler pattern. Drops setup to one command.

### Tier 3: Finish-hook continuation (in-session catchup)

When the wrapper is already running and a new message arrives mid-session,
the finish-hook scan catches it without needing a wake from outside. This is
NOT a wake method (the wrapper has to already be alive) but it's a clean
catchup mechanism that pairs with tiers 1 and 2.

| Wrapper | Hook | Mechanism |
|---|---|---|
| **Claude Code** | Stop | `gate --before finish` exits 2 → Claude continues the turn |
| **Codex CLI** | Stop | `gate --codex-hook Stop` emits `{"decision":"block","reason":"..."}` → codex continues |
| **Gemini CLI** | BeforeAgent / AfterAgent | `gate --before finish` exits 2; AfterAgent can return `{"decision":"deny","reason":"..."}` |

**Codex's insight**: codex's existing Stop hook (today) ALREADY scans the
current inbox at Stop time, so "new arrival since this turn started" is
already caught. No new mode needed for codex.

**Gemini's proposal** (round 2): promote this to a first-class command via
`agentchute gate --before continue --as <id> --json`. Returns
`{"decision":"deny","reason":"new mail from <sender>: <task>"}` on
non-empty inbox, `{"decision":"allow"}` on idle. Makes the gemini
AfterAgent integration a one-liner in `.gemini/settings.json` instead of
operator-glued shell.

---

## Where do tmux / HTTP / SSH / relay sit?

**Convenience adapters layered on top of the protocol-compliant baseline.**
None of them replace recipient polling. They give the recipient's polling
loop an *optional* low-latency wake hint.

- **tmux send-keys**: works when both agents share a tmux server. Sender
  pokes a wake string into the recipient's pane, which becomes input to the
  running wrapper. Convenient for single-host tmux setups. **Not protocol.**
- **HTTP webhook / SSH nudge / ntfy / relay** (from the prior R&D cycle):
  same shape — sender notifies the recipient's listener; recipient's
  listener triggers the local processing flow. Useful when sub-second wake
  latency matters or when the operator wants automation. **Not protocol.**

These remain documented as **optional adapters** in `AGENTCHUTE.md §8`
(adapter contract) and `EXTENSIONS.md` (per-substrate recipes). They never
shoulder the wake responsibility — recipient polling does.

The clean protocol text: *"Wake is best-effort and never required. Senders
SHOULD attempt wake via the recipient's declared `wake_method` if one is
configured; failure is logged and ignored. Recipients MUST NOT depend on
external wake — they discover mail via their own polling cadence."*

---

## v0.2 deliverables

Pure-additive on top of v0.1.3. Nothing removed; nothing protocol-changed.

### Code

- **`agentchute gate --before continue`** (new): additional hook-friendly
  mode optimized for "should the wrapper immediately continue into another
  turn?" — primary use is gemini-cli's `AfterAgent` decision-deny pattern.
  Side-effect-free; blocks/denies on unread/malformed/pending-reply state
  only. Does NOT replace or weaken the existing `gate --before finish`
  family; it's a sibling mode for mid-session interrupt vs end-of-turn
  finalization. Output shapes can be wrapper-specific
  (`--gemini-hook AfterAgent`, generic `--json`).
- **`agentchute doctor --generate-service`** (new): **emits** (prints or
  writes) launchd/systemd unit files for the preflighted-scheduler pattern,
  per wrapper. The operator chooses to install/load/start; doctor never
  starts a background agent on its own. Daemonization friction drops from
  "write the unit file from scratch" to "review + load the generated unit."
- **`agentchute self-poll --as <id>`** (deferred — codex recommends NOT a
  v0.2 blocker; gemini wants it for docs UX): if shipped, must be a
  side-effect-free alias/helper over `pending` with `--json` (scheduler
  view) and `--prompt-text` (model-facing fragment). Never introduces new
  state or consumption. `pending --fail-if-any` already covers the
  scheduler-preflight case; this is purely docs-consistency sugar.

### Docs

- **AGENTCHUTE.md §8** clarification: wake is a best-effort convenience, not
  the protocol's discovery mechanism. Recipient polling is canonical.
- **Enrollment block additions** (per wrapper):
  - `CLAUDE.md`: teach `/loop 5m` as the recommended self-poll pattern.
    Pair with a `.claude/loop.md` containing the inbox-processing prompt.
  - `CODEX.md`: teach the preflighted-scheduler pattern (CLI) and the
    Automation pattern (App), with a note on Stop-hook continuation.
  - `GEMINI.md`: teach the preflighted-scheduler pattern + the
    `AfterAgent` continuation pattern using `gate --before continue`.
  - `AGENTS.md`: cross-wrapper summary.
- **`web/blog/`** (next post): "Recipient polling, by hand and by hook" —
  the protocol-purity story.
- **README**: update "Running without tmux" to lead with the preflighted
  scheduler + per-wrapper native-loop section.

### Spec text

Addition to AGENTCHUTE.md §8 (adapter contract) with a cross-reference from
§10 (cooperative waking):

> **§8.x Wake responsibility.**
>
> The protocol's discovery mechanism is recipient-side polling. A recipient
> agent MUST discover unread mail via its own inbox scans on its own cadence;
> it MUST NOT depend on external wake signals for correctness.
>
> Wake adapters (tmux, HTTP, SSH, etc.) are **best-effort convenience
> optimizations** that reduce polling latency. Senders MAY attempt wake via
> the recipient's declared `wake_method`; failure is logged and ignored.
> Recipients MAY use external wake hints as additional signals but MUST
> remain correct in their absence.
>
> The `wake_method` registration field declares the recipient's preferred
> convenience adapter, NOT a protocol requirement. Empty `wake_method` is
> equivalent to "I poll my own inbox."

And in §10:

> Cooperative waking (§10.5) and watchdog liveness (§10.1) are latency
> accelerators over the §8.x recipient-polling correctness model; they
> are operational hygiene, not protocol obligations.

---

## Three open questions for Alex

1. **`agentchute self-poll`**: codex recommends defer (not a v0.2 blocker;
   `pending --fail-if-any --json` already covers scheduler needs). Gemini
   wants it for cross-wrapper docs UX. My read: defer unless docs
   duplication becomes visible during the v0.2 build, then revisit.

2. **`doctor --generate-service`**: scope. Just emit unit files for the
   wrapper's preflighted-scheduler tier-2 pattern? Or also generate units
   for `agentchute watch` sidecars and the v0.1.2-shipped commands?

3. **Spec text placement**: consensus is §8 (adapter contract) for the
   normative wake-responsibility text, with a §10 cross-reference noting
   cooperative waking and the watchdog are latency accelerators over the
   §8.x model. Confirmable on commit time; no friction either way.
