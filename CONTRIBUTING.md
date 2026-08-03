# Contributing

Thanks for pitching in. Here's what you need to get going.

## Development Setup

Requirements:

- Go 1.24 or newer
- Git
- POSIX shell tools for the installer smoke test
- Optional: `gitleaks` for the pre-commit hook
- Optional: TruffleHog plus an authenticated Codex or Claude CLI, only if you
  want to exercise real reviews locally — the test suite never needs them

Clone the repository and run the same gate CI runs:

```sh
make verify
```

That builds and vets every package, runs the test suite, validates the
GoReleaser configuration, builds snapshot archives, and exercises the
checksum-verifying installer against those local artifacts. Run it before
opening a pull request.

Install the optional secret-scan hook after installing `gitleaks`:

```sh
make install-hooks
```

## Test Policy

Tests must never read or write the real `~/.claude` or `~/.codex` directories,
never require TruffleHog or an engine CLI on the machine, and never call a
model. Use `t.TempDir` and the existing seams: the fake engine returns canned
verdict JSON, and the runner interfaces stub every subprocess. Normal tests
must not require network access, a GitHub login, or files from a user's home
directory.

The rule that matters most: a missing review can never approve a change. If
you touch the pipeline, keep every failure path fail-closed, and keep error
text actionable — each error should tell its receiver exactly what to do
next.

## Change Checklist

- Add focused tests for target selection, ref errors, bundle contents, and
  verdict validation.
- Keep snapshots built from tracked Git tree/blob objects so archive
  attributes cannot hide or rewrite review context.
- Keep command arguments separate from shell strings; never invoke a shell for
  user-provided refs or paths.
- Run `make verify` before opening a pull request.

## Adding An Engine Adapter

An adapter drives one review CLI using that harness's own first-party
primitives — native flags and native permission profiles. No OS-level
sandboxes, no credential staging, no environment reconstruction; if a CLI
cannot confine a review with its own mechanisms, it does not get an adapter.

The basic checklist:

1. Implement the engine interface in `internal/engine/<name>.go`: a
   `Preflight` that checks the binary is on PATH, and a `Review` that builds
   the CLI's isolation arguments and runs through the shared heartbeat and
   retry helpers.
2. Isolation must cover: user and project configuration ignored; hooks, MCP
   servers, and skills disabled; read access scoped to the review snapshot.
3. Wire model and thinking defaults into `engine.go` and validate the
   thinking levels the CLI actually accepts.
4. Add table tests with a stub runner covering argument construction, the
   invalid-JSON retry, and failure messages.
5. Never fall back to another engine — a verdict must always say which judge
   produced it.

## Release Setup

The repository owner must create a GitHub environment named `release` and add
the desired deployment protection rules. The tag workflow is already bound to
that environment; it does not change repository settings itself.

## Licensing

Contributions are licensed under the MIT License and the contributor license
agreement below. The CLA check runs on a contributor's first pull request.

## Contributor License Agreement

By commenting `I have read the CLA Document and I hereby sign the CLA` on a
pull request, you grant Janior Valle and recipients of this project a
perpetual, worldwide, non-exclusive, royalty-free, irrevocable license to use,
reproduce, modify, display, perform, sublicense, and distribute your
contribution and derivative works under the project's license.

You represent that you are legally entitled to grant this license and that,
to your knowledge, the contribution is your original work or is submitted
with permission. You are not expected to provide support for the contribution
unless you agree to do so separately.
