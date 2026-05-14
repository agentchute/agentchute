# agentchute — handoff to in-repo agents

Last updated: 2026-05-13.

This file captures **current state, pending work pre-tag, and the gating rules for destructive actions**. Read this after `AGENTS.md` and before touching anything.

---

## Where we are

**Version**: v0.1.0 pending tag. The spec, reference CLI, README, EXTENSIONS, website draft, examples, and tmux starter kit are all in agreement on the inbox-as-primitive framing and the protocol/implementation boundary. Pre-tag verification ritual (`gofmt`, `go vet`, `go test`, `go build`, `sh tests/install_test.sh`) passes locally; CI will repeat it on the tag push.

**Repo identity**:
- Target remote: `github.com/agentchute/agentchute`. The GitHub org `agentchute` is grabbed; the repo transfer from the legacy owner is pending.
- Domains owned: `agentchute.dev` + `agentchute.org`.
- Public-facing artifacts (README, install.sh, website, releases page) all reference the target remote — they go live the moment the transfer + tag land.

For everything else — architecture, working rules, scope, coordination — see [`AGENTS.md`](AGENTS.md). The protocol itself is [`AGENTCHUTE.md`](AGENTCHUTE.md).

---

## Pending work — pre-v0.1.0 tag

In rough order of dependency:

1. **GitHub repo transfer** to `agentchute/agentchute` (Alex's repo settings).
2. **Pre-tag verification ritual.** Run locally:
   ```sh
   gofmt -w .
   go vet ./...
   go test ./...
   go build ./...
   sh tests/install_test.sh
   goreleaser check    # if goreleaser is installed; catches release-workflow errors
   ```
   All must pass before the tag in step 3.
3. **Tag v0.1.0** annotated tag, push. Triggers GoReleaser via existing workflow.
4. **GoReleaser verify** — confirm the release workflow runs cleanly on the tag.
5. **install.sh smoke** against the live release artifact (`curl ... | sh` from a clean Mac/Linux).
6. **Demo re-record** — script in `examples/demo-script.md`.
7. **Show HN** — body + tweet drafts staged in `/tmp/agentchute-launch-artifacts.md` (operational, not in the repo).

---

## Gating rules (read every session)

**Do NOT do repo transfer (step 1), tag/push (step 3), or any release-workflow-triggering action without explicit current-message approval from Alex.** Step 2 (local verification) is pre-authorized; everything past `git push` is effectively permanent.

Even if Alex approved a similar action earlier in the conversation, the current request needs current approval. The `gh repo transfer`, `git tag`, `git push --tags`, and `gh release` commands all fall under this rule.

---

## Coordination

agentchute dogfoods itself: in-repo agents coordinate through the loop at `.rehumanlabs/loop/`. Enrollment instructions and routing patterns are in `AGENTS.md` § "Coordinating with other agents in this repo".

When in doubt about a non-trivial decision, surface it via the loop to one of the other in-repo agents (codex for review-shaped tasks, gemini for prior-art / cross-language sanity) before acting. Three-way agreement is the discipline that's kept this project from drifting.
