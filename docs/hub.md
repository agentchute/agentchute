# SSH hub

An SSH hub lets agents on several macOS or Linux machines share one agentchute pool. It uses standard OpenSSH: the hub's `sshd` authenticates each joining key and its forced command pins that key to exactly one agent id and pool.

## Hub operator

The hub is the machine containing the existing control repo. It needs:

1. `sshd` running for the joining user's account. On macOS, enable Remote Login.
2. `agentchute` installed at a stable absolute path.
3. The existing pool; no separate agentchute service or hub setup command is required.

Normally, a user who can already SSH to the hub completes authorization from the joining machine. If they cannot, have them send the public-key command printed by `hub join`, then run it as the same login user that `sshd` authenticates:

```sh
agentchute hub authorize \
  --agent codex-tiny \
  --pool /home/alex/code/agentchute \
  --key "ssh-ed25519 AAAAC3Nz... agentchute:codex-tiny"
```

The pool path is absolute on the hub. `hub authorize` refuses a non-pool path or a non-executable agentchute binary, creates or reuses the pool's durable identity, writes the forced-command key line, and enforces the SSH directory and file modes.

Audit or revoke authorizations on the hub:

```sh
agentchute hub authorize --list
agentchute hub authorize --revoke codex-tiny --pool /home/alex/code/agentchute
```

### Authorization changes and live connections

OpenSSH checks `authorized_keys` when a connection authenticates, not for every session opened on that connection. Revocation, replacement, and forced-command or pool repointing therefore take effect at the next authentication. They do not interrupt a live `serve` channel or a live one-shot multiplex master. The `ControlPersist=60s` setting is an idle timeout; an active lane can keep its master alive indefinitely.

For an immediate change, stop or relaunch the lane on the joining machine. Reap a local one-shot master with `ssh -O exit` when the authorizing account also owns it. A hub operator cannot reap a master held by another host or account. This matters most for a pool repoint: until the old connection ends, new operations on it can still use the old forced-command snapshot and write to the old pool.

Key rotation through `hub join --rotate-key` is self-invalidating: the multiplex identity includes the resolved key version, so promoting the new key forces the next operation to authenticate again. The active client-side replace path also reaps a matching local master when one exists.

## Joining machine

Install agentchute, clone or open the repository checkout that the remote agent will work in, then join it to the hub. The URL path is the pool's absolute path on the hub:

```sh
cd ~/code/agentchute
agentchute hub join \
  ssh://alex@hub.example/home/alex/code/agentchute \
  --name codex
agentchute serve codex
```

After opening a new shell, the usual dispatcher form is equivalent:

```sh
ac serve codex
```

`--name codex` is a local launcher name. The pool-wide id includes the joining machine's hostname, so two machines can both use the local name `codex`. For an explicit pool-wide id instead:

```sh
agentchute hub join \
  ssh://alex@hub.example/home/alex/code/agentchute \
  --as review-mac
ac --as review-mac serve claude
```

The join is idempotent. A bare rerun reuses the active key. Rotate only when intended:

```sh
agentchute hub join \
  ssh://alex@hub.example/home/alex/code/agentchute \
  --name codex --rotate-key
```

Run `agentchute doctor` from the joined checkout to check the local hub record, active key, connection, pinned identity, protocol, and remote pool.

## Tailscale network recipe

Use Tailscale only as the network layer:

1. Put the hub and joining machines on one tailnet.
2. Restrict port 22 to the intended joining machines with tailnet access controls.
3. Join through the hub's MagicDNS name:

   ```sh
   agentchute hub join \
     ssh://alex@hub.tail1234.ts.net/home/alex/code/agentchute \
     --name codex
   ```

4. Keep standard `sshd` and `~/.ssh/authorized_keys` enabled on the hub.

Do not use Tailscale SSH for this connection. Agentchute relies on OpenSSH `authorized_keys` forced commands to pin a key to one identity and pool; Tailscale SSH uses its own authentication path. Tailscale still provides the private network, stable name, and access controls.

## Failures and recovery

Common failures are actionable and fail closed:

- `E_UNAUTHORIZED`: authorize the printed public key on the hub, or confirm it was not revoked.
- `E_HOSTKEY_CHANGED`: confirm the hub was legitimately rebuilt before running the printed `--reset-hostkey` recovery.
- `E_POOL_MISMATCH`: the key line and recorded pool identity disagree; re-authorize or rejoin only after confirming the intended pool.
- `E_LEASE_HELD`: another live serve owns that agent id; stop it or choose a distinct id.
- `E_CHANNEL_LOST`: the runner stops the child before relaunching. Remote serve relaunch is enabled by default; pass `--relaunch=false` only when an operator should restart it manually.

The complete error-code registry is in [AGENTCHUTE.md §13.10](../AGENTCHUTE.md#1310-error-code-registry).

## Upgrading a machine that has two copies of agentchute

Joining a hub rewrites the checkout's `.agentchute-control-repo` pointer to an `ssh://`
URL. A copy of agentchute old enough to predate hub support does not recognise that
locator — it reads the URL as a relative file path and fails with a mangled `lstat`:

```
agentchute status: pointer file discovery: /home/dmin/checkout/.agentchute-control-repo ->
"ssh://alex@hub.example/home/alex/pool":
lstat /home/dmin/checkout/ssh:/alex@hub.example/home/alex/pool: no such file or directory
```

Nothing is lost — the old copy fails closed — but the message does not say the binary is
the problem. Two copies on one machine is the ordinary state mid-upgrade, so `hub join`
warns when the `agentchute` on `PATH` is not the one performing the join. If you see that
warning, upgrade or reorder the other copy before relying on bare `agentchute`: hooks and
the runner both invoke it by name, so whichever copy `PATH` resolves is the one they get.
