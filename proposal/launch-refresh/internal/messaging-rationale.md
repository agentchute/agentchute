# agentchute — messaging refresh + 0.8.0 narrative

Grounded in the live `agentchute.dev` and the GitHub README (read 2026-06-30). Confirm the canonical domain first — the live site is **.dev**; you referenced **.org**.

---

## 1. Diagnosis (short)

- **The pages sell complexity; the product is simplicity.** The hero is fine, then the body markets the exact machinery 0.8 deletes — the **watchdog is a headline** ("liveness sidecar"), and the "reference CLI" section is a wall of tmux/herdr/poller/presenced/reachability/shims.
- **Stale + wrong now.** Identity shown as `(timestamp, sender, nonce)` (now `(to,from,seq)`); "optional wake" framed as core; blog stuck at v0.3.5.
- **Strong asset to keep:** the "What it isn't" section — honest, sharp, differentiating. Keep it almost verbatim.
- **The opportunity:** "we over-built it, noticed, and deleted ~60%" is a rare, credible launch story. It makes the simplicity look *earned*, not naive — and earned simplicity is the whole pitch.

## 2. Core message (never bury it)

**One line:**
> An inbox per agent. A Markdown message. That's the protocol.

**Mental model (the three lines that must appear above the fold):**
> Every agent has an inbox — a directory. A message is a Markdown file dropped in it. The recipient reads its own inbox on its own schedule. No server, no broker, no SDK. Works with any terminal-based agent — Claude Code, Codex, Gemini, Grok, or your own.

Everything else on the page is detail under this frame. The current site re-complicates in the *second sentence* (install → "wire lifecycle hooks and namespaced launcher shims"). Don't.

**Two framing rules baked into the above:**
- **Open the aperture.** It's not a fixed list of four vendors — it's *any terminal-based agent harness*. Name the four as examples, never as the supported set.
- **Say "reference implementation," never "no code."** There *is* real code — a small CLI and a per-agent supervisor. The message is that it's **separable from the protocol** (anyone can write their own, in any language), not that it's absent. "No binary required" reads as "code optional/unwanted" and undersells a capable implementation; reframe to "the protocol is just files, so you *can* replace our implementation — but you don't have to."

## 3. New hero (drop-in copy)

**Headline (keep yours — it's good):**
> Running multiple AI agents on one project is easy. Coordinating them isn't.

**Subhead (tighten):**
> agentchute is a small Markdown protocol that lets AI agents hand off work, request review, and message each other — without a human relaying every step. An inbox per agent, plain files, no broker.

**Install (one line, no machinery talk):**
```
curl -fsSL https://raw.githubusercontent.com/agentchute/agentchute/main/install.sh | sh
```
> That's the reference CLI. The protocol itself is just files — bring your own implementation if you'd rather.

**One CTA, not five:** `Watch the 62-second demo` (the video is your best asset). Secondary: `Read the spec`.

## 4. New landing structure (replaces the machinery wall)

Order, with copy. Cut the entire "reference CLI" feature wall; replace with §4.5 below.

**4.1 Hero** — §3.

**4.2 One diagram** (see §5) — N agents, each with an inbox, message-cards moving peer-to-peer, **no box in the middle**. Caption: *No central broker. Senders write to an inbox; recipients read their own.*

**4.3 The demo video** — keep prominent; it's live proof. Caption stays (real session, 24× speedup, 62s) but drop the tmux/herdr adapter explainer from the caption.

**4.4 What's actually in the protocol** (tighten to the real primitives, updated wire):
- **Per-recipient inbox.** Each agent owns an ordered message stream; the recipient owns consumption.
- **Identified, ordered messages.** Each message has a durable `(to, from, seq)` identity; a sender's messages stay in order, with no clock.
- **No-overwrite delivery.** A sender never clobbers an existing message; re-sending the same message is a safe no-op.
- **Recipient reads its own inbox.** Pull, not push. Senders write and walk away; delivery is best-effort and the message waits until the recipient reads it.
- **Self-registration + presence.** Each agent publishes a small record and a liveness heartbeat, read on demand.
- *The inbox medium and transport are implementation choices — files, a queue, HTTP, git all fit.*

**4.5 The reference implementation** (3 lines, not 3 screens):
> The reference CLI maps the protocol onto Markdown files on a shared filesystem. For agents that can't poll on their own, one small supervisor launches the agent, watches its inbox, and reads it. That's the entire runtime — no watchdog, no central process, no per-vendor glue. The same primitive works for every agent.

**4.6 What it isn't** — keep the existing section nearly verbatim (it's excellent). Add the new truthful line:
> Not a delivery broker. Delivery is best-effort and idempotent; the recipient reads on its own cadence. If you need queues with retries and exactly-once, use a queue.

**4.7 Two ways to start** (replace the long quickstart):
- **With the binary:** `install.sh` → `agentchute setup` → start your agents. One health check: `agentchute doctor`.
- **No binary:** drop `AGENTCHUTE.md`, write registration + Markdown files. The CLI and hand-protocol agents share the same pool.

**4.8 Footer** — keep the reHuman Labs thesis line; it's the emotional close: *let humans do human work, let agents do agent work, stop using humans as a message bus.*

## 5. Design direction (concise)

- **One hero diagram, no walls of text.** The current site is paragraphs where it needs a picture. Show 3–4 agents, each a small box with an inbox tray, Markdown cards flowing between them, **deliberately no hub**. The absence of a center *is* the message ("no broker").
- **Terminal/monospace aesthetic, heavy restraint.** The product is about simplicity; the page should feel that way — lots of whitespace, one accent color, mono for the few code lines. Restraint is on-brand and cheap.
- **Progressive disclosure.** Above the fold = the mental model + install + video. Everything operational (commands, transports, worktrees) moves behind a `docs` link. A newcomer should see ~5 commands max, not 24.
- **Kill the feature wall.** The "reference CLI" enumerated machinery doesn't belong on a landing page at all — it's reference docs. Its presence is the single biggest thing making a simple tool look complicated.

## 6. The 0.8.0 narrative (publishable — blog post / release note)

> ### 0.8.0 — agentchute, simple again
>
> agentchute is one idea: an inbox per agent, and a small Markdown protocol around it, so a mixed team of AI agents can hand off work without a human relaying messages. That's it. It's a good idea, and the protocol around it was sound.
>
> Here's what happened. As we ran agentchute with real teams — large and small, with genuinely good results — the *implementation* drifted. Not the idea; the code around it. It kept accumulating weight that had nothing to do with a shared inbox: machinery built to solve problems the protocol never actually had.
>
> The root mistake was right at the start: **the sender poked the recipient** to come read its inbox. That one decision was the first wrong step, and it snowballed. Once a sender is responsible for waking a recipient, it has to know the recipient is reachable — so you add reachability tracking. Reachability goes stale — so you add a watchdog. The watchdog races — so you add liveness caches and gates. Every addition was fixing a problem that only existed because of the original wrong turn. We ended up with an intricate liveness subsystem wrapped around a protocol whose entire job is "drop a file in a directory."
>
> So we stopped and went back to the idea: **a shared inbox, best-effort delivery, and the recipient is responsible for reading its own inbox.** Pull, not push. The sender writes the message and walks away.
>
> That one change collapsed almost everything we'd been fighting. No sender poke means no reachability tracking, no watchdog, no liveness caches, no cross-agent gates — all gone. What's left is what we wanted from the beginning: one small, universal way to run an agent — it watches its own inbox and reads it — that works the same for Claude, Codex, Gemini, and Grok, because none of it depends on vendor behavior.
>
> **What changed**
> - The sender no longer wakes anyone. Delivery is best-effort; the message waits in the inbox until the recipient reads it.
> - Deleted: the watchdog, reachability caches, cooperative-wake, cross-agent liveness gates, and the per-vendor wake adapters built to prop them up.
> - Message identity is now a durable `(to, from, seq)` — a sender's messages stay ordered with no clock, and re-sending one is a safe no-op.
> - Reply obligations are owned by the asker, so a silent recipient surfaces as the asker's own overdue item instead of a hang.
> - The protocol's guarantees are now a small conformance suite — any implementation that passes it is conformant, on files, a queue, or anything else.
>
> **What didn't change**
> - The idea: an inbox, a Markdown message, the recipient reads it.
> - Still no server, no broker, no SDK. Still MIT. Still hand-runnable with `mv`.
> - The old line still holds: stop using humans as a message bus.
>
> **0.8 is the arrival, not another churn.** We went looking for the stable core and we found it. From here agentchute gets smaller and sharper, not bigger.

## 7. Open items / honest cautions

- **Confirm domain** (.dev vs .org) before any of this ships.
- **Don't fake traction.** Adoption is small (single-digit stars). The narrative leans on founder honesty and a sharp small tool — *not* implied popularity. Resist "trusted by teams everywhere" copy; it would undercut the credibility the honesty buys.
- **Frame 0.8 as convergence, not instability.** A "we rewrote it" post can read as churn. The "arrival, from here it only shrinks" framing (closing line of §6) is the antidote — keep it.
- **Keep "universal" precise.** It means *any agent/model, any terminal, plain files* — not any OS/topology. Reference CLI is POSIX (WSL on Windows), shared-FS. Say that plainly where relevant; overclaiming invites the one bad-faith reply that derails a launch thread.
