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

## Part 2 — Hot load: stale knowledge is worse than no knowledge

Instrumentation landed (f5a96aac8, c2319b17a): an unscored `warmup`
phase runs N payments through the identical code path before the
scored batch, optionally aged by `stale_gap_sec`, optionally sent from
another node, and optionally followed by a liquidity restore.

**The first sweep measured the wrong thing, and the failure is worth
recording.** Warmup payments are real payments: they teach the router
AND drain the network the scored batch then has to use. Across N = 0,
25, 100, 400 on mainnet every router got monotonically worse
(objective 0.79 → 0.65 → 0.43 → 0.20), and at N = 400 the whole field
collapsed to a 22% success rate where lnd "led" on objective purely by
abandoning a dead network faster than anyone else. That is depletion,
not the value of a cache.

The control that separates the two is a liquidity snapshot taken
before the warmup and restored after it. Be precise about what that
arm then measures: the network is fresh again, but the router's
beliefs describe the drained network it just explored, so this is
knowledge about a network state that has since been completely
churned — **a maximally stale cache**, which is the worst case for the
weight-serving API and not the same thing as a fresh one.

| tier | lnd | hb1 | mx_c3 | **atomic1** |
|---|---|---|---|---|
| cold | 0.694 (19.8 att) | 0.790 (2.3) | 0.791 (2.3) | 0.790 (1.6) |
| stale-25 | 0.617 (19.6) | 0.738 (1.6) | 0.734 (1.8) | **0.797 (1.8)** |
| stale-100 | 0.377 (26.9) | 0.550 (1.4) | 0.550 (1.2) | **0.783 (2.2)** |
| stale-400 | 0.228 (32.4) | 0.347 (0.6) | 0.347 (0.6) | **0.775 (2.0)** |

Paired vs mx_c3: atomic1 +0.063 (p=.004) at 25, **+0.233 (p=.002)** at
100, **+0.428 (p=.002)** at 400. This is the first time in the
program's history that any router has beaten a champion on mainnet
with statistical significance.

**Three failure modes, each legible in the attempt counts.**

1. **lnd THRASHES.** Attempts climb 19.8 → 32.4 while success falls
   0.79 → 0.35. Mission control's pair entries are permanent zeros on
   this tier (no clock section, so decay never fires — WHY.md §0), so
   a stale blacklist keeps steering it onto fresh-looking routes that
   are no better, and it never gives up.
2. **The champions ABANDON.** mx_c3 and hb1 collapse to 0.6
   attempts/payment at 36% success — they quit almost immediately.
   Their `upperFail` bound is a HARD zero, so a stale bound declares a
   perfectly good channel dead, and enough dead channels make the
   payment look hopeless before it is tried.
3. **atomic1 SHRUGS.** 0.790 → 0.775, a 2% degradation against the
   champions' 56% and lnd's 67%, at a nearly unchanged 2 attempts. Its
   persisted bounds clamp to a 0.012 probability floor instead of
   zero, so stale evidence makes a channel unattractive rather than
   forbidden and one retry is enough to correct it.

**Why this matters beyond the experiment.** A served weight cache is
stale by construction — that is what serving it means. These
measurements say the consumer's staleness policy dominates the value
of the cache, and that the safe policy is a floor, never a hard zero.
atomic1's scope-split (savage in-payment evidence, soft persisted
bounds) was bred in the atomic arena for entirely different reasons
and turns out to be the property that makes imported knowledge safe.
That is the most directly upstream-shaped result the program has
produced: it argues for a probability floor on learned evidence in
mission control, which is a small change to an existing estimator
rather than a new paradigm.

**What this arm does NOT show.** It does not show that a *fresh* cache
is worthless — no arm here has yet warmed a router with knowledge that
stays valid. The natural next arm warms with small probe payments
(a few percent of the scored amounts) and does not restore: small
payments teach channel structure without materially draining, so the
knowledge remains true when it is used. That is the honest model of a
probe-warmed node and it is queued.

## Part 2b — Hot load with valid knowledge (queued)

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

## Part 4 — Third-party weights: a stranger's knowledge is not worse

Design: score from a well-connected mainnet vantage, warm from either
that same node (self) or a degree-31 stranger (foreign), with
everything else matched — same 18 files, same 25 warmup payments, same
liquidity restore, so the ONLY variable is who gathered the knowledge.
A first attempt at three files was underpowered noise (CIs spanning
[0, 0.5]) and was discarded rather than reported.

| router | self-vantage | foreign-vantage | Δ (foreign − self) |
|---|---|---|---|
| lnd | 0.155 (3.0 att) | **0.176 (0.8 att)** | **+0.021** |
| mx_c3 | 0.174 (0.4 att) | 0.169 (0.5 att) | −0.005 |
| atomic1 | 0.209 (0.6 att) | 0.209 (0.6 att) | 0.000 |

**Nobody is hurt by using a stranger's observations, and lnd is
helped.** For atomic1 the two arms are identical to three decimals;
for mx_c3 they are within noise. That is the expected result for
per-directed-channel bounds, which are facts about a channel and carry
no trace of who observed them.

**lnd improving on a stranger's knowledge is the surprise, and it
sharpens the vantage story rather than confirming it.** The
instrumentation agent had already observed that mission control
history is only partly vantage-entangled: a failure at a remote relay
records the pair `(relay, target)`, which contains no reference to the
observer and transfers fine. What this sweep adds is the sign of the
entangled remainder. Warming from lnd's own vantage populates the
pairs involving its own local channels — precisely the pairs every one
of its payments must traverse — with stale zeros it cannot decay away
on this tier, and its attempt count triples (0.8 → 3.0) as it thrashes
around its own poisoned first hop. A stranger's warmup cannot touch
those pairs, so it teaches the transferable part and leaves the
critical part clean.

So the practical answer to "what should a weight-serving API serve" is
narrower than the vantage-independence argument suggested, and more
interesting: **serve remote-pair observations, and do not serve (or do
not import) observations about the consumer's own local channels.**
Those are the ones a node can cheaply measure itself, they are the
ones whose staleness is most damaging, and they are the only part of
mission control that is genuinely vantage-bound.

**Caveat.** Every arm here is in the stale regime by construction (the
restore control means imported knowledge describes a churned network).
Absolute objectives are low (0.15–0.21) because 25 stale warmup
payments on this corpus is a punishing setup; the paired deltas are
the result, not the levels. Fresh third-party knowledge — a stranger's
observations of the network as it currently is — remains unmeasured
and needs the part 2b probe-warm arm first.

## Part 4b — Fresh third-party weights (queued, needs part 2b)

The structural asymmetry worth measuring: mission control history is
pair-based and entangled with the observing node's vantage, while the
champions' per-directed-channel bounds are vantage-independent facts
about channels. Import observations gathered from a DIFFERENT source
node and measure what each design can use. If channel-interval
knowledge transfers across vantages and MC history does not, that is
a concrete argument for what a weight-serving API should actually
serve.
