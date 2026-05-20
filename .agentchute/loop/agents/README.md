# `agents/` — registration format reference

Each agent in the agentchute pool writes one file here: `<agent-id>.md`. These files are **gitignored** — they contain machine-specific paths, tmux targets, and frequently-updated `last_seen` timestamps.

Tracked example files (`*.example.md`) demonstrate the format and serve as starting templates for new agents.

The full registration semantics are in root [`AGENTCHUTE.md`](../../../AGENTCHUTE.md) §5.

## Format

YAML frontmatter (required) + free-text Markdown body (optional):

```markdown
---
agent_id: <slug>              # required; short identifier
vendor: <vendor>              # required; e.g., anthropic, openai, google, local, human
control_repo: <abs-path>      # required; absolute path to the control repo
working_repos:                # optional; list of repos this agent edits
  - <abs-path>
host: <hostname>              # optional; defaults to os.Hostname() at registration
wake_method: <adapter>        # conditional: the wake adapter; v0.1 reference is "tmux". Empty = non-pokable.
wake_target: <addr>           # conditional: opaque address parsed by the adapter; required when wake_method is set
last_seen: <iso-8601-utc>     # required; updated each turn
status: active                # optional; one of active | exhausted | offline (default active)
restart_at: <iso-8601-utc>    # optional; forward-looking estimate of when next poke is worthwhile
last_active: <iso-8601-utc>   # optional; last successful inbox processing
---

# Free-text notes (optional)

Role, constraints, local context, etc.
```

See root `AGENTCHUTE.md` §5 for full field semantics, §10 for how `status` and `restart_at` interact with the watchdog.

## Adding a new agent

Easiest path — use the binary:

```sh
# from the repo root, after `go build -o agentchute`
./agentchute register --as <your-id> --vendor <vendor-slug> --wake-method tmux --wake-target <pane-id>
```

By hand:

1. Copy an existing `<id>.example.md` to `<your-id>.md`.
2. Edit the frontmatter (see field reference above and `AGENTCHUTE.md` §5).
3. Verify the file is gitignored: `git check-ignore <your-id>.md` (should print the path).
4. Create your inbox: `mkdir -p ../inbox/<your-id>`.
5. Send an initial test message to one of the existing agents to confirm the registration is live.

## Updating `last_seen`

Update at the start of each turn. macOS:

```sh
sed -i '' "s/^last_seen:.*$/last_seen: $(date -u +%Y-%m-%dT%H:%M:%SZ)/" agents/<your-id>.md
```

Linux:

```sh
sed -i "s/^last_seen:.*$/last_seen: $(date -u +%Y-%m-%dT%H:%M:%SZ)/" agents/<your-id>.md
```

Or just let `agentchute send` / `agentchute check` / `agentchute status` handle it; the binary updates `last_seen` on every successful operation.

## Removing an agent

1. Delete `agents/<id>.md`.
2. Optionally delete `inbox/<id>/` if empty.
3. Other agents reading the registry will see no registration for that id and stop poking the pane.

The `<id>.example.md` stays as historical reference.
