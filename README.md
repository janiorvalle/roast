# roast

<p align="center">
  <img src="assets/hero.png" alt="roast - code review at the pass. nothing ships raw." width="840">
</p>

Your coding agents write a lot of code, and somebody has to check it before it
ships. `roast` is that somebody: an independent AI code reviewer that judges
your diff and answers with an exit code. Findings mean **RAW** (exit 1). Clean
means **well done** (exit 0). Agents loop on it until the chef says send it.

The reviewer isn't blind and isn't captured. It reads your actual code — a
snapshot of tracked files at the reviewed revision — plus your project docs as
labeled evidence, but it runs through your own Codex or Claude CLI with the
project's configuration ignored. Full read access to the code, zero obedience
to its instructions. Every verdict records what produced it: target, tree
fingerprint, engine, and isolation mode.

You'll need two things on your machine:

- [TruffleHog](https://github.com/trufflesecurity/trufflehog) — roast scans
  every changed file for verified secrets before anything is sent to a model,
  and refuses to run without it.
- An authenticated [Codex](https://developers.openai.com/codex/cli) or
  [Claude Code](https://code.claude.com) CLI — roast drives the one you
  already log into. It never handles your credentials itself.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/janiorvalle/roast/main/install.sh | sh
```

The installer downloads the release for your platform, verifies its SHA-256
against the published checksums, and installs a single `roast` binary to
`~/.local/bin` (override with `ROAST_INSTALL_DIR`). No sudo.

On Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/janiorvalle/roast/main/install.ps1 | iex
```

Same checks, and `roast.exe` lands in `%LOCALAPPDATA%\Programs\roast` on your
user PATH (override with `ROAST_INSTALL_DIR`).

With Go installed this also works:

```sh
go install github.com/janiorvalle/roast/cmd/roast@latest
```

## Use it

Make a change, then run it:

```sh
roast                  # auto: uncommitted work, else current PR, else origin/main
roast --dirty          # only uncommitted changes
roast --base origin/main
roast --commit HEAD
roast 42               # review PR #42 via gh
```

A finding looks like this:

```text
RAW. Fix 1 finding(s) before serving this change:
P0 auth.go:4: Every user is granted administrator status
  rationale: For any non-admin username, IsAdmin("guest") now returns true.
  Any authorization guard relying on this function grants access to everyone.
  suggestion: Restore the administrator identity check.
```

Useful flags: `--engine codex|claude`, `--model` and `--thinking` for
per-engine overrides, `--max-priority P0|P1|P2|P3` (default P1),
`--intent <text>` or `--intent-file <path>` to judge the change against what
the task asked for (a problem outside the intent is an observation, never a
finding, and the verdict's provenance names the intent it was judged against),
`--json-output <path>` for the full verdict as JSON, and `--plain` for CI logs
without the chef voice. The intent file and the verdict path must both be
outside the repository so review artifacts never become reviewed source.

Two things worth knowing up front. Reviews spend tokens through your own CLI
subscription or API key — that's the deal with an AI reviewer, and roast
doesn't add a meter on top. And a big diff costs several reviews: each
prompt is capped at 1 MiB (`ROAST_MAX_PROMPT_BYTES` changes the cap), and a
diff that doesn't fit is split by file into chunks under the cap, one engine
call per chunk, every chunk with the full snapshot and project docs. The chunk
verdicts merge into one verdict, and the output and the verdict's provenance
say how many chunks ran and how big. Binary patch payloads are omitted because
a reviewer cannot inspect them. One file whose change alone exceeds the cap
stops roast before any engine call and names the file, so split that change
into its own commit. Reviewing one task's diff at a time is the intended use
anyway.

## Agents

The first time roast runs it installs a skill into any Claude Code or Codex
home it finds (`roast install-skill --force` reinstalls). The skill teaches
the agent the loop: freeze the task's intent to a file, run the cheap gates,
run roast with that intent every round, treat findings as claims, verify each
one against the real code before fixing it, and repeat until the verdict is
well done.

TruffleHog scans the full pre- and post-change content before any engine
runs, and files like `.env` or key stores are excluded from what the reviewer
sees. One thing to know about how that works: TruffleHog's verification step
may contact a credential provider's endpoint with a detected candidate to
confirm it's live. A verified secret in the change stops the review entirely —
revoke it first.

## Development

```sh
make verify
```

That builds, vets, tests, validates the release config, builds snapshot
archives, and exercises the checksum-verifying installer against them — the
same gate CI runs. See [CONTRIBUTING.md](CONTRIBUTING.md) for the rest,
including the test policy and how engine adapters are put together.

## License

[MIT](LICENSE)
