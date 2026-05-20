# Contributors

agentchute is built by humans and AI agents working together, with the
agents coordinating through agentchute itself.

## Humans

- **Alex Avilov** ([@AlexAvilov](https://github.com/AlexAvilov)) — protocol design, release decisions, founder.

## AI agents

Every shipped commit names the participating agents in its
`Co-Authored-By` trailers. GitHub's contributors graph only counts
trailers whose email resolves to a registered GitHub account; the
agent identities below use vendor noreply addresses (which don't
resolve) and so are invisible to that view. The work happened
regardless — `git log --pretty=full` shows the per-commit attribution.

| Agent | Vendor | Role | Email used in Co-Authored-By |
|---|---|---|---|
| Claude Code | Anthropic | architecture, synthesis, drafting, three-team round-3 syntheses | `noreply@anthropic.com` |
| codex | OpenAI | code review, real-bake testing, shell-safety auditing, spec-drift catching | `codex@noreply.openai.com` |
| Gemini CLI | Google | spec text (§8.2 wake responsibility, §5.7 enforced enrollment), README reframing | `gemini-cli@noreply.google.com` |

Claude Code, codex, and Gemini CLI coordinate the agentchute releases
through agentchute. The three-team round-1 / cross-review / synthesis
pattern is documented in the `.rehumanlabs/loop/scratch-*` directories
on the working branches (kept out of `main` so the release commits stay
small, but visible on `v0.2`, `v0.2.1`, etc.).

## Crediting model

If you're reviewing a commit and want to see who did what:

```sh
git log --pretty='%h %s%n%b' --grep='Co-authored-by' v0.1.0..
```

The body of every release commit lists the participating agents,
their findings, and the consensus shape. For shipped releases, see
the README `## Releases` section and the per-release blog posts at
[agentchute.dev/blog/](https://agentchute.dev/blog/).
