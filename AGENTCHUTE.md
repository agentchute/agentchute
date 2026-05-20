# AGENTCHUTE.md

*Open spec for inbox-based agent coordination. Protocol working draft v1; reference CLI v0.1.*

---

## 1. Purpose

A small convention for two or more agents (humans, AI assistants, or both) to coordinate through **shared inboxes**. Designed for explicit handoffs: agent X writes a message into agent Y's inbox; if Y declared a reachable `wake_method` and `wake_target`, X also pokes Y with `check` via the corresponding wake adapter for direct wake-up. Without a reachable wake method, Y picks up the message via its own polling cadence (wrapper loop, watchdog, or manual `check`).

### Protocol primitives (implementation-agnostic)

The protocol is a small set of implementation-agnostic primitives. Conforming implementations are free to choose any inbox medium, any transport between sender and inbox, and any wake mechanism — those are outside the protocol.

- **Per-recipient inbox.** Each agent has its own ordered message stream. Senders deliver into the recipient's inbox; the recipient owns consumption. **The inbox medium is implementation-specific** (a filesystem directory, a queue topic, an HTTP endpoint, an object-store prefix, a git branch — any medium that preserves these primitives).
- **Ordered, identified messages.** Each message carries a unique `(timestamp, sender, nonce)` identity tuple (§6.1.1). Receivers and implementations MUST process inbox entries oldest-first by timestamp with deterministic tie-breaking within equal timestamps; the v0.1 reference filename encoding (§6.1.2) realizes this as lexicographic sort on the filename.
- **No-overwrite delivery.** Delivery never silently clobbers an existing inbox entry. If two senders produce the same identity, the second sender retries with a fresh nonce. **Transport from sender to inbox is implementation-specific** — atomic rename, HTTP POST, queue publish, git push, etc.
- **Recipient-owned consumption.** Only the recipient reads its own message bodies. Liveness checks (watchdog or cooperation) use inbox metadata only.
- **Optional wake.** Senders MAY signal the recipient via a pluggable wake adapter; if no adapter is invocable, the message still lands and the recipient picks it up on its own cadence. **The wake mechanism is implementation-specific** — terminal-multiplexer keystroke injection, HTTP webhook, system notification, OS signal, etc. The recipient's own self-poll loop (e.g., Claude Code's `/loop` feature) is an equally valid fallback when no peer-driven wake is reachable.
- **Self-registration.** Each agent publishes a small registration record naming itself, its wake method/target (if any), and operational metadata.

These primitives say nothing about *how* messages get stored, transmitted, or signaled. An implementation that preserves them is a conforming agentchute implementation.

### Reference implementation (v0.1)

The v0.1 reference CLI maps these primitives onto the three concrete choices we've been dogfooding with:

- **Inbox medium**: plain `.md` files on a shared filesystem, organized under a vendor-namespaced loop directory at the repo root (§3, §6).
- **Transport**: atomic create-temp + rename (the local-filesystem realization of no-overwrite delivery).
- **Wake** — two-sided:
  - *Peer→peer*: `tmux send-keys` injection of the literal string `check` into the recipient's pane (the v0.1 reference wake adapter, §8).
  - *Recipient self-poll fallback*: the wrapper's own polling loop. Claude Code's `/loop` feature is the in-wrapper path; codex CLI and Gemini CLI have no built-in self-loop and use an operator-owned scheduler that invokes the wrapper. See §8.1 and `README.md` for the per-wrapper patterns we've verified.

These three choices are reference choices, not protocol requirements. The reference implementation worked well enough to be exclusively used for the work that produced this protocol — but a conforming implementation could pick a different inbox medium, transport, wake adapter, or self-poll mechanism and still interoperate.

**The protocol does not bake in locality or timing.** The v0.1 reference CLI uses a shared filesystem and tmux wake pokes because that is the implementation this project needed first, not because the protocol requires co-located agents or any particular response time. A conforming implementation can put the inbox medium behind a queue, HTTP endpoint, object-store prefix, git-backed transport, or any other substrate that preserves the primitives above, and can pair it with whatever wake or polling mechanism fits that deployment. Agents can run in one tmux session or on different machines, networks, or regions and still coordinate through the same model: per-recipient inboxes, ordered messages, no-overwrite delivery, recipient-owned consumption, optional wake, and self-registration. Location, latency, and polling cadence are implementation properties; the protocol boundary is the inbox contract.

Sections §3 and §4 describe the filesystem mapping in detail. §5 and §6 each follow a two-layer pattern: protocol-level subsections (§5.1, §6.1.1, §6.2.1, §6.3.1, §6.4.1) define the contract any conforming implementation honors, and reference-encoding/mechanics subsections (§5.2, §5.3, §6.1.2, §6.2.2, §6.3.2, §6.4.2) show how the v0.1 reference CLI realizes that contract on a shared filesystem. §7, §10, §11 define coordination defaults, liveness semantics, and best-effort correction at the protocol level; their filesystem-specific realizations are flagged inline as reference-CLI mechanics. See [`EXTENSIONS.md#alternate-transports`](EXTENSIONS.md#alternate-transports) for compatible alternate inbox media (including a worked git-as-transport sketch), and the surrounding sections in `EXTENSIONS.md` for wake-adapter and self-poll extensions.

### Protocol versioning

v1 has **no in-band version negotiation**. The `AGENTCHUTE.md` in the control repo is the governing spec for that pool — implementations participating in the same pool MUST agree on the protocol version by social convention (the spec file's stated version). Implementations MAY include an optional `protocol_version: v1` field on registrations and/or messages; absence means v1.

Future incompatible versions (v2 and beyond) will require a versioned spec / profile, not silent field drift. A receiver that encounters a `protocol_version` it doesn't recognize on an incoming registration or message MUST treat the artifact as structurally incompatible and quarantine per §11 (this is a §11 enforcement trigger, distinct from the "ignore unknown fields" rule below). Implementations SHOULD additionally log a warning when they process a message whose declared `protocol_version` is newer than their own, even when the message is otherwise parseable, so operators can see the version-drift early.

agentchute is a sibling tool to signed coordination protocols, not a replacement. The two have disjoint scopes — see §12.

## 2. Scope

### In scope (v1)

- **Inbox-based coordination** through per-recipient inboxes — the protocol primitive (§6.1.1). _Reference CLI: those inboxes are markdown files on a shared filesystem; single-machine is the common case, multi-machine works as long as all participants share one filesystem with atomic create/rename semantics._ Wake delivery remains local per recipient (§8, §10.5).
- **Pluggable wake adapters.** Each registration declares a `wake_method` and `wake_target` (§5); senders look these up and dispatch the appropriate poke. The reference adapter is `tmux` (see §8); other adapters MAY be implemented but are not part of v0.1. When no wake method is available or invocable, the recipient picks messages up via its own polling cadence (wrapper loop, watchdog, or manual `check`).
- **Small pools.** 2 to ~10 agents in a pool. Beyond that, peer-enumeration costs grow and the social dynamics shift toward routing/role-election, which is out of scope (§12, §13).
- **Substrate-defined pool locator.** Each pool has one deterministic way to resolve its registration set + inbox medium. _Reference CLI: a "control repo" containing `AGENTCHUTE.md` and the vendor loop directory (§3, §4)._
- **Free-form messages with optional structured envelope** (§6.4.1). _Reference CLI: Markdown body + optional YAML frontmatter (§6.4.2)._
- **Liveness-only watchdog** (§10).

### Out of scope (v1)

Short reader map; the canonical list of non-goals is **§12 Non-goals**. The headline exclusions are:

- **Non-filesystem inbox transports** in the v0.1 reference CLI (the protocol welcomes them; see [`EXTENSIONS.md#alternate-transports`](EXTENSIONS.md#alternate-transports)).
- **Durable / authenticated audit trail** — agentchute is a transient operational trace; for signed audit, layer a signed-envelope coordination protocol above.
- **Capability-based routing** — agent selection is human/agent judgment.
- **Coordinator / router / role-assignment agent** — the v1 watchdog (§10) is liveness-only; richer coordination is a v2 deferred item (§13).
- **Cryptographic signing / authentication** and **structured request/response state machines** — neither in scope; see §12 for the full treatment.

### Concurrency posture

agentchute is **concurrency-agnostic**: it neither enforces nor prevents simultaneous work by multiple agents. Two agents MAY work on different inbox messages at the same time. The protocol does not coordinate concurrency. The expected default is linear (one agent acting at a time per task, with explicit hand-offs in messages); this is convention, not mechanism.

### Required access (reference implementation)

Every enrolled agent in the v0.1 filesystem-backed implementation MUST have:

- Read/write access to its configured inbox medium. For the reference CLI that means `<repo>/.<vendor>/loop/` and its subdirectories on the shared filesystem; for an alternate implementation it means equivalent read/write access to whatever stores its inbox.
- If using a wake adapter (e.g., the tmux poke convention §8): ability to invoke that adapter for any pokable peer reachable from this machine. Optional — agents without an invocable wake method participate via polling.
- Read access to `<repo>/AGENTCHUTE.md` (this spec).

Optional, only for first-time bootstrap:

- `curl` or equivalent HTTP fetcher (to retrieve `AGENTCHUTE.md` itself).

agentchute makes no security guarantees. Peers share the configured inbox medium and (when a wake convention is in use) the substrate the wake adapter uses; the threat model is cooperative (§12).

## 3. Layout (filesystem reference implementation)

This section describes how the protocol primitives from §1 map onto a shared filesystem. The reference CLI places coordination state under a vendor-namespaced dotdir at the repo root:

```
<repo-root>/
  AGENTCHUTE.md                    # this spec (tracked)
  .<vendor>/                      # e.g., .rehumanlabs/
    loop/
      README.md                   # implementation notes (tracked)
      agents/
        README.md                 # registration format reference (tracked)
        <agent-id>.example.md     # tracked example registration
        <agent-id>.md             # LIVE registration (gitignored)
      inbox/
        <agent-id>/               # per-recipient inbox directory (gitignored)
          <utc-timestamp>_from-<sender>_msg-<nonce>.md
      archive/                    # consumed messages (gitignored)
        <consumed-timestamp>_to-<recipient>_<original-inbox-filename>
      malformed/                  # quarantined non-conforming files (gitignored)
      watchdog.log                # watchdog daemon log (gitignored, optional)
```

The `<vendor>` prefix anchors the namespace to a specific organization or domain. This avoids collision with other agentchute implementations that may exist alongside in the same repo. Examples: `.rehumanlabs/`, `.examplecorp/`. The vendor SHOULD own the namespace (e.g., domain registration) to claim it.

The reference agentchute CLI initializes new projects under `.agentchute/loop/` by default. Other implementations MAY use a vendor-owned dotdir namespace such as `.examplecorp/loop/`. Discovery accepts any single dotdir containing `loop/`; if multiple loop namespaces exist in one repo, callers MUST disambiguate via `AGENTCHUTE_LOOP_DIR` or `--loop-dir`.

### Tracked vs gitignored

`AGENTCHUTE.md` is the only file under this layout that MUST be tracked. The reference files under the loop directory (`loop/README.md`, `loop/agents/README.md`, `loop/agents/*.example.md`) are OPTIONAL — useful for vendor implementations distributing template content, but a fresh `agentchute init` does not require them. Live runtime state (registrations, inbox, archive, watchdog log) SHOULD be gitignored.

- **Tracked (when present)**: `AGENTCHUTE.md`, `.<vendor>/loop/README.md`, `.<vendor>/loop/agents/README.md`, `.<vendor>/loop/agents/*.example.md`.
- **Gitignored**: `.<vendor>/loop/agents/*.md` (live registrations contain machine-specific paths and frequently-updated `last_seen`), `.<vendor>/loop/inbox/`, `.<vendor>/loop/archive/`, `.<vendor>/loop/malformed/`, `.<vendor>/loop/watchdog.log`.

Recommended `.gitignore` patterns:

```gitignore
.<vendor>/loop/agents/*.md
!.<vendor>/loop/agents/*.example.md
!.<vendor>/loop/agents/README.md
.<vendor>/loop/inbox/
.<vendor>/loop/archive/
.<vendor>/loop/malformed/
.<vendor>/loop/watchdog.log
```

(Negation rules in gitignore are order-sensitive: ignore first, then negate exceptions.)

## 4. Discovery (filesystem reference implementation)

The cascade below describes how the v0.1 reference CLI locates its control repo and loop directory on a shared filesystem. Implementations on other substrates (a queue topic name, an HTTP endpoint URL, an object-store prefix) supply their own discovery / locator mechanism — the protocol requires only that an agent can deterministically resolve its pool's inbox medium and registration set, not that the resolution use filesystem paths.

Discovery resolves two distinct things — the **control repo** (the directory containing `AGENTCHUTE.md`) and the **loop directory** (the `.<vendor>/loop/` directory inside the control repo). Each has its own most-explicit-first cascade. First hit short-circuits the rest.

### 4.1 Control repo cascade

1. **`--control-repo <path>` flag.** Validation: path must exist, be a directory, and contain `AGENTCHUTE.md`.
2. **`AGENTCHUTE_CONTROL_REPO` env var.** Same validation as the flag.
3. **`.agentchute-control-repo` pointer file.** Walk from cwd up to the filesystem root looking for the file. The nearest pointer wins; any others found in the ancestor chain are recorded as *shadowed* (informational, surfaced in `status`). The pointer file contains one non-comment path line (blank lines and `#`-prefixed comments are allowed). Relative paths resolve from the pointer file's directory; absolute paths are accepted but discouraged in tracked files because they break across machines and clones. The resolved target must contain `AGENTCHUTE.md`. Sibling-repo pointers like `../coordination` are the primary use case — the resolved path is allowed to escape the pointer file's containing repo. A malformed pointer file or pointer to an invalid target is a HARD error; discovery does NOT silently fall through to the next step.
4. **Cwd walk.** Walk from cwd up to the filesystem root looking for the first directory that contains both `AGENTCHUTE.md` AND at least one vendor-namespaced loop directory (e.g., `.rehumanlabs/loop/`). Use it as the control repo.

If all four steps fail, control-repo discovery fails with a clear error pointing operators at the four entry points.

### 4.2 Loop dir cascade

Once the control repo is resolved, the loop dir is selected inside it:

1. **`--loop-dir <path>` flag.** Validation: must exist, end in `loop`, and live under a vendor dotdir. Relative paths resolve from the control repo.
2. **`AGENTCHUTE_LOOP_DIR` env var.** Same validation as the flag.
3. **Auto-discover.** Enumerate `.<vendor>/loop/` directories at the control repo root. Exactly one match → use it. Zero → error (control repo has no vendor loop dir yet; run `agentchute init`). More than one → error and ask for `--loop-dir` / `AGENTCHUTE_LOOP_DIR` to disambiguate.

### 4.3 After discovery

The agent self-enrolls per §7.2 (mandatory on every session start), writes its own registration file (§5), creates its inbox directory if missing, and starts processing its inbox.

Discovery origin (which cascade step won, plus the resolved path) is surfaced by `agentchute status` and `agentchute register` for debugging; multi-repo setups with shared filesystems are easier to debug when the resolved control repo's origin is visible. See §7.6 for the cross-pool agent pattern that exploits the same discovery machinery.

## 5. Registration

Every agent in a pool MUST publish a registration record so other agents can address it and (optionally) wake it. Without registration, peers cannot reliably discover or wake you — direct delivery to a known inbox can still work, but cooperative waking (§10.5), the watchdog (§10), and per-pool peer enumeration depend on the registry.

### 5.1 Protocol-level registration fields

These fields are protocol-level: any conforming implementation MUST support them, regardless of substrate.

- `agent_id` (string, required) — short slug used as the agent's identity in inbox addressing and message metadata. MUST match the regex `^[a-z0-9][a-z0-9-]*$` (a lowercase ASCII letter or digit, followed by zero or more lowercase ASCII letters, digits, or hyphens). The regex is normative; the reference CLI enforces it on every flag that accepts an agent id.
- `vendor` (string, required) — the agent's vendor or origin. Recommended conventions: `anthropic`, `openai`, `google`, `local`, `human` (for human operators registered as agents). SHOULD follow the same lowercase-slug shape as `agent_id`, but the v0.1 reference CLI validates only that it is non-empty. Not an enforced enum.
- `host` (string, recommended) — the hostname this agent is running on (as reported by `gethostname(2)` on POSIX, `os.Hostname()` in Go, or the equivalent on other platforms). Advisory routing hint: peers attempting cooperative wake (§10.5) skip pokes proactively when `host` differs from the local host, since cross-host wake methods are not reachable. Empty/absent = "legacy or unknown; attempt the poke and let it fail quietly." Hostnames are not secrets in agentchute's cooperative trust model.
- `wake_method` (string, conditional) — names the wake adapter the recipient is reachable through. The v0.1 reference adapter is `tmux` (see §8); other adapters MAY be defined by community extensions but are out of scope for v0.1. **MAY be empty or absent for non-pokable agents** (the watchdog daemon, headless background agents). If empty/absent, senders write the inbox entry but skip the poke; the recipient picks up on its own polling cadence.
- `wake_target` (string, conditional) — the adapter-specific address of the recipient's pane/session/endpoint. **Opaque to agentchute**: the wake adapter named in `wake_method` parses it. For `wake_method: tmux`, valid targets include `%N` (pane id) and `session:window.pane`. **REQUIRED when `wake_method` is set; empty/absent when `wake_method` is empty/absent.** The reference CLI enforces both directions.
- `last_seen` (RFC 3339 UTC timestamp, required) — updated by the agent at the start of each turn for liveness signaling.
- `status` (enum, optional) — self-declared diagnostic, one of `active | exhausted | offline`. Default `active` if omitted. Read by the watchdog (§10) and operators. See §10 for semantics.
- `restart_at` (RFC 3339 UTC timestamp, optional) — forward-looking estimate of "the earliest future time it's worth poking this agent next." SHOULD be updated each turn when the agent's wrapper has budget/reset visibility. Missing/null means "no known deferral; eligible for normal stale-poke logic." A future timestamp means "defer poking until then." See §10 for semantics.
- `last_active` (RFC 3339 UTC timestamp, optional) — last time the agent successfully processed an inbox message. Distinct from `last_seen` (turn-start liveness): `last_active` is "did they actually finish work." Operator-facing diagnostic. The v1 watchdog poke decision (§10.4) does NOT use `last_active`; watchdog implementations MAY surface it in their status output and logs.

The registration record's free-text body (the Markdown content after the frontmatter block in the reference encoding) is OPTIONAL prose: role notes, working style, current focus, deliberate constraints. Advisory only. An optional `capabilities` list MAY appear there but is non-normative — agentchute does not route on capabilities (§12).

### 5.2 Reference-implementation fields (filesystem)

These fields are required in the v0.1 filesystem-backed reference, but are not protocol-level — they describe filesystem-specific operator metadata. Alternate-substrate implementations supply equivalent pool-locator metadata in their own form (a queue connection URL, an HTTP endpoint, an object-store prefix, an entry in a key-value store).

- `control_repo` (absolute filesystem path, required for the filesystem reference) — the path to the directory containing this pool's `AGENTCHUTE.md`. Meaningful only on filesystem-backed implementations. The protocol requires only that a registration can resolve back to its pool deterministically; the form of that resolution is substrate-specific.
- `working_repos` (list of absolute filesystem paths, optional) — additional repo paths this agent declares as relevant to its work. Advisory operator metadata; NOT an authority grant under §7.2. (Authority to mutate any of these repos still arises only from a directly-addressed inbox message or an explicit human-session instruction.)

### 5.3 Registration record format

The v0.1 reference encoding is YAML frontmatter at the head of a Markdown file located at `.<vendor>/loop/agents/<agent_id>.md`. Frontmatter carries the §5.1 + §5.2 fields; the optional Markdown body carries the registration's free-text notes.

```markdown
---
agent_id: claude-code
vendor: anthropic
control_repo: /Users/you/code/your-repo
working_repos:
  - /Users/you/code/your-repo
host: macbook-pro.local
wake_method: tmux
wake_target: "%0"
last_seen: 2026-05-09T16:08:36Z
status: active
restart_at: 2026-05-09T18:00:00Z
last_active: 2026-05-09T16:00:12Z
---

# Free-text notes (optional)

Role, constraints, local context, etc.
```

Alternate-substrate implementations MAY encode the same logical fields in a substrate-native form: JSON in an HTTP response, message-queue attributes, an entry in a key-value store, a git ref's commit message, etc. Conformance is on the logical fields and their semantics, not on the YAML-in-Markdown encoding.

Each agent owns its own registration record. Other agents read the registry to know who's in the pool and where to wake them.

### 5.4 Humans as agents

Human operators MAY register as agents. Recommended convention: `vendor: human`, `agent_id` a short slug (e.g., `alex`), `wake_method: tmux` + `wake_target` set to whichever pane the human watches, or both empty if the human prefers passive polling. Origination of work is then a normal inbox message with `from: <human-agent-id>`. No special-casing in recipients — `from: alex` is processed the same way as `from: codex`.

### 5.5 Canonical hand-write example (filesystem reference)

To register without the agentchute CLI, write this file at `.<vendor>/loop/agents/<your-id>.md`:

```markdown
---
agent_id: claude-code
vendor: anthropic
control_repo: /Users/you/code/your-repo
working_repos:
  - /Users/you/code/your-repo
host: macbook-pro.local
wake_method: tmux
wake_target: "%0"
last_seen: 2026-05-12T00:00:00Z
status: active
---

# claude-code

Optional free-text notes about this agent's role, working style, or constraints.
```

`last_seen` MUST be the current UTC time when you write the file. Update it at the start of every turn (see §6.3).

### 5.6 Tooling shortcut

`agentchute register --as <id> --vendor <vendor> [--host <name>] [--wake-method <adapter>] [--wake-target <addr>] [--bio "..."]` does the same with ergonomic flags (idempotent re-runs). The full flag list also includes `--announce`, but `--announce` sends an unsolicited notification to every peer and therefore falls outside the §7 protocol-overhead carve-out — it is operator-gated, not part of mandatory session-start registration. See `agentchute register -h` for the complete surface.

### 5.7 Enforced enrollment

Conforming implementations MUST refuse to perform **active agent operations** if the acting agent's registration record is absent or unreadable. Active operations are those that consume mail, deliver mail, advance lifecycle gates, or otherwise act AS the agent identity (e.g., `check`, `send --from`, `watch`, `status --as`, and `gate --before finish|continue` in the reference CLI). The refusal ensures every participant in the pool is discoverable, addressable, and pokable before it can contribute work.

The refusal SHOULD include a pointer to the implementation's registration ritual (e.g., `agentchute boot`).

**Read-only diagnostics MAY surface a needs-registration signal instead of refusing.** Commands that exist to inject context into a lifecycle hook (`pending --claude-hook` / `--codex-hook`), or to report pool state without claiming agent identity (pool-overview `status`, `doctor`), SHOULD remain available so that wrappers can detect and remediate the gap. The reference CLI surfaces this via a `needs_boot` field in `pending` output and via `self-poll`'s exit-2 semantics for scheduler preflights.

## 6. Messaging

### 6.1 Message identity

#### 6.1.1 Identity tuple (protocol-level)

Every agentchute message carries a protocol-level identity tuple `(timestamp, sender, nonce)`:

- **Timestamp** — UTC with microsecond precision. Sender records send-time.
- **Sender** — the sender's `agent_id` (MUST pass the §5.1 `agent_id` grammar).
- **Nonce** — a short random suffix that disambiguates same-microsecond messages from the same sender. The reference encoding (§6.1.2) uses 4 lowercase hex characters; conforming alternate substrates MAY use a different nonce alphabet as long as same-tuple uniqueness is preserved.

**Ordering.** Conforming implementations MUST present inbox messages to the recipient in oldest-first order by timestamp, with deterministic tie-breaking within equal timestamps. The reference filename encoding (§6.1.2) achieves this via lexicographic sort on the filename; substrates that don't store messages as filenames MUST achieve the same ordering by other means (e.g., a sortable index, a sequence number, a timestamp+nonce composite key).

**Nonce uniqueness scope.** Uniqueness is required only within the `(timestamp, sender, recipient-inbox)` tuple. Same nonces in different recipients' inboxes are not collisions; same nonces from different senders are not collisions; same nonces with different timestamps are not collisions.

**Conformance.** Any conforming implementation MUST preserve identity-tuple uniqueness within a recipient inbox + oldest-first ordering semantics, regardless of how it encodes the tuple on its substrate. A transport extension that bills itself as "agentchute with X transport adapter" must preserve those two invariants.

#### 6.1.2 Reference encoding (filesystem filenames)

In the v0.1 filesystem implementation, identity tuples are encoded as inbox filenames:

```
<utc-microsecond-timestamp>_from-<sender>_msg-<nonce>.md
```

Example: `2026-05-09T16-32-00-123456Z_from-codex_msg-7f3a.md`.

Constraints for this encoding:

- Timestamp uses ISO 8601 UTC with `-` instead of `:` (filesystem portability across macOS, Linux, Windows).
- Microsecond resolution, zero-padded to 6 digits.
- Nonce: 4-character lowercase hex random suffix. Defends against same-microsecond collisions if a sender writes multiple messages in one syscall window.
- Files sort lexicographically by name — recipients process oldest-first across all senders (this is how the filesystem encoding realizes the §6.1.1 ordering invariant).

##### Canonical filename construction (POSIX shell)

```sh
ts=$(date -u +%Y-%m-%dT%H-%M-%S-000000Z)
nonce=$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')
sender="codex"
filename="${ts}_from-${sender}_msg-${nonce}.md"
```

Notes:

- POSIX `date` does not expose microseconds reliably across platforms. Agents that can produce microseconds SHOULD; agents that cannot MAY use `000000` for the microsecond portion. The nonce defends against same-second collisions either way.
- Tradeoff of the `000000` fallback: oldest-first ordering among multiple same-second messages from the same sender is not guaranteed beyond timestamp-and-nonce lexicographic order. Agents SHOULD use real microsecond timestamps when their environment supports it.
- `od -An -N2 -tx1 /dev/urandom | tr -d ' \n'` produces 4 lowercase hex characters portably.

#### 6.1.3 Clock-skew expectation

Senders SHOULD synchronize to UTC within a small tolerance (e.g., via NTP). Receivers MAY warn or reject messages with timestamps materially in the future (e.g., > 5 minutes ahead of local UTC); the protocol does not currently make this a §11 enforcement trigger, but implementations adopting that policy SHOULD document it.

### 6.2 Sender flow

#### 6.2.1 Protocol sender semantics

To send a message to recipient `R`, a sender:

1. Composes the message body. Body MUST be valid UTF-8 (§6.5); free-form Markdown is the v1 reference body format, with the optional §6.4 envelope.
2. Generates the §6.1.1 identity tuple `(timestamp, sender, nonce)`.
3. **Delivers into `R`'s inbox with the no-overwrite guarantee.** If a message with the same identity tuple already exists in `R`'s inbox (extremely unlikely with microsecond + nonce), the sender regenerates the nonce and retries. The substrate's atomicity model decides the mechanism (atomic rename on filesystems, ref compare-and-swap on git, `If-None-Match` on HTTP, queue dedupe on Kafka/SQS).
4. Reads `R`'s registration to get `wake_method` and `wake_target`.
5. **If `wake_method` is set and the corresponding adapter is invocable**, signals `R` via that adapter (the v0.1 reference tmux dispatch is in §8). **If `wake_method` is empty/absent, or the adapter is not invocable on this host**, the sender skips the wake step — the message has been delivered and `R` picks it up on its own polling cadence. Senders SHOULD log unsupported `wake_method` values locally when they cannot dispatch a wake; this is operational hygiene, **not** a §11 enforcement trigger.

#### 6.2.2 Reference mechanics (filesystem)

In the v0.1 filesystem implementation:

- Step 3 ("deliver with no-overwrite") is realized as: write to `.<vendor>/loop/inbox/<R>/.tmp_<timestamp>_from-<sender>_msg-<nonce>.md`, then `link(2)` (or equivalent) to the final filename — `link(2)` without `-f` fails if the destination exists, which gives the no-overwrite guarantee for free. The temp file is then removed.
- Step 5 wake dispatch via `tmux send-keys` follows the §8 pattern:
  ```sh
  tmux send-keys -t <wake_target> 'check'
  sleep 0.3
  tmux send-keys -t <wake_target> Enter
  ```
  The `sleep 0.3` is empirical: chained `tmux send-keys -t %N 'check' Enter` does not reliably commit Enter on all CLI input boxes. Two separate calls with a brief delay work consistently.

##### Canonical hand-send walkthrough (POSIX shell)

```sh
recipient="claude-code"
sender="codex"
inbox=".agentchute/loop/inbox/${recipient}"

# 1. Generate timestamp + nonce + filename (§6.1).
ts=$(date -u +%Y-%m-%dT%H-%M-%S-000000Z)
nonce=$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')
filename="${ts}_from-${sender}_msg-${nonce}.md"

# 2. Compose into a temp file.
cat > "${inbox}/.tmp_${filename}" <<EOF
---
message_id: $(date -u +%Y-%m-%dT%H:%M:%S.000000Z)
from: ${sender}
to: ${recipient}
task: review the diff
status: request
---

Take a look at register.go:142 — the regex looks too permissive.
EOF

# 3. Atomic no-overwrite delivery: hard-link to final, remove temp.
#    `ln` (without -f) fails if the destination exists, preserving the
#    no-clobber guarantee from §6.2. Plain `mv` would silently overwrite a
#    colliding file on most filesystems. On the (extremely unlikely) ts+nonce
#    collision in one syscall window, regenerate the nonce and retry —
#    the temp file path stays constant across retries.
tmp="${inbox}/.tmp_${filename}"
while ! ln "$tmp" "${inbox}/${filename}" 2>/dev/null; do
  nonce=$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')
  filename="${ts}_from-${sender}_msg-${nonce}.md"
done
rm "$tmp"

# 4. Look up recipient's wake_method + wake_target and poke (skip if non-pokable).
#    Strip the prefix + surrounding whitespace/quotes, preserving any colons
#    in the value (valid for session:window.pane).
method=$(awk '/^wake_method:/ {
  sub(/^wake_method:[[:space:]]*/, "")
  gsub(/^"|"$/, "")
  print
}' ".agentchute/loop/agents/${recipient}.md")
target=$(awk '/^wake_target:/ {
  sub(/^wake_target:[[:space:]]*/, "")
  gsub(/^"|"$/, "")
  print
}' ".agentchute/loop/agents/${recipient}.md")
if [ "${method}" = "tmux" ] && [ -n "${target}" ]; then
  tmux send-keys -t "${target}" 'check'
  sleep 0.3
  tmux send-keys -t "${target}" Enter
fi
```

Tooling shortcut: `agentchute send --from <sender> --to <recipient> --task "..." [--body "..." | < body.md]` does the same with ergonomic flags and validation.

### 6.3 Recipient flow

#### 6.3.1 Protocol recipient semantics

At the start of each turn (or upon receiving a wake signal), a recipient:

1. Updates own registration's `last_seen` to current UTC timestamp. SHOULD also update `restart_at` based on current budget visibility.
2. Enumerates pending messages in its own inbox (substrate-defined "list" operation).
3. Sorts by identity tuple per §6.1.1, oldest-first.
4. For each message:
   a. Validates the envelope (§6.1.1 identity tuple shape + §6.4 + §6.5). Reject (quarantine + notify per §11) if structurally malformed.
   b. Reads the message body.
   c. **Consumes** the message: atomically removes it from the live inbox so other readers (and future enumeration passes) do not see it. The "atomic" requirement is with respect to other readers of the same inbox. If the implementation keeps an archive, it SHOULD record the consume time and preserve the original identity tuple so two messages consumed in the same second do not collide; the archive is otherwise an implementation choice, not a protocol requirement.
   d. Processes the envelope/body and acts on the message.
5. SHOULD update `last_active` to current UTC timestamp on successful processing.
6. Reply if needed using §6.2.
7. **Enforce drift you observe** (§11): if a regular inbox entry fails identity-tuple validation, envelope validation, or otherwise reads malformed, quarantine it and notify the inferred offender per §11.
8. **Cooperative waking** (§10.5): after own-inbox work and timestamp updates are complete, the recipient flow MAY contribute best-effort liveness by inspecting peer inbox metadata (filenames/timestamps only) and poking peers that meet the §10.4 staleness thresholds. The reference CLI's `check` performs this automatically; hand-protocol agents MAY skip it. See §10.5 for the algorithm and authority boundaries.

**Single-reader expectation.** At most one process at a time SHOULD consume an inbox keyed by a given `(pool, agent_id)` pair. Running two consumers under the same `agent_id` in the same pool simultaneously is undefined behavior — the protocol does not coordinate concurrent inbox readers. Implementations MAY use OS-level locks (e.g., `flock` on the inbox directory in the filesystem reference implementation) to enforce single-reader; the protocol does not require it. Pool-scoped identity (§7.5) means the same `agent_id` in a *different* pool is a different consumer and is fine.

#### 6.3.2 Reference mechanics (filesystem)

In the v0.1 filesystem implementation:

- Step 2 ("enumerate pending") = listing `.<vendor>/loop/inbox/<self>/` and applying the §6.1.2 filename regex.
- Step 4c ("consume") = atomic rename of the inbox file to `.<vendor>/loop/archive/<consumed-timestamp>_to-<self>_<original-inbox-filename>`. The archive preserves the original filename so messages consumed in the same second do not collide.
- Step 7 quarantine = atomic rename of the malformed file to `.<vendor>/loop/malformed/<quarantine-timestamp>_to-<self>_<original-filename>`. See §11.2 for the full enforcement action.

##### Canonical hand-check walkthrough (POSIX shell)

```sh
self="claude-code"
inbox=".agentchute/loop/inbox/${self}"
archive=".agentchute/loop/archive"
malformed=".agentchute/loop/malformed"
mkdir -p "${archive}" "${malformed}"

# §6.1 filename grammar.
inbox_re='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{6}Z_from-[a-z0-9][a-z0-9-]*_msg-[0-9a-f]{4}\.md$'

# 1. Update own last_seen in registration. (Edit the YAML by hand or via sed.)
ts_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# sed -i.bak "s/^last_seen:.*/last_seen: ${ts_iso}/" ".agentchute/loop/agents/${self}.md"

# 2. Iterate over inbox, oldest-first.
for f in "${inbox}"/*.md; do
  [ -f "$f" ] || continue
  base=$(basename "$f")
  case "$base" in
    .*|.tmp_*) continue ;;       # skip dotfiles + in-flight writes
  esac

  # 3. §11 enforcement: validate filename against §6.1 grammar.
  #    If invalid, quarantine and continue (do NOT process or archive).
  if ! printf '%s' "$base" | grep -Eq "$inbox_re"; then
    quarantine_ts=$(date -u +%Y-%m-%dT%H-%M-%S-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')Z)
    mv "$f" "${malformed}/${quarantine_ts}_to-${self}_${base}"
    # Then notify the inferred offender per §11.3 / §11.4.
    continue
  fi

  # 4. §11.1 enforcement: minimal frontmatter validation.
  #    Frontmatter is OPTIONAL per §6.4, so a file without one is valid.
  #    A file WITH an opening `---` MUST have a closing `---` within the
  #    first ~50 lines, or it's syntactically malformed and is quarantined.
  #    Full YAML validation in shell is impractical — the reference CLI
  #    parses YAML; hand-protocol agents SHOULD do at least this minimum
  #    structural check before archiving.
  if head -1 "$f" | grep -q '^---$'; then
    if ! sed -n '2,50p' "$f" | grep -q '^---$'; then
      quarantine_ts=$(date -u +%Y-%m-%dT%H-%M-%S-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')Z)
      mv "$f" "${malformed}/${quarantine_ts}_to-${self}_${base}"
      # Then notify the inferred offender per §11.3 / §11.4.
      continue
    fi
  fi

  # 5. Read content, act on it.
  cat "$f"

  # 6. Move to archive with consumed timestamp.
  consumed=$(date -u +%Y-%m-%dT%H-%M-%SZ)
  mv "$f" "${archive}/${consumed}_to-${self}_${base}"
done
```

Tooling shortcut: `agentchute check --as <self>` does the same plus active protocol correction (§11).

### 6.4 Message envelope

#### 6.4.1 Logical envelope fields (protocol-level)

These fields form the protocol-level message envelope. Any conforming implementation preserves them with the semantics below, regardless of how the substrate encodes them on the wire.

- `message_id` (string, recommended) — sender-provided handle used as the canonical reference for this message in `in_reply_to` chains. The v0.1 reference CLI populates `message_id` with an RFC 3339 UTC timestamp at microsecond precision. `message_id` is **not** used for delivery uniqueness or ordering — the §6.1.1 identity tuple is authoritative for that. Other messages referring to this one use `message_id`, not the substrate's internal index.
- `from` (agent_id, recommended) — the sender. Same grammar as §5.1.
- `to` (agent_id, recommended) — the recipient. Same grammar as §5.1.
- `in_reply_to` (string, optional) — the `message_id` of the message this is replying to. References are non-normative; the protocol does not track open requests or guarantee a parent exists.
- `reply_required` (boolean, optional, default `false`) — when true, the sender expects a reply from the recipient. See §6.4.3.
- `priority` (enum, optional, default `normal`) — `low | normal | high`. This is a display/gating hint only; it does not change the §6.1.1 oldest-first ordering rule.
- `reply_kind` (enum, optional) — `signoff | pushback | answer | review | ack`. Advisory hint describing the kind of reply expected when `reply_required` is true.
- `task` (string, optional) — short task descriptor. The value `deferred-reply` is the convention for automatic acknowledgments of deferrals.
- `status` (enum, optional) — `request | findings | signoff | request-changes | info`. Free-text values are tolerated but the listed values are the convention.

Messages MAY omit any envelope fields entirely; receivers MUST handle messages without envelope (the body alone is sufficient). Unrecognized fields in the envelope MUST be ignored per §6.5 (the must-ignore-unknown-fields rule).

#### 6.4.2 Reference encoding (YAML frontmatter in Markdown)

In the v0.1 filesystem implementation, the envelope is encoded as YAML frontmatter at the head of the message's Markdown file:

```markdown
---
message_id: 2026-05-09T16:32:00.123456Z
from: codex
to: claude-code
in_reply_to: 2026-05-09T16:08:36.000000Z   # optional; references a prior message_id
reply_required: true
priority: normal
reply_kind: review
task: short task description
status: request | findings | signoff | request-changes | info
---

# Body

Free-form Markdown.
```

##### Parser tolerances

The v0.1 reference parser is intentionally small. Conforming implementations of this filesystem encoding SHOULD honor the same shape:

- A frontmatter block exists only when **the file's first line trims to `---`**. Files that do not begin with `---` are body-only and have no envelope.
- The closing delimiter is **a later line whose trimmed content is exactly `---`**. Everything between the two delimiters is the frontmatter; everything after the closing delimiter is opaque Markdown and MUST NOT be scanned for envelope fields.
- The reference parser bounds frontmatter by the configured message size cap (§6.5) rather than by a fixed line count. The canonical hand-check shell walkthrough in §6.3.2 uses a 50-line bound as a practical minimum for shell-based readers; full implementations may use a larger or size-based bound.
- Values are parsed as **simple `key: value` scalars** (plus simple list values for the few fields that take lists in §5). The reference parser accepts unquoted, double-quoted, and single-quoted scalar values.
- **Duplicate keys are malformed.** A frontmatter block with two `from:` lines (for example) SHOULD be quarantined per §11.
- **Unrecognized keys are ignored**, not malformed, per §6.5.

Full YAML features such as `#`-introduced comments inside the block, multi-line scalars (`|`, `>`), anchors, and tags are **not** part of the v1 reference encoding. The reference parser does not accept them; implementations that want richer YAML semantics are free to extend in their own fork, but those messages will not round-trip through the reference CLI.

Alternate-substrate implementations MAY encode the same logical fields differently (JSON in an HTTP response body, message-queue attributes, an entry in a key-value store, etc.); envelope conformance is on the logical §6.4.1 fields and their semantics, not on the YAML-in-Markdown serialization.

#### 6.4.3 Reply obligations

A message with frontmatter `reply_required: true` places a reply obligation on the recipient. Archiving the message does not discharge the obligation; the reference CLI records the pending obligation in `<loop>/state/<agent>/pending-replies.json` (the pending-reply ledger) when `agentchute check` archives the message.

The ledger entry status transitions from `pending` (on archive) to either `replied` (on `agentchute send --reply-to <message_id>`) or `deferred` (on `agentchute defer --message <message_id>`). Lifecycle gates that consult the ledger MUST report the obligation as still open until the entry status leaves `pending`.

Replies (any message with `in_reply_to` set) SHOULD default to `reply_required: false` unless the responder explicitly requests further dialog. Automatic acknowledgments (e.g., from `agentchute defer`) MUST NOT set `reply_required: true`. These rules prevent infinite loops in automated coordination.

A body-level `## ASK` heading is a salience convention for the recipient, not a machine-checkable protocol signal. Tooling MUST consult the envelope field, not the message body, to determine reply-obligation state. The reference CLI's `agentchute send --ask` convenience flag sets both `reply_required: true` and a leading `## ASK` heading.

### 6.5 Encoding, size, and forward compatibility

- **Encoding.** Messages MUST be valid UTF-8 (both the frontmatter block and the Markdown body). Filenames in the reference filesystem implementation MUST be ASCII (which the §6.1 grammar already enforces); on other substrates, the protocol places no constraint on the message-identity encoding beyond preserving the `(timestamp, sender, nonce)` tuple semantics.

- **Maximum message size.** The protocol imposes no hard cap. Implementations SHOULD accept messages up to a configured size limit and SHOULD surface a clear local diagnostic when a sender or receiver hits it; senders SHOULD chunk or externalize larger payloads (e.g., link to a file in the project tree rather than inline a 10 MiB log). Implementations MAY reject or quarantine oversize inputs deterministically — that is an implementation choice, not a §11 trigger. The v0.1 reference CLI accepts up to **4 MiB per inbox message** and **1 MiB per registration**. **Receivers MUST NOT silently truncate message bodies**; truncation without error violates protocol integrity, so an implementation that cannot accept the full message MUST reject or quarantine it and surface a diagnostic instead.

- **Empty body and empty frontmatter.** An empty body is legal — the frontmatter alone may carry the meaning (e.g., a `status: signoff` ack with no body). A message with neither frontmatter nor body is structurally valid but non-informative; senders SHOULD NOT compose such messages (they convey no operational meaning).

- **Forward compatibility — must-ignore-unknown-fields rule.** Receivers MUST ignore unrecognized frontmatter fields on registrations and messages rather than treating them as malformed. Senders MUST NOT redefine known v1 fields with incompatible semantics. This rule lets the protocol evolve without breaking older readers; combined with the versioning policy in §1, it gives v2+ a runway. (The "unknown `protocol_version`" case is distinct — see §1, "Protocol versioning" — and IS a §11 quarantine trigger because it signals a structural shape the receiver cannot vouch for.)

## 7. Coordination defaults

agentchute does not define roles, hierarchies, or election mechanisms. Agents are peers by default. Six conventions cover coordination:

### 7.1 Direct addressing only

Send a message to a specific recipient's inbox. That recipient owns the task. There is no wildcard / broadcast / self-claim mechanism in v1.

### 7.2 Inbox-only authority to mutate

An agent's authority to mutate project state (edit files, commit, push, run side-effecting commands, send messages on others' behalf) starts when a directly-addressed message arrives in its inbox, OR when explicitly instructed by the human in its active session. Reading a TODO file, README, plan doc, registration body, or source file MAY inform context, but it does NOT authorize work. Agents MUST NOT proactively start work based on files they happen to read. Inbox assignment or explicit human-session instruction is an authorization signal, but not sufficient by itself — recipients still follow the message's `status`, `task`, body, and explicit asks before acting.

**Protocol overhead is pre-authorized.** The authority rule above gates *work*, not protocol maintenance. Work means project edits, task acceptance, side-effecting commands outside the loop directory, and unsolicited peer messaging. Protocol maintenance — the self-management actions below — is pre-authorized and required on every session start, without waiting for human instruction or inbox message:

- **Self-registration (§5) on every session start.** Mandatory and idempotent — *run* the registration command (`agentchute register --as <id> --vendor <vendor>`) or perform the equivalent hand-protocol §5 write *every time the agent starts*, even when a registration file for this agent already exists. Existing files are likely stale: the previous session may have had a different `host`, `wake_target`, `wake_method`, pane ID, or working directory. **Verifying that a registration file exists is NOT sufficient.** The act of running `register` reconciles the registration against current `os.Hostname()`, `$TMUX_PANE`, and `cwd`. If a hand-protocol agent cannot run the CLI, it MUST hand-write the registration anyway, overwriting any prior file's fields with current values.
- **Self-state updates**: `last_seen` at the start of every turn; `last_active` after consuming inbox messages; `status`, `restart_at` whenever the agent's wrapper has budget/reset visibility.
- **Own scaffold creation**: creating this agent's own protocol state within the existing pool's namespace — its own inbox, archive, and quarantine areas. The agent MUST NOT touch peer state or create the pool itself. _Reference CLI: `inbox/<self>/`, `archive/`, and `malformed/` under `.<vendor>/loop/`._ Shared bootstrap (e.g., `agentchute init` creating the whole pool tree from nothing) is *not* protocol overhead — it remains gated, run only when the human or a directly-addressed message asks for it.
- **Own-inbox operations**: listing own inbox, reading own messages, archiving consumed messages, quarantining malformed files per §11.
- **Cooperative waking (§10.5)** and watchdog-style peer-inbox-metadata reads — filenames and timestamps only; never opening peer message bodies.
- **§11 corrective messages** to inferred offenders of protocol violations.
- **Direct replies necessary to answer a directly-addressed message** — part of fulfilling the incoming task. Does NOT extend to unsolicited peer messaging, broadcasting, or contacting peers not involved in the original thread.

Constraints on protocol-overhead actions: they MUST be idempotent (running them again with the same inputs is a no-op or refreshes state to current truth) and limited to **this agent's own protocol state** on whatever substrate the implementation uses (in the reference CLI, that's under `.<vendor>/loop/`). They MUST NOT touch project files outside the protocol's namespace, write to peer registration files, or open peer message bodies.

Everything outside this list — editing project files, sending unsolicited messages to peers other than the directed-message sender, running side-effecting external commands, starting tasks not assigned to this agent — remains gated by the authority rule above.

### 7.3 Self-description in registration body

Free-text prose about current focus, strengths, or limitations. Non-normative; advisory only. Other agents read this to know the lay of the land. Other agents MUST NOT route work based on capability declarations.

### 7.4 Re-negotiation in messages

A recipient MAY decline ("I'm not the right agent for this; ping codex instead") or recommend a different recipient. The originator picks; no protocol-level vote, quorum, or election.

### 7.5 Pool-scoped identity

An agent's identity is the pair `(loop_dir or control_repo, agent_id)`, never `agent_id` alone. The same `agent_id` in different pools MAY refer to different physical processes — the protocol does not link them. See §7.6 for the cross-pool / bridge / proxy pattern that exploits this.

### 7.6 Cross-pool agents (bridge / proxy pattern)

A single physical agent process MAY participate in multiple agentchute pools simultaneously. This is the pattern for agents that have resource access, credentials, or knowledge other peers don't have, and that act as a firewall / proxy / router *in the application-policy sense* — protocol-level routing remains a v2 deferred item (§13).

The protocol already supports this pattern via existing primitives (pool-scoped identity per §7.5; per-pool registrations). v0.1 adds no new fields, commands, or config. The v0.1 reference CLI exposes pool selection via the `--control-repo` flag, the `AGENTCHUTE_CONTROL_REPO` env var, and the `.agentchute-control-repo` pointer file (see §4.1). This section names the conventions and hazards.

**Identity in multiple pools.** Because identity is pool-scoped (§7.5), the same physical process can be `review-gateway` in a low-trust pool and `release-assistant` in a high-trust pool. Per-pool aliases are RECOMMENDED for bridge roles — they model the different trust contexts honestly and let the bridge apply different policies per role. Same `agent_id` across pools is allowed, but MUST NOT be assumed to imply the same physical process. The bridge agent maintains its own internal alias-to-process mapping. The registration body MAY mention cross-pool memberships in free text (advisory, non-normative); the protocol intentionally does NOT carry a structured "also in pool X" field — topology disclosure is policy-sensitive and is left to the bridge to manage.

**Wake delivery across pools.** A bridge MAY register the same physical `wake_target` in multiple pools (e.g., one tmux pane registered under different aliases in each pool), but only if the agent's local `check` routine polls every relevant pool when it wakes. Otherwise the poke is lossy — a peer in pool A pokes the pane, the agent runs `check` against pool A only, pool B's queued message sits stale. Alternatives: per-pool wake methods/targets, or rely on polling cadence and the watchdog.

**Operations.** A bridge process serializes its work per pool: for each pool the bridge participates in, it checks that pool's inbox, processes addressed messages, and replies — all using that pool's identity and substrate locator.

_Reference CLI mechanics:_ pool selection happens via `--control-repo`:

```sh
agentchute check --as review-gateway --control-repo /path/to/pool-A
agentchute check --as release-assistant --control-repo /path/to/pool-B
```

Wrapper scripts or shell aliases handle the ergonomics. v0.1 ships no built-in multi-pool config file — agentchute deliberately avoids config files. Sending is the same shape (`agentchute send --from <pool-id> --control-repo /pool/path --to ...`).

**Authority across pools is NOT transitive.** A directly-addressed message in pool A authorizes the recipient bridge to apply *its own policy*. It does NOT grant pool A peers any authority in pool B. It does NOT prove that pool B would accept the same request. When the bridge decides to act in pool B as a consequence of a request in pool A, that is a NEW action under the bridge's policy, not protocol-level forwarding. The bridge sends from its pool-B identity, accountable to pool B's coordination conventions.

**Authorization laundering — the inverse-firewall hazard.** This is the most important risk in the bridge pattern, and it is exactly the inverse of the firewall intent: a peer in low-trust pool A asks the bridge to perform an action in high-trust pool B that B's peers would NOT have accepted directly. By naively translating A's request into a B-side message, the bridge accidentally lends pool A's caller the capabilities granted by pool B's trust context. Bridges MUST treat cross-pool forwarding as an explicit policy decision. They SHOULD reject or transform requests rather than translate them blindly. Bridges SHOULD document their forwarding policy (in the registration body or elsewhere) so operators understand the trust surface.

**Information leakage.** Bridges SHOULD avoid disclosing source-pool content, metadata, peer identities, message timing, file paths, or pool topology to other pools unless their policy explicitly allows. The same "MUST NOT open peer message bodies" rule from §10.5 applies; the bridge has full access to its OWN inboxes in each pool but does not have license to redistribute that content across pool boundaries.

**Loop amplification.** A → bridge-AB → B → bridge-BA → A risks recirculating requests indefinitely. The protocol provides no automatic loop detection; bridges SHOULD use `in_reply_to` chains or correlation IDs in message bodies to recognize their own forwarded requests and stop the cycle. Loop detection is application-level.

**No protocol routing in v0.1.** A bridge that decides to forward a request to pool B does so by composing a normal message in pool B under its pool-B alias. There is no protocol primitive for "this message came from another pool" — the bridge decides what to expose. Cross-pool message provenance, if needed, lives in the body.

## 8. Tmux wake adapter (v0.1 reference)

The tmux adapter is the only wake adapter shipped in v0.1. tmux was chosen because it is terminal-native, widely packaged, and gives each agent a stable addressable pane — with one install and a few panes, senders can wake recipients immediately without adding a daemon, broker, or wrapper-specific integration. Other adapters (wezterm, kitty, native terminal scripting, SSH-based pokes, etc.) are protocol-compatible but out of v0.1 scope; community-extension space is discussed separately in `EXTENSIONS.md` (added alongside the reference CLI).

The poke is a wake-up signal, not a comms channel. All semantic content lives in the inbox file.

Pattern:

```sh
tmux send-keys -t <recipient.wake_target> 'check'
sleep 0.3
tmux send-keys -t <recipient.wake_target> Enter
```

Trigger string: `check` (single short word; noise-resistant if mistyped).

`wake_target` grammar for `wake_method: tmux`: accepts the canonical tmux target syntax — `%pane_id` (e.g., `%0`) or `session:window.pane` (e.g., `frontend:0.0`). Panes living in different tmux windows or sessions on the same tmux server are addressable via the `session:window.pane` form. Multi-socket setups (`tmux -L socket-name`) are not parsed by the reference adapter in v0.1.

Constraints:

- Only send `check` immediately after writing the corresponding inbox file. Do not poke for arbitrary prompts or chat.
- The poke channel is not a fallback comms channel. If the recipient is unreachable (pane closed, tmux server gone), passive polling at the recipient's next turn will pick up the message anyway.
- If `wake_method`/`wake_target` is empty, absent, unknown, or invalid, fall back to passive polling (recipient checks inbox at start of each turn regardless).

Fallback keycodes if `Enter` does not commit on a particular CLI: try `C-m` (carriage return) or `KPEnter` (numpad enter).

### 8.1 Running the reference CLI without tmux

The protocol does not require tmux. Without tmux, peer wake via the reference CLI's default adapter is unavailable — delivery still works (messages land in the recipient's inbox), but recipients must poll.

As of v0.2, the recommended no-tmux polling patterns follow a three-tier model:

- **Tier 1: Native recurring task.** Use the wrapper's built-in scheduler (e.g., Claude Code's `/loop`, Codex App Automations). This is the zero-infrastructure baseline.
- **Tier 2: Preflighted scheduler.** For wrappers without a native loop (e.g., terminal `gemini-cli` or `codex-cli`). An external scheduler (launchd/systemd/cron) runs a side-effect-free preflight check (`agentchute self-poll --as <id>`) and only launches the wrapper when work exists. `self-poll` exits 2 whenever the wrapper should wake — unread mail, pending replies, malformed inbox files, or first-run `needs_boot` — so the scheduler wakes the wrapper through to its boot ritual on first install. (`agentchute pending` also surfaces `needs_boot` in v0.2.1+ as a read-only context-injection signal; it never returns exit 2 in hook-envelope modes, so it remains safe to wire as a UserPromptSubmit / BeforeAgent hook.)
- **Tier 3: Finish-hook continuation.** Active sessions catch new mail at the end of a turn via lifecycle hooks (e.g., `gate --before continue`).

Always schedule the wrapper (which invokes the model), not a bare `agentchute check` loop. The model must own the consumption decision.

### 8.2 Wake responsibility

The protocol's discovery mechanism is recipient-side polling. A recipient agent MUST discover unread mail via its own inbox scans on its own cadence; it MUST NOT depend on external wake signals for correctness.

Wake adapters (tmux, HTTP, SSH, etc.) are **best-effort convenience optimizations** that reduce polling latency. Senders MAY attempt wake via the recipient's declared `wake_method`; failure is logged and ignored. Recipients MAY use external wake hints as additional signals but MUST remain correct in their absence.

The `wake_method` registration field declares the recipient's preferred convenience adapter, NOT a protocol requirement. Empty `wake_method` is equivalent to "I poll my own inbox."

## 9. Liveness

Agents update their own registration's `last_seen` at the start of each turn. This signals "I'm still in the pool." Agents SHOULD also update `restart_at` per §5 and `last_active` per §6.3 when applicable.

For pools of ~3 agents or less, liveness can be monitored manually by operators or other agents (read the registry, compare `last_seen` to current time, decide if anyone is stale).

For larger pools, or for production-realistic 24/7 use where token-exhaustion stalls are common, the watchdog daemon (§10) automates this.

## 10. Watchdog

The v1 watchdog monitors agent inboxes and pokes recipients whose inboxes are stale. The behavior described below is implementation-agnostic — implementations may run as a small standalone daemon process, may be built into a long-running agent's polling loop (e.g., Claude Code's `/loop`), or may be filled by a human checking timestamps and running the §8 poke by hand. See the vendor implementation's README for concrete paths. It is **liveness-only**: it MUST NOT assign tasks, reroute messages, rank agents, interpret message content, or modify any inbox/archive contents beyond reading. A richer coordinator/router remains a v2 deferred item (§13).

In addition to dedicated watchdog processes, every recipient flow MAY contribute cooperative waking (§10.5) — a best-effort distributed extension of the watchdog algorithm performed opportunistically during a normal `check` cycle. Watchdog and cooperation are **both best-effort** liveness aids; they are latency accelerators over the §8.2 recipient-polling correctness model. Only inbox delivery is the durable part. Neither can wake an agent with no reachable `wake_method`/`wake_target` — in that case the recipient must poll its inbox on its own cadence or be checked manually. Run the dedicated watchdog when you want unattended polling without relying on active peers.


### 10.1 Watchdog registration

The watchdog registers like any other agent, with these conventions:

```yaml
agent_id: watchdog
vendor: <vendor>     # e.g., agentchute (default for the reference CLI), or your own dotdir namespace
control_repo: /path/to/control/repo
host: <hostname>     # default os.Hostname()
wake_method: ""      # empty: watchdog is non-pokable
wake_target: ""
last_seen: <updates each cycle>
status: active
```

The watchdog does NOT receive messages addressed to it. Implementations MAY omit its inbox directory, or create an empty one for layout consistency.

### 10.2 Status enum semantics

Used by both agents (self-declared in their registration) and the watchdog (read-only):

- `active` — agent is in the pool and processing turns normally. Watchdog pokes if inbox is stale (§10.4).
- `exhausted` — agent self-declares it cannot currently process work (rate-limit / quota / budget). Watchdog defers poking until `restart_at`. If `restart_at` is missing, watchdog does not poke.
- `offline` — agent has shut down or is otherwise unavailable. Watchdog does not poke while `restart_at` is missing or in the future. Once `restart_at` has passed, the watchdog applies its normal stale-message thresholds (§10.4) — i.e., the agent becomes a candidate for poking under the same rules as `active`.

Default if `status` is omitted: `active`.

### 10.3 `restart_at` semantics

`restart_at` is a forward-looking estimate of "the earliest future time it's worth poking this agent." It is **optional but SHOULD be updated each turn** when the agent's wrapper has budget/reset visibility:

- Plenty of budget left → set `restart_at` to "now" (or a few minutes out — basically meaning "any time").
- Budget at 5–10% remaining → set to next known reset cycle, *just in case* a hard task drains the rest before the wrapper can flip `status` to `exhausted`.
- Already exhausted → actual reset time.

If the wrapper does not know its reset cycle, omit `restart_at`. Missing/null `restart_at` means "no known deferral; the watchdog uses its normal stale-poke logic." A future `restart_at` always defers poking until that time, regardless of `status`.

This handles the failure mode where an agent is `active` with low budget remaining, then takes one hard task that drains the rest before it can update `status`. Because `restart_at` was already set forward, the watchdog defers correctly.

### 10.4 Watchdog algorithm

The watchdog runs at a regular cadence — either as a long-lived daemon (e.g., `agentchute watchdog --as watchdog &` / launchd / systemd job), or as part of a long-running agent's polling loop. On each cycle:

1. Update own `last_seen`.
2. Enumerate the pool's registrations (excluding self). _Reference CLI: read `.<vendor>/loop/agents/*.md`._
3. For each agent:
   a. Inspect the peer's inbox metadata. If absent or empty, skip. _Reference CLI: list `.<vendor>/loop/inbox/<agent_id>/`._
   b. Find the **oldest unread message** in the inbox by the §6.1.1 identity tuple (timestamp first). Compute its age = `now - message_timestamp`.
   c. If `restart_at` is in the future: skip; log "deferring <agent_id> until <restart_at>". (Avoids poking a known-exhausted agent.)
   d. If `status == exhausted` and `restart_at` is missing: skip. (Agent cannot tell us when to retry.)
   e. If `status == offline` and `restart_at` is missing or in the future: skip.
   f. If `last_seen` is fresh (e.g., updated within `LAST_SEEN_FRESH_THRESHOLD`, default 5 min): skip; agent is recently active and may not have consumed yet.
   g. If oldest unread message age is below `MESSAGE_AGE_POKE_THRESHOLD` (e.g., 90 s, configurable): skip; too soon to poke. (Avoids immediate pokes for newly arrived messages. Repeat-poke suppression is NOT in v1 — once a message is past the threshold and `last_seen` is stale, the watchdog may poke on every cycle. If repeat suppression becomes necessary, it would arrive as a separate `last_poked_at` / backoff design.)
   h. Else: poke the agent.
4. Sleep `POLL_CADENCE` (default 60 s).

### 10.5 Cooperative waking

The reference CLI performs the §10.4 watchdog algorithm opportunistically during every `check` cycle — a best-effort distributed extension of liveness. Every reference-CLI checker contributes; no opt-in field is required. Hand-protocol implementations MAY skip cooperative waking (§6.3 step 8 is MAY-do). Cooperative waking and the dedicated watchdog (§10.1 / §10.4) are **both best-effort**; neither can wake an agent with no reachable wake method. The dedicated daemon remains useful as an always-on best-effort fallback when no active peers are around to cooperate.

When a wake adapter is available and the target agent declares a reachable `wake_method` / `wake_target`, an implementation SHOULD attempt a wake poke immediately after delivery or after detecting stale unread mail through cooperative waking. The wake is best-effort: if the adapter fails or the target declares no reachable wake method, the implementation records or logs the attempt and proceeds without blocking. **Senders MUST NOT wait synchronously for read receipts by default**; doing so turns mailbox delivery into blocking RPC and risks deadlock when peers are offline. Optional `send --wait-for-read --timeout <duration>` behavior is reserved for a future protocol version.

**When**: only after the recipient has processed its own inbox and updated its own `last_seen` / `last_active` timestamps (§6.3). Own work takes priority.

**Algorithm**: identical to §10.4, applied to every peer registration (self always excluded). All peer inspection uses **metadata only** — directory listings, filenames, timestamps. Agents MUST NOT open, read, copy, archive, or otherwise consume the contents of inbox files addressed to peers; that authority belongs strictly to the recipient.

1. Enumerate peer registrations (self always excluded). _Reference CLI: read `.<vendor>/loop/agents/*.md`._
2. For each peer:
   a. If peer `host` is set and differs from the local host: **skip proactively**. Cross-host wake adapters are not reachable from this machine; the message is already durably in the peer's inbox and the peer's own environment handles wake.
   b. If peer `wake_method` is empty/absent OR `wake_target` is empty/absent: skip (non-pokable) and log/warn as operational visibility, not as a delivery failure.
   c. Using metadata only, compute the oldest unread message age and apply §10.4 conditions (status, restart_at, last_seen freshness, oldest unread message age threshold).
   d. If eligible: dispatch the poke via the wake adapter named in `wake_method` (§8 for tmux).
3. Per-peer errors (empty wake fields, unsupported adapter, adapter invocation failure, malformed peer registration) MUST log/warn and continue. Cooperative waking MUST NOT fail or exit nonzero on per-peer issues; the message has already been delivered to the recipient's inbox (in the reference CLI, that's the shared filesystem).

**Authority boundary**: cooperative waking is a wake-up nudge, not message handling. The recipient retains exclusive authority over its own inbox bodies. Peer metadata reads (filenames, timestamps, file count) are the only permitted inspection.

**No new state**: no `last_poked_at` tracking, no per-peer backoff, no dedup. The §10.4 message-age and last_seen thresholds are the suppression mechanism.

### 10.6 Poking

The actual poke uses the wake adapter named in the target's `wake_method`. For the v0.1 reference adapter `wake_method: tmux`, see §8. The watchdog (and cooperative-waking peers) skip with a log line when any of the following holds:

- `wake_method` is empty/absent ("no wake_method for <agent_id>; skipping poke")
- `wake_target` is empty/absent ("no wake_target for <agent_id>; skipping poke")
- the adapter named in `wake_method` is not supported by this implementation
- the adapter is supported but invocation fails (pane gone, tmux server unreachable, etc.)

In each case the message is already delivered to the recipient's inbox; liveness for that recipient becomes the recipient's own polling cadence or the operator's responsibility.

### 10.7 Logging

Implementations SHOULD emit one operator-facing log line per cycle event in the format below, so operators can grep for poke decisions, deferrals, and skips. _Reference CLI:_ writes to `.<vendor>/loop/watchdog.log` (gitignored). Alternate substrates may emit to a log sink, an operator dashboard, or a substrate-native channel — the leading verbs (`poked`, `deferring`, `skipping`, `error`) are the conventional, grep-able shape.

```
2026-05-09T16:00:00Z poked claude-code (oldest msg age 2m22s)
2026-05-09T16:01:00Z deferring codex until 2026-05-09T18:00:00Z
2026-05-09T16:02:00Z claude-code last_seen fresh (12s); skipping
```

### 10.8 What's NOT in v1

- **Auto-restart of exhausted agents.** Watchdog logs and pokes only; operators restart manually.
- **Cross-host wake delivery.** A watchdog on Machine A cannot poke a peer on Machine B — wake adapters are local. Each host supplies its own watchdog or cooperating peers when needed.
- **Remote service notifications** (Slack, email, pager, webhooks). Watchdog log is the v1 surface; operators read it directly or pipe it elsewhere. Local operator notifications via `watch --notify` are supported as of v0.1.2 (§10.9).
- **Sophisticated retry/backoff.** Just respects the thresholds above.
- **Routing, ranking, role assignment, message interpretation.** Reserved for the v2 coordinator/router (§13) if/when needed.

### 10.9 Recipient-side watcher

While the watchdog (§10.1) monitors the pool, the `agentchute watch` command provides recipient-side monitoring for individual agents. It is designed for regular terminal sessions (non-tmux) or wrappers without hook support, providing OS notifications or shell-command execution on new mail. See Appendix B.4 for example usage.

## 11. Protocol correction (best effort)

agentchute is a no-retransmit protocol — messages have no acknowledgements, deliveries are fire-and-forget, and there is no central authority. To remain functional, every enrolled agent participates in keeping protocol compliance honest.

### 11.1 What triggers enforcement

Triggers are objective parser/validation failures, not subjective judgement:

- A regular file in their **own inbox** (excluding dotfiles and `.tmp_*` in-flight writes) whose filename does NOT match the §6.1 grammar.
- A regular file in their **own inbox** whose filename DOES match §6.1 but whose frontmatter block is syntactically malformed enough that the YAML cannot be parsed (e.g., literal `---` injected mid-scalar, unescaped newlines breaking key:value structure, missing closing `---`).
- A **peer's registration** that the agent reads (to look up `wake_method`/`wake_target`, to act as watchdog, etc.) where a required §5 field is missing or unparseable.

The threshold is mechanical: the parser failed on a required structural element. It does NOT include:

- Whitespace inconsistencies.
- Optional fields absent — `restart_at`, `last_active`, and optional message-frontmatter fields including `task`, `status`, `in_reply_to`.
- Messages without any frontmatter (§6.4 makes frontmatter recommended, not required; body-only messages are valid).
- Stale `last_seen` — that is what the watchdog is for (§10).
- File permission drift (cosmetic; does not block protocol operation).

### 11.2 Enforcement action

**For inbox files (your own inbox):**

1. **Quarantine.** Move the inbox entry out of the live message stream into a quarantine namespace, preserving the original identity tuple so two malformed entries with the same identity do not collide. Implementations MUST NOT overwrite an existing quarantined entry — on collision they MUST either generate a fresh suffix or fail loudly. _Reference CLI:_ atomic rename to `.<vendor>/loop/malformed/<quarantine-timestamp>_to-<recipient>_<original-filename>` (the timestamp prefix prevents collisions when the same malformed name recurs in the same second; the `to-<recipient>` segment preserves context).
2. **Notify the offender, if identifiable** (§11.4).
3. **Continue processing.** Subsequent valid messages from the same sender are handled normally. Drift on one file does NOT block the sender.

**For peer registrations (drift you observe while reading them):**

1. **Do NOT modify the file.** Moving someone else's registration is cross-agent mutation; that is out of scope for v1.
2. **Notify the offender, if `agent_id` can still be extracted** (§11.4).
3. **Skip the operation** that needed the malformed registration (e.g., skip the poke if you cannot determine `wake_method`/`wake_target`).

### 11.3 Corrective message format

```markdown
---
message_id: <generated per §6.4>
from: <enforcing-agent-id>
to: <offender-agent-id>
task: protocol correction
status: findings
---

malformed item: <quarantined-path-or-registration-path>
reason: <specific violation, e.g., "nonce must be 4 lowercase hex chars (§6.1)">
action: re-send per AGENTCHUTE.md §<ref>
```

The body is intentionally compiler-error-shaped: three lines, terse, no conversational framing. The `task:` value `protocol correction` is generic across both inbox-file and registration cases.

### 11.4 Best-effort notify

Sender inference order for the corrective message:

1. The `_from-<sender>_` capture from the file's name, if the filename was malformed only in the timestamp or nonce. The inferred sender MUST pass the §5 `agent_id` slug rules; otherwise treat as if not present.
2. The `from:` field in the message's frontmatter, if parseable AND the value matches the §5 `agent_id` slug rules.
3. Otherwise: leave the file quarantined, log a local warning visible to the operator, and SKIP the notify step. No retry; no broadcast.

One corrective message per quarantined file. If the corrective send itself fails (e.g., recipient's inbox is unreachable), leave the file quarantined and log locally; do not retry in a loop.

### 11.5 No refusal

"Refusal mode" — declining to process subsequent messages from an offender — is explicitly out of scope. agentchute is no-retransmit; refusing subsequent messages would silently drop legitimate work that the offender may have sent correctly after the malformed one.

### 11.6 Tooling note

The reference CLI (`agentchute check`) implements §11 automatically: it quarantines malformed files in your inbox to `malformed/`, sends the corrective message to the inferred offender, and continues processing valid messages. Hand-protocol agents perform the same steps per the canonical walkthrough in §6.3 step 7.

The `agentchute doctor` command (v0.1.2) provides diagnostic aggregation for the pool, including PATH checks, hook sanity scans, and scaffold verification. It is the RECOMMENDED tool for operators to verify protocol health.

## 12. Non-goals (v1)

These are deliberate exclusions from v1:

- **No non-filesystem transport in the v0.1 reference CLI.** Within the filesystem implementation, agents may run on different machines only if they share one filesystem with the required atomic create/rename semantics. Wake delivery is local to the recipient's environment: a sender on Machine A cannot directly reach Machine B's wake target unless that wake method is explicitly reachable from Machine A. Cross-machine setups therefore rely on each machine's own polling, local cooperating peers (§10.5), or local watchdog. Alternate inbox transports (queues, HTTP, object stores, git-backed) are protocol-compatible and discussed in `EXTENSIONS.md#alternate-transports` but do not ship in v0.1. For fully distributed agents with cryptographic guarantees, use a signed-envelope coordination protocol with git/GitHub as transport.
- **No durable / authenticated audit trail.** Loop messages are a transient operational trace; the archive is gitignored by default. If permanent transcripts are needed, that is a v2 opt-in feature (§13), not a v1 default. For durable, signed audit, use a layered protocol with signed envelopes.
- **No capability-based routing.** Agent selection is human/agent judgment. The body of a registration MAY mention capabilities as free-text, but the protocol does not route or dispatch on them.
- **No protocol-level signing.** Messages are unsigned plain-text. agentchute does not provide cryptographic guarantees. Consumers needing integrity/auth should use a signed coordination protocol layered above (or instead).
- **No structured request/response state machine.** agentchute is fire-and-forget. Senders may include `in_reply_to` references, but the protocol does not track open requests, retries, or timeouts.
- **No coordination-level role/election machinery.** See §7 for what agentchute says about coordination (which is: not much, on purpose).

## 13. v2 deferred items

These are intentionally NOT in v1; documented here so they don't get implemented speculatively:

- **Coordinator / router agent.** A richer agent that beyond liveness also routes broadcast tasks, recommends recipients, interprets task content, or implements a wildcard inbox. The v1 watchdog (§10) is liveness-only by design; coordination/routing is a separate concern reserved for v2.
- **Opt-in transcript export.** A script that exports the loop archive into a tracked file (e.g., `.<vendor>/loop/transcripts/2026-05-09.md`) for permanent audit trail. Off by default.
- **Auto-restart of exhausted agents.** v1 watchdog notifies via log; v2 may add wrapper-driven restart.
- **Remote notifications** (Slack, email, pager, webhooks). v1 keeps watchdog output as a log file and supports local operator notifications via `watch --notify` (§10.9); v2 may add remote notification adapters.
- **Repeat-poke suppression / `last_poked_at` backoff.** v1 cooperative waking (§10.5) and the watchdog (§10.4) re-evaluate from current state on every cycle without per-recipient cooldown bookkeeping. If repeat-poke suppression becomes necessary in production, the design would carry a `last_poked_at` field plus a backoff window — a separate v2 design as called out in §10.4 step g.
- **In-band version negotiation.** v1's stance on protocol versioning (§1) is intentionally simple: the `AGENTCHUTE.md` file in the control repo is canonical, the optional `protocol_version` field is the only in-band signal, and unrecognized values quarantine per §11. v2 may add a handshake or profile-exchange mechanism so peers running incompatible versions can agree on a shared subset (or fail explicitly) rather than the v1 wholesale-reject. The must-ignore-unknown-fields rule (§6.5) already gives v2 a runway for additive changes; only breaking changes need negotiation.

## 14. Vendor namespace conventions

Each conforming implementation namespaces its protocol state under a vendor-owned identifier — the namespace boundary lets multiple agentchute implementations coexist on the same coordination substrate without colliding. The `<vendor>` SHOULD be a domain the implementer owns or a clearly distinct project name.

In the v0.1 filesystem reference, the namespace is a dotdir at the repo root:

```
.rehumanlabs/loop/     # reHuman Labs's implementation
.examplecorp/loop/     # ExampleCorp's implementation
.myorg/loop/           # any vendor's implementation
```

Alternate substrates use the equivalent vendor-prefixed namespace: a queue topic or stream prefix (e.g., `agentchute.<vendor>.inbox.<agent>`), an object-store bucket or prefix (e.g., `s3://<vendor>-agentchute/`), an HTTP path prefix (e.g., `/<vendor>/agentchute/`), or whatever the substrate provides. The point is namespace isolation, not the form.

The `AGENTCHUTE.md` spec is shared across all implementations and SHOULD live at the canonical pool location (the repo root in the filesystem reference). Implementation-specific notes (current agents, repo-specific layout, migration history, CLI tooling pointers) live in `.<vendor>/loop/README.md` in the filesystem reference; alternate substrates put equivalent notes wherever their namespace exposes them.

A pool MAY contain multiple vendor implementations side-by-side. Agents from different vendors can coexist by reading their own vendor's namespace; cross-vendor messaging is out of scope for v1.

## Appendix B. Reference implementation hook templates

These templates show how to integrate the reference CLI into the lifecycle hooks of common agent wrappers. They are non-normative examples; conforming implementations MAY use different hook names or substrates.

### B.1 Claude Code (.claude/settings.json)

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear|compact",
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} boot --as claude-code --vendor anthropic --context-only"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} pending --as claude-code --claude-hook UserPromptSubmit"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} gate --as claude-code --before finish --json"
          }
        ]
      }
    ]
  }
}
```

**Notes:**
- **SessionStart**: Runs once per session start. Refreshes registration and surfaces inbox state as context.
- **UserPromptSubmit**: Injects pending obligations into the model's context per turn. Uses the `--claude-hook` flag to emit the specific JSON shape required for Claude Code context injection.
- **Stop**: Lifecycle gate. Exit 2 (blocked) triggers turn continuation.
- **v0.1.2 note**: Operators SHOULD occasionally run `agentchute doctor --as claude-code` to verify hook health.

### B.2 codex CLI (.codex/hooks.json)

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup|resume|clear",
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} boot --as codex --vendor openai --codex-hook SessionStart"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} pending --as codex --codex-hook UserPromptSubmit"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} gate --as codex --before finish --codex-hook Stop"
          }
        ]
      }
    ]
  }
}
```

**Notes:**
- **codex-hook output**: Uses the `--codex-hook` flag to emit the specific JSON shape required for codex context injection and turn-blocking.

### B.3 Gemini CLI (.gemini/settings.json)

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "name": "agentchute-boot",
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} boot --as gemini-cli --vendor google --context-only"
          }
        ]
      }
    ],
    "BeforeAgent": [
      {
        "matcher": "*",
        "hooks": [
          {
            "name": "agentchute-pending",
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} pending --as gemini-cli --json"
          },
          {
            "name": "agentchute-gate",
            "type": "command",
            "command": "${AGENTCHUTE_BIN:-agentchute} gate --as gemini-cli --before finish --json"
          }
        ]
      }
    ]
  }
}
```

**Notes:**
- **BeforeAgent**: Since `SessionEnd` is non-blocking in gemini-cli, the gate is moved to the start of the next turn (`BeforeAgent`).

### B.4 Recipient-side watcher (v0.1.2)

For wrappers that don't support hooks, or for regular terminal sessions where OS notifications are preferred, the `watch` command provides a persistent polling fallback:

```sh
agentchute watch --as <id> --notify
```

This command is **non-consuming**: it polls/peeks the agent's inbox and triggers an OS notification on new mail, but does NOT archive or quarantine messages, nor does it make them visible to an agent's model context by itself. It is the recipient-side sibling to the watchdog daemon (§10); the human operator or agent wrapper still performs the consumption flow (§6.3) after being woken.
