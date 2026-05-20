# claude-code wake-method R&D, Round 1

Three candidates spanning the friction/power spectrum. Each is described as a complete proposal (sender side + recipient side + registration + failure modes + protocol/impl split).

---

## Candidate A: `agentchute serve` — embedded HTTP wake listener (self-hosted, cross-host)

### Shape

The recipient runs a small long-lived process (`agentchute serve --as <id> --listen <addr>`) that exposes a single HTTP endpoint and translates incoming pokes into the same local action set `watch` already supports (`--notify` / `--print` / `--exec`).

**Wire**: a sender POSTs JSON to `<recipient.wake_target>`:

```http
POST https://recipient-host:9434/wake
Content-Type: application/json
X-Agentchute-Token: <shared-secret-from-registration>

{
  "agentchute_version": "0.1.4",
  "to": "claude-code",
  "from": "codex",
  "message_id": "2026-05-20T...Z",
  "filename": "2026-05-20T..._from-codex_msg-abcd.md",
  "task": "review the diff",
  "reply_required": true
}
```

**Recipient response**:

- `200` with `{"action":"fired","method":"notify|exec|print"}` — wake fired.
- `200` with `{"action":"deduped","reason":"already seen"}` — sender's poke arrived after the recipient's own polling sweep already saw the file.
- `401` if the token doesn't match.
- `503` if the recipient is overloaded / rate-limited.

The wake payload is **metadata only**. Bodies stay in the mailbox. The recipient verifies the message actually exists in its inbox before firing the action — pokes that reference a phantom message are dropped (with a log line) and 200'd. This is the §11 protocol-correction discipline applied to wake: the wire is best-effort; the truth is the inbox.

### Registration

```yaml
agent_id: claude-code
vendor: anthropic
host: laptop.local
wake_method: http
wake_target: https://laptop.local:9434/wake
wake_token_ref: secret://agentchute/laptop.local/claude-code   # optional; spec-shaped pointer
```

`wake_token_ref` is opaque to agentchute — it names a secret stored elsewhere (env var, file path, secret manager URI). The reference CLI's resolver checks `${WAKE_TOKEN_REF#secret://}` against env / `~/.agentchute/secrets/<path>` / nothing. Optional: if absent, no auth; suitable for trusted LAN setups.

### Cross-host story

Real: the URL is reachable. Single-host: `127.0.0.1:9434`, no network. Cross-host on a LAN: hostname or static IP. Cross-host over the public internet: caller's problem (tailscale, ngrok, public IP + TLS).

### Failure modes

- **Recipient daemon not running**: sender's wake POST returns connection-refused; sender records `wake_result: failed (connection refused)`. Message is still in the inbox; recipient will see it on next watch poll or wrapper restart's SessionStart `boot`.
- **Recipient on different network**: sender DNS or TCP fails; same fallback.
- **Token mismatch**: 401; sender records and reports.
- **Daemon crash mid-run**: launchd / systemd / brew services / `nohup &` is the operator's job. Doctor adds a check.
- **Replay**: sender's pokes carry `message_id`; the recipient daemon dedupes against an in-memory cache of recently-fired IDs (10-second window). Lost on daemon restart, which is fine — restart re-snapshots inbox like `watch` does.

### Protocol vs. impl

**Protocol** (AGENTCHUTE.md addition): defines `wake_method: http`, payload shape, status codes, and the metadata-only contract. Specifies that a poke is best-effort and the inbox is authoritative.

**v0.1 reference CLI**: `agentchute serve` daemon, plus `loop.PokeWakeTarget` extends to dispatch HTTP. Token resolution via env var or local file.

### Why this works

- One new daemon per recipient, deps-free Go stdlib `net/http`.
- The same `--notify / --print / --exec` action set as watch — operator already knows the shape.
- Cross-host without an external service.
- Recipient owns its endpoint; no centralized broker.
- Inbox stays the source of truth; pokes are hints.

---

## Candidate B: `ntfy.sh` (or pluggable pub/sub) — third-party push, zero recipient daemon

### Shape

ntfy.sh is a free, end-to-end-encrypted, public pub/sub service (or self-hosted). Each recipient subscribes to a private topic; senders publish a message to that topic; the recipient's local `ntfy` client fires a configured shell command.

**Wire**: sender does:

```sh
curl -fsSL https://ntfy.sh/agentchute-claude-code-<token> \
  -H "Title: agentchute: new mail from codex" \
  -H "Tags: agentchute" \
  -d "msg-id=<id>;from=codex;task=review"
```

Recipient runs (once, as a service):

```sh
ntfy subscribe --exec "agentchute boot --as claude-code --vendor anthropic; <relaunch-wrapper>" \
  https://ntfy.sh/agentchute-claude-code-<token>
```

### Registration

```yaml
wake_method: ntfy
wake_target: https://ntfy.sh/agentchute-claude-code-<random-topic-token>
```

Topic IS the auth — anyone who knows the URL can poke. ntfy.sh supports access tokens for stronger auth; small docs lift.

### Cross-host story

Trivial: ntfy.sh is the cross-host plane. Senders can be anywhere; recipient just needs outbound HTTPS to ntfy.sh.

### Failure modes

- **ntfy.sh outage**: senders' pokes 5xx. Recipient still discovers mail via polling.
- **Recipient offline**: ntfy.sh holds messages briefly; recipient receives on reconnect (subject to ntfy's retention).
- **Token leak**: anyone can spam the topic. ntfy's access-control add-ons mitigate.

### Protocol vs. impl

**Protocol**: defines `wake_method: ntfy` payload shape. Same wake-is-best-effort discipline.

**Reference CLI**: `PokeWakeTarget` extends with an ntfy dispatcher (HTTP POST). No recipient daemon needed — `ntfy subscribe` is operator-installed.

### Why this works

- **Zero new agentchute infrastructure**. Lean on ntfy.sh.
- **Cross-host out of the box**.
- **Trivial setup** for operators already familiar with ntfy.

### Caveats

- Third-party dependency (ntfy.sh availability). Could be self-hosted ntfy for the paranoid.
- Recipient needs the `ntfy` CLI installed and a subscribe-service running. Mirrors the watch-daemon ask but adds an external tool.

---

## Candidate C: Make `watch` push-aware via FS notifications — single-host, sub-second

### Shape

Most operating systems have a kernel-level filesystem notification API: `inotify` (Linux), `kqueue` / `fsevents` (macOS), `ReadDirectoryChangesW` (Windows). On a single host (or a shared filesystem that supports these), the recipient can subscribe to its inbox dir and react to writes in milliseconds, not the 10-second polling interval.

`watch` adds a `--fs-events` flag (or auto-detects). On supported substrates, it bypasses the polling loop and reacts to kernel-issued events. On unsupported substrates (NFS without inotify, SMB, S3-mounted FUSE), it falls back to polling.

No new senders' wire format. Senders still write files; the recipient OS notifies the watcher. Conceptually: the **filesystem is the wake adapter** for the single-host case.

### Registration

No registration change. wake_method stays as the existing tmux / http / ntfy / etc.; `--fs-events` is a recipient-side speed optimization, orthogonal to declared wake_method.

### Cross-host story

**This is the explicit non-goal of this candidate.** Cross-host wake is handled by A or B. Candidate C just makes the existing single-host (or shared-FS) story responsive enough that the wake-from-outside conversation can stop pretending the polling interval is the bottleneck.

NFSv4 with delegations sometimes carries change notifications; not reliable across implementations. Treat NFS as polling-only.

### Failure modes

- **Substrate doesn't support FS events**: detect at startup; print a one-line message; fall back to polling silently.
- **Kernel resource limits**: inotify watches are capped per user (`fs.inotify.max_user_watches`). Doctor adds a check.
- **Sub-second avalanche**: rapid arrivals can fire many events; coalesce by inbox-dir-changed signal, then scan-and-diff.

### Protocol vs. impl

**Protocol**: no change. This is purely reference-CLI optimization.

**Reference CLI**: adds dep on `github.com/fsnotify/fsnotify` (commonly accepted single-dep). Or use `os.Notify`-style direct syscalls per OS (deps-free but more code).

### Why this matters

- Improves the single-host story from "10s polling" to "sub-second".
- Pairs cleanly with hooks: SessionStart context shows pending-reply state set sub-second after the sender wrote it.
- Removes the most common "we need a wake method" complaint without inventing one.

---

## What I'm leaning toward

- **A and C ship together as v0.2.** A solves cross-host, C makes the local path snappy. They're orthogonal.
- **B as an EXTENSIONS.md alternate.** ntfy.sh is a great recipe but it's not the only push service operators might pick. The reference CLI should ship ONE structured wake adapter (A's HTTP) and document B as "the same protocol shape works against ntfy / Pushover / FCM / your pager — here's the recipe".

This way the protocol grows by exactly one wake adapter (HTTP) and the reference CLI gets a measurable responsiveness win (FS events) without a third-party dependency on a public service.

Open to seeing what codex and gemini propose.
