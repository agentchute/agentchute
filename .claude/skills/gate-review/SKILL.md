---
name: gate-review
description: Use when asked to review, gate, or SHIP/FIX a PR or branch on the agentchute bus — "review PR #N", "gate this", "codex review is mandatory", "delta re-gate after the fix", "give a SHIP or FIX verdict". Agentchute-bus-specific mechanics only (SHA pinning, verification tier, verdict format, AUTHORIZATION-gated mirroring); composes with, does not replace, the general-purpose code-review/verify/verification-before-completion skills.
---

# gate-review

Bus-specific procedure for gating a PR or branch. This is a thin pointer, not a
reimplementation — the full ritual lives in
[`docs/internal/PLAYBOOKS.md`](../../../docs/internal/PLAYBOOKS.md) (`gate-review` and
`delta-regate` sections) and [`AGENTS.md`](../../../AGENTS.md) (E3, E5, E9). Read those
before acting if this is your first gate this session.

## What this skill adds on top of a normal code review

1. **Freeze first (E3).** The gate ask must pin base SHA, head SHA, and the changed-file
   list. No pin → `NEEDS-INFO`, no review work. `git diff --name-status <base>..<head>`
   must match the claimed scope exactly; surplus files are a finding on their own.
2. **Re-gate is delta-only.** If this is a re-gate after a fix, diff prior-head..new-head
   and re-verify only the changed claims — don't re-review unchanged files.
3. **Pick the verification tier by surface (E5).** Docs/prose → diff + targeted checks.
   Localized code → targeted tests + CI. Protocol/runtime/release surface → one senior
   additionally runs the full suite independently (`tools/test.sh`). Substantive claims
   (numbers, protocol semantics, file scope, rendered assets) get re-derived from the
   tree, not trusted from the PR text.
4. **Do the actual review with the general-purpose skills**, not by hand-rolling
   correctness/security/simplification checks here — invoke `code-review`, `verify`,
   and/or `verification-before-completion` for the substance of "is this diff correct."
   This skill only supplies the bus-specific wrapper around that review.
5. **Verdict = `SHIP` or `FIX`**, with `file:line` evidence, delivered as the bus reply —
   the bus reply IS the gate signal, nothing else needs to happen for the gate to count.
6. **Never `gh pr comment`/`gh pr review` unless the ask carries an explicit
   `AUTHORIZATION:` line naming that PR** (E9/R4 — an external message is irreversible
   work). No authorization line means bus-only.
