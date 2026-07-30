# EXP-029 — The foreign balance sheet: the ordering survives on balances nobody fit to

**Date:** 2026-07-30.
**Status:** complete. The strongest de-circularization result in the
program; frontier unchanged; the integration branch tracks the
champions on external data.

## Why this ran

The standing correction in WHY.md §0: our mainnet tier is real
topology and real policies but SYNTHETIC liquidity — balances drawn
from the same `ExpFloat64()*0.05` generator the evolved priors were
fit to. Every escape we had built so far (exp-017's thirteen
authored families) was still a family we chose. dijkstrasden's
revised model graph is the first liquidity family we did NOT author:
11,255 nodes / 37,203 edges in describegraph form plus per-edge
balances generated from ln-scores mission-control data through a
fee-conditioned mixture model (their report:
https://fee-liquidity-correlation.lightning.wiki/). Soft U-shape:
32.7% of channels in the sub-5%/over-95% tails against ~63% for our
generator, with a fat middle ours never draws.

## Method

The 7d10989fd loader flag (`liquidity_model: "from_graph"` +
`unbalanced_source`) assigns the graph's own balances. The tier
mirrors the sealed exp-009 shape — the same hub vantage carried
through the graph's `pub_key_og` mapping (degree 2,013, rank 1, as
in the original), targets drawn to match the sealed tier's empirical
target-degree distribution (a uniform draw would have put 58% of
payments on degree-1 leaves, the exp-012 reachability trap), 100
payments per file, n=10. Three variants per file: A = foreign
balances, B = identical topology under our bimodal generator (the
family isolation), A_prod = A plus the production-default 50,000 ppm
fee limit. Seven arms: lnd, seed, hb1, mx_c3, atomic1, econ2, and
ilnd (the interval-sim tree at the round-6 tip plus the loader
merge, reuse gate re-verified). Replicate protocol throughout: a
3-sample determinism screen lied again; 53/200 cells are
nondeterministic (all lnd or ilnd, worst objective range 0.0012, two
orders below every margin), the five candidates exactly
deterministic.

## Q1 — the ordering survives, and the gap is WIDER

| arm − lnd | Δobj | CI | pairs | p |
|---|---|---|---|---|
| atomic1 | +0.132 | [+0.109,+0.151] | 10/0 | .002 |
| mx_c3 | +0.127 | [+0.102,+0.149] | 10/0 | .002 |
| hb1 | +0.126 | [+0.104,+0.147] | 10/0 | .002 |
| ilnd | +0.126 | [+0.102,+0.148] | 10/0 | .002 |
| econ2 | +0.116 | [+0.094,+0.137] | 10/0 | .002 |
| seed | +0.105 | [+0.081,+0.125] | 10/0 | .002 |

Absolute: lnd 0.596, ilnd 0.722, hb1/mx_c3 0.723, atomic1 0.728.
The tier is harder for everyone than classic mainnet (−0.06 to
−0.10 absolute), and the paradigm's lead over lnd GREW by about a
third (+0.097 → +0.127..0.132), all of it attempts converted to
success and efficiency (attempts −11 to −13 per payment against
lnd's).

atomic1 on top is a prediction come true on data it never saw:
exp-017 filed it as the flat-liquidity specialist (ladder rank 4→1
monotone as liquidity flattens), and this graph's soft U-shape is
the flattest realistic family we have ever scored.

## Q2 — the release candidate tracks the champions exactly

ilnd vs mx_c3 −0.0009 [−0.0044,+0.0025]; vs hb1 −0.0008; vs atomic1
−0.0060 — every CI straddling zero. Under the production-default fee
limit it GAINS +0.0038 while paying 267 fewer ppm, with
`fee_limit_failures` at zero for every arm (round 6's
production-default finding reproduced on external data). For the
release case: the integrated branch is champion-grade on a world
neither it nor the champions were ever fit to.

## Q3 — the de-circularization answer

A−B, paired on identical files and topology with only the liquidity
family switched: every arm gains a little on the foreign balances
(+0.009 to +0.025) and NOT ONE of the seven CIs excludes zero. The
spread between arms is smaller than any single arm's interval, and
the direction runs opposite to the overfitting story: the fitted
champion mx_c3 gains LEAST (+0.009), the never-fitted seed among the
most (+0.025). A source-rebalance control shows the
`unbalanced_source` choice carries none of it. If the champions'
mainnet numbers had been an artifact of priors fit to our generator,
this swap is exactly where it would have shown. It did not show.
The WHY.md §0 caveat is now measured: what the synthetic-liquidity
choice was worth is approximately nothing, and what remains authored
is the topology-shaped part every arm shares.

## Q4 — the fee-liquidity signal: present, unexploited

The graph's construction embeds the fee→depletion correlation, and
it is measurable here: Spearman −0.149 between a directed end's fee
rate and its balance fraction across 74,406 ends. The literal
"did routes exploit it" question needs a per-route counterfactual no
aggregate carries, so it is reported as not cleanly measurable
rather than forced. The well-posed substitute is negative: if the
signal were being exploited, fee-pricing arms should gain more on A
than B, and they do not (econ2 sits mid-pack, bracketed by the
never-fitted seed and stock lnd). The IDEAS entry stands: a router
that reads fees as a liquidity prior is unclaimed ground, and this
tier is where the signal is guaranteed live.

## Caveats

One vantage (the mapped exp-009 hub); n=10; the graph is still a
MODEL fit to one prober's mission-control history, not a measured
balance sheet — the residual escape remains offline replay on real
payment data (queued next week). The tier's generator script and
og→new key mapping are committed; the graph itself lives at
`~/codez/data/realistic_graph.json` (96MB, not in-repo).

## Artifacts

`exp-029-results-summary.json` (verdict, tier_facts,
balance_family_effect, prod_default_effect, determinism blocks),
`exp-029-tables.txt.gz`, `exp-029-commands.md`, `exp-029-gen-tier.py`
(deterministic; reads the sealed tier for the target-degree
distribution), `exp-029-tier-readme.json`,
`exp-029-fee-liquidity-signal.json`. interval-sim tip ffe5e5537
(loader merge) on the roasbeef fork.
