# Security Policy

## Trust model

agentchute is a **cooperative, local, single-tenant** coordination tool. It assumes every agent sharing a loop directory is run by, and trusted by, the same operator. It is **not** a security boundary between mutually distrusting parties.

Concretely, on a shared `.agentchute/loop/`:

- **Messages are unsigned plaintext.** There is no authentication, no signing, no encryption, and no sandbox. A message's `from` field is a self-asserted string.
- **Any process with read/write access to the loop directory (or the repo) can read, delete, replay, or spoof messages** for any agent, and can read every agent's inbox, archive, and ledgers. Delivery is a filesystem `link()`; presence is a plaintext `.live` file.
- **The runner launches operator-chosen argv.** `agentchute run` (and the `ac` dispatcher) launch the wrapper command the operator configured and inject `[agentchute:run] check inbox` cues into that wrapper's local PTY. There is no remote wake and no sender-reachable endpoint (coordination is pull-only), but a process that can write your inbox can cause you to be cued to read attacker-controlled content.
- **Not suitable for hostile or multi-tenant hosts.** If untrusted code shares the machine, the user account, the repo, or the loop directory, it can fully compromise the pool.

### The rule

**Don't run agents — or share a loop directory — you don't trust on your machine.** The threat model agentchute defends against is *your own agents making mistakes*, not a malicious co-tenant.

## Supported versions

Only the **latest release** receives fixes. There is no LTS or backport line.

## Reporting a vulnerability

If you find a way the guarantees above **fail within the stated trust model** — for example, an agent able to escape the loop directory, a runner executing argv the operator did not configure, `setup --reset --wipe-state` escaping its canonical-loop-dir / symlink / live-bus guards, or clean-all removing outside its allowlisted, current-user, non-symlink, regular-file backup scope — please report it privately:

- Use **GitHub Private Vulnerability Reporting**: the repository **Security** tab → **Report a vulnerability**.

Please include the version (`agentchute version`), your OS, and a minimal reproduction. We aim to acknowledge within a few days.

Reports that a documented non-guarantee holds (e.g. "a peer process spoofed a message" — expected under cooperative trust) are welcome as **documentation** issues rather than vulnerabilities; open a normal issue for those.
