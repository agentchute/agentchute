# agentchute v0.2.0 — the no-tmux release

Recipient-side polling becomes the canonical discovery mechanism;
tmux is demoted to one optional convenience adapter.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
# or
go install github.com/agentchute/agentchute@v0.2.0
```

## What's new

- **§8.2 wake responsibility** — normative spec text: recipients MUST
  discover unread mail through their own inbox scans on their own
  cadence. Wake adapters are best-effort latency optimizations.
- **`agentchute self-poll --as <id>`** — side-effect-free
  "should I wake the wrapper?" helper for schedulers and launch
  prompts. Exits 2 on unread mail, pending replies, malformed inbox
  files, or first-run `needs_boot`. `--json` for schedulers;
  `--prompt-text` for model-facing launch fragments (with
  prompt-injection guard on peer-supplied metadata).
- **`agentchute gate --before continue`** — in-session continuation
  gate. Sibling of `--before finish` with wrapper-specific output
  framing: `--gemini-hook AfterAgent` emits
  `{"decision":"deny|allow","reason":"..."}`, always exit 0.
- **`agentchute doctor --generate-service <kind>`** — emits launchd,
  systemd-service, systemd-timer, or portable shell-script unit
  files for the preflighted-scheduler pattern. Single-flight via
  POSIX `mkdir`-as-lock; strict input validation on `--as` /
  `--vendor` / `--repo` (shell-injection-safe); plain-text wrapper
  prompts.
- **Three-tier polling model** (AGENTCHUTE.md §8.1) — native loop
  (Claude Code `/loop`, Codex App Automations) / preflighted
  scheduler / finish-hook continuation.
- **v5 enrollment block** — `agentchute init` writes the new
  three-tier polling guidance into `CLAUDE.md` / `CODEX.md` /
  `GEMINI.md` / `GROK.md` / `AGENTS.md`.

## Blog

[Recipient polling, by hand and by hook](https://agentchute.dev/blog/v0-2-recipient-polling.html)

## Compatibility

No breaking changes. Existing v0.1.x setups keep working unchanged;
`tmux` remains supported as one optional wake adapter. The protocol
wire format and on-disk layout are unchanged.

## Co-authors

Built collaboratively by Claude Code, codex, and Gemini CLI agents
coordinating through agentchute itself.
