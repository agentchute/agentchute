# Delegated destructive authorization: keep the direct-human boundary

Status: PROPOSED
Base: `main` at `5a94ceaf9c6741198ce7e4028b238645b1aa6906`
Recommendation: **DO NOT BUILD an in-repo grant mechanism.** Adopt the explicit action-tier vocabulary below, and keep direct operator confirmation for operator-reserved scopes unless a deployment supplies an operator signing identity that no lane can access.

## Decision

agentchute will not add `grant`, `verify-grant`, or `revoke` commands, grant records under `.agentchute/loop`, a grant audit ledger, or guard integration.

Delegation may authorize lane-local and repository-additive scopes. It may assign and prepare operator-reserved work, but it cannot authorize the reserved step. Immediately before that step, the operator must confirm it through the target lane's direct human-input channel. An inbox message may request the confirmation or describe the intended action, but it is never evidence that confirmation occurred.

This bottleneck is deliberate. In the reference deployment, the operator and every lane run as the same OS user. A local file can prove that the OS user wrote bytes; it cannot prove that the human operator, rather than an agent process acting on inbox content, authorized those bytes.

The action tier is an explicit lookup, not an interpretation of adjectives such as "destructive", "external", or "irreversible". During this design, the PM and implementer classified a new owned branch push and in-repository PR differently under those adjectives. The scope table below makes those additive actions PM-authorized and fails closed on actions it does not enumerate.

## Threat model

### Defended threat

An attacker can control every byte delivered through an inbox, including the apparent sender, frontmatter, quoted operator text, task instructions, and references to other files. The attacker may replay an old message or claim that a PM or operator approved an action.

The required invariant is:

> No fact supplied through an inbox, or written by a process with the same authority as a lane, can authorize an operator-reserved action.

The target is indirect prompt injection, not a malicious host process. The recipient must be able to process an inbox task without treating any part of it as human authorization.

### Threats agentchute does not defend

A malicious or compromised process running as the pool's OS user can already edit the repository, rewrite `.agentchute/loop`, invoke Git or GitHub with the shared credentials, and delete the guard latch. This is the protocol's documented cooperative-trust boundary, not a capability that a grant file can contain.

Compromise of the wrapper's direct human-input channel or of the hosting platform is also out of scope. That channel is the existing trusted distinction: the wrapper presents operator input as a direct user turn, not as inbox data.

## Why the proposed grant record fails

An operator-written record is trustworthy only if the verifier can authenticate its author. The proposed local mechanism has no such identity boundary:

- A record under `.agentchute/loop/state` is writable by any lane running as the same OS user. Modes `0700` and `0600` exclude other users, not sibling processes owned by that user.
- A serve token proves which runner session currently owns an agent ID. It does not prove human presence and is available to the lane it fences.
- A grant nonce, narrow scope, expiry, or revocation flag can limit a genuine grant, but none authenticates who created it. An attacker can create a fresh, narrow, unexpired record just as easily as an operator can.
- A shared secret stored in the repository, filesystem, process environment, or command line is available to the lane and therefore to commands induced through that lane.
- An append-only local audit file is neither append-only nor authenticated against the same OS user. It cannot serve as authorization evidence.

Public-key signatures could establish provenance only if the signing key is unavailable to every lane. That requires a separate OS account, hardware-backed user presence, or an external signing service, plus key enrollment, canonical serialization, rotation, revocation, clock, replay, and recovery rules. It would reverse the protocol's explicit non-goals of signing, authentication, capabilities, and durable authenticated audit, and it is not a small addition to this CLI.

## Retained mechanism

1. A PM or peer may assign the task over agentchute. The task names the applicable scope tokens and exact targets. The lane may perform lane-local and repository-additive scopes within that assignment.
2. The lane stops before an operator-reserved step. The PM routes the need to the operator.
3. The operator sends a direct user message to that lane naming the exact action and target. Relayed text, an inbox quote, a repository file, or a claimed grant ID does not count.
4. The lane resolves and compares the actual targets immediately before execution. A changed target or broader action requires new direct confirmation.
5. After execution, the lane reports the result to the PM. That report is status, not reusable authority.

The guard remains independent. It protects the mail claim/commit path as a best-effort speed bump; it does not consult authorization and must not be described as enforcing this decision.

## Scope vocabulary and authority tiers

The vocabulary answers which authority is required. It is policy for task routing, not a capability language or a token the binary verifies.

| Scope | Included action | Minimum authority |
|---|---|---|
| `lane.local` | Inspect or edit the task worktree; run tests; build local artifacts; create local commits, branches, or worktrees; remove task-created scratch after verifying that no unique work will be lost. | Assigned task |
| `repo.ref.create` | Push a new, previously absent branch ref created and owned by the lane; never update or replace an existing remote ref. | PM task naming repository and ref |
| `repo.pr.open` | Open a PR from an owned new ref to a named base in the same repository. | PM task naming repository, head, and base |
| `repo.pr.write` | Comment on or review a named PR in the same repository. | PM task naming repository, PR, and action |
| `repo.shared.mutate` | Merge to a shared branch, delete or replace a shared ref or data, force-update, rewrite published history, or change repository settings. | Direct operator confirmation to the acting lane |
| `repo.tag.mutate` | Create, move, or delete a tag. | Direct operator confirmation to the acting lane |
| `repo.release.publish` | Publish, edit, or delete a release. | Direct operator confirmation to the acting lane |
| `account.mutate` | Read or change credentials, account state, access control, or service settings. | Direct operator confirmation to the acting lane |
| `external.write` | Send, publish, mutate, or delete anything outside the named repository. | Direct operator confirmation to the acting lane |

An action not listed above is operator-reserved until the table is deliberately amended. A mixed task uses the highest required authority. `repo.ref.create` becomes `repo.shared.mutate` if the resolved remote ref already exists; the lane checks that fact immediately before pushing.

Tasks SHOULD name scopes with `SCOPE:` and must still name the concrete action and targets. `AUTHORIZATION:` remains the recommended presentation for the exact action, not a token the binary parses. For example:

```text
SCOPE: repo.ref.create, repo.pr.open
AUTHORIZATION: push new branch codex/delegated-authorization at <sha> to origin; open a PR from that branch to main.
```

Broad descriptions such as `clean up stale branches`, `do whatever is needed`, or `apply this grant` are not sufficient. Shell syntax alone is also insufficient when it contains unresolved globs, substitutions, or variables; the task or confirmation names the resolved repository, service, or filesystem root and exact branch, ref, PR, tag, release, setting, endpoint, or path.

## Lifetime, expiry, and revocation

There is no durable grant to expire or replay.

- PM authorization applies only to enumerated PM scopes in the named task. Direct operator confirmation applies only to the named reserved action and targets in the current directly authenticated task context.
- It is consumed when that action completes and does not survive a handoff, relaunch, later task, changed target, or copied transcript.
- The operator may set a shorter deadline or revoke before execution through the same direct channel.
- An inbox instruction may always narrow, pause, or cancel work. It cannot extend, restore, or transfer operator-reserved authorization.

This avoids clock and revocation-list correctness claims. If execution has already produced an irreversible side effect, revocation cannot undo it; recovery is an operational concern.

## Audit trail

The wrapper session transcript records the direct operator confirmation, and Git, GitHub, or the affected service records the resulting action where those systems provide logs. The lane's report to the PM links the action back to the task operationally.

agentchute does not promote its gitignored archive or a new local ledger into an authenticated audit trail. A same-UID process can rewrite either. Deployments that require durable authorization evidence must use an external system whose writer identity and retention are outside lane control.

## What full inbox write access still cannot do

Under this decision, control of an inbox cannot authorize an operator-reserved scope. It cannot:

- create or replay a machine-verifiable authorization, because none exists;
- turn a quoted operator statement or PM assertion into direct human confirmation;
- broaden the action or targets named in a direct confirmation or move an action into a lower tier;
- transfer confirmation to another lane, task, or session; or
- make the guard or CLI bless an otherwise unauthorized destructive command.

Inbox control can still request work, misrepresent facts, cause denial of service, and exercise the PM scopes that the cooperative routing model assigns to a trusted PM identity. A process with the pool user's general shell access can bypass the coordination layer and act directly; those stronger threats are outside agentchute's security model.

## Reconsideration threshold

Reconsider delegated grants only when a supported deployment has an operator authentication principal that lanes cannot use. A future proposal must demonstrate all of the following before specifying scope or CLI syntax:

1. the authorizing key or service is inaccessible to every lane and same-UID process;
2. operator intent is authenticated with explicit user presence;
3. grants bind a lane, repository, exact action, exact targets, nonce, and short mandatory expiry;
4. verification, consumption, replay prevention, revocation, clock handling, rotation, and recovery fail closed; and
5. the audit sink is append-only outside lane control.

That is an authentication system and belongs in an extension or external service unless agentchute's security and protocol non-goals are deliberately reopened first.
