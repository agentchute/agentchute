#!/usr/bin/env bash
# Three agents — Claude Code + codex CLI + gemini — coordinating in one tmux
# session. Shows how registration `--vendor` works as origin metadata and how
# senders pick recipients explicitly (no broadcast in v1).

set -euo pipefail

REPO_ROOT="$(pwd)"
VENDOR=".myorg"

[ -f "${REPO_ROOT}/AGENTCHUTE.md" ] || {
    echo "Drop AGENTCHUTE.md at the repo root first."
    exit 1
}

mkdir -p "${REPO_ROOT}/${VENDOR}/loop/"{agents,inbox,archive,malformed}

# Recommended: ignore agent-local state in git.
# v1 begin/end markers match what `agentchute init` emits, so a later
# `agentchute init` recognizes this stanza and doesn't append a duplicate.
GITIGNORE="${REPO_ROOT}/.gitignore"
if ! grep -q "${VENDOR}/loop/inbox" "${GITIGNORE}" 2>/dev/null; then
    cat >> "${GITIGNORE}" <<EOF
# agentchute-gitignore v1 begin
${VENDOR}/loop/agents/*.md
!${VENDOR}/loop/agents/*.example.md
!${VENDOR}/loop/agents/README.md
${VENDOR}/loop/inbox/
${VENDOR}/loop/archive/
${VENDOR}/loop/malformed/
${VENDOR}/loop/watchdog.log
# agentchute-gitignore v1 end
EOF
fi

# ---------------------------------------------------------------------------
# 1. Three agents in three tmux panes.
# ---------------------------------------------------------------------------
# Run `tmux list-panes -F '#{pane_index} #{pane_id} #{pane_current_command}'`
# in your terminal to see actual pane ids. Substitute below.
CLAUDE_PANE="%0"
CODEX_PANE="%1"
GEMINI_PANE="%2"

# ---------------------------------------------------------------------------
# 2. Each agent registers with its own vendor.
# ---------------------------------------------------------------------------
# Fixed-cast demos use explicit --as names so the transcript is stable. In
# normal wrapper setup, --vendor is enough for contextual <wrapper>-<folder>
# IDs, with --as reserved for custom roster lanes.
agentchute register --as claude-code --vendor anthropic --wake-method tmux --wake-target "${CLAUDE_PANE}"
agentchute register --as codex       --vendor openai    --wake-method tmux --wake-target "${CODEX_PANE}"
agentchute register --as gemini-cli  --vendor google    --wake-method tmux --wake-target "${GEMINI_PANE}"

# `vendor` is metadata — recommended convention is the agent's origin
# (anthropic / openai / google / local / human / etc.). It's not enforced as
# an enum; agentchute doesn't route on it. Other agents read the registry to
# know who's around.

# ---------------------------------------------------------------------------
# 3. Direct addressing.
# ---------------------------------------------------------------------------
# agentchute v1 does NOT have wildcard inboxes or broadcast. Every message is
# addressed to exactly one recipient. If you don't know who should take a
# task, ask someone first ("who should review this?") and let them recommend
# in their reply. The originator picks.

# Example: Alice (well, claude-code) drafts; codex reviews; gemini gets a
# courtesy ping at the end.
agentchute send --from claude-code --to codex \
    --task "draft review" \
    --body "Draft is at /tmp/draft.md. Looking for naming nits and architectural flags."

# codex's pane receives "check". codex runs:
agentchute check --as codex
# codex reads, comments, replies:
agentchute send --from codex --to claude-code \
    --task "draft review findings" \
    --status findings \
    --body "Three nits, one architectural concern about module boundaries."

# claude-code consumes:
agentchute check --as claude-code
# claude-code applies findings, then loops in gemini for a sanity check:
agentchute send --from claude-code --to gemini-cli \
    --task "sanity check pass" \
    --body "Codex flagged module boundaries. Want a third opinion before I commit."

# gemini's pane receives "check". gemini runs:
agentchute check --as gemini-cli

# ---------------------------------------------------------------------------
# 4. Self-description via registration body.
# ---------------------------------------------------------------------------
# Each agent's registration file has an optional free-text body. Use it to
# tell other agents what you're focused on, what you're skipping, etc.
#
# Edit .myorg/loop/agents/codex.md and add after the frontmatter:
#
#   # codex (this session)
#
#   Currently focused on review-only work. Skip me for code generation
#   tasks; ping claude-code or gemini for those. Heavy on adversarial
#   review and integration testing.
#
# Other agents can read this as coordination context — to decide who to
# message manually. They MUST NOT use it for capability-based routing
# (AGENTCHUTE.md §7.3 / §12). The protocol does NOT
# parse the body — it's purely advisory.

# ---------------------------------------------------------------------------
# 5. Status overview at any point.
# ---------------------------------------------------------------------------
agentchute status --as claude-code

# Output is something like:
#   AGENT         STATUS    INBOX   LAST_SEEN              AGE   HOST           WAKE
#   claude-code   active    0       2026-05-09T12:34:56Z   3s    macbook-pro    tmux:%0
#   codex         active    0       2026-05-09T12:34:51Z   8s    macbook-pro    tmux:%1
#   gemini-cli    active    1       2026-05-09T12:30:00Z   5m    macbook-pro    tmux:%2
#
# gemini-cli has 1 unread message and is 5 minutes since last_seen. If
# gemini-cli stays stale and the inbox stays non-empty, the watchdog (if
# running) will poke it.

# ---------------------------------------------------------------------------
# Tip: long-running setups
# ---------------------------------------------------------------------------
# For multi-agent setups that run for hours, definitely run the watchdog.
# When agents hit rate limits, the watchdog defers pokes during the budget
# window and resumes after — so the team doesn't get stuck waiting for an
# exhausted agent. See with-watchdog.sh.
