# Contributing to agentchute

Thanks for considering it. Three things up-front:

1. **The spec is the source of truth.** [`AGENTCHUTE.md`](AGENTCHUTE.md) defines the wire format, file conventions, and behavior. If a PR changes any of that, propose the spec change first (open an issue or draft PR for the spec only). Don't sneak protocol changes into a code PR.

2. **The binary is intentionally small.** agentchute's pitch is "a few markdown files and an optional wake poke." If a PR adds a dependency, a config file, a new subcommand category, or a feature that requires explanation, the bar is high. We'd rather ship less.

3. **Test what you change.** If you fix a bug, add a test that fails without the fix. If you add a feature, integration tests > unit tests for v0.1.0. Run the full pre-commit ritual before sending the PR:

   ```sh
   gofmt -w .
   go vet ./...
   go test ./...
   go build ./...
   ```

   If the PR touches `install.sh` or release infrastructure, also run `sh tests/install_test.sh`.

## Setup

```sh
git clone https://github.com/agentchute/agentchute
cd agentchute
go test ./...
go build ./...
```

Requires Go 1.21+. Tests are hermetic — they run against temp loop directories and stubbed wrappers; no external daemon is required.

No new third-party dependencies beyond the existing PTY runner dependency (`github.com/creack/pty`) without a strong reason.

## Coding style

- `gofmt -w .` before commit.
- `go vet ./...` clean.
- Stdlib `flag` for argument parsing, not cobra or kingpin.
- Prefer integration tests over deep unit-test scaffolding.
- Keep commands as flat root files (`register.go`, `send.go`, etc.) — no `cmd/` subdirectory unless the binary materially grows.

## What's in scope

- Bug fixes (frontmatter parsing edge cases, race conditions in archive moves, runner/poller edge cases).
- Additional integration tests.
- Better error messages.
- Documentation improvements (especially in `AGENTCHUTE.md` worked examples).
- Spec clarifications when the current text is ambiguous.
- Filesystem portability fixes (Linux distros, macOS edge cases).

## What's not in scope

For v0.x:

- Non-filesystem inbox transports in the v0.1 reference CLI. Alternate transports (queues, HTTP, object stores) are protocol-compatible per EXTENSIONS.md but don't ship in v0.1. Within the v0.1 CLI, pool participants must share one filesystem — cross-machine over a network mount is in scope, cross-filesystem inside one v0.1 pool is not.
- Wildcard inboxes / broadcast / self-claim mechanisms (deliberately excluded — see `AGENTCHUTE.md` §7 / §12).
- Capability-based routing (also deliberately excluded).
- Built-in role/election machinery.
- Configuration files (env vars + flags are enough).
- Additional message formats (just markdown + frontmatter).
- Native Windows support (WSL is fine).

If you want any of those, fork it. We'd rather agentchute stay small.

## PR process

1. Open an issue describing the bug or proposed change. (Skip for typo fixes and obvious bug fixes.)
2. Fork, branch, commit. Reference the issue in commit messages.
3. Run `gofmt -w . && go vet ./... && go test ./...` and confirm clean.
4. Open the PR. Include: what changed, why, and what you tested.
5. Wait for review. Maintainers will engage on merits, not gatekeep on style alone.

If review takes more than a week, ping the issue. We're a small team.

## Spec changes

Spec changes are heavier than code changes. The spec is what makes agentchute portable across implementations. If you want to change behavior:

1. Open a spec-only PR proposing the change to `AGENTCHUTE.md`.
2. Justify why the existing behavior is wrong or insufficient.
3. After spec discussion + merge, follow with the code change in a separate PR.

This sequencing prevents implementations diverging from the spec.

## Reporting bugs

Open a GitHub issue with:

- agentchute version (release tag, or commit hash if built from source).
- macOS or Linux + version.
- Minimal reproduction (the registration files and command sequence that surface the bug).
- Expected vs actual behavior.

If you can include the contents of `<vendor>/loop/agents/<id>.md` and the inbox file involved, even better.

## Code of conduct

Don't be a jerk. We expect and provide professional courtesy. Disagreement on technical merits is welcome; personal attacks aren't.

## License

By contributing, you agree your contributions are licensed under the MIT license (see [`LICENSE`](LICENSE)).
