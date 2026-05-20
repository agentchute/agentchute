# Investigation: why self-registration "never worked"

Research by: **gemini-cli** (integration seat)

Alex reported: *"why i do not see self-registration anymore, it never worked, every agent was ignoring it, why?"*

---

## 1. Findings (Reproduction)

We confirmed the implementation gap in v0.2.0:

1.  **Invisible Requirement**: While AGENTCHUTE.md §5 says registration is a `MUST`, the reference CLI's operational commands treat it as optional for the *acting* agent.
2.  **Reproduction A (`check`)**: Running `agentchute check --as unregistered-agent` reports `(inbox empty)` even if the agent registration and inbox directory do not exist. It silently fails to discover that it isn't even part of the pool.
3.  **Reproduction B (`send`)**: Running `agentchute send --from unregistered-agent --to gemini-cli` succeeds. It leaves a message in the recipient's inbox from an agent who cannot be poked back (because they have no registration).
4.  **Ghost Agents**: Agents are "ignoring" registration because the tool lets them. If an agent skips the `boot` ritual (common in no-hook or unreliable-LLM setups), it can still perform all coordination tasks without ever publishing its existence to the pool.

---

## 2. Reframing the Primitive

The name **"Self-registration"** is a bit of a misnomer in v0.2.0. It implies an autonomous behavior that the protocol currently leaves as an manual "ritual" (`boot` or `register`).

If we want it to "just work," the CLI must transition from **Advisory Registration** (soft requirement) to **Enforced Enrollment** (hard requirement).

---

## 3. Proposed Solutions

### A. Strict Enforcement (The "Loud" path)
Modify `check` and `send` to fail immediately if the actor is not registered.

- **`check --as <id>`**: If `agents/<id>.md` is missing, exit 1 with `error: agent "<id>" is not registered. Run agentchute boot --as <id> --vendor <vendor> first.`
- **`send --from <id>`**: Same check.

**UX Outcome**: If an LLM tries to coordinate without registering, the tool pushes back loudly. The model sees the error and is forced to "introduce itself" per the enrollment block.

### B. Auto-Registration (The "Silent" path)
Allow agents to "self-register" implicitly during their first operation.

- **`check --as <id> --vendor <v>`** (implicitly runs `boot`).
- **`send --from <id> --vendor <v>`** (implicitly runs `boot`).

**Critique**: This is messy. It adds `--vendor` to every command and hides the enrollment ritual. I recommend **Option A** instead.

### C. Protocol Clarification (§5 / §6)
Update AGENTCHUTE.md to formalize that a registration record is a **precondition for loop participation**.

- Add §5.7: "Enforcement: Conforming implementations MUST refuse to process or send messages if the acting agent's registration record is absent or unreadable."

---

## 4. Synthesis Recommendation

1.  **Close the "Silent Empty" Gap**: `check` must never report `(inbox empty)` if the agent is not registered. It must report `(not registered)`.
2.  **Mandate Sender Registration**: `send` must refuse to deliver mail from unregistered senders.
3.  **Promote `doctor`**: The enrollment block should tell agents to run `agentchute doctor` as their first move to verify their enrollment status.

I'll wait for codex's and claude's takes before drafting the code/spec changes.
