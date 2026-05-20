# v0.2 site-review — round-1 proposed edits (claude-code team)

Anchor: the new §8.2 normative wake-responsibility text + the v0.2
three-tier polling model + v0.2 commands (self-poll,
doctor --generate-service, gate --before continue).

Approach: surgical edits. Existing site structure is sound; the bug is
that v0.1-era framing presents tmux as a prerequisite. Fix the framing,
keep the layout.

---

## web/index.html

### Edit 1 — Featured-post pointer (block at lines 76-92)

Replace the v0.1.2 featured-post block with the v0.2 post:

```html
<section class="featured-post">
  <div class="wrap">
    <a href="blog/v0-2-recipient-polling.html" class="featured-post-link">
      <img
        src="images/blog/v0-2-workflow.jpg"
        alt="Four-panel comic of the agentchute workflow: human writes instructions, hands to agents, agents coordinate, deliver result back."
        class="featured-post-thumb"
        loading="lazy"
        width="240" height="160">
      <div class="featured-post-text">
        <p class="featured-post-meta">Latest from the blog</p>
        <p class="featured-post-title">v0.2: recipient polling, by hand and by hook <span class="featured-post-arrow" aria-hidden="true">→</span></p>
        <p class="featured-post-excerpt">tmux made the first agentchute demos feel immediate. It also blurred the protocol boundary. v0.2 fixes that — self-poll, gate --before continue, doctor --generate-service, and a §8.2 spec clarification that recipient polling is canonical.</p>
      </div>
    </a>
  </div>
</section>
```

### Edit 2 — Reference-implementation section header + framing
(lines 113-149)

Change `<h2>The v0.1 reference implementation</h2>` to
`<h2>The v0.1+ reference implementation</h2>` and rewrite the
introductory paragraph to align with §8.2:

> The CLI in this repo picks one concrete answer for each
> implementation-specific axis. It's not the protocol — it's the
> implementation we used while building the protocol, and it shipped
> the protocol's own releases.
>
> Recipient-side polling is canonical; the reference CLI ships one
> built-in wake adapter (`tmux send-keys`) as a convenience for
> single-host setups. Without tmux, the same protocol runs — recipients
> just poll their own inbox on their own cadence (v0.2 ships first-class
> helpers for that: `self-poll`, `gate --before continue`, and
> `doctor --generate-service`).

Update the bullet list to lead with polling, not tmux:

- Inbox medium: unchanged
- Transport: unchanged
- **Recipient discovery (v0.2):** `agentchute self-poll --as <id>`
  is the side-effect-free preflight; `doctor --generate-service`
  emits launchd/systemd/script unit files that run it on a cadence.
  Three tiers in [AGENTCHUTE.md §8.1](https://github.com/agentchute/agentchute/blob/main/AGENTCHUTE.md#81-running-the-reference-cli-without-tmux).
- **In-session catchup (v0.2):** `agentchute gate --before continue`
  emits decision JSON for Claude Stop / codex Stop / gemini AfterAgent
  hooks.
- **Optional peer wake (tmux):** `tmux send-keys` injects `check`
  into the recipient's pane. Best-effort convenience, not required —
  see [§8.2](https://github.com/agentchute/agentchute/blob/main/AGENTCHUTE.md#82-wake-responsibility).

### Edit 3 — "One prerequisite: tmux" section (lines 269-290)

This whole H3 + paragraphs is now misleading. Replace with:

```html
<h3>tmux is optional</h3>
<p>
  The v0.1 reference CLI ships <code>tmux send-keys</code> as one
  built-in peer-wake adapter. It is a convenience accelerator, not a
  prerequisite. Per
  <a href="https://github.com/agentchute/agentchute/blob/main/AGENTCHUTE.md#82-wake-responsibility">§8.2</a>,
  recipients are responsible for discovering their own mail on their
  own cadence; wake pokes are best-effort.
</p>
<p>
  Three recipient-polling tiers cover every wrapper:
</p>
<ul class="commands">
  <li><strong>Native loop:</strong> Claude Code's <code>/loop 5m</code> or Codex App Automations.</li>
  <li><strong>Preflighted scheduler:</strong> <code>agentchute doctor --generate-service launchd|systemd-service|systemd-timer|script --as &lt;id&gt;</code> emits the unit file. Each tick runs <code>agentchute self-poll --as &lt;id&gt;</code> (side-effect-free) and launches the wrapper only when work exists.</li>
  <li><strong>Finish-hook continuation:</strong> <code>agentchute gate --before continue</code> in Stop / AfterAgent hooks catches mid-turn arrivals.</li>
</ul>
<p>
  Important: a bare <code>agentchute check</code> shell loop without a
  wrapper is <em>not</em> a valid pattern. It consumes and archives
  messages without any model acting on them. Always schedule the
  wrapper, never the CLI alone.
</p>
```

### Edit 4 — Commands list (lines 256-264) gets v0.2 entries

Add three lines under the existing command list:

```html
<li><code>agentchute self-poll --as &lt;id&gt;</code> — side-effect-free "should I wake?" helper for schedulers and prompters (v0.2).</li>
<li><code>agentchute gate --before continue --gemini-hook AfterAgent</code> — in-session catchup decision for finish hooks (v0.2).</li>
<li><code>agentchute doctor --generate-service &lt;kind&gt; --as &lt;id&gt;</code> — emit launchd / systemd / script unit files for the preflighted-scheduler pattern (v0.2).</li>
```

### Edit 5 — Launch video caption (line 67-71)

Optional. The launch video shows tmux which is historically accurate.
Add one sentence acknowledging this is the v0.1 setup:

> Three agents coordinating in tmux during the final pre-release
> cleanup pass. 24x speedup of a real working session. (v0.1 setup;
> v0.2 also runs without tmux — see the latest blog post.)

---

## web/spec.html

### Edit 1 — meta + intro (lines 7, 35)

```
- <meta name="description" content="The agentchute protocol spec. Working draft v1; reference CLI v0.1. Inbox-based agent coordination.">
+ <meta name="description" content="The agentchute protocol spec. Working draft v1; reference CLI v0.1.x / v0.2. Inbox-based agent coordination via recipient-polled mailboxes.">
```

And in the intro:

```
- Working draft v1; reference CLI v0.1. The canonical version is
+ Working draft v1; reference CLI v0.1.x / v0.2. The canonical version is
```

### Edit 2 — Reference-implementation framing (lines 52-60)

```html
<strong>Reference implementation</strong> (v0.1 / v0.2):
shared filesystem inbox medium · atomic create-temp + rename delivery ·
vendor-namespaced dotdir at repo root · recipient-polling canonical
(v0.2 ships <code>self-poll</code> / <code>doctor --generate-service</code>
helpers) · tmux as one optional wake adapter.
```

### Edit 3 — Section list (lines 69-76)

- Add a §8.2 line under §8: `<li><strong>§8.2 Wake responsibility</strong> — the normative recipient-polling-is-canonical statement (v0.2).</li>`
- Rename `§8 Tmux wake adapter` → `§8 Wake adapters (best-effort)`.

### Edit 4 — Anchor ids

Add `id="8-wake-adapters"` and `id="82-wake-responsibility"` (or similar)
to the §8 and §8.2 entries so the v0.2 blog's deep links land on the
spec page (currently we link to GitHub anchors; would be nicer to link
to local).

NOTE: this requires section-content presence, not just the section
list. The current web/spec.html is just a section guide. To make
local anchors work, we'd need to embed the full spec content. Out of
scope for this round; the GitHub anchors are fine.

---

## web/blog/you-are-the-message-bus.html

Light touch only — the post is already well-aligned with §8.2.

### Edit 1 — Update line 131 reference

```
- reference CLI, that wake method is <code>tmux send-keys</code>, because our agents
- were running in tmux panes. Without a reachable wake method, delivery still works.
- The recipient picks up the message on its next polling cadence.
+ reference CLI, the built-in wake adapter is <code>tmux send-keys</code> for the
+ tmux case. v0.2 adds recipient-polling helpers (<code>self-poll</code>,
+ <code>doctor --generate-service</code>) for the no-tmux case. Either way,
+ delivery is the durable part; the recipient picks up the message on its own
+ polling cadence — see <a href="https://github.com/agentchute/agentchute/blob/main/AGENTCHUTE.md#82-wake-responsibility">§8.2</a>.
```

### Edit 2 — Line 204 sentence

```
- filesystem, atomic temp-file plus rename delivery, and tmux for peer wake. Those
+ filesystem, atomic temp-file plus rename delivery, and the recipient's own
+ polling cadence as the canonical discovery mechanism. tmux is one optional
+ wake adapter that reduces latency when both agents share a server. Those
```

### Edit 3 — Line 384

```
- rename is a well-understood delivery primitive on a shared filesystem. tmux is
+ rename is a well-understood delivery primitive on a shared filesystem.
+ Recipient-side polling is the canonical discovery mechanism. tmux is
```

---

## Out of scope for this round

- Full spec content embedded in web/spec.html (so local anchors work).
  Big work; defer.
- New "running multiple AI agents" hero rewrite. Current hero copy is
  fine and timeless.
- The two `<section class="flow">` animations. They show two-agent and
  multi-agent flows; they don't depend on tmux. Leave as-is.
- Quickstart shell snippet (lines 217-239). Already uses `.agentchute/`
  default namespace, no tmux mentioned. Leave as-is.

## Open questions for round-2 cross-review

1. The launch video on the landing page is tmux-flavored. Keep it
   (historical accuracy, "this is how it started") or commission a
   no-tmux version for v0.2? I lean keep + caption.
2. The featured-post block is now v0.2; should we keep the "Latest
   from the blog" meta-label or change it to "v0.2"? I lean keep.
3. Do we want a per-page anchor at the top of web/index.html for
   v0.2-specific commands (a small "v0.2 quick links" callout), or
   is the featured-post link + reference-impl section enough? I lean
   "section is enough."
