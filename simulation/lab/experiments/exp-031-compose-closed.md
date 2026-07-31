# EXP-031 — The compose world is closed: 800 evals returns the seed again

**Date:** 2026-07-30/31 (overnight, code_full3).
**Status:** complete. The second and last pre-registered escape from
exp-026's wall has failed. No candidate produced; frontier unchanged.

## The arm

exp-026 found the first world the seed-plus-insights recipe could not
improve at the standard budget, and pre-registered two escapes:
seed from a specialist (exp-028: worse — the give-up attractor), and
budget scaling. This is budget scaling: the identical corpus, prompt,
optimizer and hand-written seed as exp-026's code_full1, at 800 evals
instead of 400.

## Result: the seed, to seven digits

best val 0.3162367 — the seed's own gate number — and held-out test
0.21568 equal to the seed's, digit for digit. The run was healthy and
generous: 60 proposals, zero hijacks, NINETEEN pool accepts on
subsample wins (code_full1 had eight), every one of them below the
seed on the full validation set. The run exited with 102 evals
unspent when the remainder could no longer fund another iteration;
698 evals were consumed against exp-026's 385, so the wall received
roughly 1.8x the search and returned the same answer.

## The ladder, final form

| world | seed | budget | outcome |
|---|---|---|---|
| clean | hand | 400 | +0.05 to +0.06 |
| lying channel | hand | 400 | +0.044 |
| economic | hand | 400 | +0.022 |
| compose | hand | 400 | +0.000 (exp-026) |
| compose | econ2 | 400 | −0.030 held-out (exp-028) |
| compose | hand | 800 | +0.000 (this) |

Monotone to zero and stable there under both pre-registered
perturbations. Read with exp-024 (the alternative optimizer converges
below gepa at 10x budget), the conclusion is now earned rather than
suspected: **the compose world is closed to this recipe at any seed
and any practical budget.** The mixed environment prices complexity
faster than 60 reflective proposals can pay for it.

## What this means for the program

The evolution track has found its boundary, and the boundary is
informative: every mechanism the frontier owns (intervals, budget
arithmetic, attribution confidence) was bred in a world that applied
ONE pressure at a time, and no run has ever produced machinery under
two pressures at once. The value has moved to the integration branch
— which carries all three mechanism families, hand-assembled, and
which exp-027/029/030 have validated far beyond any single evolved
router — and to the replay work that starts next week. Breeding
resumes if a new world (the foreign graph as a training corpus, or
replay-derived scenarios) reopens the search space.

## Artifacts

`exp-031-full3-summary.json`, `exp-031-full3-run.log.gz`. Corpus,
prompt hashes, and gate identical to exp-026 (recorded there);
launch script guards in the session scratch (launch-full3.sh).
