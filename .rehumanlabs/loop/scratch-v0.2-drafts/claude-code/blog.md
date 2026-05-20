# Recipient polling, by hand and by hook

*v0.2 release — agentchute now ships first-class no-tmux integration*

When we wrote the v0.1 spec for agentchute, tmux was the obvious wake
adapter. Two agents sharing a tmux server is a tight, low-latency loop:
a sender writes a file, fires `tmux send-keys` at the recipient's pane,
and the recipient picks it up within a frame. The pattern worked. It
worked so well that we wrote half the docs assuming you had one.

Then people started running agents in separate machines. Or in
containers. Or one in their terminal and one in a desktop app. tmux
stopped being the obvious answer, and the question we'd quietly
avoided rose to the surface: **what is the protocol's actual discovery
mechanism, independent of any adapter?**

## The answer was already in the spec, just not loud enough

agentchute is best-effort. The sender's durable responsibility is
delivering the message — writing a file into the recipient's inbox
directory under the agreed naming rules. That's the whole wire
protocol.

The recipient's responsibility is symmetric and quieter: discover the
message and process it. The protocol doesn't dictate how. tmux is one
way. So is HTTP, SSH, ntfy, a relay service, a launchd timer, or you
typing `agentchute check` by hand. Any of those produces a discovery
event. The protocol doesn't care which.

v0.2 makes this loud. We're calling it out in §8.2 of the spec:

> The protocol's discovery mechanism is recipient-side polling. A
> recipient agent MUST discover unread mail via its own inbox scans on
> its own cadence; it MUST NOT depend on external wake signals for
> correctness.
>
> Wake adapters (tmux, HTTP, SSH, etc.) are best-effort convenience
> optimizations that reduce polling latency.

This isn't a breaking change. tmux still works exactly the way it did.
The change is that we now have a clear answer for "what if I can't
use tmux?" — and the answer is the same one we should have led with
all along: poll your own inbox.

## Three tiers, in order of how lazy they let you be

### Tier 1 — Native recurring task

If your wrapper has a built-in scheduler, use it. Claude Code has
`/loop`. Codex App has Automations. Both let you say "every 5 minutes,
run this prompt" and you're done — no infrastructure, no operator
script, no extra process.

```
/loop 5m Run `agentchute check --as claude-code`; process any obligations.
```

Each tick fires your normal SessionStart hook, which fires
`agentchute boot`, which catches any new mail. Cost: one model turn per
tick. Tradeoff: pick the cadence you can afford.

### Tier 2 — Preflighted scheduler

If your wrapper doesn't have a native loop — codex-cli, gemini-cli,
plain Python scripts — let the operating system schedule the polling.
The key insight is **preflight**: don't launch the (expensive) wrapper
on every tick. Launch a tiny side-effect-free check first.

```
agentchute pending --as codex --fail-if-any
# exit 0: idle, do nothing
# exit 2: work exists, launch the wrapper
```

`pending --fail-if-any` is a read-only scan. It doesn't archive
anything, doesn't update `last_seen`, doesn't consume the mail. It
just answers a yes/no question. The wrapper only launches when the
answer is yes.

v0.2 ships a `doctor --generate-service` generator that writes the
launchd plist, systemd unit, or portable shell script for this
pattern. One command, one file:

```
agentchute doctor --generate-service launchd --as codex \
  --out ~/Library/LaunchAgents/com.agentchute.codex.plist
launchctl load ~/Library/LaunchAgents/com.agentchute.codex.plist
```

The generated unit polls every 30 seconds, launches `codex exec`
under flock single-flight when work exists, and is fully
protocol-compliant: the model still owns the consumption decision,
the bare `agentchute check` is never run as a daemon, and nothing
about your inbox changes until the wrapper actually processes a turn.

### Tier 3 — Finish-hook continuation

Tiers 1 and 2 wake a stopped wrapper. Tier 3 handles the case where
the wrapper is already running and new mail arrives mid-session: the
Stop hook (Claude / codex) or AfterAgent hook (gemini) catches the
new mail and tells the wrapper to keep going.

The v0.2 surface is `gate --before continue`, with wrapper-specific
output framing:

```
{
  "hooks": {
    "AfterAgent": [
      {
        "command": "agentchute gate --before continue --as gemini-cli --gemini-hook AfterAgent"
      }
    ]
  }
}
```

On clear inbox: `{"decision":"allow"}` and the session ends normally.
On new mail: `{"decision":"deny","reason":"..."}` and gemini
continues into another turn.

## What about tmux?

tmux is still there. We didn't remove anything. The convenience-
adapter contract in §8 is unchanged. If you have a setup that already
uses tmux send-keys for sender-side wake pokes, you don't need to
touch anything.

What we did is make sure that if you *can't* use tmux — or you don't
want to — you have a first-class, documented, generator-supported
path that's just as good. Better, in some ways: tier 2 is
single-flight, observable through systemd/launchd logs, and works
across hosts.

## The protocol's job, the operator's job

One thing this rewrite clarified: the protocol's job is to define
*what* delivery and discovery mean (best-effort, recipient-owned).
The operator's job is to pick *how* discovery happens for their setup.

For a long time we conflated the two by leading with tmux. v0.2 makes
the separation explicit. The model is still simple: write to an
inbox, poll your inbox. Every tier — `/loop`, launchd, an AfterAgent
hook, even you typing `agentchute check` once an hour — is just a
flavor of "poll your inbox."

If you've been hesitant to wire up agentchute because you don't run
tmux, v0.2 is the release for you.

---

*Upgrade: `go install github.com/agentchute/agentchute@latest` and
run `agentchute doctor --generate-service --help` to see the new
generator. See [AGENTCHUTE.md §8.2](AGENTCHUTE.md#82-wake-responsibility)
for the normative spec text.*
