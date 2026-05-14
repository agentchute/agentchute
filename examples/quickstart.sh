#!/usr/bin/env bash
# Two agents, two tmux panes, one repo. The smallest useful agentchute setup.
#
# Read this top-to-bottom rather than running it blindly — it documents the
# minimum viable flow. Each command is intentionally separated so you can
# follow along.

set -euo pipefail

# ---------------------------------------------------------------------------
# 0. Prerequisites
# ---------------------------------------------------------------------------
# - tmux running (you're already in a session if you're here)
# - agentchute on PATH:  go install github.com/agentchute/agentchute@latest
# - this script run from your repo root

REPO_ROOT="$(pwd)"
VENDOR=".myorg"   # change to your owned namespace, e.g., .yourcompany

# ---------------------------------------------------------------------------
# 1. Drop AGENTCHUTE.md and the vendor loop directory.
# ---------------------------------------------------------------------------
# AGENTCHUTE.md lives at the repo root. Grab it from the agentchute release page
# or copy from a checkout. It's a single Markdown spec — read it once and you've seen the whole protocol.
[ -f "${REPO_ROOT}/AGENTCHUTE.md" ] || {
    echo "Drop AGENTCHUTE.md at the repo root before running this script."
    echo "  curl -O https://raw.githubusercontent.com/agentchute/agentchute/main/AGENTCHUTE.md"
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
# 2. Find your two tmux panes.
# ---------------------------------------------------------------------------
# Run this manually in your terminal to see your panes:
#   tmux list-panes -F '#{pane_index} #{pane_id} #{pane_current_command}'
#
# For this example we'll use %0 (Alice's pane) and %1 (Bob's pane).
ALICE_PANE="%0"
BOB_PANE="%1"

# ---------------------------------------------------------------------------
# 3. Each agent registers itself.
# ---------------------------------------------------------------------------
# In Alice's pane (or running this script as Alice):
agentchute register \
    --as alice \
    --vendor anthropic \
    --wake-method tmux \
    --wake-target "${ALICE_PANE}"

# Then in Bob's pane (have Bob run this; or here for demo):
agentchute register \
    --as bob \
    --vendor openai \
    --wake-method tmux \
    --wake-target "${BOB_PANE}"

# At this point:
# - .myorg/loop/agents/alice.md  exists (gitignored, machine-specific state)
# - .myorg/loop/agents/bob.md    exists (gitignored)
# - .myorg/loop/inbox/alice/     exists (empty)
# - .myorg/loop/inbox/bob/       exists (empty)

# ---------------------------------------------------------------------------
# 4. Alice sends Bob a message.
# ---------------------------------------------------------------------------
# This writes a markdown file into .myorg/loop/inbox/bob/, then pokes Bob's
# pane with two tmux send-keys calls (one for the literal "check", a short
# sleep, then one for Enter — the chained 'check' Enter form is unreliable
# across tmux versions). Bob's REPL receives "check" as input and Bob processes.
agentchute send \
    --from alice \
    --to bob \
    --task "review the README" \
    --body "Take a look at README.md and reply with edits."

# ---------------------------------------------------------------------------
# 5. Bob receives the poke and consumes the inbox.
# ---------------------------------------------------------------------------
# Bob's pane just got "check" typed into it. Bob runs:
agentchute check --as bob

# This prints the message and moves it to .myorg/loop/archive/.
# Bob can now reply:
agentchute send \
    --from bob \
    --to alice \
    --task "README review" \
    --body "Two findings: typo in line 3, missing license link."

# Alice's pane receives "check". Alice runs:
agentchute check --as alice

# Both inboxes are now empty. The archive contains both messages with
# consumed-timestamps prepended for chronology.

# ---------------------------------------------------------------------------
# That's all of it.
# ---------------------------------------------------------------------------
# - No daemon was started.
# - No config file was written.
# - No service is running in the background.
# - The whole interaction is files + tmux send-keys.
#
# For 24/7 setups where agents may run out of tokens, add the watchdog
# (see with-watchdog.sh).
#
# For status of all agents and their inbox depths:
agentchute status --as alice
