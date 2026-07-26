# EXP-012 — Cold cache, hot load: what is a mission control worth?

**Date:** 2026-07-26 (started)
**Status:** complete — all four parts measured and reported. Two
follow-ons are named at the end and both need simulator changes.

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

## Part 2b — Probe-warm: valid knowledge does not pay either

100 unscored probes at 2% and at 10% of the scored amounts, no
restore, so what the router learns stays TRUE when it is used. This is
the honest model of a probe-warmed node.

| tier | lnd | mx_c3 | atomic1 |
|---|---|---|---|
| cold | 0.694 (19.8 att) | 0.791 (2.3) | 0.790 (1.6) |
| 100 probes @ 2% | 0.664 (19.5) | 0.768 (2.2) | 0.766 (1.9) |
| 100 probes @ 10% | 0.597 (15.9) | 0.653 (2.7) | 0.651 (2.6) |

Nobody gains, and the loss grows with probe size. lnd's attempts do
fall at 10% probes (19.8 → 15.9), the only sign of genuine warming
anywhere in this experiment, but its success falls faster, so the
objective still drops. The champions' attempt counts barely move
(mx_c3 2.3 → 2.2 → 2.7, atomic1 1.6 → 1.9 → 2.6).

## Verdict — there is no hot-cache regime here, and one design flaw explains why

Across every arm — knowledge-with-depletion, stale knowledge with the
network restored, foreign-vantage knowledge, and small valid probes —
**no amount of warming ever moves any router above its cold-start
score, and mission control never approaches the champions.** The
original question ("does lnd's MC eventually cross, given enough
observations?") has a clean negative answer at 25, 100 and 400
observations, with fresh knowledge and with stale, from its own
vantage and from a stranger's.

Two mechanisms explain the negative, and they are worth separating:

1. **The champions have nothing to learn.** They are within noise of
   their asymptote on payment one (part 1: 2.4 attempts on the first
   three mainnet payments, before any evidence exists). Their edge is
   the prior, so warming cannot add to it and can only subtract by
   spending liquidity or by going stale.
2. **lnd cannot learn fast enough for this to matter.** 100
   observations on a 12,161-node graph is roughly 1% pair coverage, and
   what it does record is a permanent zero on tiers with no clock, so
   the marginal observation is as likely to poison a future route as
   to inform one.

**But the experiment also has a design limit we should state plainly.**
Every arm here derives knowledge from *payments*, and payments cost
liquidity. That makes "free knowledge" unconstructible: the drain arm
pays in depletion, the restore arm pays in staleness, the probe arm
pays in both, just less. A served weight cache in the real proposal
costs the consumer *nothing* — it arrives over an API. To measure that,
the simulator needs to inject beliefs directly into mission control (or
into a candidate's state) from a file, with no payments sent at all.
That is a small addition (`--import-weights`) and it is the only design
that can isolate the value of imported knowledge from the price of
acquiring it. **Until it exists, exp-012's negative is a statement
about probe-warming, not about weight-serving.**

What the experiment DOES establish, and what carries upstream:
- The champions' advantage is a prior, not accumulated history, so a
  fresh node with the right prior is fast immediately (part 1).
- Under stale knowledge, the consumer's staleness policy dominates
  everything: hard zeros poison, floors survive (part 2). This is the
  actionable upstream finding.
- Remote-pair observations transfer across vantages; observations about
  your own local channels do not, and importing them is actively
  harmful (part 4).
- Our background-traffic engine is ~5× weaker than configured, which
  caveats exp-008 and exp-010b (part 3).

## Part 2c — Direct weight import (designed, needs a sim change)

Every arm above buys knowledge with payments, and payments cost
liquidity, so none of them can construct the thing the weight-serving
proposal actually offers: knowledge that cost its consumer nothing.
The missing capability is an `--import-weights` path that seeds
mission control (and a candidate's persisted state) from a file with
no payments sent at all. That is the only design that separates the
value of imported knowledge from the price of acquiring it, and it is
what the proposed API does in reality. Until it exists, this
experiment's negative is a statement about probe-warming.
## Part 3 — Staleness gap: a null, and why the null is the finding

Design: identical 25-payment warmup in every arm, no restore, then an
idle gap of 0, 600, 3600 or 21600 virtual seconds during which
background traffic runs, then the same scored batch. Only the gap
varies, so depletion is held constant and the gap effect is isolated.

| gap | lnd | mx_c3 | atomic1 |
|---|---|---|---|
| 0 s | 0.544 (24.6 att) | 0.614 (2.6) | 0.650 (2.5) |
| 600 s | 0.544 (24.6) | 0.614 (2.6) | 0.650 (2.5) |
| 3600 s | 0.543 (24.7) | 0.614 (2.6) | 0.650 (2.5) |
| 21600 s | 0.543 (24.7) | 0.614 (2.6) | 0.650 (2.5) |

Six virtual hours of churn changes nothing, to three decimal places,
for any router.

**Manipulation check (run before believing the null).** The gap really
does run traffic: background payments sent scale 700 → 720 → 820 →
1420 across the four arms, exactly the prorated volume `AdvanceIdle`
promises. The knob works; the world just does not move enough for it
to matter.

**And that is the finding, because it indicts our churn model.** Two
numbers explain the null. First, 720 extra background payments is
nothing against a 12,161-node graph with tens of thousands of
channels — the odds that churn touches the specific corridors a scored
payment needs are small. Second, and worse: **only about 18% of
background payments settle** (129 of 700 in the manipulation check).
The traffic engine sends naive fee-optimizing payments that mostly
fail, and a failed payment moves no liquidity, so our exogenous
process is roughly five times weaker than its configuration suggests.

**This weakens a published conclusion.** exp-008 concluded that
time-decay "buys nothing at realistic churn." That conclusion is
sound about *our* churn, and our churn is far gentler than intended.
The honest restatement: decay buys nothing at the weak churn this
simulator generates, and the drift experiment never reached a regime
where evidence genuinely goes stale. The same caveat applies to
exp-010b's atomic arena, whose per-attempt drift is drawn from the
same engine.

**Fix before any staleness claim is made again**, in order of
directness: (a) make background traffic succeed — size its amounts to
what the network can actually carry, or let it retry, so a settled
fraction near 1 moves real liquidity; (b) aim a share of traffic at
the corridors the scored payments use, instead of uniformly at random;
(c) only then re-run this sweep, and re-run exp-008's decay question
underneath it.

## Part 3b — Staleness under real churn (queued behind the traffic fix)

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

## Part 4b — Fresh third-party weights (queued, needs part 2c)

Part 4 measured third-party knowledge in the STALE regime only, and
inverted the reasoning this section was originally written on. The
superseded framing — "mission control history is entangled with the
observing node's vantage while channel bounds are not, so intervals
should transfer and MC should not" — turned out to be half wrong: most
of mission control transfers fine, because a failure at a remote relay
records `(relay, target)` and names no observer. Only the pairs
crossing the consumer's own local channels are vantage-bound, and
importing those is what hurts.

What remains unmeasured is FRESH third-party knowledge: a stranger's
observations of the network as it currently is, rather than of a
network that has since churned. That needs part 2c, because a fresh
foreign cache cannot be built out of payments either.
