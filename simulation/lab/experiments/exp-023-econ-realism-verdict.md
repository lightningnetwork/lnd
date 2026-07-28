# EXP-023 — The economic-realism verdict: the edge is informational

**Date:** 2026-07-28
**Status:** measurement phase complete (1,920 runs, zero errors);
the evolution arm in the economic world remains. Champions unchanged.

## What ran

Five routers (lnd, seed, hb1, mx_c3, atomic1) across the full
economic tier set the build phase enabled: fee budgets at six rungs
across two tier families, tight and empirical HTLC limits, heavy and
empirical inbound fees plus the real mainnet policies, concurrency at
windows {1, 2, 4} with churn calibrated constant (multipliers 1.000 /
1.531 / 1.824, converged in one pass), latency rungs sequential and
concurrent, and one exploratory composition tier with all five knobs
live. Gates first: every clean sealed tier reproduced the exp-022
table to the last bit, 24/24 cells, and every knobbed tier proved its
engagement counters nonzero before its results counted. Full tables,
seeds and commands: `exp-023-results-summary.json`,
`exp-023-sweep-tables.txt.gz`, `exp-023-sweep-commands.md`.

## The headline

**Economic realism closes the champion gap on exactly the two pricing
mechanisms, and on none of the three constraint, timing or contention
mechanisms.**

- **Fee budgets, the largest effect in the sweep.** The gap narrowing
  clears the full pre-registered bar unanimously on mainnet at every
  rung (hb1 −0.154/−0.186/−0.190 at 400/100/25 ppm, sign tests to
  p=.002; mx_c3 similar). The champions' significant clean-mainnet
  lead goes negative at every rung as a point estimate — stated with
  its caveat: those inversions have CIs excluding zero but sign tests
  of .07 to .29, so the inversion itself does NOT clear the bar; the
  narrowing does. lnd's fee-aware pathfinding never violates a budget
  (zero refusals at all six rungs); the interval routers, which
  cannot see fees they never priced, walk into it.
- **Inbound fees, on the authored rung.** Under heavy inbound fees
  the champions' significant lead over lnd disappears entirely
  (hb1 +0.194 → +0.041 ns, mx_c3 +0.230 → +0.051 ns). On the REAL
  mainnet inbound-fee policies: exactly null, ten ties of ten — the
  real network's inbound fees are too sparse to matter yet.
- **HTLC limits: no.** The gap does not move, and tight caps hurt lnd
  MOST (−0.104, p=.021). H-A2 refuted: free public bounds do not
  substitute for learned ones.
- **Concurrency: no.** Window 4 significantly costs every router
  (lnd −0.074 to hb1 −0.112, all clearing the bar) but roughly
  equally; every gap move straddles zero at n=20.
- **Latency: no.** The E-a null landed in its strongest form: with
  traffic removed, all five routers are exactly identical under
  latency alone, lnd included. Objective L never flips a sign.

The reading, and the sentence the program has been converging on from
three directions: **the champions' edge is informational, not a
pricing edge. Port the belief system, not the cost model.** That is
also a direct validation of the interval-router branch's design,
which pairs the evolved belief system with lnd's own fee-aware
pathfinding rather than porting the champions' crude fee handling.

## Hypothesis scoreboard

Confirmed: H-B2 (larger than stated), H-C1 (unanimous and monotone),
E-a (strongest form). Refuted: H-A2, E-c on its sign-change claim
(ordering component holds). Partially confirmed: H-D2
(router-specific: hb1's attempts triple under contention while
atomic1 is nearly immune, 0.048 self-contention per attempt — the
hold-and-release arena bred exactly the machinery this tier prices).
Underpowered as pre-registered: H-D3 (point estimates split hb1
negative from atomic1 positive), E-b. Not testable by measurement,
reserved for the evolution arm: H-A1, H-B1, H-C2, H-D1.

## Second-order findings

1. **atomic1 is the fee-pressure-robust champion** (+0.061 with CI
   excluding zero at 400 ppm where hb1/mx_c3 go negative) and the
   contention-immune one. Its stock keeps rising every time the
   environment gets more real.
2. **Fee budgets induce genuine champion abandonment** (the operative
   knob-induced gate flags hb1/mx_c3 at the 4000/5000 rungs with CIs
   excluding zero); concurrency and latency do not.
3. The literal abandonment gate from the spec flags every clean
   control (champions always sat below the seed on raw success; that
   is the attempts-for-success trade) — recorded so the next spec
   writes the knob-induced form directly.
4. Stage D's H-D3 smoke reversal half-reproduces: lnd absorbs 1.8x
   the seed's self-contention per attempt at window 2, ties at 4,
   and hb1 is above both at 4.
5. The stage C fee-visibility leak is tier-dependent (20 to 30%, and
   0.0% on atomic tiers, where hold-and-release returns the money).

## Consequences

1. The evolution arm is now the run that matters: H-A1/B1/C2/D1 all
   need machinery no incumbent carries (budget tracking, inbound-fee
   pricing, contention planning), and stage E needs no new contract
   surface at all. One run in the composition world tests whether the
   paradigm grows economics the way exp-022 grew attribution
   confidence — and whether it pays for it the same way.
2. The interval-router branch needs no redesign in light of this: the
   hybrid (evolved beliefs + lnd pricing) is what the data says to
   build, and it is what was built.
3. atomic1's fee robustness and contention immunity deserve a source
   audit before the evolution arm's background prompt is written; its
   mechanisms are the ones the economic world rewards.

## Caveats

n=10 per tier (n=20 on contention) as always; the composition tier is
exploratory only (its fee rung was calibrated on the wrong profile
and crushes lnd's fees asymmetrically); heavy/tight/25ppm are
authored rungs and labelled as such; the empirical rungs are close to
inert, which is a statement about today's mainnet policy distribution
as much as about the routers.
