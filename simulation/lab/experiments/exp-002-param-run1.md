# EXP-002 — Parameter-mode full run (run1)

**Date:** 2026-07-24
**Status:** in flight

## Setup
- Corpus: 20 train / 8 val / 8 test (seed 2026), amounts capped (singles
  ≤40% of channel capacity, MPP ≤100%), source channels rebalanced 50/50.
- Seed: lnd defaults. Budget: `max_evals=400`, minibatch 4, concurrency 8.
- Score: success_rate − 0.01·min(extra_attempts,15) −
  0.00002·min(fee_ppm,5000).
- Reflection: codex exec, gpt-5.6-sol.

## Final result

**Seed returned unchanged.** 400 evals, 33 iterations, ~16 distinct
proposals. Best on val aggregate = the lnd defaults (val 0.3647, sealed
test 0.3430 = baseline). The Pareto front kept two bimodal *specialists*
(best on val examples 6 and 7 only); nothing generalized across the val
set. Several proposals were accepted on their minibatch but collapsed on
full val (e.g. iter 33: minibatch 0.63→0.67, val 0.086) — minibatch
acceptance is noisy and proposals overfit it.

## Conclusion

Within the current paradigm's parameter space, the lnd defaults are
locally robust on this corpus — parameter tuning is not where the
headroom is. Contrast exp-003: a paradigm-different naive router beats
the tuned-or-not lnd stack on 16/16 examples with ~2× success rate. The
bottleneck is the algorithm, not its knobs. Phase 3 (code-mode evolution)
is the main event.

Loop improvements if we revisit param mode: larger reflection minibatch
(less acceptance noise), eval caching (seed re-scored dozens of times),
multi-objective `info["scores"]`, background prompt that pushes
apriori-side exploration (all proposals fixated on switching to bimodal).

## Interim observations (~164 evals in)
- 13 distinct candidates proposed; healthy exploration.
- Reflection is fixated on switching to the bimodal estimator with much
  lower attempt costs (1000 msat / 10 ppm) and lower min_probability
  (1e-4 to 1e-6). Minibatch means so far: seed 0.435; best variant 0.564;
  most bimodal variants worse (0.14–0.41).
- Hypothesis to check post-run: does anything tune bimodal
  `scale_msat` toward the actual channel-size scale of the corpus
  (2–10M sat)? Default 300M msat scale is wildly off for these nets.

## To fill in when complete
- Final best candidate vs seed on val and sealed test.
- Accepted-lineage summary; params diff of the winner.
- Whether the winner generalizes across liquidity models or overfits
  bimodal.
