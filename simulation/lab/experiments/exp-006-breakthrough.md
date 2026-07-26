# EXP-006 — Breakthrough: evolved router beats lnd AND the seed on held-out sets

**Date:** 2026-07-24 (night)
**Status:** validated result

## Result

The `code_hard1` run (pure gepa, codex/gpt-5.6-sol reflection, hard
bimodal corpus) accepted an 872-line evolved router (`hb1`, frontier
candidate 1) that beats both lnd's production stack and the hand-written
seed router — on the **sealed test set** and **out-of-distribution**, not
just training. Composite objective = success − 0.01·min(extra_attempts,15)
− 0.00002·min(fee_ppm,5000).

| set | router | objective | success | attempts/pmt |
|---|---|---|---|---|
| hard **val** (10) | lnd | 0.111 | 0.289 | 50.5 |
| | seed | 0.197 | 0.379 | 46.6 |
| | **hb1** | **0.317** | **0.456** | **10.7** |
| hard **sealed test** (10) | lnd | 0.309 | 0.493 | 45.5 |
| | seed | 0.530 | 0.704 | 34.7 |
| | **hb1** | **0.586** | **0.732** | **9.3** |
| **OOD** corpus-v2 test (10) | lnd | 0.357 | 0.525 | 58.8 |
| | seed | 0.487 | 0.648 | 30.1 |
| | **hb1** | **0.545** | **0.656** | **8.2** |

hb1 wins every split on objective and success while using **4–7× fewer
attempts**. It generalizes: trained on hard bimodal small-channel nets,
it still wins on corpus-v2's unseen scale-free/mainnet-like topologies.
Clean — zero sandbox-exploit tokens (validated post-seal, exp-005).

## What GEPA discovered

The interesting part is *what* it evolved, from nothing but the objective
and per-attempt failure traces:

1. **It rediscovered the bimodal liquidity model.**
   `candidatePriorProbability` is an explicit bimodal prior over the
   amount/capacity ratio:
   - `lowMode  = 0.45·exp(-ratio/0.025)` — near-certain for tiny amounts,
   - `highMode = 0.50/(1+exp((ratio-0.92)/0.04))` — logistic cliff as the
     amount approaches capacity,
   - floored/capped to [0.005, 0.985].
   This is the same "funds sit at one end of the channel" hypothesis
   lnd's bimodal estimator was analytically derived from.

   **Correction (2026-07-26, WHY.md §0):** this bullet originally
   ended "reinvented by the LLM from failure feedback alone," and that
   is false. The harness prompt states the bimodal hypothesis verbatim
   under "environment truths worth exploiting," and has since the
   first committed version of `run_gepa_code.py`. What the run
   actually produced is the functional form (exponential low mode plus
   logistic cliff), every constant in it, and the interval machinery
   in the next bullet — real work, but not the discovery we claimed.
   Sharper still: `0.025` here and `0.018` in mx_c3 sit close to the
   `ExpFloat64()*0.05` that `sim_liquidity.go` generates with, so the
   prior is fitted to our generator. Until a corpus carries liquidity
   from some other source, treat the prior as calibration and the
   intervals as the finding.

2. **Per-edge liquidity bounds with confidence.** `edgeProbability`
   tracks `lowerOK` (largest amount known to pass), `upperFail` (smallest
   known to fail), a point `estimate`, and a `conf` score. It returns
   0.995 above a known-good amount, 0 above a known-fail, and otherwise
   blends a confidence-weighted estimate with the bimodal prior — a
   Bayesian-flavored liquidity belief per channel.

3. **Risk-adjusted Dijkstra + shard planning** (`candidateQueue` scored
   by cost and risk; `candidateShardAmounts`) rather than the seed's
   naive halving.

The efficiency win (≈9 attempts vs lnd's ≈50) comes from (1)+(2): it
stops probing channels the bimodal prior says are hopeless and never
retries above a proven-fail bound, so it converges on a working
route/split fast instead of grinding through the retry budget the way
lnd's apriori penalty does.

## Caveats (honest)

- Scored on the **current sim**, which still has the batched fidelity
  gaps (exp-005 M1 wall-clock MC clock, M2 frozen MPP hints, M3 memory
  asymmetry). M3 in particular *favors the candidate contest fairness*
  question — but hb1 beats the seed too, and both candidates run under
  identical sim rules, so the hb1-vs-seed comparison is apples-to-apples.
  **M1 checked empirically:** ran the hard sealed-test scoring 5× for
  both lnd and hb1 — stdev 0.00000, identical to 4 decimals every run
  (lnd 0.3092, hb1 0.5860). The wall-clock nondeterminism is theoretical
  at these timescales (µs of elapsed time vs a 1-hour penalty half-life
  never flips a decision), so the headline numbers are fully
  reproducible. The virtual-clock fix is still worth landing for
  principled determinism, but it does **not** change this result.
- Small-channel synthetic + scale-free topologies, not a real mainnet
  describegraph snapshot. Directionally strong; final validation wants
  the real graph.
- `code_hard1` was still running when this was measured (frontier had 2
  candidates; iters after the accept kept failing to compile — the
  code-evolution "complexity wall"). hb1 is the current champion, not
  necessarily the run's final.

## Frontier sibling: hb2 (1166 lines, frontier candidate 2)

As code_hard1 continued it accepted a third frontier member, hb2, a
further-elaborated router. It Pareto-trades against hb1 rather than
strictly dominating — GEPA keeps both because they specialize on
different val examples:

| set | hb1 | hb2 |
|---|---|---|
| hard sealed test (obj) | **0.586** | 0.545 |
| OOD corpus-v2 test (obj) | 0.545 | **0.577** |

hb1 is the hard-regime specialist; hb2 generalizes better
out-of-distribution. Both beat lnd (0.309 / 0.357) and the seed (0.530 /
0.487) on both sets, both clean. Saved as
`champions/router_hb2_v1.go`. This is a genuine Pareto frontier of
evolved routers, not a single point — a good input for an ensemble or a
generalist follow-up run.

## Final run outcome

`code_hard1` terminated at 135/400 evals when a pathological evolved
candidate hit an infinite loop and blew the 600s subprocess timeout,
which propagated (gepa `raise_on_exception=True`) and stopped the run.
GEPA's own valset selection had by then settled on **candidate 1 (hb1) as
the best program, val aggregate 0.31654** — matching the independent
hard-val measurement (0.3165) exactly. So hb1 is the definitive champion
by both GEPA's selection and my three-way validation; hb2/hb3 are
frontier siblings that win individual examples but not the aggregate.

Champions by role:
- **hb1** — best overall / hard-regime specialist (sealed test 0.586).
- **hb2** — best out-of-distribution generalist (OOD 0.577).

Harness hardening applied post-run (safe, no active run):
`evaluate_code.py` now catches `TimeoutExpired` (tightened to 120s) and
JSON-decode errors, scoring a hung/garbage candidate 0.0 with actionable
feedback instead of crashing the run. This alone would have let
code_hard1 use its full 400-eval budget.

## Next
- Let code_hard1 finish; re-validate its final champion.
- Batch-2 fidelity fixes (esp. M1 virtual clock), then re-run the
  three-way to harden the lnd comparison.
- Feed hb1's bimodal-prior structure back as a seed for a follow-up run.
- Champion saved: `simulation/champions/router_hb1_v1.go`.
