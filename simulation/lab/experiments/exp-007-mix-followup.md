# EXP-007 — code_mix1: continuing evolution from the hb1 champion

**Date:** 2026-07-24 (night)
**Status:** in flight, but verdict already clear

## Setup
Seed = hb1 (the 872-line code_hard1 champion, via `--seed-file`), on a
mixed corpus (hard bimodal small-channel + corpus-v2 scale-free, 48/20/20
train/val/test) for generalization pressure. Pure gepa,
codex/gpt-5.6-sol, 500-eval budget, timeout-hardened harness.

## Interim verdict (95/500 evals, ~13 iterations)

**All 12 post-seed proposals rejected.** The best-on-valset is frozen at
the hb1 seed's mixed-corpus aggregate of **0.5102**; `candidates.json`
holds only the seed. GEPA cannot improve hb1 by mutating it further on
this corpus within the budget spent so far.

## Reading
- **hb1 is a robust local optimum.** From scratch (code_hard1) GEPA
  climbed quickly to hb1; continuing *from* hb1 yields only rejects. The
  quick early gains + hard tail is the expected shape.
- Confirms the giant-seed learning (IDEAS.md): reflecting on an 872-line
  candidate is slow and rarely produces an accepted improvement — the
  edit surface is large and most edits break or regress it. Diminishing
  returns vs the from-scratch run.
- hb1's mixed-corpus val aggregate (0.5102) is much higher than its
  hard-only figure (0.3165) simply because the mixed corpus folds in
  easier scale-free examples — not a regression, just an easier average.

## Implication for the next run
Don't continue from the giant champion. Instead (IDEAS.md):
- seed from the SMALL original router and **enrich the background
  prompt** with hb1's discovered structure (bimodal prior + per-edge
  liquidity bounds), so the *insight* transfers without dragging 872
  lines through every reflection; and/or
- add a candidate-size penalty / "simplify" reflection instruction to
  keep the edit surface tractable.

code_mix1 is left running to complete its budget (harness is
timeout-safe), but hb1/hb2 remain the champions of record regardless of
its outcome.

## Update (163/500 evals): first accepted proposal validated — still no new champion

code_mix1 eventually accepted one proposal to the Pareto front (mb1, 1306
lines, iteration 17). Validated three-way on held-out sets:

| set | hb1 | hb2 | mb1 |
|---|---|---|---|
| hard sealed test | **0.586** | 0.545 | 0.570 |
| OOD corpus-v2 test | 0.545 | **0.577** | 0.529 |

mb1 does NOT dominate: it's below hb1 on the hard test and below both on
OOD. GEPA accepted it only because it wins specific *val* examples on the
Pareto front (its minibatch score was actually 0.431 vs the seed's 0.519
— accepted for per-example specialization, not aggregate gain).

## Update #2 (314/500 evals): the follow-up DID pay off — a generalist champion

By ~314 evals code_mix1 had accepted three more Pareto members
(1306/1410/1525 lines). The newest, **mx_c3** (1525 lines, saved as
`champions/router_mx3_generalist_v1.go`), is the best all-around router.
Three-way on the held-out sets:

| set (objective) | hb1 | hb2 | mx_c3 |
|---|---|---|---|
| hard sealed test | **0.586** | 0.545 | 0.583 |
| OOD corpus-v2 test | 0.545 | 0.577 | **0.581** |
| combined average | 0.565 | 0.561 | **0.582** |

- mx_c3 **strictly dominates hb2** (wins both sets).
- vs hb1 it's a statistical tie on the hard test (0.583 vs 0.586, within
  noise) and a clear win OOD (0.581 vs 0.545) → **best combined average
  and the most balanced generalist.**
- Clean, no exploit. Structurally it extends hb1's bimodal-prior +
  liquidity-bounds core with a `candidateLowerRetryFactor` (retry-at-
  lower-amount logic) among other refinements — a more adaptive
  retry/split policy that helps on the diverse mixed corpus.

**Revised verdict:** the champions of record are now **hb1** (hard-regime
specialist, marginally best on the hardest corpus) and **mx_c3** (best
generalist — dominates hb2, best OOD, best combined average). hb2 is
superseded. The earlier "giant-seed = only diminishing returns" call was
premature: continuing from hb1 *did* find a better generalist, it just
took ~300 evals (consistent with slow, fragile giant-seed reflection, not
zero gain). This vindicates letting the run continue.

(Note: code_mix1's summary best_score 0.9962 remains the inflated
per-minibatch metric; the numbers above are the held-out three-way
validation, which is what determines champions.)

## Final sweep (462/500 evals, frontier = 6): mx_c3 confirmed best

By the end of the run the Pareto frontier held 6 members. Validated the
two newest (frontier 4/5) against the champion on held-out sets:

| router | hard | OOD | combined |
|---|---|---|---|
| hb1 | **0.586** | 0.545 | 0.565 |
| **mx_c3** | 0.583 | **0.581** | **0.582** |
| mx_c4 (1134 ln) | 0.581 | 0.549 | 0.565 |
| mx_c5 (1107 ln) | 0.577 | 0.542 | 0.560 |

Neither newer member beats mx_c3 — they win specific val examples (hence
Pareto acceptance) but not the held-out aggregate. **Definitive verdict:
hb1 (hard-regime specialist) + mx_c3 (generalist, best combined) are the
champions of the whole effort.** The frontier has converged; later
members grow in size without improving generalization — the code-
evolution complexity wall again. code_mix1 finishes its last evals but
the champions are settled.
