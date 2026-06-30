# agentchute 0.8 — launch refresh bundle

Everything for the website + README + blog refresh that ships with 0.8. For team final review, then release.

## What's here / where it goes

| File | Destination | Notes |
|---|---|---|
| `README.md` | repo root (`README.md`) | Lean rewrite for the pull-only 0.8 design. **0.8 draft** — merge when 0.8 actually ships. |
| `docs/agentchute-hero.svg` | repo `docs/agentchute-hero.svg` | The hero diagram (team-reviewed revision: consistent presence dots, open-aperture aria-label, a subtle "recipient reads its own inbox" pull cue). README references it at this exact path. Self-contained background (renders the same in light/dark GitHub themes). |
| `blog/v0-8-0-simple-again.md` | site blog | The "why we trimmed" post. Render to your blog's HTML; keep the slug `v0-8-0-simple-again` so the README link resolves. Embeds `simple-deleted.svg`. |
| `blog/simple-deleted.svg` | next to the rendered post | The post's before/after image (overbuilt tangle → one inbox). Self-contained background. Keep it resolvable from the published post (relative path or your asset pipeline). |
| `site/index.html` | `agentchute.dev` landing | Self-contained mockup (inlines its own copy of the diagram + web fonts). Open in a browser to review. |
| `internal/messaging-rationale.md` | **not shipped** | The positioning doc — critique, the framing rules, and copy rationale. For the team's review of *why* the copy is what it is. Do not publish. |

## Before you publish — checklist

1. **Install URL — resolved.** The landing and README both now use the real `raw.githubusercontent…/install.sh`. If you later wire the `https://get.agentchute.dev` vanity redirect, you can swap it back in both places — but don't ship a `get.` link until the DNS/redirect resolves (it currently does not).
2. **Place the SVGs.** Hero at `docs/agentchute-hero.svg` (or update the `<img src>` in `README.md`); the blog's `simple-deleted.svg` must resolve from the published post.
3. **Verify SVG rendering** in both light and dark GitHub README themes — both SVGs ship a self-contained background, but confirm after placement.
4. **Blog slug + link.** Publish the post at `…/blog/v0-8-0-simple-again.html` (or change the link in `README.md` and the README callout). Also refresh the live site's "Latest from the blog" slot if it still points at an old release (this mockup has no blog slot).
5. **Don't merge the README before 0.8 ships.** It describes the lean shipped state (no watchdog, pull, `(to,from,seq)`, supervisor, conformance suite). If it lands ahead of the code it will describe things that don't exist yet. (Reconcile against the more detailed README already on `feat/simple-again-v2` — decide which is the launch README.)
6. **EXTENSIONS.md is stale — relink only after updating.** This bundle dropped the README's `EXTENSIONS.md` link because that doc still describes the deleted wake adapters/watchdog and the old `(timestamp,sender,nonce)` identity. Update `EXTENSIONS.md` to the pull-only v0.8 model before linking it anywhere (including the branch README, which still links it).
7. **Landing anchors.** The hero CTA and footer now say "Read the protocol" / link the real `AGENTCHUTE.md`. If you add a separate `spec.html`, point the nav/footer there instead.
8. **Aperture + framing held.** Copy says "any terminal-based agent" (the four are examples), qualifies that the reference runner ships ready-made `ac-*` launchers for the four, and frames the binary as a *separable reference implementation*, not "no code." Keep all three if you edit further — they were the corrections that mattered.

## Intentionally not in this bundle (optional follow-ups)

- **Dark-mode variant** of the landing page.
- **OG/social card** — the current `og:image` is GitHub's auto-generated card; a real one would help link shares. Say the word and I'll build either.
- Updates to `AGENTCHUTE.md`, `EXTENSIONS.md`, `CONTRIBUTING.md` — those are the team's protocol/contributor docs, out of scope for the marketing refresh.

## One-line summary for the team

The pages now sell the idea (inbox + Markdown + pull, any agent, open protocol) instead of the machinery 0.8 deletes — and the trim itself is the launch story.
