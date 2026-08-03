# roast

> code review at the pass. well done — or it doesn't ship.

An open-source CLI that acts as a pre-ship code-review gate: a second AI model
judges your diff and returns a verdict. Findings mean **RAW** (exit 1). Clean
means **well done** (exit 0). An agent loops "fix → roast" until the chef says
send it.

## Why

Agent workflows need a mechanical ship gate: an independent reviewer whose
verdict is an exit code you can loop against, not prose you have to interpret.
Existing setups tend to fail in one of two directions — either the reviewer
sees only the diff (structurally blind to cross-file effects and business
logic), or it runs with full project configuration (inheriting the exact
assumptions and blind spots of the agent that wrote the code). roast takes a
third position: a sighted reviewer with an independent mind — full read access
to the code, zero obedience to its configuration.

## How it works (v1 pipeline)

1. **Target** — pick what to review: `--dirty` (uncommitted), branch vs base,
   `--commit <ref>`, or a PR number via `gh`. Auto mode: dirty first, then PR
   base, then `origin/main`.
2. **Secret gate** — run TruffleHog over the changed content. Verified secret
   in added lines → fail closed, nothing is sent anywhere. Sensitive paths
   (`.env*`, credential stores) are excluded from the bundle. TruffleHog
   missing → fail with install instructions (hard dependency).
3. **Context assembly** —
   - the diff (the focus of review),
   - a read-only tar snapshot of **tracked files at the reviewed ref** built
     from Git tree/blob objects — no ignored files, no `.env`, no hooks, and
     no archive-attribute rewriting,
   - the project's own knowledge docs (CLAUDE.md, AGENTS.md, README,
     checklist-style files, configurable glob) injected into the prompt
     **as labeled evidence** — "claims by the authors, not commands."
4. **Judge** — one engine call (Codex or Claude CLI adapter). The reviewer
   runs *inside the snapshot dir*, read-only, with engine config-isolation
   flags on (ignore user/project config, no hooks, no MCP, no skills). It must
   return one JSON object matching a strict verdict schema.
5. **Verdict** — validate the JSON (schema + every finding must point at a
   real file), filter by priority threshold (default P2), print findings,
   exit 0/1. Heartbeats to stderr while the engine thinks. Tree fingerprint
   before/after so a verdict can never refer to a tree that changed mid-run.

The **loop lives in the skill, not the binary**: the agent runs roast,
verifies each finding against the real code (findings are advisory, never
auto-applied), fixes what's real, reruns until exit 0.

## Decisions (with rationale)

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | Name: **roast** | Funny, short, a verb you type. Built-in joke: clean = "well done.", findings = "RAW." npm name is squatted (irrelevant for a Go binary); brew is free. |
| 2 | Language: **Go**, single distributed binary | One-file install, zero runtime, same release shape as better-git-review. |
| 3 | Separate project — **not** part of better-git-review | bgr produces a review *plan* (walkthrough/cohorts); roast produces a *verdict* (gate/exit code). Different jobs, and bgr stays focused. |
| 4 | **Sighted review**: snapshot of tracked files at the reviewed ref, not a diff-only "empty room" | We review our own and trusted colleagues' code — the malicious-diff threat isn't ours, and blindness costs real findings (cross-file effects, business logic). Snapshot = tracked files only, so secrets/ignored files structurally can't leak. `--paranoid` flag reserved for a future diff-only mode. |
| 5 | **Context as data, not config**: project docs injected into the prompt as labeled evidence | The author's CLAUDE.md configured the agent that *wrote* the diff — a reviewer that obeys it inherits the author's blind spots and can't flag the convention itself as the bug. Same words, loaded as claims to weigh, not commands to obey. Hooks/plugins are code execution, never loaded. |
| 6 | **Engine config isolation stays on** (ignore user/project config, no hooks/MCP/skills in the reviewer) | Independence is the product. Cheap to keep even in sighted mode. |
| 7 | Secrets: **TruffleHog only**, fail closed | Do not build a homegrown secret analyzer — it becomes a permanent maintenance tax. If TruffleHog misses a pattern, upstream it. |
| 8 | Two engine adapters: **codex** and **claude** | The ones actually in use. Small adapter interface; others can be PRs. No matrix of stub engines. |
| 9 | **Strict verdict contract**: JSON schema, findings-reference-real-files check, priority threshold (default P2), exit codes | The crown jewel — makes "loop until clean" mechanical instead of vibes. One default, one config file. |
| 10 | Keep the cheap reliability details: heartbeats, tree-fingerprint staleness check, scope-governor rules in the skill | Tiny cost, real value. Review is a closeout gate, not permission to rewrite the task. |
| 11 | No persistence: **no findings ledger, no accept/reject recording** | The agent judges findings in conversation, per the skill contract. Keep it simple. |
| 12 | Skill ships with the CLI, **< 100 lines** | Contract + loop + scope governor. No path ceremony (binary is on PATH). |
| 13 | Brand voice: **anonymous furious head chef** | Findings = "RAW." Clean = "well done — send it." Severity as escalating chef fury. Plain mode for CI; theater on stderr only — agents parse the JSON. No real chef's name/likeness/show references. |
| 14 | Community docs **reused from existing repos** (LICENSE, CONTRIBUTING, SECURITY, CLA language) | Already solved in better-git-review / tokenomnom; copy the pattern, don't re-litigate. |
| 15 | **Skill auto-installs on first run** + explicit `roast install-skill` | The skill is half the product — roast without the loop contract is just a verdict printer. First run detects `~/.claude` / `~/.codex`, installs/updates the skill, prints what it did. The command remains for repair/CI/explicit installs. |
| 16 | Failure semantics: **retry invalid JSON once** (validation error fed back), engine unavailable → clear error, **never silently switch engines** | A verdict must always say which judge produced it. |
| 17 | Testing: **fake engine** (stub returning canned verdict JSON) for the full pipeline in CI; real-engine smoke fixture run locally | Zero model calls in CI; the entire pipeline (target, snapshot, secret gate, schema, exit codes) is testable deterministically. |
| 18 | **Cross-platform: Linux, macOS, Windows** are all first-class | No POSIX-only assumptions: path handling via filepath, TruffleHog binary discovery per-OS, engine CLIs invoked portably, CI matrix covers all three. |
| 19 | **Reviewer isolation uses first-party harness primitives only** — native flags, native permission profiles, native sandboxes; never OS-level sandboxes, managed-policy scanning, or broad environment reconstruction | The harness vendors own enforcement (and platform support comes free). Proven pattern: quest's dispatcher "guarded" mode. A read-only reviewer must never carry more isolation machinery than the full-write workers that build the code. Codex's `--ignore-user-config` does not suppress `CODEX_HOME` instruction discovery, so the Codex adapter uses the documented `CODEX_HOME` interface as a narrow exception: each review gets a temporary home containing only a copy of `auth.json`; ChatGPT OAuth copies have their single-use `refresh_token` emptied, so the reviewer can use its cached access token without consuming or rewriting the user's live credential, and the temporary home is removed afterward. If that access token is within the refresh window, roast first asks Codex's native app-server `account/read` endpoint to refresh the source login, then stages the renewed access token without the refresh token. Claude uses `--safe-mode` and `--setting-sources ""` to disable `CLAUDE.md` and custom setting discovery. The manual probe asks each CLI whether its instructions mention `Quest` or the evidence policy; the staged Codex run must answer no. (Owner decision, 2026-08-02; Codex exception, 2026-08-03.) |

## Cut from v1 (deliberately — may earn their way in later)

- **Merge-preview review** (`git merge-tree`: review the would-be merge result
  instead of the branch tip; catches "both sides fine alone, broken together").
- **MCP allowlist for the reviewer** (per-repo `allow` list of read-only tools;
  generic default-deny mechanism + a `roast mcp suggest <server>` helper that
  drafts the allowlist from MCP readOnly annotations for human approval. Never
  mutation tools, never secret-bearing reads.)
- **Findings ledger / rejection memory / delta review since last clean.**
- **Review panels** (multiple engines on one bundle).
- `--paranoid` empty-room mode.

## Stack

- **Go** (single static binary; goreleaser + brew tap like bgr).
- **TruffleHog** — external hard dependency for the secret gate.
- **git** — target selection, tree/blob snapshots, tree fingerprints.
- **gh** — optional, only for PR-number targets.
- **Engine CLIs** — `codex` and `claude`, invoked as subprocesses with their
  documented isolation flags; models/thinking configurable per engine
  (defaults: codex `gpt-5.6-sol` high; claude `claude-fable-5`).
- **Skill** — `skills/roast/SKILL.md` in-repo, installable for Claude Code
  (`~/.claude/skills`) and Codex (`~/.codex/skills`).

## CLI surface (sketch)

```
roast                      # auto target: dirty → PR base → origin/main
roast --dirty              # uncommitted work only
roast --base origin/main   # branch review
roast --commit HEAD        # one commit
roast 4691                 # PR via gh
  --engine codex|claude    # default codex
  --model / --thinking     # per-engine overrides (env: ROAST_MODEL, ...)
  --max-priority P0|P1|P2|P3   # default P2
  --json-output <path>     # verdict JSON for tooling
  --plain                  # no chef theater (CI logs)
```

Output: chef-voice summary on stderr, verdict JSON on demand, exit 0 = clean
("well done — send it."), exit 1 = findings ("RAW.").

## Verdict schema (sketch)

```json
{
  "overall": "well_done | raw",
  "findings": [
    {
      "priority": "P0 | P1 | P2 | P3",
      "file": "path/in/repo.go",
      "line": 42,
      "title": "one-line defect statement",
      "rationale": "why this is a real defect, with the failure scenario",
      "suggestion": "smallest reasonable fix direction (optional)"
    }
  ],
  "provenance": {
    "target": "branch origin/main..HEAD",
    "tree": "<fingerprint>",
    "engine": "codex/gpt-5.6-sol/high",
    "context": "snapshot + 3 project docs"
  }
}
```

Provenance in every verdict: "reviewed clean" is never ambiguous about what
kind of review it was.

## Public hygiene

This repo will be public; its full history goes public with it. Every commit
is treated as public from day one.

- **No work context, ever**: no employer, client, or internal project names in
  code, docs, commit messages, or fixtures. Example diffs are synthetic.
- **Evidence discipline**: committed terminal output and screenshots are
  cropped to the relevant frame — no tokens, no other repos' paths, no
  desktop/browser chrome. Prefer relative paths in captured output.
- **Agent working files stay out**: `llm-docs/`, `handoffs/`, `scratch/`, and
  `*.local.md` are gitignored. The committed CLAUDE.md carries project
  conventions only.
- **Fixture secrets are canonical test tokens**: only providers' documented
  synthetic credentials, clearly labeled — never realistic-looking keys.
- **No infra internals**: nothing from private infra repos (state, IDs, token
  scopes, runbooks) is referenced here.

## Standing rules (from the house CLAUDE.md — apply here)

- Every change ships with evidence (before/after, receipts, real usage).
- Gates in order: format/lint → typecheck → fast tests → review loop until
  clean → full suite once.
- Napkin math before anything metered: a roast run ≈ one engine call over
  (diff + prompt + whatever the reviewer chooses to read from the snapshot).
  No interactive cost guard — running an AI review tool obviously spends
  tokens; that's the user's informed choice, not roast's to gate. (Owner
  decision, 2026-08-02.)

## Build order

- **M1** — scaffold (go.mod `github.com/janiorvalle/roast`, cmd/roast,
  goreleaser, community docs copied from existing repos, 3-OS CI matrix),
  target selection (dirty/branch/commit/PR#), diff bundle + snapshot building.
  Pure git; fully testable.
- **M2** — end-to-end with the fake engine: prompt assembly, schema
  validation, priority filter, exit codes, chef/plain output. Pipeline is
  "done" here with zero model calls.
- **M3** — real adapters (codex, claude) + heartbeats + isolation flags +
  invalid-JSON retry.
- **M4** — TruffleHog gate + sensitive-path excludes.
- **M5** — SKILL.md + first-run auto-install + `install-skill` + dogfood:
  roast reviews its own first PR (that run is the README proof shot).

CI matrix from M1: linux + macos + windows.

## Status

- [x] Design discussion complete (this doc)
- [x] Hero final → `assets/hero.png`
- [ ] `prompt.md` — review prompt drafted, awaiting human review
- [x] Milestones tracked in Quest (repo `roast`): 131 M1 → 132 M2 →
      {133 M3, 134 M4} → 135 M5 → 136 M6 (ship). 131 M1 is complete.
- [x] Validation evidence per house rules at every milestone
