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
- Run the review again after each fix round.
- Continue the fix -> review loop until the verdict is `well done`.
- Run the full check suite once after the review is clean.

## Scope governor

Roast is a ship gate, not permission to rewrite the task.

**Freeze the fence before the first review.** Before round 1, write down the
task's scope in a few lines: the original request in one sentence, the intended
behavior, the owner boundary, and the files the change touches. Every finding
is judged against this baseline. Write it once — never re-derive scope
per finding.

**Classify every verified finding against the fence:**

- **In-scope blocker** — introduced by this diff, affects the same owner
  boundary, fixable without changing the task's contract. Fix it; it gates.
- **Follow-up** — real, but belongs to an adjacent surface, cleanup, or
  broader hardening. Record it where the project tracks work and move on; it
  does not gate and it does not re-enter the loop.
- **Stop-and-escalate** — requires a new contract, API shape, migration,
  storage change, different owner boundary, or a design decision beyond the
  original request. Correct-in-a-vacuum architecture at the wrong altitude
  lands here. Stop and bring it to the humans; do not fix it quietly, and do
  not relitigate it with the reviewer round after round.

**Convergence tripwires:**

- Two fix rounds without converging → pause and reclassify every remaining
  finding against the fence before another edit.
- The diff grows past 2x the original files or non-test lines → scope broke;
  stop and say so instead of continuing.

**Only these may expand the fence:** active data loss, a crash, broken
build/install, or a concrete security exposure. A finding that is none of
those is never critical enough to grow the task.

- Work only on defects introduced by the current task or made worse by it.
- Do not expand the change into unrelated refactors, style work, or speculative
  hardening.
- A finding outside the diff needs a concrete failure path before it changes
  the task.

## Loop until clean

1. Freeze the scope fence, run the fast gates, then run `roast` on the
   current change.
2. Read each finding and verify its failure path in the repository.
3. Classify each verified finding against the fence; fix in-scope blockers
   only, keeping the patch within the scope governor.
4. Repeat from step 1 (skipping the freeze) until `roast` reports `well done`
   with no unrecorded follow-ups.
5. Run the full check suite once and attach its real output as evidence.
