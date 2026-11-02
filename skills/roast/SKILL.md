---
name: roast
description: Use roast as an independent code-review ship gate and loop until the change is clean.
---

<!-- roast-managed-skill: v1 -->
# Roast

Run `roast` as the independent ship gate after implementing a change.

## Contract

- Review the current task's diff, not the whole codebase.
- Run cheap gates first: format/lint, typecheck or compile, then fast tests.
- Treat findings as claims. Verify every finding against the real code and the
  current diff before fixing it.
- Fix verified defects only. Do not blindly apply a review suggestion.
- Never pass `--max-priority`. The threshold belongs to the human and the
  default is theirs.
- Run the full check suite once after the review is clean.

## The fence

Roast is a ship gate, not permission to rewrite the task.

**Freeze the fence before the first review.** Before round 1, write the
task's scope to a file outside the repository, such as
`$TMPDIR/roast-intent-<task>.md`, in a few lines: the original request in one
sentence, the intended behavior, the owner boundary, and the files the change
touches. Write it once, never re-derive scope per finding, and pass the same
file to every round: `roast --intent-file <path>`. Roast judges the change
against it, reports a problem outside it as an observation instead of a
finding, and names the intent in the verdict's provenance. The file stays
outside the repository, the same rule as `--json-output`, so the fence never
becomes part of the reviewed change.

**Classify every verified finding against the fence:**

- **In-scope blocker**: introduced by this diff, inside the owner boundary,
  fixable without changing the task's contract. Fix it; it gates.
- **Pre-existing**: the line predates the diff. Roast tags it `pre-existing`
  from git blame against the reviewed base. Resolve it with that reason; fix it
  only when it is data loss, a crash, a broken build or install, or a concrete
  security exposure.
- **Out of fence**: real, but on an adjacent surface or at a different
  altitude. Resolve it with a reason and record it where the project tracks
  work; it does not gate.
- **Wrong**: resolve it with the evidence.
- **Needs a design call**: a new contract, API shape, migration, or owner
  boundary. Stop and bring it to the humans instead of relitigating it with the
  reviewer.

## Pass resolutions back

Write each round's verdict with `--json-output <path>`, outside the
repository. For every finding you set aside, add a `resolution` to that
finding in the file, with a `kind` of `pre-existing`, `out-of-fence`, or
`wrong` and a one-sentence `reason`, and set `overall` to `well_done` when no
finding is left open. Pass the annotated file to every later round with
`--prior-verdict <path>`, once per round you annotated. A finding that recurs
with the same file and title comes back tagged resolved, prints in its own
block, never counts toward raw, and keeps its resolution in the new verdict.
A finding roast tags `pre-existing` is still counted until a resolution says
otherwise.

## The loop

The loop itself, delta-only rounds after round one, and resolving with a
reason, live in the harness's `land-pr` skill, so the two never drift. In
short: freeze the fence, run the fast gates, run round one on the whole
change with the intent file, verify and classify every finding, fix in-scope
blockers only, annotate the verdict, then run `roast --dirty` with the same
intent file and the annotated verdicts on each fix until roast says well done
or every open finding carries a reason. Only a design call stops the loop;
the fence, never a round count, is what bounds it.
