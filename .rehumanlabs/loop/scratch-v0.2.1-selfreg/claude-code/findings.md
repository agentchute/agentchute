# self-registration "never worked" — investigation (claude-code)

## Confirmed: the implementation tolerates missing self-registration silently

**`send.go` 148-156**:

```go
// Update sender's own last_seen if registration exists.
selfPath := cfg.AgentRegistrationPath(fromID)
if _, err := os.Stat(selfPath); err == nil {
    selfExists = true
    if err := loop.UpdateLastSeen(selfPath, now); err != nil {
        return fmt.Errorf("update last_seen for %s: %w", fromID, err)
    }
} else if !os.IsNotExist(err) {
    return fmt.Errorf("stat own registration: %w", err)
}
// ... continues with normal send flow ...
```

**`check.go` 70-79** has the exact same pattern.

So an agent can run `send` and `check` **forever** without ever having
its own registration record on disk. The implementation never:

- refuses to operate
- prints a warning
- auto-creates the registration
- prompts to run `boot` or `register`

The recipient is checked (line 168-169: must be registered, error
otherwise), but the **sender's own self-registration is treated as
optional**.

## Why agents "ignored" self-registration

Three failure modes, in increasing order of how easy they are to hit:

1. **Hook installation skipped.** v5 enrollment block says:
   > **If hooks are configured**, boot runs automatically. If not,
   > run it yourself before the first user prompt.
   This trusts an LLM agent to read its enrollment block and execute
   the boot command. LLMs are unreliable at "every session start, no
   exceptions" rituals — especially if the user's prompt is task-shaped.

2. **No fall-through enforcement.** Even if the LLM skips boot,
   nothing breaks visibly. `send` and `check` keep working. So the
   agent gets no feedback that anything is wrong, and there's no
   "this agent never showed up in /loop/agents/" alarm.

3. **Drift on environment changes.** An agent that DID boot once may
   not re-boot in a later session with a different `host`, `wake_target`,
   `cwd`, or `$TMUX_PANE`. AGENTCHUTE.md §A.1 explicitly says "the
   act of running `register` reconciles the registration against
   current `os.Hostname()`, `$TMUX_PANE`, and `cwd`. Verifying that a
   registration file exists is NOT sufficient" — but the
   implementation has no way to detect or enforce reconciliation.

## What "self-registration" promises vs. what code delivers

The spec promises:

> Each agent publishes a small registration record naming itself, its
> wake method/target (if any), and operational metadata.

The code delivers: "an agent MAY publish a registration record by
running `register`/`boot`. If it doesn't, `send` and `check` still
work; only `gate` notices (via `boot --quiet` failing) and peers
sending to this agent get a clear error from `send`."

That's a gap between protocol primitive ("Self-registration") and
implementation enforcement ("optional, soft-failing").

## Proposed fixes (ranked)

### Fix #1 (recommended): Refuse send/check on missing self-registration

Change the `os.Stat(selfPath)` blocks in `send.go` and `check.go` to
return a clear error:

```
agentchute send: --from %q is not registered. Run
  agentchute boot --as %s --vendor <vendor>
first. (Registration is required per AGENTCHUTE.md §5.)
```

**Pros**:
- Self-registration becomes load-bearing in the implementation,
  matching the spec.
- Failure mode is loud, immediate, and points at the fix.
- Aligns with §8.2 thinking: discovery (polling) is required, not optional.

**Cons**:
- Breaking change for any one-off scripts that used `send` without
  a registration. Mitigation: well-documented, easy to fix (run boot).
- New users hit it on first send — but the error message tells them
  exactly what to do.

### Fix #2: Auto-boot on first send/check

If self-registration is missing, infer `--vendor` from a vendor preset
(claude-code → anthropic, codex → openai, gemini-cli → google) or
fail with the same error as fix #1.

```
agentchute send: --from claude-code has no registration; auto-booting...
Registered claude-code (anthropic) — inbox clear, no pending replies
... continues to send ...
```

**Pros**:
- Backward-compatible (no commands fail that worked before).
- Solves "LLM forgot to run boot" silently.

**Cons**:
- "Magic" — the user sees a different command than the one they ran.
- Vendor presets are hard-coded; users with custom agent ids fail.
- The §A.1 reconciliation goal (refresh host / wake_target / pane
  each session) is harder to achieve when boot is implicit.

### Fix #3: Make boot a hard gate at hook level

Strengthen the enrollment-block wording from "run it yourself" to
"REQUIRED" + add a doctor check that fails BLOCKER if no live
registration exists for the current agent on the current host. Don't
change send/check behavior.

**Pros**:
- No code change to lifecycle commands.
- Doctor surfaces the gap visibly.

**Cons**:
- Still relies on the LLM reading and acting. Same root failure mode.
- Doctor only catches the gap if doctor is run, which it usually isn't.

## My pick

**Fix #1 + a small note in §5 that "send and check refuse to operate
without a self-registration."** That makes self-registration what its
spec text promises: a primitive load-bearing enough to be enforced.

The §A.1 reconciliation requirement is a stretch goal — register/boot
detect host/wake_target drift and refresh, but that's a separate v0.2.2
bite-size piece.

## Related observation

Live registration files (.rehumanlabs/loop/agents/<id>.md) are
correctly gitignored. The tracked `<id>.example.md` files are
templates — fine.

But `.rehumanlabs/loop/state/<agent>/pending-replies.json` files are
NOT gitignored and are accidentally committed (we saw this during the
v0.2 merge). That's a separate but related operational-state-leak
issue. Worth fixing in the same pass since the gitignore stanza needs
attention either way.
