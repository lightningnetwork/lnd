# EXP-008 — Drift: does time-awareness re-evolve under background traffic?

**Date:** 2026-07-24 (started), 2026-07-25 (verdict)
**Status:** complete

## Question
The champions carry zero time-based logic, and we suspected that was
partly a simulator artifact: hidden liquidity only moved when OUR
payments moved it, so evidence never went stale and hard bounds were
unbeatable by construction. With a virtual clock and exogenous
background traffic (commit d11a20dcb), knowledge now decays for real.
Does time-awareness re-evolve? Candidate outcomes, all informative:
(a) decay re-emerges → validates lnd's rationale with evolved
constants; (b) something better emerges (e.g. interval-widening with
elapsed time); (c) intervals still win → decay was overweighted.

## Environment
`corpus-drift` (seed 3031): hard topologies + bimodal liquidity, ten
virtual minutes between payments, one second per attempt, background
senders per gap scaled to network size (≥10, num_nodes/10), amounts
log-uniform from ~dust to half a channel. lnd's mission control runs
on the virtual clock, so its decay half-lives genuinely operate. The
reflection prompt describes drift neutrally and flags the hard-bounds
insight as learned in a static world — we do not prescribe the answer.

## Baseline (before evolution)

| router | drift-val obj | drift-test obj | test succ | test att |
|---|---|---|---|---|
| lnd stack | 0.213 | 0.203 | 0.388 | 34.5 |
| seed | 0.320 | 0.377 | 0.592 | 48.3 |
| hb1 | **0.387** | 0.455 | 0.642 | 11.8 |
| mx_c3 | 0.380 | **0.457** | 0.642 | 12.3 |
| gen2 | 0.383 | 0.456 | 0.642 | 12.7 |

Two findings already:
- **The champions' hard bounds do NOT collapse under drift.** They
  still beat lnd by >2× objective at a third of the attempts. Stale
  bounds degrade gracefully — a wrong lowerOK just costs a retry.
- **lnd's decay does not close the gap.** Even with its half-lives
  finally operating over meaningful time spans, the production stack
  stays last. Decay as lnd implements it is not the missing
  ingredient; whatever drift-awareness helps here has to look
  different.
- Everyone lost ground vs the static hard corpus (champions ~0.59 →
  ~0.42 combined val/test; attempts up from ~9-10 to ~12): drift
  creates genuine headroom for the evolution run to claim.

## Evolution run
`code_drift1`: pure gepa, codex/gpt-5.6-sol reflection, small seed +
insights prompt (with the drift paragraph and the static-world caveat
on hard bounds), 400 evals, corpus-drift. Success criterion: beat the
champions on held-out drift-test; then inspect whether the winner
encodes any function of view.Now() or evidence age.

## Verdict

Both halves of the question got clean answers, and they point in
different directions.

**Yes, time-awareness re-evolved.** The run finished 400/400 (51
accepted candidates) and its winner is the first evolved router with
time-based logic. The mechanism is exactly the hypothesized outcome
(b) — evidence softening, not lnd-style penalty fading:

- Every per-directed-channel belief carries an `updatedAt` stamp.
- Belief confidence decays exponentially with a **35-minute
  half-life** (`conf·exp(−ln2·age/35min)`, evolved constant; the
  corpus gap is 10 minutes, so evidence survives ~2–3 payments near
  full strength).
- Hard bounds **expire outright after 20 minutes**: lowerOK/upperFail
  reset to zero — "bounds become hints rather than permanent facts."
- Edge probability is `conf·learned + (1−conf)·prior`: as evidence
  ages the router slides smoothly from its interval beliefs back to
  the bimodal prior. Selection pressure produced decay of *confidence
  in evidence*, never decay of *penalties*.

**No, it does not beat the time-less champions — even on drift.**

| tier | lnd | seed | hb1 | mx_c3 | gen2 | drift1 |
|---|---|---|---|---|---|---|
| drift-test | 0.203 | 0.377 | 0.455 | **0.457** | 0.456 | 0.417 |
| hard test | 0.309 | 0.530 | **0.586** | 0.583 | 0.565 | 0.580 |
| OOD v2 | 0.357 | 0.487 | 0.545 | **0.581** | 0.563 | 0.544 |
| mainnet | 0.694 | 0.762 | 0.790 | **0.791** | 0.787 | 0.790 |

The sharpest comparison is drift1 vs gen2: same seed style, same
400-eval budget, and gen2 never saw drift during evolution — yet the
static-bred, time-less gen2 (0.456) beats the drift-bred, time-aware
drift1 (0.417) on the drift corpus itself. Meanwhile drift1 matches
the champions on the static tiers (hard 0.580, mainnet 0.790), so the
time machinery cost nothing where nothing drifts — it just didn't buy
anything where things do.

## Reading

- **lnd's rationale is validated, its mechanism is not.** Time-decay
  re-emerged under genuine drift, confirming the intuition that stale
  knowledge should fade. But the evolved form (confidence
  interpolation toward the prior + bound expiry) is structurally
  different from lnd's penalty half-life — and even so, it could not
  outperform simply keeping hard bounds and letting wrong ones cost a
  single retry. Failure evidence is cheap to refresh; decay protects
  against a cost the interval design barely pays.
- Within its own lineage the time logic won selection (it beat its
  time-less ancestors on the drift minibatches), so the emergence is
  real, not noise. The champions' edge likely comes from their deeper
  refinements (Pareto route search, bidirectional evidence, richer
  shard ladders) accumulated over 900+ evals — refinements drift1's
  budget went partway toward rebuilding.
- **Champions of record unchanged: hb1 + mx_c3**, now validated on a
  fourth tier. mx_c3 leads or ties every tier except the hard test.
- Caveats: one drift intensity (10-minute gaps, ~num_nodes/10
  payments per gap), one traffic model (naive fee-optimizing
  senders), sequential shard settlement, 400-eval budget. A heavier
  drift regime or a longer run could still tip the balance toward
  time-awareness; what this settles is that at realistic-ish churn,
  evidence bounds are far more robust than the decay intuition
  suggests.

## Artifacts
- Winner source: `exp-008-drift1-best-candidate.go` (this dir),
  exploit-grep clean, 1,147 lines.
- Sweep numbers: `drift1-validation.json` in session scratch
  (regenerable via `sweep_drift1.py`).
