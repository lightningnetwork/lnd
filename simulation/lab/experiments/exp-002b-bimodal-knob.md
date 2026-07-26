# EXP-002B — The knob that was never turned: lnd's bimodal estimator, tuned

**Date:** 2026-07-26
**Status:** complete

## Why this ran

`WHY.md` §0 flagged an untested claim. This program has repeated since
exp-002 that "parameter tuning could not beat lnd's defaults, so the
paradigm is the lever, not the knobs" — but the one configuration that
would make lnd's own machinery match this environment had never been
evaluated. lnd ships a **bimodal estimator** whose hypothesis is the
same one the champions exploit, and its `scale_msat` is an ABSOLUTE
amount, defaulting to 300M msat. Our liquidity generator draws balance
fractions with mean 5% of each channel's capacity, so the scale that
matches the environment is 5% of a typical channel: 100M msat on the
hard corpus (2M sat channels), 150M on v2 (3M sat). The staged
`params_lnd_bimodal.json` used the raw default. So the closest
analogue to the champions inside lnd had never been given its best
shot. Cheapest outstanding experiment in the program, and a
prerequisite for any upstream conversation.

## Design

Seven bimodal configurations bracketing the environment-matched scale
by an order of magnitude either way, against lnd's shipping apriori
default and the champion, on two sealed tiers. Same binary, same
corpora, same objective; only `--params` differs.

## Results

| router | hard obj | hard succ | hard att | v2 obj | v2 succ | v2 att |
|---|---|---|---|---|---|---|
| lnd apriori (ships) | **0.298** | 0.421 | 30.9 | **0.357** | 0.525 | 58.8 |
| bimodal 10M | 0.259 | 0.429 | 63.9 | 0.345 | 0.528 | 73.3 |
| bimodal 50M | 0.261 | 0.456 | 77.7 | 0.319 | 0.518 | 76.0 |
| **bimodal 100M** (matched, hard) | 0.261 | 0.456 | 78.7 | 0.330 | 0.528 | 77.4 |
| **bimodal 150M** (matched, v2) | 0.273 | 0.467 | 78.9 | 0.330 | 0.518 | 81.2 |
| bimodal 300M (staged default) | 0.280 | 0.478 | 76.9 | 0.330 | 0.528 | 79.0 |
| bimodal 1000M | 0.283 | 0.478 | 77.2 | 0.331 | 0.528 | 79.2 |
| **mx_c3** | **0.479** | 0.592 | 8.1 | **0.581** | 0.695 | 8.4 |

(Corpora were regenerated after a reboot, so absolute levels differ
slightly from earlier writeups; every row here shares one corpus.)

## Verdict: the claim survives, and the mechanism is now explicit

**No bimodal scale beats lnd's own apriori default on either tier**,
and none comes within 0.19 of the champion. The
environment-matched scale is not even the best bimodal setting — it is
among the worse ones. "The paradigm is the lever, not the knobs" is
now tested against lnd's closest analogue and it holds.

**The interesting part is HOW bimodal fails, because it confirms
WHY.md's central thesis empirically rather than by argument.** Look at
the success and attempt columns together on the hard tier: bimodal
RAISES success (0.421 → 0.478) and simultaneously **more than doubles
attempts** (30.9 → 77). A better liquidity prior makes lnd more
willing to keep trying — it correctly believes some route might still
work — so it finds more payments at a much higher price. Under an
objective that charges for attempts, that is a net loss.

What it does not do is change *what lnd retries*. lnd's `findPath`
takes the amount as a fixed argument and only halves when path finding
fails outright, which on a large graph almost never happens; so with
any estimator it keeps retrying the SAME amount over different routes.
The champions read their `upperFail` bound and retry a DIFFERENT
amount. A better prior improves route ranking within a broken retry
strategy; it cannot supply the missing one. That is the paradigm gap,
and it is now measured: the estimator swap moves the objective by at
most 0.02, while the paradigm difference is worth 0.18–0.22.

## Consequence for the upstream story

The honest framing is stronger than the old one, not weaker. We are no
longer saying "we didn't manage to tune lnd into competitiveness"; we
are saying **lnd's own bimodal hypothesis, given its best scale for
this environment, buys success at double the attempts and still loses
by a wide margin, because the estimator is not the part that needs
changing.** The part that needs changing is that nothing in the
retry loop reads `FailAmt` to size the next attempt — and mission
control already records it.
