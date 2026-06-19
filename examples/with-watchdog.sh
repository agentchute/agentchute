#!/usr/bin/env bash
# Two agents + a watchdog daemon. For 24/7 setups where agents will eventually
# hit rate limits and stall.
#
# agentchute's watchdog is liveness-only: it scans peer inboxes, pokes
# recipients whose inboxes have stale messages, defers pokes for agents
# self-declaring exhaustion. It does NOT route, rank, assign, or interpret.

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
# 1. Two regular agents.
# ---------------------------------------------------------------------------
agentchute register --as alice --vendor anthropic --wake-method tmux --wake-target "%0"
agentchute register --as bob   --vendor openai    --wake-method tmux --wake-target "%1"

# ---------------------------------------------------------------------------
# 2. Watchdog registers with empty wake_method (non-pokable).
# ---------------------------------------------------------------------------
# The watchdog is non-pokable on purpose: it doesn't receive messages. The
# empty --wake-method tells senders to skip pokes targeted at it.
# --vendor is the agent's *origin* (anthropic, openai, google, local, human),
# NOT the loop namespace. The watchdog process is just a script in this repo,
# so we mark its origin as `local`.
agentchute register --as watchdog --vendor local --wake-method "" --wake-target ""

# ---------------------------------------------------------------------------
# 3. Run the standalone watchdog daemon.
# ---------------------------------------------------------------------------
# In most pools you don't need this — `agentchute check` runs cooperative
# waking on every cycle (AGENTCHUTE.md §10.2). The standalone daemon below
# is the unattended-liveness fallback for pools without an actively polling
# peer (e.g., overnight or 24/7 setups where all the human-facing wrappers
# might be paused).
#
# This is a long-lived process. Background it, or run as launchd/systemd job.
# Defaults: --interval 60 (cycle every 60s), --stale-threshold 300 (5 min),
# --message-age-threshold 90 (don't poke for messages younger than 90s).
agentchute watchdog --as watchdog &
WATCHDOG_PID=$!
echo "Watchdog running as PID ${WATCHDOG_PID}"

# Logs go to .myorg/loop/watchdog.log — `tail -f` it to see what the watchdog
# is doing.
#
#   tail -f .myorg/loop/watchdog.log
#
# Sample output:
#   2026-05-10T08:00:00Z poked bob (oldest msg age 2m22s)
#   2026-05-10T08:01:00Z deferring alice until 2026-05-10T10:00:00Z
#   2026-05-10T08:02:00Z bob last_seen fresh (12s); skipping
#   2026-05-10T08:03:00Z poked alice (oldest msg age 3m7s)

# ---------------------------------------------------------------------------
# 4. Agents declare exhaustion when they detect rate limits.
# ---------------------------------------------------------------------------
# When an agent's wrapper detects it's about to hit a rate limit, the wrapper
# updates its registration:
#   status: exhausted
#   restart_at: <UTC timestamp of next budget reset>
#
# When restart_at is in the future, the watchdog defers pokes to that agent.
# Once restart_at passes, the watchdog resumes normal stale-poke logic.
#
# Agents SHOULD update restart_at every turn even when active, so the
# watchdog has a forward estimate if the agent dies mid-task before being
# able to flip status. Example registration with continuous restart_at:
#
#   ---
#   agent_id: alice
#   vendor: anthropic
#   ...
#   status: active
#   restart_at: 2026-05-10T10:00:00Z   # next budget cycle, just in case
#   last_active: 2026-05-10T07:55:23Z
#   ---

# ---------------------------------------------------------------------------
# 5. Wrapper self-loop alternative: agent-hosted watchdog
# ---------------------------------------------------------------------------
# If one of your agents has a built-in recurring-task feature (Claude Code's
# /loop; other wrappers ship the same capability under different names), that
# agent can do the watchdog work in its existing polling loop. No separate
# daemon process needed.
#
# In Claude Code, invoke /loop with a polling task that:
#   1. Updates own last_seen and restart_at.
#   2. Processes own inbox (consume + act).
#   3. Runs `agentchute watchdog --once --as <id>` — pokes any stale peer.
#
# Caveat: an agent can't watchdog its own staleness. If you need full coverage
# including the wrapper-hosted agent's own liveness, run the standalone
# daemon (above) in parallel.
#
# See AGENTCHUTE.md §10 for the full algorithm and threshold semantics.

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
# To stop the watchdog:
#   kill ${WATCHDOG_PID}
#
# Or if running under launchd/systemd, use the unit's stop command.
