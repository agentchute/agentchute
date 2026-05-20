# Recipient polling, by hand and by hook

tmux made the first agentchute demos feel immediate: Claude sends a message, agentchute types `check` into codex's pane, codex wakes up, and the handoff keeps moving.

That worked. It also blurred the protocol boundary.

agentchute is not tmux. agentchute is the mailbox.

The durable protocol step is simple: a sender writes a no-overwrite message into the recipient's inbox. Everything after that is best-effort liveness. A wake poke can reduce latency, but it is not what makes the protocol correct. The recipient must still discover its own inbox on its own cadence and run the normal recipient flow.

v0.2 makes that no-tmux story explicit.

## The rule

Schedule the wrapper, not `check`.

`agentchute check` is a consuming command. It reads message bodies, archives inbox files, quarantines malformed protocol state, and records reply-required obligations. If a shell timer runs `check` by itself, the model never sees the message. That is silent drain.

The safe loop has two phases:

1. A read-only preflight asks, "Should I wake the wrapper?"
2. If work exists, the wrapper starts a model turn, and the model runs `check`.

That is the difference between automation and data loss.

## Three recipient polling tiers

Different wrappers give us different scheduling surfaces, but the same invariant holds in each one.

Claude Code has a native loop. A user can arm:

```text
/loop 5m process any agentchute mail; reply or defer required messages before stopping
```

Hooks do the rest. `UserPromptSubmit` injects the current pending state into the turn. The model runs `check` when there is mail. The Stop hook refuses to finish if the inbox or reply ledger is still dirty.

Codex App has native Automations. The automation prompt is the loop: run the agentchute recipient flow, process mail if present, stop cleanly if idle.

Terminal codex CLI and Gemini CLI do not have native recurring tasks today, so v0.2 leans on the simplest dependable primitive: an operator scheduler. The scheduler runs a side-effect-free preflight and launches the wrapper only when work exists.

```sh
agentchute self-poll --as codex --json
```

Exit 0 means idle. Exit 2 means wake the wrapper. Any other nonzero exit is a real problem.

That lets a launchd job, systemd timer, cron entry, or shell loop poll every few seconds without spending model tokens on empty inboxes. The expensive part, `codex exec` or `gemini -p`, only runs when the recipient has work.

## `self-poll`

`self-poll` is deliberately boring. It is a read-only helper over the same inbox and ledger state that `pending` already exposes.

It does not archive. It does not quarantine. It does not send corrective messages. It does not update `last_seen`. It does not poke peers.

It answers one question for a scheduler:

```json
{
  "agent": "codex",
  "should_wake": true,
  "reasons": ["unread"],
  "unread_count": 1,
  "replies_pending": 0,
  "recommended_prompt": "..."
}
```

It also has `--prompt-text` for wrapper launchers that want a model-facing fragment. That text starts with agentchute-authored instructions and labels peer-supplied metadata as untrusted data. Message bodies stay out of the preflight path; the model sees bodies only after it runs `check`.

## Generated services

The common setup problem is daemonization, not protocol design.

v0.2 adds:

```sh
agentchute doctor --generate-service script --as codex --interval 30
```

The generator emits a launchd plist, systemd service/timer, or portable shell script for the preflighted scheduler pattern. It emits files only. It does not install, load, or start background agents for you.

The generated loop is still the same two-phase shape:

1. `self-poll` decides whether work exists.
2. The wrapper is launched with a prompt that tells the model to run `check`.

## Finish hooks catch the in-between cases

Polling handles idle recipients. Finish hooks handle recipients that are already awake.

If a new message arrives while a model turn is running, the Stop or AfterAgent hook scans the current inbox at the end of the turn. If work appeared, the hook refuses to let the wrapper go idle.

Codex already uses a Stop hook that emits a block decision. Gemini gets a v0.2-friendly continuation surface:

```sh
agentchute gate --as gemini-cli --before continue --gemini-hook AfterAgent
```

On work it emits `{"decision":"deny","reason":"..."}`. On clear state it emits `{"decision":"allow"}`.

This is not sender-side wake. It is recipient-side catchup at a lifecycle boundary.

## tmux becomes what it always was

tmux is still useful. If two agents share one tmux server, typing `check` into the recipient pane is a fast wake hint. The same idea can be implemented with HTTP, SSH, notification services, or a relay.

Those are convenience adapters. They are not the protocol.

The protocol stays small: deliver to the inbox; recipient polls; model consumes; reply or defer obligations before stopping.

That is enough to run without tmux.
