# v0.2.1 — "Enforced Enrollment" — round-3 synthesis

Three teams (claude-code, codex, gemini-cli) independently investigated
Alex's report that self-registration "never worked." All three agree on
the root cause and on the shape of the fix. This is the synthesis.

## Root cause

Self-registration is **normative in the spec** (AGENTCHUTE.md §5 says
every agent MUST publish a registration record; §7.2 calls it
mandatory protocol overhead on every session start) but **not enforced
by the reference CLI** at the entry points where it matters.

Specifically:
- `send.go` 148-156, `check.go` 70-79: missing own registration is
  silently tolerated. The command continues normally.
- `internal/loop/inbox.go`: a missing inbox directory reads as an
  empty inbox (`(nil, nil, nil)` on `os.ErrNotExist`). So
  `pending`/`check`/`watch` for an unregistered agent return
  "nothing here" instead of "not registered."
- `gate --before finish` and `--before continue` check inbox/ledger
  work but not self-registration presence.
- `pending` doesn't surface `needs_boot` like `self-poll` does, so
  UserPromptSubmit/BeforeAgent hooks don't see the gap.
- `agentchute init` installs the enrollment block text but does NOT
  install the hook files. Without hooks, the LLM is expected to read
  the enrollment block and run `boot` voluntarily — unreliable.

Net: an LLM agent that skips reading its enrollment block (or where
hooks aren't installed) can call `send`/`check` indefinitely, never
appear in `agents/`, never be discoverable by peers, and never see an
error. That's why every agent could "ignore" self-registration.

## Reframe: "Enforced Enrollment"

Gemini's framing: "self-registration" implies autonomy the
implementation doesn't deliver. v0.2.1 moves the primitive from
**advisory registration** (soft requirement, easy to skip) to
**enforced enrollment** (hard requirement, fails loudly on skip).

Spec/code now load-bear the §5 promise.

## v0.2.1 deliverables

### Code

1. **`internal/loop/inbox.go`** — `ListInboxMessages*` returns a
   distinct error (or sentinel) when the inbox dir doesn't exist,
   separate from "exists but empty." Callers can then distinguish
   "not enrolled" from "enrolled and idle."

2. **`send.go`** — if `agents/<from>.md` missing, refuse with:
   ```
   send: --from %q is not registered. Run
     agentchute boot --as %s --vendor <vendor>
   first. Self-registration is required per AGENTCHUTE.md §5.
   ```

3. **`check.go`** — same refusal pattern.

4. **`watch.go`** — same refusal pattern (acting agent must be
   registered).

5. **`status.go`** — when called with `--as <id>` for an unregistered
   id, same refusal pattern. (Pool-overview `status` without `--as`
   stays read-only and unaffected.)

6. **`gate.go`** — `--before finish` and `--before continue` add a
   "missing self-registration" reason to the blocked-reasons surface.
   Exit 2 / decision: deny, with the same boot pointer.

7. **`pending.go`** — port `self-poll`'s `needs_boot` detection.
   When registration is missing, surface a needs-boot reason in text
   and `--json` output and per-wrapper hook envelopes. Stays
   side-effect-free.

8. **`hooks install`** subcommand (or `init --install-hooks`) —
   copies hook templates from `examples/hooks/<wrapper>/...` into the
   wrapper-specific config location. Optional, with `--dry-run` and
   `--force` flags. Required to make the enrollment story
   "just work" without LLM cooperation.

### Spec

9. **AGENTCHUTE.md §5.7 (new)**: "Enforcement. Conforming
   implementations MUST refuse to process or send messages if the
   acting agent's registration record is absent or unreadable. The
   refusal SHOULD include a pointer to the registration command for
   the implementation (for the reference CLI: `agentchute boot --as
   <id> --vendor <vendor>`)."

### Docs

10. **Enrollment block (v6)** — drop the hedge ("If hooks are
    configured, this runs automatically. If not, run it yourself").
    Replace with: "Hooks installed via `agentchute hooks install`
    run this for you. Without hooks, `send` / `check` will refuse
    until `boot` has been run once."

11. **README** — new "Enrollment is enforced" subsection under the
    Hooks section, explaining the breaking change in v0.2.1 terms
    and the `hooks install` ergonomics.

12. **Blog post**: "Enrollment, enforced." Why self-registration was
    silent before; what v0.2.1 changes; how the LLM-trust-the-text
    failure mode is now caught at the tool layer.

### Tests

13. Refusal tests for each command (`check`, `send`, `gate`, `watch`,
    `status --as`, `pending`, hook envelopes).
14. Inbox-missing distinct from inbox-empty test.
15. `hooks install` test (writes to a temp config dir).

## Compatibility / migration

**This is a breaking change** for any caller (script, CI, agent
wrapper) that runs `send`/`check`/`watch`/`gate` without ever having
called `boot`/`register`. The breakage is loud and the fix is
single-command (`agentchute boot --as <id> --vendor <vendor>`).

Pools already in use are unaffected: every active agent in our own
pool already has a registration record on disk (we just verified).

The protocol wire format and on-disk layout are unchanged. Existing
messages, archives, and pending-reply ledgers work as-is.

## Three-team votes (round 1)

- claude-code: refuse send/check; spec note
- codex: refuse check/send/watch/status; gate block; pending
  needs_boot; hook installer
- gemini-cli: refuse check/send; new §5.7 normative text; promote
  doctor in enrollment

This synthesis takes codex's broader code-scope and gemini's spec
text, plus the round-1 hypothesis about hook installation as the
operator-facing fix.

## Open question for Alex

`hooks install` — should it default to `~/.claude/`, `~/.codex/`,
`~/.gemini/` (user-global) or `.claude/`, `.codex/`, `.gemini/`
(repo-local)? Different wrappers default to different scopes; this
might need a per-wrapper `--scope user|repo` flag.

## Out of scope for v0.2.1

- §A.1 reconciliation (refresh host / wake_target / pane on every
  boot, detect drift) — separate v0.2.2 bite.
- `pending-replies.json` gitignore fix (operational state leaking
  into commits) — small standalone fix, can ride with v0.2.1 if
  cheap.
- Watchdog auto-removal of long-stale registrations — out of scope.
