# EXP-017 — The paradigm survives generators it was never fit to

**Date:** 2026-07-27
**Status:** complete — the de-circularization sweep. Champions unchanged.

## Why this ran

WHY.md §0 records the program's most uncomfortable correction: the
evolved priors fit our own generator. `sim_liquidity.go` draws
`ExpFloat64()*0.05`; atomic1's low mode is `exp(−x/0.055)`; the
mainnet tier overwrites real balances with that same draw. Every
published number therefore sat on a distribution we authored, and
"the champions beat lnd" could in principle have meant only "the
champions memorized our generator."

This experiment is the cheap test of that possibility: parameterize
the generator family, move the liquidity world underneath every
router, and ask whether the ordering survives. The advisor also
flagged a sibling circularity nobody had listed — the payment-amount
distribution is authored by us too — so amounts got their own axis.

## Design

Three instruments, all landed at `817804ea5..388fc426b`:

- `AssignLiquidity` now accepts parameterized families:
  `bimodal:<scale>` (the fitted shape at the wrong scale),
  `beta:<a>:<b>` (U-shaped with polynomial rather than exponential
  tails, or unimodal-centered — the world where the bimodal
  hypothesis is simply false), and `hubdrain:<scale>` (the depleted
  end faces the higher-degree node with p=0.85 — the first generator
  correlated with topology rather than drawn blind). The legacy
  strings are golden-tested byte-identical, so every old corpus
  regenerates unchanged.
- `gen_family_corpora.py` emits ten hard-tier base scenarios once,
  then one directory per family where file i is identical to the base
  except the single field under test. Paired per-file deltas
  therefore isolate the generator from topology noise.
- `gen_mainnet_variants.py` re-liquifies the exp-009 mainnet tier
  (now checked in under `simulation/lab/scenarios/mainnet/`) the same
  way: a one-line substitution with a parse-and-compare assertion
  that nothing else moved.

Thirteen tiers of ten files — seven liquidity families, two amount
families (lognormal, round-value clustering; liquidity held at the
control), four mainnet families — times five routers (lnd, seed, hb1,
mx_c3, atomic1): 650 runs. Bootstrap 10k, two-sided sign tests, and
three sanity gates, the sharpest being that the untouched mainnet
control had to reproduce the published exp-009 numbers. It does, to
three decimals: 0.694 / 0.762 / 0.790 / 0.791.

## Result

Objective (success / attempts per payment):

| tier | lnd | seed | hb1 | mx_c3 | atomic1 |
|---|---|---|---|---|---|
| liq-bimodal_0.01 | 0.143 (0.30/43.9) | 0.372 (0.52/42.3) | **0.441** (0.55/8.4) | 0.432 (0.55/9.1) | 0.266 (0.36/10.4) |
| liq-bimodal *(control)* | 0.192 (0.37/40.0) | 0.397 (0.55/29.1) | **0.471** (0.58/8.6) | 0.462 (0.58/10.0) | 0.375 (0.48/8.0) |
| liq-bimodal_0.2 | 0.318 (0.48/43.1) | 0.472 (0.62/17.7) | **0.551** (0.66/6.3) | 0.532 (0.66/9.9) | 0.514 (0.62/6.6) |
| liq-beta_0.3_0.3 | 0.240 (0.40/48.1) | 0.434 (0.59/21.0) | 0.489 (0.61/6.8) | 0.481 (0.61/7.9) | **0.523** (0.64/8.0) |
| liq-beta_2_2 | 0.370 (0.52/60.2) | 0.531 (0.64/8.4) | 0.574 (0.66/4.4) | 0.546 (0.64/5.0) | **0.644** (0.72/3.6) |
| liq-uniform | 0.369 (0.53/52.0) | 0.516 (0.63/10.0) | 0.579 (0.67/4.7) | 0.554 (0.65/5.3) | **0.626** (0.71/4.3) |
| liq-hubdrain_0.05 | 0.212 (0.34/30.5) | 0.238 (0.38/39.9) | 0.303 (0.42/11.7) | 0.298 (0.42/13.1) | **0.306** (0.41/7.6) |
| amt-lognormal | 0.185 (0.36/33.5) | 0.282 (0.43/31.6) | **0.395** (0.49/6.0) | 0.384 (0.50/9.5) | 0.308 (0.41/7.3) |
| amt-round | 0.212 (0.38/43.4) | 0.301 (0.46/31.4) | **0.377** (0.50/9.0) | 0.354 (0.49/11.3) | 0.307 (0.40/7.2) |
| mn-control | 0.694 (0.79/19.8) | 0.762 (0.82/6.1) | 0.790 (0.81/2.3) | **0.791** (0.81/2.3) | 0.790 (0.80/1.6) |
| mn-bimodal_0.2 | 0.657 (0.77/19.7) | 0.753 (0.80/5.2) | 0.781 (0.80/2.2) | 0.781 (0.80/2.3) | **0.789** (0.80/1.6) |
| mn-beta_0.3_0.3 | 0.688 (0.79/20.9) | 0.796 (0.85/5.2) | 0.807 (0.83/2.3) | 0.807 (0.83/2.3) | **0.818** (0.83/1.9) |
| mn-uniform | 0.678 (0.79/21.4) | 0.786 (0.84/5.1) | **0.801** (0.82/2.1) | 0.801 (0.82/2.2) | 0.799 (0.81/1.6) |

Full paired deltas with CIs live in
`exp-017-results-summary.json`; the ones the findings rest on are
quoted inline below.

## Finding 1: the ordering survives every world we could build

lnd is rank 5 on all thirteen tiers, the hand-written seed is rank 3
or 4 on all thirteen, and an evolved router is rank 1 on all
thirteen. hb1−lnd carries a bootstrap CI excluding zero on 12 of 13
tiers, mx_c3−lnd on 10 of 13. Moving the liquidity family, the
amount family, and the mainnet balances did not once bring lnd near
the champions.

The margins do shrink as the generator flattens away from the fitted
`exp(0.05)` world — hb1−lnd falls from +0.298 on `bimodal:0.01` to
+0.210 on `uniform` — and read alone that shrinkage looks like the
overfitting signature we were hunting. The control that kills that
reading is the seed:

| margin vs lnd | bimodal_0.01 | control | bimodal_0.2 | beta_0.3_0.3 | beta_2_2 | uniform |
|---|---|---|---|---|---|---|
| hb1 | 0.298 | 0.279 | 0.233 | 0.249 | 0.204 | 0.210 |
| mx_c3 | 0.289 | 0.271 | 0.214 | 0.241 | 0.176 | 0.186 |
| **seed (never fit to anything)** | **0.229** | **0.205** | **0.154** | **0.194** | **0.162** | **0.147** |
| atomic1 | 0.123 | 0.183 | 0.195 | 0.283 | 0.274 | 0.257 |

The seed's margin decays with the same shape and by a similar
fraction as the champions' — and the seed predates every constant
under suspicion. The common cause is visible in lnd's own column:
lnd climbs from 0.143 to 0.369 as liquidity flattens, so everyone
compresses toward a ceiling on the easy worlds. If the champions'
compression came from fitted priors, the unfitted seed would hold its
margin. It does not. **The margins are regime difficulty, not
memorized constants.**

## Finding 2: atomic1 is a flat-liquidity specialist, and the ladder proves it

The one genuine reordering tracks the generator exactly: atomic1's
rank across the liquidity ladder is 4 → 4 → 3 → 1 → 1 → 1, its
margin over lnd rising monotonically from +0.123 to +0.283 as the
world flattens, and it takes rank 1 on three of the four mainnet
families. On `beta:2:2` this is unambiguous quality, not
abandonment: the highest success of any router (0.722) at the fewest
attempts (3.6). The mirror image is equally real — on `bimodal:0.01`
it is worse on both axes at once (success 0.36 against hb1's 0.55),
which is the abandonment signature exp-013 taught us to read.

So the three evolved routers now have legible regimes: hb1 owns
sharply bimodal liquidity, atomic1 owns flat liquidity, and mx_c3
sits between them without owning either. Which leads to finding 3.

## Finding 3: the "generalist champion" title is eroding

mx_c3−hb1 is at or below zero on 12 of 13 tiers. The effects are
tiny — never beyond |0.028| — but one clears both bars:
`liq-uniform` at −0.025, CI [−0.064,−0.003], sign test 0/9, p=.004.
And the tier family that anchored mx_c3's title gives it no shelter:
on all four mainnet families the pair ties to within 0.001. On four
of the six ladder tiers the two have *identical success* and differ
only in attempts, mx_c3 spending 0.7–3.6 more per payment.

Stacked on exp-015's fresh-corpus result (hb1 +0.009, p=.014, at
n=40), the evidence now points one way: hb1 is at least mx_c3's
equal everywhere we have looked recently, and better wherever they
differ. This is still not a champion swap — the standing rule
requires a held-out paired sweep across the full original tier set,
and OOD/split (the tiers mx_c3's title was earned on) were not in
this sweep. But the adjudicating experiment is now cheap and
well-defined, and it should run before the next round of docs
carries the "generalist" framing any further.

## Finding 4: the amount axis was never a threat

Holding liquidity at the control and moving only the amount
distribution barely touches the champions: hb1−lnd is +0.210 on
lognormal (10/10 files, p=.002, the strongest single result in the
sweep) and +0.165 on round-value clustering. The sibling circularity
the advisor flagged is real in principle but empty in practice — the
champions' edge does not depend on how we draw amounts.

## Finding 5: give_up_rate does not mean what its name says

For all four candidate routers, `give_up_rate == 1 − success_rate`
holds to three decimals on every tier: a candidate "gives up"
whenever it returns failure without exhausting the attempt budget,
and that is how candidates always fail. Only lnd, which burns its
budget, deviates. The field is therefore a router-style
fingerprint, not an abandonment signal, and the ASI warning added at
`4d1a20994` fired on everything. Abandonment remains readable only
jointly — low attempts AND low success, as in finding 2 — and the
evaluator hint has been rewritten to state that rule unconditionally
instead of thresholding on the field (`evaluate_code.py`).

## Anomalies

- **`liq-hubdrain_0.05` is underpowered and internally inconsistent
  at n=10:** every router collapses (hb1−lnd +0.091), hb1 wins 9/10
  files (p=.021) yet one file drags the CI across zero. The first
  topology-correlated world clearly deserves its own experiment
  rather than a verdict from this one.
- No degenerate tiers: no file has all five routers producing
  identical output, so the exp-012 multivantage trap did not recur.
- Fee spread is negligible everywhere; the objective differences are
  entirely success and attempts.

## What this does and does not de-circularize

The claim we can now make: **the champion ordering, and most of the
margin, survive liquidity and amount distributions the evolved
constants were never fit to, including two (beta:2:2, uniform) where
the bimodal hypothesis embedded in their priors is false.** The
paradigm — per-channel amount bounds learned from attempt evidence —
is what wins, not the constants; the constants' contribution is the
residual atomic1's ladder exposes at the regime edges.

What remains authored: every world in this sweep, including the
re-liquified mainnet tiers, is still a distribution we chose. The
full escape from "simulator-shaped" is unchanged — degraded
attribution, and offline replay against a real node's attempt
stream. Those move up now; the generator-family question is closed.
