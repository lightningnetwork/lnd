# EXP-001 — Parameter-mode smoke run

**Date:** 2026-07-24
**Status:** complete (negative result, diagnosed)

## Setup
- Corpus: 6 train / 3 val / 3 test scenario files (seed 99), mixed
  topologies, bimodal-heavy liquidity.
- Candidate: lnd default params JSON (apriori estimator).
- Budget: `max_evals=60`, reflection minibatch 4, reflection LM
  `codex exec` + gpt-5.6-sol.

## Result
Budget exhausted with **zero accepted proposals**; best candidate = seed
(val 0.5076, test 0.5050 = baseline).

## Diagnosis
1. **Budget starvation:** 7 iterations happened; the seed alone consumed
   32 evals (repeated minibatch subsampling re-evaluates without cache),
   each proposal cost 4 more. 60 evals ≈ 7 proposals — too few.
2. **Score shaping:** the attempt penalty was unbounded
   (−0.01/attempt, observed per-example scores down to −2), so success
   rate deltas drowned. All 7 proposals scored worse on their minibatch
   and were rejected. Every proposal switched estimator to bimodal
   (nudged by the background prompt) with defaults ill-suited to small
   channels.

## Fixes applied
- Attempt penalty saturates at 15 extra attempts; fee penalty capped:
  worst-case total penalty −0.25.
- Real run budget: 400 evals on 20/8/8 corpus (EXP-002).

## Mechanics validated
Reflection → proposal → minibatch eval → rejection loop all worked;
codex-headless reflection produced valid JSON candidates every time.
