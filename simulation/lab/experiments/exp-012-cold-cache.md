# EXP-012 — Cold cache, hot load: what is a mission control worth?

**Date:** 2026-07-26 (started)
**Status:** in flight — warmup curves measured, warmup/staleness
instrumentation under implementation

## Question

roasbeef's field observation: mission control's weights matter
enormously on a network full of unbalanced, unreliable nodes, and a
NEW node has none — it burns a long, expensive warmup on its first
payments. The proposed fix is to serve cached weights from an API so
a fresh node can hot-load instead of probing from scratch. Three
questions follow, and none of our published numbers answer them:

1. How fast does each design get cheap (the warmup curve)?
2. What is imported knowledge worth (hot load vs cold start)?
3. How stale can imported knowledge be and still help?

Every result in this program to date is a COLD-START result: each
scenario file begins with an empty mission control and empty
candidate beliefs. That cuts both ways — it means the champions'
8.6× attempt advantage was earned with no more history than lnd had,
and it means we have never tested the regime where a production
node's mission control holds thousands of observations.

## Part 1 — Warmup curves (analysis only, no simulator change)

Payments inside a scenario file run in order against one mission
control and one set of candidate beliefs, so the attempt count at
payment index i measures what the first i−1 payments taught the
router. Tool: `simulation/warmup_curve.py`. Corpora: mainnet
(10 files × 10 payments, real 12,161-node graph) and hard test.

Raw curves are confounded: index i is a DIFFERENT payment, so the
series mixes learning with per-payment difficulty. The clean read is
each router's attempt count relative to the champion on the SAME
payment, early in the batch versus late:

| router | mainnet first-3 | mainnet last-3 | hard first-3 | hard last-3 |
|---|---|---|---|---|
| lnd stack | 4.72× | **11.88×** | 5.10× | 4.42× |
| seed | 1.68× | 4.44× | 4.29× | 7.44× |
| hb1 | 0.91× | 0.99× | 1.06× | 1.34× |
| mx_c3 | 1.00× | 1.00× | 1.00× | 1.00× |
| **atomic1** | 0.72× | 0.58× | 1.56× | **0.73×** |
| opus1 | 2.08× | 3.35× | 1.33× | 1.52× |

(Absolute means: lnd goes 10.1 → 31.2 attempts/payment on mainnet,
mx_c3 2.4 → 2.6, atomic1 1.5 → 1.5.)

**Three findings.**

1. **lnd's mission control does not warm within a realistic batch.**
   Its disadvantage does not shrink with experience — on mainnet it
   GROWS (4.7× → 11.9×), and on hard it is flat. Ten payments of
   history buys nothing measurable. This is the empirical form of the
   field defect: the warmup is not slow, it has not started.

2. **The champions are cheap from payment one, so their advantage is
   PRIOR, not history.** mx_c3 needs 2.4 attempts on its first three
   mainnet payments, before it has learned anything at all. That is
   the encouraging result for the hot-load idea, but it reframes it:
   the thing worth shipping to a fresh node may be the bimodal prior
   plus interval machinery, not a cache of someone else's
   observations.

3. **Only the memory-carrying hybrid demonstrably learns.** atomic1
   halves its ratio to the champion across the hard batch (1.56× →
   0.73×) — the single clearest within-batch learning signal in the
   family, and consistent with its package-level cross-payment
   network memory. Both Opus-lineage routers are stateless across
   payments and show no such improvement. The lineage split found in
   the exp-010b docs pass is now visible in the measurements.

**Caveat.** The ratio normalizes for payment difficulty but not
perfectly: each router perturbs liquidity differently as it goes, so
by payment 10 the routers face slightly different networks. Parts 2
and 3 remove the confound by holding the scored batch fixed and
varying only what the router knew when it started.

## Part 2 — Hot load (pending instrumentation)

An unscored `warmup` phase in the runner: N payments run before the
scored batch, warming mission control and candidate state but not
counting toward any metric. Sweep N ∈ {0, 25, 100, 400} and compare
the same scored batch. Works identically for lnd and candidates with
no contract change, so it measures exactly what a served cache would
buy each design. If lnd's curve crosses the champions' at some N,
that is the hot-cache regime where production mission control wins,
and we will have found it.

## Part 3 — Staleness (pending instrumentation)

`stale_gap_sec`: after warmup and before scoring, advance the virtual
clock and run proportional background traffic. Plot payment-1
attempts against gap length. This is the API-cache question exactly:
how old can served weights be and still help? The champions'
scope-based staleness handling (exp-010b) and drift1's confidence
half-life (exp-008) are the two candidate primitives for weighing
aged imported evidence.

## Part 4 — Third-party weights (designed)

The structural asymmetry worth measuring: mission control history is
pair-based and entangled with the observing node's vantage, while the
champions' per-directed-channel bounds are vantage-independent facts
about channels. Import observations gathered from a DIFFERENT source
node and measure what each design can use. If channel-interval
knowledge transfers across vantages and MC history does not, that is
a concrete argument for what a weight-serving API should actually
serve.
