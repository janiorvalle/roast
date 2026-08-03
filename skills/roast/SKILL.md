---
name: roast
description: Use roast as an independent code-review ship gate and loop until the change is clean.
---

<!-- roast-managed-skill: v1 -->
# Roast

Run `roast` as the independent ship gate after implementing a change.

## Contract

- Review the current task's diff, not the whole codebase.
- Prefer a review engine that did not write the change to preserve cross-model independence.
- Run cheap gates first: format/lint, typecheck or compile, then fast tests.
- Treat findings as claims. Verify every finding against the real code and the
  current diff before fixing it.
- Fix verified defects only. Do not blindly apply a review suggestion.
- Run the review again after each fix round.
- Continue the fix -> review loop until the verdict is `well done`.
- Run the full check suite once after the review is clean.

## Scope governor

- Work only on defects introduced by the current task or made worse by it.
- Do not expand the change into unrelated refactors, style work, or speculative
  hardening.
- A finding outside the diff needs a concrete failure path before it changes
  the task.
- If a verified finding needs an unrelated redesign, record it and stop at the
  task boundary; do not hide scope expansion inside the fix.

## Loop until clean

1. Run the fast gates, then run `roast` on the current change.
2. Read each finding and verify its failure path in the repository.
3. Fix only verified findings, keeping the patch within the scope governor.
4. Repeat from step 1 until `roast` reports `well done`.
5. Run the full check suite once and attach its real output as evidence.
