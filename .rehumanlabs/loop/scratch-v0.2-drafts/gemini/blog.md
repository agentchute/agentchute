# Recipient polling, by hand and by hook

The agentchute protocol is reaching its v0.2 milestone with a significant reframing of how agents discover and process work. We're moving past the "tmux as requirement" phase and embracing a protocol-pure reality where **recipient polling is the canonical discovery mechanism.**

## The best-effort contract

In v0.2, we've formalized the wake responsibility in the spec (AGENTCHUTE.md §8.2). The contract is simple:
- **Senders** are responsible for delivery. Wake pokes are best-effort convenience optimizations.
- **Recipients** are responsible for discovery. They poll their own inboxes on their own cadence.

This reframing means agentchute no longer "requires" tmux. If you can share a filesystem, you can coordinate.

## The Three Tiers of Polling

To make no-tmux coordination effortless, we've implemented a three-tier polling model:

### 1. Native Loops (Zero Infrastructure)
If your wrapper supports recurring tasks (like Claude Code's `/loop` or Codex App's Automations), you use them. The model self-prompts, checks its inbox, and processes work autonomously. No daemons, no background scripts.

### 2. Preflighted Schedulers (The Efficient Baseline)
For wrappers without a native loop (Gemini, terminal Codex), we've introduced the **Preflighted Scheduler**. An external scheduler (launchd/systemd/cron) runs a side-effect-free check using the new `agentchute self-poll` command. It only launches the wrapper when actual work exists, ensuring zero idle token cost.

To make this easy, `agentchute doctor --generate-service` now emits ready-to-load unit files for your specific wrapper and OS.

### 3. Finish-Hook Continuation (In-Session Reactivity)
For active interactive sessions, we've standardized the "Interrupt" pattern. With `gate --before continue`, an agent can "notice" new mail arriving mid-turn and immediately transition into processing it, without the operator needing to ferry the message.

## What's in the Box (v0.2)

- **`agentchute self-poll`**: A surgical helper for schedulers and prompters.
- **`gate --before continue`**: The continuation primitive for Gemini AfterAgent hooks.
- **`doctor --generate-service`**: One-command daemonization for the preflighted scheduler.
- **Refined Spec**: Clear normative boundaries between protocol (polling) and implementation (wake adapters).

v0.2 turns agentchute from a "terminal tool" into a "coordination primitive" that fits any agent architecture.

[MIT License](LICENSE) · [Spec](AGENTCHUTE.md) · Built by [reHuman Labs](https://rehumanlabs.com)
