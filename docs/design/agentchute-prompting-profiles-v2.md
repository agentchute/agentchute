# agentchute prompting profiles — v2

Profiles are **presentation overlays** over ONE canonical task contract. They reshape wording, density, and order ONLY — never semantics, and never the set of required sections. There is exactly one task-body contract for every agent: **GOAL / CONTEXT / CONSTRAINTS / ACCEPTANCE / OUTPUT / ACTION MODE** (the Agent-to-Agent Communication Rules in [`AGENTS.md`](../../AGENTS.md)). A profile is HOW an agent likes to receive that one contract; it is NEVER a per-vendor task schema — different or extra required sections per vendor are forbidden.

## 1. The rule

When composing an agentchute task, resolve the recipient registration vendor if available; write ONE direct-addressed message using the standard task body contract; apply the recipient vendor profile ONLY as a style overlay; if vendor is missing/unknown use the generic standard contract; profiles never appear on the wire and never affect delivery, ordering, liveness, gates, or reply obligations.

**Fallback:** a missing or unknown `Registration.vendor` means compose the generic canonical contract with no overlay.

## 2. Where profiles sit

The protocol is **substrate + envelope** — vendor-blind. Profiles are **presentation** — a layer above the bus.

- substrate (immutable record files, per-`(sender,recipient)` seq, `.live`) and envelope (`from`, `reply_required?`, `in_reply_to?`) know nothing about vendors.
- Profiles shape PRESENTATION; the canonical task contract (GOAL / CONTEXT / CONSTRAINTS / ACCEPTANCE / OUTPUT / ACTION MODE) stays stable and identical across vendors.
- Do not confuse the prompt profile (body presentation, owned by the **sender**) with the submit-dialect (how `serve` injects the consume trigger into the REPL, owned by **`serve`**). The submit-dialect is a transport detail, not a prompting concern.

The lean protocol gives no vendor hooks on the wire on purpose, so profiles cannot leak into delivery.

## 3. The overlays (apply by RECIPIENT vendor)

Each row reshapes the SAME canonical contract — same six sections, same semantics, different presentation.

| Vendor | Presentation guidance |
|---|---|
| **anthropic** (Claude) | Richer structure welcome — explicit sections / XML tags and reasoning scaffolds land well; it uses structure well, so don't over-trim. |
| **openai** (Codex) | Concise, outcome-first — state outcomes not steps; do NOT ask for an upfront plan (can trigger an early stop); durable rules live in `AGENTS.md`. |
| **google** (Gemini) | Context-first, instruction-last, terse — front-load CONTEXT, end with the instruction; keep any structure uniform; preserve conversation history. |
| **xai** (Grok) | Fully self-contained / stateless — repeat every needed fact in each message; name tools explicitly; assume no session memory. |

## 4. The profile is the recipient's, not the sender's

Look up the **recipient's** `vendor` and write the body in **that** overlay. The orchestrator writes in the recipient's profile, not its own — Opus over-scaffolding a Gemini task hurts Gemini, whose weak axis is instruction-following. Identity in the body is the agentchute id; the vendor used to pick the overlay is the registration field.

## 5. Integration invariants

- Presentation only: overlays reshape wording / density / order, never the required section set or the semantics of any section (preserves R7 in `AGENTS.md`).
- One contract for all vendors; unknown or missing vendor → generic canonical contract, no overlay.
- Profiles never appear on the wire and never affect delivery, ordering, liveness, gates, or reply obligations.
- Per-vendor receive preferences live in each wrapper's `## Communication profile` section (CLAUDE.md / CODEX.md / GEMINI.md / GROK.md); this doc is the overlay summary they point back to.

## 6. Selection input

Overlay selection reads `Registration.Vendor` (an advisory field, already present). It is **not** wired into the send / body-composition path — the send path reads the registration only for delivery (the wake receipt), not to compose or style the body — so applying a profile is docs/convention-first, no code.
