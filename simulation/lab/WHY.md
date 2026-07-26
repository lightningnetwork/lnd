# WHY — mechanism by mechanism, how the evolved routers differ from lnd

The scores are settled and they live in the experiment writeups. This
document is about the machinery underneath them: for each decision an
lnd sender makes, what does the production code do, what did evolution
do instead, and which measurement tells us the difference mattered.

Read it next to five files: `routing/pathfind.go`,
`routing/missioncontrol.go`, `routing/probability_apriori.go`,
`routing/probability_bimodal.go`, and
`simulation/champions/router_mx3_generalist_v1.go`.

The one-sentence answer, before the detail:

> lnd asks "how likely is *this* amount over *this* pair?" once per
> edge, with the amount already fixed by the caller. The evolved
> routers store a bound instead of a probability, so they can ask the
> inverted question — "what is the largest amount that still has
> hope?" — and they ask it before choosing what to send. Almost every
> measured difference falls out of that inversion.

That is a claim about interfaces and control flow, not about
information. It is deliberately good news for lnd: mission control
already stores the bound.

---

## 0. Three corrections to the record

Two docs passes before this one each found something the received
story got wrong. So did this one. These come first because they change
how you should read everything after them.

### 0.1 The bimodal prior was in the prompt

`simulation/champions/README.md` says of the bimodal prior: "Nobody
put it in the prompt." `exp-006-breakthrough.md` calls it "reinvented
by the LLM from failure feedback alone."
`simulation/champions/router_hb1_v1.md` says "Nobody told the
evolutionary search about it. It fell out of failure traces."

The harness says otherwise. `simulation/run_gepa_code.py`, in the
`BACKGROUND` block handed to every reflection call, under a heading
that reads *Environment truths worth exploiting*:

```
- Hidden liquidity is drawn mostly from a BIMODAL distribution: channel
  funds sit almost entirely on one side. A 50/50 assumption is usually
  wrong; a failure at amount a on a channel is strong evidence the whole
  channel is depleted in that direction, and a success means most capacity
  is available.
```

That bullet is present in the earliest committed version of the file
(`f7ad893bd`), and the parameter-mode prompt (`run_gepa.py`) states the
same fact. Git cannot prove what the prompt said during `code_hard1`
itself — that commit's contents already reference hb1 and mx_c3 by
name, so the file was committed after the run it describes — but the
bullet sits under *environment truths*, not under the later *insights
from prior successful runs* section, which is where the deliberately
transferred discoveries live. The most likely reading is that the
bimodal hypothesis was stated to the proposer from the beginning.

What survives: the *shape* of the prior (exponential low mode plus a
logistic cliff, written directly as a probability rather than derived
by integration), the constants, and the interval machinery built on top
were not in the prompt in exp-006. What does not survive is
"rediscovered from failure traces alone." Downgrade the claim to: told
that liquidity is bimodal, evolution produced a calibrated bimodal
prior and then went well past it.

### 0.2 The evolved prior fits the simulator's generator, not the network

`routing/sim_liquidity.go` generates hidden balances like this:

```go
case LiquidityBimodal:
        frac := rng.ExpFloat64() * 0.05
```

An exponential with mean 5% of capacity, hugging one randomly chosen
end. Now line the evolved low modes up against that constant:

| router | low-mode scale | source |
|---|---|---|
| simulator generator | **0.050** of capacity | `sim_liquidity.go` |
| atomic1 | 0.055 | `exp-010b-atomic1-best-candidate.go` |
| hb1 | 0.025 | `router_hb1_v1.go` |
| mx_c3 | 0.018 | `router_mx3_generalist_v1.go` |

atomic1 lands within 10% of the generative constant. hb1 and mx_c3 sit
tighter than it, which is what you would expect from routers bred on
the `--hard` corpus, where amounts are large relative to capacity and
over-pessimism is cheap.

This is not cheating and it is not a bug. It is what fitting a
generative model looks like when you can see the samples. But it does
constrain the claim: the champions learned *this simulator's liquidity
constant*, and whether 5% of capacity describes mainnet is an
assumption `sim_liquidity.go` makes without a citation. The exp-009
"mainnet validation" uses the real 12,161-node topology and the real
policies, and then **overwrites the balances** with this same
generator (half the files bimodal, half uniform, per
`gen_mainnet_scenarios.py`). The topology is real; the liquidity is
ours.

### 0.3 lnd's closest analogue had never been measured fairly

lnd ships a bimodal estimator. It is not the default
(`DefaultEstimator = AprioriEstimatorName`, `pathfind.go:62`), and the
NOTEBOOK already lists "no lnd+bimodal baseline arm" as an upstream red
flag. The staged fix, `simulation/params_lnd_bimodal.json`, sets:

```json
"bimodal": { "scale_msat": 300000000, ... }
```

That is `DefaultBimodalScaleMsat`, an **absolute** 300,000 sat. Our
corpora use channels of 2M, 3M, 10M, 48M, 96M and 192M sat, and the
generator's scale is a **fraction** of whichever capacity the channel
has. So the staged baseline gives lnd a scale that is 10% of a 3M-sat
channel, 3% of a 10M-sat channel and 0.3% of a 96M-sat corridor, while
the truth is 5% everywhere. exp-002 noticed this in passing —
"Default 300M msat scale is wildly off for these nets" — filed it under
"hypothesis to check post-run," and the writeup still carries an unmet
*To fill in when complete* section.

So: exp-002's headline ("parameter tuning could not beat lnd's
defaults") was true of the search that was run, and that search did
reach for bimodal repeatedly and lose. But the one configuration that
would make lnd's own machinery match the environment — bimodal with
`scale_msat` set near 5% of the corpus's channel capacity — had never
been evaluated, so "the paradigm is the lever, not the knobs" rested on
an absence rather than a result.

That experiment has since run. exp-002b swept seven scales bracketing
the matched value by an order of magnitude either way, on two sealed
tiers, and it is the one correction of the three that a measurement
closed rather than deepened: no bimodal scale beats lnd's own shipping
apriori default, and the environment-matched scale is among the worse
settings rather than the best. The full table and, more usefully, the
mechanism behind the failure are in §1, because what exp-002b measures
is precisely this document's central thesis. What survives as a
correction is the citation habit: every sentence in this repo that
leaned on exp-002 for "the paradigm is the lever" was, until 2026-07-26,
leaning on a search that had never handed lnd's own bimodal hypothesis
the scale this environment calls for.

---

## 1. The probability model

### lnd, default path: a step function with node-level contagion

`AprioriEstimator.calculateProbability` is fifteen lines and worth
reading in full:

```go
lastPairResult, ok := results[toNode]
if !ok {
        return nodeProbability
}
if amt <= lastPairResult.SuccessAmt {
        return p.prevSuccessProbability          // 0.95
}
if lastPairResult.FailTime.IsZero() || amt < lastPairResult.FailAmt {
        return nodeProbability
}
weight := p.getWeight(now.Sub(lastPairResult.FailTime))
return nodeProbability * (1 - weight)
```

Three regions, two of them constant. Below `SuccessAmt`: 0.95. At or
above `FailAmt`: `nodeProbability * (1 - 2^(-age/halflife))`, which is
**exactly zero** when the failure is fresh. Everywhere in between: the
node probability, a single number that does not vary with the amount at
all.

`nodeProbability` is where the interesting arithmetic lives. With the
defaults (`AprioriWeight = 0.5` so `aprioriFactor = 1`,
`AprioriHopProbability = 0.6`, `capacityFactor ≈ 1` below ~95% of
capacity):

- no history for the node: 0.6
- one fresh failure: `0.6 / (1 + 1)` = **0.30**
- two fresh failures: `0.6 / 3` = **0.20**
- three: 0.15

Each fresh failure adds 1 to `totalWeight` and nothing to
`probabilitiesTotal`. That is the node-level contagion, and it is
deliberate: the comment in `getNodeProbability` says so, "this is the
part that incentivizes nodes to make sure that all (not just some) of
their channels are in good shape."

### lnd, non-default path: a real posterior over the interval

`BimodalEstimator.probabilityFormula` integrates the two-exponential
density `P(x) ~ exp(-x/s) + exp((x-c)/s) + 1/c` and renormalizes:

```go
prob := p.integral(capacity, amount, failAmount)
reNorm := p.integral(capacity, successAmount, failAmount)
prob /= reNorm
```

This is a graded interpolation across `[successAmount, failAmount]`,
derived from the Pickhardt formalism, with hard endpoints handled
above it (`amount >= failAmount → 0`, `amount <= successAmount → 1`).
Structurally it is the same object the champions evolved. It is not the
default, and until exp-002b nobody had run it with a scale matched to
this environment; the control is at the end of this section.

### The evolved model

mx_c3's prior:

```go
ratio := float64(amt) / float64(edge.capacity)
lowSide := 0.495 * math.Exp(-ratio/0.018)
highSide := 0.495 / (1 + math.Exp((ratio-0.965)/0.018))
probability := 0.005 + lowSide + highSide
```

and then a branch table over the stored interval, of which the
interesting rung is:

```go
case state.upperFail != 0:
        position := (float64(amt) - lower) / math.Max(upper-lower, 1)
        probability = 0.01 + 0.94*math.Pow(1-position, 2.8)
        probability = 0.90*probability + 0.10*prior
```

Same three regions as apriori — certain below `lowerOK`, zero at or
above `upperFail` — but the middle is a curve in the amount rather than
a constant, and the curve is anchored at both ends of the *observed*
interval rather than at the channel's capacity.

### So what does the interval actually add?

Not information. lnd's bimodal estimator can answer the same questions,
and even apriori distinguishes "below the failure amount" from "at or
above" it. The difference is in what the answer is *shaped for*.

Compare the two retry stories after a channel refuses amount `a`:

| next amount | lnd apriori | mx_c3 (`candidateLowerRetryFactor`) |
|---|---|---|
| 0.76·a | 0.30 | ×0.004 |
| 0.50·a | 0.30 | ×0.018 |
| 0.10·a | 0.30 | ×0.075 |
| 0.02·a | 0.30 | ×0.30 |
| 0.005·a | 0.30 | ×0.88 |

apriori prices every amount below the failure identically, so it
carries no signal about *which* smaller amount to try. And that turns
out not to matter to lnd, because lnd never asks. `findPath` takes
`amt` as an argument and holds it fixed for the entire Dijkstra;
`paymentSession.RequestRoute` changes the amount only in one place,
after pathfinding has failed outright:

```go
case err == errNoPathFound:
        ...
        // This is where the magic happens. If we can't find a
        // route, try it for half the amount.
        maxAmt /= 2
```

So lnd's retry policy is **same amount, different route**. The
champions' is **different amount, informed route**: `upperFail` is read
directly to build the shard ladder, `(failedAt - 1) / {2,4,8,16,32}`,
before anything is sent.

That is the causal chain behind the headline number. On the 12,161-node
mainnet snapshot there is almost always another route, so lnd's
"different route" branch never terminates early and never reaches the
halving fallback: 19.8 attempts per payment at 0.790 success. mx_c3
reads its bounds, resizes, and settles at 0.810 success in 2.3
attempts. 8.6× (exp-009). On the small hard corpora, where alternative
routes run out, the ratio compresses to 4–7× (exp-006: 45.5 versus 9.3
on the hard sealed test) because lnd's search fails fast instead of
grinding.

### The control: hand lnd a better prior and watch nothing improve

If the probability model were the lever, then lnd's own bimodal
estimator should close most of the gap once its scale matches the
environment. It encodes the same hypothesis the champions exploit, and
it derives the posterior rather than fitting one. exp-002b ran that
control at seven scales, on two sealed tiers, changing nothing but
`--params`.

| hard tier | objective | success | attempts |
|---|---|---|---|
| lnd apriori (ships) | **0.298** | 0.421 | 30.9 |
| bimodal 100M msat (matches the generator) | 0.261 | 0.456 | 78.7 |
| bimodal 1000M msat (best of the seven) | 0.283 | 0.478 | 77.2 |
| **mx_c3** | **0.479** | 0.592 | 8.1 |

Every row shares one corpus, regenerated after a reboot, so the levels
sit a little below the hard-tier numbers quoted elsewhere in this repo.
The OOD tier orders identically: apriori 0.357, best bimodal 0.345,
matched 150M 0.330, mx_c3 0.581.

No scale beats the shipping default, and the scale that matches the
generator is among the worse ones rather than the best. But read the
success and the attempt columns together, because that is where the
mechanism shows. Bimodal *raises* lnd's success rate, 0.421 → 0.478, and
it more than doubles lnd's attempts, 30.9 → 77. A better liquidity prior
makes lnd more willing to keep trying, and it is right to be: some route
really might still work. What the prior cannot do is change *what* lnd
tries next, because `findPath` takes the amount as a fixed argument. So
lnd buys extra success at a price the objective charges it for, and the
whole estimator swap moves the objective by at most 0.02 in either
direction while the paradigm difference is worth 0.18 to 0.22.

That is this section's thesis, measured rather than argued: a better
prior improves route ranking inside a broken retry strategy, and it
cannot supply the missing one.

The upstream-shaped observation: mission control already stores
`FailAmt` per pair and already exposes it
(`GetPairHistorySnapshot`). Nothing in lnd reads it to choose a shard
size. That is a small change, not an estimator replacement.

---

## 2. What is remembered, and keyed by what

### lnd

```go
type TimedPairResult struct {
        FailTime    time.Time
        FailAmt     lnwire.MilliSatoshi
        SuccessTime time.Time
        SuccessAmt  lnwire.MilliSatoshi
}
```

stored as `map[route.Vertex]NodeResults`, that is, keyed by
`(fromNode, toNode)`. Capped at `DefaultMaxMcHistory = 1000` results on
disk, persisted, reloaded on startup.

### The champions

```go
type candidateEdgeKey struct {
        chanID uint64
        from   route.Vertex
        to     route.Vertex
}
```

in a package-level map that is never evicted and never persisted.

Four consequences, in increasing order of how much they matter.

**Parallel channels.** Two channels between the same pair share one
belief in lnd and get separate beliefs in the champions. In the
simulator there are no parallel channels, so this costs the champions
nothing and buys them nothing. On mainnet it cuts both ways: separate
beliefs are more precise, but non-strict forwarding means the peer may
forward over a *different* channel than the one you named, so a
directed-channel bound can be attributed to the wrong channel. See §7.

**Contagion.** lnd spreads failures deliberately. `failNode` marks the
node's incoming and outgoing pairs failed **in both directions** at
amount zero, which is amount-independent, and `getNodeProbability`
drags every untried channel of the node down as computed in §1.
`failPair` likewise fails both directions at amount zero for most error
codes. Only the liquidity path is surgical:

```go
case *lnwire.FailTemporaryChannelFailure:
        reportOutgoingBalance()   // failPairBalance: one direction, real amount
```

The champions have no node-level state at all. Every penalty is per
directed channel, and the unattributable case is handled by
elimination (`recordAnonymousFailure`) rather than by blaming a range.

**Reverse-direction inference.** This one lnd genuinely does not have.
mx_c3, on every observation:

- a probe that forwards `amt` raises forward `lowerOK` to `amt` and
  drops the *reverse* `upperFail` to `capacity - amt + 1`, because
  liquidity on this side cannot also be on the other;
- a failure at `amt` drops forward `upperFail` and raises reverse
  `lowerOK` to `capacity - amt + 1`, recording a *success* on the
  reverse direction;
- a settlement shifts the forward interval down by the settled amount
  and the reverse interval up.

```go
forward.estimate = preEstimate - amt
if forward.lowerOK > amt { forward.lowerOK -= amt } else { forward.lowerOK = 0 }
if forward.upperFail > amt { forward.upperFail -= amt } else { forward.upperFail = 0 }
```

lnd's success path raises `SuccessAmt` and, if a success lands inside
the failure range, sets `FailAmt = successAmt + 1`. It never debits the
direction it just used, and the reverse pair is a separate key it never
touches. So after settling a shard, lnd still believes the channel can
carry what it just carried, and the champions know it cannot.

**Vantage — and the measurement that inverted the argument.** The
reasoning used to run like this. Mission control's history is a record
of *my* payments: which pairs I happened to route through, at which
amounts, in which order. A per-directed-channel bound is a claim about
the channel that any observer standing anywhere would have recorded
identically at the same instant. So bounds should transfer between
nodes and pair history should not.

exp-012 part 4 priced it. Eighteen mainnet files, an identical
25-payment warmup, an identical liquidity restore afterwards so both
arms are equally stale, and one variable: who sent the warmup payments,
the scoring node itself or a degree-31 stranger.

| router | self-vantage | foreign-vantage | Δ (foreign − self) |
|---|---|---|---|
| lnd | 0.155 (3.0 att) | **0.176 (0.8 att)** | **+0.021** |
| mx_c3 | 0.174 (0.4) | 0.169 (0.5) | −0.005 |
| atomic1 | 0.209 (0.6) | 0.209 (0.6) | 0.000 |

Nobody is hurt by a stranger's observations, and lnd is *helped*. Half
the old argument survives, and it is the half about the champions:
atomic1's two arms agree to three decimals and mx_c3's sit within noise,
which is how vantage-free facts about channels ought to behave. The half
that was wrong is the half about mission control. A failure at a remote
relay records the pair `(relay, target)`, which names no observer at
all, so most of mission control transfers perfectly well.

The genuinely entangled remainder is narrow and specific: the pairs that
cross the consumer's *own* local channels. Every payment it sends must
traverse one of them. Warming from its own vantage therefore fills
exactly those pairs with stale zeros that no clock will decay away on
this tier, and lnd's attempt count triples, 0.8 → 3.0, as it thrashes
around its own poisoned first hop. A stranger's warmup cannot reach
those pairs, so it teaches the transferable part and leaves the critical
part clean.

The rule for a weight-serving API is therefore narrower and sharper than
"mission control does not transfer": **serve remote-pair observations,
and never import observations about the consumer's own local channels.**
Those are the ones a node can measure cheaply for itself, the ones whose
staleness does the most damage, and the only genuinely vantage-bound
part of the structure.

One caveat on the levels. Both arms are stale by construction — the
restore control is what makes the imported knowledge describe a churned
network — so the absolute objectives are low, 0.15 to 0.21, and the
paired deltas are the result. Fresh third-party knowledge remains
unmeasured.

---

## 3. Time

This is the thread the program spent the most effort on, and lnd
deserves the fair version of its own argument first.

### Why lnd decays

Two independent decays, for two different reasons.

`AprioriEstimator` fades the *penalty*: `getWeight(age) = 2^(-age/H)`
with `DefaultPenaltyHalfLife = 1 hour`, so a channel that failed
recovers to the node probability on a clock. `BimodalEstimator` fades
the *evidence*: `canSend` shrinks `SuccessAmt` toward zero and
`cannotSend` grows `FailAmt` toward capacity, both over
`DefaultBimodalDecayTime = 7 days`.

The rationale is sound and mostly about a regime our corpora do not
contain. A production node keeps mission control on disk, reloads it on
startup, and may act on a bound recorded a week ago after a restart, a
reorg, or a peer's rebalance run. Without decay, one bad afternoon
would permanently prune corridors. Decay is also a liveness property:
it guarantees that any channel eventually becomes reroutable, which
matters when the failure was caused by something transient (an offline
peer, a full HTLC slot table) or by an adversary who wants to be
blacklisted out of your route set.

### What evolved

Grep the champions for `time.`: zero hits in hb1, mx_c3 and atomic1
alike. No half-life, no clock read, no `view.Now()`.

exp-008 tested whether that was a simulator artifact by giving the
simulator a virtual clock and exogenous background traffic, then
re-running evolution on the drifting corpus. Time-awareness did come
back, in a form structurally unlike lnd's: `drift1` decays *confidence
in evidence*, never penalties. Confidence half-life 35 minutes, hard
bounds expiring outright at 20 minutes, and edge probability
interpolated as `conf·learned + (1-conf)·prior`, so aging evidence
slides back toward the bimodal prior rather than toward optimism.

And it lost, on drift, to routers with no clock:

| tier | lnd | seed | hb1 | mx_c3 | gen2 | drift1 |
|---|---|---|---|---|---|---|
| drift-test | 0.203 | 0.377 | 0.455 | **0.457** | 0.456 | 0.417 |

The sharpest cut in that table is gen2 versus drift1. Same seed style,
same 400-eval budget, and gen2 never saw drift during evolution — yet
the static-bred, time-less router beats the drift-bred, time-aware one
on the drift corpus itself. The mechanism is cheap to state: under this
churn a stale bound costs about one retry to refresh, and decay throws
away more than one retry's worth of good evidence to avoid it.

### atomic1's answer: scope, not clock

The third answer in the family, and the one worth stealing. atomic1
keeps two kinds of failure record with different severities and
different lifetimes, and neither of them reads a clock.

Persisted across payments, soft:

```go
if belief.upperFail > 0 && total >= belief.upperFail {
        if p > 0.012 { p = 0.012 }
        return p * retryScale
}
```

Scoped to this payment, savage:

```go
if failure.count >= 2 { return 0 }
if failure.upper > 0 {
        if total >= failure.upper { return 0 }
        retryCeiling := failure.upper * 2 / 3
        if total > retryCeiling { return 0 }
        retryScale = 0.35
}
```

Two strikes on a directed channel kills it for the rest of the payment;
across payments the same bound is a 0.012 ceiling rather than a zero.
Fresh evidence is certainty, old evidence is a strong prior, and the
belief's *lifetime* varies rather than its weight. mx_c3 returns a hard
zero in both cases; drift1 computed a weight from a half-life. Three
lineages, three answers, and the scope-split is the only one that
generalizes without a collapse tier.

### The stale-cache arm, which decided between the three answers

exp-012 then paid the scope-split a compliment nobody bred it for. Warm
a router with unscored payments, restore the network's liquidity, and
score it: the router now holds beliefs about a state that no longer
exists. That is a maximally stale cache, and it is the worst case for
any weight-serving API, because a served cache is stale by construction.

| mainnet tier | lnd | hb1 | mx_c3 | atomic1 |
|---|---|---|---|---|
| cold | 0.694 (19.8 att) | 0.790 (2.3) | 0.791 (2.3) | 0.790 (1.6) |
| 100 stale | 0.377 (26.9) | 0.550 (1.4) | 0.550 (1.2) | **0.783 (2.2)** |
| 400 stale | 0.228 (32.4) | 0.347 (0.6) | 0.347 (0.6) | **0.775 (2.0)** |

The field splits three ways, and the attempts column names each
mechanism.

lnd **thrashes**. Attempts climb 19.8 → 32.4 while success falls 0.79 →
0.35. Its pair entries are permanent zeros on this tier — the honest
caveat below, in its most expensive form — so a stale blacklist keeps
steering it onto fresh-looking routes that are no better, and it never
gives up.

The champions **abandon**. They fall to 0.6 attempts per payment at 36%
success, quitting almost before they start, because a hard `upperFail`
zero turns a stale bound into a dead channel and enough dead channels
make a live payment look hopeless.

atomic1 **shrugs**: 0.790 → 0.775, a 2% loss against the champions' 56%
and lnd's 67%, at roughly two attempts throughout. Its persisted bounds
clamp to a 0.012 probability floor rather than zero, so stale evidence
makes a channel unattractive instead of forbidden, and one retry is
enough to correct it.

Paired against mx_c3, atomic1 is +0.233 (p=.002) at 100 warmup payments
and +0.428 (p=.002) at 400 — the first statistically significant win
over a champion in the program's history. It does not move the champions
of record, which are decided on the standing tiers and not on this one.
What it does is locate the axis where mx_c3's hard zero is a genuine
liability, and it hands the program its most directly upstream-shaped
result: the consumer's staleness policy dominates the value of any
imported knowledge, and the safe policy is a floor rather than a zero
(§9).

### The honest caveat, which matters

lnd's decay constants are tuned for a node that runs for weeks. Our
corpora are ten payments long. Worse, on every static tier the
simulator runs mission control on the **wall clock**:

> `SimClockParams` configures the virtual clock. Without one the
> simulation runs on the wall clock, where a whole batch finishes in
> well under any [half-life].

A ten-payment mainnet batch completes in microseconds, so `age ≈ 0`,
so `getWeight(age) = 1`, so `nodeProbability * (1 - weight) = 0`. On
the hard, OOD and mainnet tiers, **lnd's decay never fires at all** and
mission control behaves as a monotone within-batch blacklist. exp-006
verified this is deterministic (five runs, stdev 0.00000) but
determinism is not the same as fidelity.

The consequence for our own prose: every sentence in this repo that
contrasts "lnd decays, the champions do not" is describing a mechanism
that three of our five tiers never exercised. The only tier where lnd's
half-lives genuinely operate is exp-008's drift corpus, and there lnd
scores 0.203 against mx_c3's 0.457 — which is the honest place to make
the argument, and it is a stronger result than the static tiers, not a
weaker one.

### And the drift tier is weaker than it looks

exp-012 part 3 varied one thing: the idle gap between warming a router
and scoring it, at 0, 10 minutes, 1 hour and 6 virtual hours of
background traffic. Nothing moved, to three decimal places, for any
router.

The manipulation check passes, so this is not a broken knob. Background
payments sent scale 700 → 1420 across the four arms, exactly the
prorated volume `AdvanceIdle` promises. The traffic runs; the world just
does not move. And the reason it does not move is a simulator defect
worth more than the null: **only about 18% of background payments
settle**, 129 of 700 in the check. The engine sends naive fee-optimizing
payments that mostly fail, and a failed payment moves no liquidity, so
our exogenous process is roughly five times weaker than its
configuration implies.

That reaches backwards into this section. exp-008 concluded that decay
"buys nothing at realistic churn"; the honest restatement is that it
buys nothing at the weak churn we generate, and that the drift corpus
never reached a regime where evidence genuinely goes stale. exp-010b's
per-attempt drift comes from the same engine and inherits the same
caveat. The measured comparison between drift1 and the time-less routers
stands — same corpus, same budget — but what it licenses is narrower
than the sentence we have been quoting.

One arm did reach genuine staleness, and it got there by restoring
liquidity rather than by churning it. There, as the stale-cache table
above shows, the aging policy mattered enormously. That is the clearest
hint we have about what a fixed traffic engine would find, and it is a
reason to fix it (§8) before making any staleness claim again.

---

## 4. How a route is chosen

### lnd

Weight is money plus a time-lock tax:

```go
timeLockPenalty := int64(lockedAmt) * int64(timeLockDelta) *
        RiskFactorBillionths / 1000000000
return int64(fee) + timeLockPenalty
```

with `RiskFactorBillionths = 15`. Probability enters once, at the end,
as a reciprocal:

```go
dist := float64(weight) + penalty/probability
```

The derivation above `getProbabilityBasedDist` is exact and worth
respecting: if you will try route A and then route B, `F + c/P` is the
correct ordering key, with `c` the virtual cost of a failed attempt
(`DefaultAttemptCost` 100 sat plus `DefaultAttemptCostPPM` 1000 ppm,
scaled by the time preference). lnd is not maximizing the success
probability of one attempt. It is minimizing the expected total cost of
a *sequence* of attempts, in millisatoshis, and it is right to do so.

Partial paths below `MinProbability = 0.01` are pruned, and each node
keeps exactly one best distance.

### The champions

mx_c3's per-edge cost:

```go
edgeRisk := -math.Log(probability)
feePenalty := 5.0 * float64(fee) / math.Max(float64(deliver), 1)
hopPenalty := 0.045 + 0.003*float64(item.hops)
capacityPenalty := 0.30 * x * x        // x = (ratio-0.70)/0.30, ratio > 0.70
```

Four contrasts, each with a reason:

- **`-log P` instead of `c/P`.** Additive over hops, so minimizing path
  cost maximizes the *product* of hop probabilities directly, and the
  risk term composes inside a relaxation. It is also far gentler: a 1%
  route costs `100c` under lnd's key and `4.6` under the champions'.
  The champions recover the missing brutality with a route-level
  threshold in `RequestRoute` rather than a per-edge floor.
- **Proportional fees.** lnd's weight is absolute millisatoshis; the
  champions normalize by the delivered amount, so fee sensitivity is
  scale-free across a corpus that spans 2M-sat and 192M-sat channels.
- **A hop penalty that grows with depth** (`0.045 + 0.003·hops`). lnd
  discourages long routes only indirectly, through accumulated fees,
  the CLTV risk factor and `MinProbability` multiplying down.
- **A capacity ramp starting at 70% utilization**, quadratic. lnd has
  the same instinct in `capacityFactor`, but expressed as a probability
  multiplier and centered at `CapacityFraction = 0.9999` of capacity,
  where it barely bites: at 90% of capacity the factor is 0.99.

And one structural difference. lnd keeps one best distance per node;
mx_c3 keeps up to 24 Pareto-incomparable labels, dominated only on all
three of score, amount and hop count at once:

```go
if old.active &&
        old.score <= label.score+1e-12 &&
        old.amount <= label.amount &&
        old.hops <= label.hops {
        return false
}
```

The reason is specific and correct: search runs backwards from the
target, so the required amount *grows* along the path as fees accrue. A
route that is cheaper but carries more money is genuinely not
comparable, because the larger amount may be refused further upstream.
A single-distance Dijkstra cannot represent that trade-off. The price
is search cost: 24 labels per node and 120,000 expansions per shard,
against lnd's one distance per node.

Finally, the champions carry **no CLTV term at all**. They track
`timeLockDelta` only to build a valid route. In a simulator that
charges nothing for locked-up capital that is free to discard, and
evolution discarded it. On a real node it is not free, and this is one
of the clearest places where an evolved cost function should not be
ported verbatim.

---

## 5. MPP splitting

### lnd: halving, as a fallback

The whole splitting policy is the `errNoPathFound` branch of
`paymentSession.RequestRoute` quoted in §1: halve `maxAmt`, floor at
`DefaultShardMinAmt = 10,000 sat`, stop at `MaxParts` (16 by default
through `routerrpc`), require an MPP or AMP feature bit on the
destination. The trigger is indirect: pathfinding must fail entirely,
which happens when every partial path falls under `MinProbability`.
lnd cannot split before it has failed to find a path, and every shard
size is a power-of-two fraction of the original amount.

### mx_c3: a priced ladder with evidence rungs

`candidateShardAmounts` unions four sources: the ceil-division ladder,
the halving chain, small multiples of the minimum, and the substantive
one — for every per-channel failure bound this payment has discovered,
`(failedAt - 1) / {2, 4, 8, 16, 32}`. Shard sizes fitted just under
amounts already proven not to fit. Every rung gets a full route search
and a utility score, with an appetite weight that reads the payment's
own history:

```go
progressWeight := 0.72
switch {
case r.successfulParts > 0: progressWeight = 0.94   // committed, finish it
case r.failedAttempts >= 3: progressWeight = 0.50   // struggling, split smaller
case r.failedAttempts > 0:  progressWeight = 0.60
}
```

So mx_c3 can split on the first attempt before any failure, can pick a
shard that is no power-of-two fraction of anything, and can size a
shard against a bound it learned two attempts ago.

### atomic1: joint planning against a reservation ledger

atomic1 plans a whole shard set up front and prices contention into the
probability function, at the one place it cannot be routed around:

```go
reserved := r.reserved[edge.key]
total := amt + reserved

if !edge.policyAllows(amt) || total > edge.capacity {
        return 0
}
```

Every subsequent test in that function reads `total`, not `amt`. Note
the asymmetry on the first line: `policyAllows` tests `amt`, because
minHTLC and maxHTLC apply per HTLC, while capacity is tested against
the sum. That is the correct reading of the protocol.

The ledger doubles as a measurement instrument. On a successful hop:

```go
r.learnSuccess(key, amt+r.reserved[key])
```

A hop that forwarded 1M while already holding 3M of ours has proven it
can carry 4M, and `lowerOK` records 4M.

### What the measurements say

**Joint planning is elicitable and not decisive.** exp-010 built a
corridors corpus where unequal splitting is mandatory, and three
proposer lineages independently evolved joint route-set planners. The
deepest of them (opus1, 1,931 lines) posted the program's first
statistical tie with mx_c3 on the corpus it was bred for (split-val
+0.005, p=.07, higher raw success) and then scored 0.303 on the sealed
hard test against mx_c3's 0.583. exp-010b's docs pass traced that
collapse to a single overfit constant — `maxRouteHops = 7`, on a corpus
that needs 9-to-23-hop routes — not to the architecture. exp-010b then
bred atomic1, the first challenger with no collapse tier, and it still
lost the home tier by 0.044 at p=.07.

**lnd's halving was being subsidized by instant settlement.** This is
the sharpest finding in the splitting thread. Until exp-010b, a shard
that traversed successfully settled immediately: it moved the money,
released the liquidity, and reported back before the next route
request. Probe-learn-resize therefore got all of joint planning's
information at none of its cost. The atomic arena made shards *hold*
liquidity until the whole payment resolves, and reordered the field
before evolution ran:

| router | atomic-test obj | success | attempts/pmt |
|---|---|---|---|
| lnd stack | 0.338 | 0.500 | **104.8** |
| seed | 0.385 | 0.536 | 56.5 |
| hb1 | 0.444 | 0.554 | 10.7 |
| **mx_c3** | **0.444** | 0.571 | 12.6 |
| opus1 (joint planner) | 0.425 | 0.571 | 23.5 |

lnd goes from second place on the non-atomic corridors corpus (0.837)
to last (0.338) at 105 attempts per payment. The mechanism is direct:
lnd's halving ladder re-probes at the full amount over and over, and
under atomic MPP each successful traversal now *holds* the liquidity it
used, so lnd's own probes crowd out lnd's own retries along exactly the
corridors it needs. The champions hold their position, and the joint
planners close the gap to noise. Deeper joint planning already pays
under honest pricing; it just has not paid enough to win.

---

## 6. Cold start, and what a warm cache is worth

exp-012 measured the warmup curve: attempts at payment index *i*
relative to mx_c3 on the same payment, early in a batch versus late.

| router | mainnet first-3 | mainnet last-3 | hard first-3 | hard last-3 |
|---|---|---|---|---|
| lnd stack | 4.72× | **11.88×** | 5.10× | 4.42× |
| seed | 1.68× | 4.44× | 4.29× | 7.44× |
| hb1 | 0.91× | 0.99× | 1.06× | 1.34× |
| mx_c3 | 1.00× | 1.00× | 1.00× | 1.00× |
| **atomic1** | 0.72× | 0.58× | 1.56× | **0.73×** |
| opus1 | 2.08× | 3.35× | 1.33× | 1.52× |

Absolute means on mainnet: lnd 10.1 → 31.2 attempts per payment, mx_c3
2.4 → 2.6, atomic1 1.5 → 1.5.

**Why lnd's history cannot help in ten payments.** Mission control
learns only about pairs it has *tried*. The mainnet snapshot has 39,659
channels, so roughly 79,000 directed pairs. Ten payments at ~20
attempts and ~5 hops each is on the order of 1,000 pair observations,
about 1% coverage, concentrated on the corridors the first few payments
happened to walk. Untried pairs fall back to `nodeProbability`, and
`nodeProbability` is only informative for a node that has already been
tried — which is the same 1%. There is no mechanism in mission control
that generalizes an observation on one channel to a channel it has
never touched, except the node blend, which is the same mechanism that
spreads contamination.

**Why a prior beats history at small observation counts.** The
champions' prior is a function of `(amt, capacity)` and nothing else.
It is available for all 79,000 directed pairs at zero observations, and
it is right on average because it encodes the shape of the liquidity
distribution rather than any particular channel's balance. That is why
mx_c3 needs 2.4 attempts on its *first three* mainnet payments, before
it has learned anything: its advantage is a prior, not history. This
reframes roasbeef's hot-load proposal in a useful way. The thing worth
shipping to a fresh node may be the prior plus the interval machinery,
not a cache of someone else's observations.

**Why lnd gets worse rather than merely staying flat.** On mainnet its
ratio grows 4.7× → 11.9× and its absolute attempts triple. Two facts
constrain the explanation. First, on the hard corpus the same curve is
flat (5.10× → 4.42×). Second, per §3, the static tiers run on the wall
clock, so no penalty ever decays within a batch. The reading that fits
both: mission control accumulates within-batch blacklists that never
expire, and the consequence depends on how many alternatives the graph
offers. On mainnet, with 2,015 channels at the source, blacklisting one
corridor just sends the search down a worse one, so attempts pile up.
On the hard corpus alternatives run out, the payment fails early, and
the attempt count stops growing.

**That last paragraph is an inference, not a measurement.** The
decisive control costs almost nothing: re-run the mainnet warmup curve
with mission control reset between payments inside a file. If lnd's
attempts stop growing, accumulation is the cause. If they still grow,
the cause is liquidity depletion by lnd's own earlier payments and the
finding is about the corpus, not about mission control. Run it before
quoting the 11.9× at anyone.

**Only the memory-carrying hybrid learns within a batch.** atomic1
halves its ratio to the champion across the hard batch, 1.56× → 0.73×.
Both Opus-lineage routers are stateless across payments and show no
improvement. The lineage split found in the exp-010b docs pass is now
visible in the measurements.

### Warming a router does not help it

The rest of exp-012 stopped reasoning about the warm start and tried to
buy one, in four arms: warm with real payments and let the depletion
stand; warm and then restore the liquidity, which is the stale-cache
table in §3; warm from a stranger's vantage, which is the table in §2;
and warm with probes small enough to leave the network, and therefore
the knowledge, intact.

That last arm is the honest model of a probe-warmed node. A hundred
payments at 2% or 10% of the scored amounts teach channel structure
without materially draining anything, so what the router learns is still
true when it comes to use it.

| mainnet tier | lnd | mx_c3 | atomic1 |
|---|---|---|---|
| cold | 0.694 (19.8 att) | 0.791 (2.3) | 0.790 (1.6) |
| 100 probes @ 2% | 0.664 (19.5) | 0.768 (2.2) | 0.766 (1.9) |
| 100 probes @ 10% | 0.597 (15.9) | 0.653 (2.7) | 0.651 (2.6) |

Nobody gains, and the loss grows with the probe size. Exactly one number
in that table is a genuine warming signal, and it belongs to lnd: its
attempts fall 19.8 → 15.9 at 10% probes, the only place in the whole
experiment where knowledge visibly made a router cheaper. Its success
fell faster, so the objective still dropped. The champions' attempts
barely move at all, 2.3 → 2.7, because there was nothing there to warm.

Put the four arms together and the answer to the original question is a
clean negative. Across depletion, staleness, a foreign vantage and valid
probes, at 25, 100 and 400 observations, **no amount of warming lifts
any router above its cold-start score, and mission control never
approaches the champions.** Two mechanisms produce that negative and
they are not the same mechanism. The champions have nothing to learn:
they are within noise of their asymptote on payment one, so warming can
only subtract, by spending liquidity or by going stale. lnd cannot learn
fast enough for it to matter: 100 observations is roughly 1% pair
coverage on this graph, and what it records is a permanent zero.

**The design limit is as important as the result.** Every arm here buys
knowledge with payments, and payments cost liquidity, which makes free
knowledge unconstructible in the current simulator. The drain arm pays
in depletion, the restore arm pays in staleness, the probe arm pays a
little of both. A served cache in the actual proposal costs its consumer
nothing — it arrives over an API. Measuring that needs beliefs injected
from a file with no payments sent at all (`--import-weights`, §8). Until
that exists, exp-012's negative is a statement about probe-warming and
not about weight-serving.

---

## 7. What lnd does that the evolved routers do not

This is the credibility section. The candidates run in a sealed
simulator against a paradigm-free interface (`SimRouter`: two methods,
`RequestRoute` and `ReportAttempt`). Everything below is real lnd work
that no evolved router has ever had to do, with the measured advantage
that could shrink in each case.

**A truthful, instant, exactly attributed failure channel.** Every
failure in the simulator names its source hop and its code, arrives
before the next `RequestRoute` call, and is never a lie. Mainnet has
unattributable timeouts, held HTLCs, and peers with an incentive to
misreport. The Fable simulator advisor's framing stands: sealed gossip
restricts the candidates' *inputs*, but the feedback channel is
strictly *more* generous than mainnet — a precision paradise. The
champions' per-directed-channel bounds are the optimal exploitation of
a noiseless attribution channel, so **the 8.6× is an upper bound until
a degraded-attribution run exists.** atomic1 is more exposed than
mx_c3 here: its unattributable path does no elimination reasoning at
all, it just makes the route expensive and moves on.

**Non-strict forwarding and parallel channels.** The simulator has
neither, so `(chanID, from, to)` is an unambiguous key. On mainnet a
peer may forward over a different channel than the one you named, so a
bound recorded against one channel may describe another. lnd keys on
the node pair *for this reason* — the `minSecondChanceInterval` comment
says so explicitly. **This is the single strongest argument against the
directed-channel key**, and none of our measurements address it.

**Fee limits.** A real sender enforces `FeeLimit` as a hard constraint
inside pathfinding (`if totalFee > 0 && ... > r.FeeLimit { return }`).
The evolved routers carry fee only as a soft penalty term, tuned
against an objective that caps the fee penalty at 5,000 ppm. Under a
real fee limit some of the routes the champions choose would simply be
illegal, and their attempt economy would worsen by an unmeasured
amount.

**Channel reserve, commitment fees, HTLC slot limits, and
max_htlc_value_in_flight.** The simulator checks capacity and policy
(minHTLC, maxHTLC). It does not model the reserve, the commitment
transaction's fee, the 483-slot limit, or the in-flight value cap. All
four are real reasons a channel refuses an HTLC that a
liquidity-interval model would predict succeeds, and all four are
partly *amount-independent*, which is precisely the failure class the
interval cannot represent and lnd's amount-zero `failPair` can.

**Route blinding and trampoline.** `pathfind.go` and
`result_interpretation.go` carry substantial machinery for blinded
paths (introduction points, dummy hops, `FailInvalidBlinding`
attribution, payload sizing). None of it exists in the simulator. A
blinded path is a segment with no per-hop attribution at all, so it is
the degraded-attribution case in production form.

**Second chances for stale gossip.** lnd grants a policy failure one
retry per `minSecondChanceInterval` because a stale channel update is
usually the sender's fault, not the peer's. mx_c3 arrived at something
similar independently (`sessionBlocked`, expiring with the payment);
hb1's `candidateBlockEdge` is permanent and process-global, a real
hazard; atomic1 sets `policyBlocked` for the rest of the payment. None
of them re-quotes the policy and retries, which is the actually correct
behaviour and which lnd implements.

**Sphinx payload budget.** `findPath` rejects paths exceeding
`sphinx.MaxRoutingPayloadSize`. The champions' 24-hop routes and
atomic1's *unbounded* hop count would not all fit in a real onion.

**Persistence, restart and bounded memory.** Mission control flushes to
disk, reloads on startup, caps at 1,000 results, and is namespaced. The
champions' `candidateKnowledge` map is a package-level global, mutex
guarded, unbounded, never evicted, never persisted. Every result in
this program is a cold-start result by construction, which cuts both
ways: the champions earned the 8.6× with no more history than lnd had,
and until exp-012 we had never tested the regime where a node starts
with observations already in hand. exp-012 has now pushed that regime as
far as payments can push it, and more history helped nobody (§6). The
specific hazard of an unbounded store of permanent zeros, though, is no
longer hypothetical: it is the thrash in §3's stale-cache table.

**An adversary.** A permanent hard `upperFail` with no expiry is a
griefing target: a peer that wants to be removed from your route set
need only fail one HTLC. lnd's decay is, among other things, a defence
against that. Nothing in this program has an attacker in it — but
exp-012 measured the accidental version of the same attack, and it is
expensive. No adversary is required, only evidence that has gone stale,
and the champions give up 56% of their objective to it (§3).

**Everything else a payment does.** Inbound fees (lnd computes them and
clamps them at zero), AMP, time preference, the CLTV budget as a real
constraint, `OutgoingChannelIDs` and `LastHop` restrictions, bandwidth
hints from the live link rather than a per-payment snapshot, and the
HTLC switch's own view of what a channel can carry right now.

---

## 8. The measurements that would change my mind

Two items from the earlier version of this list have since run, and both
are folded into the sections above: lnd's bimodal estimator at a matched
scale, which confirmed §1 rather than threatening it, and third-party
weight transfer, which inverted §2's vantage argument. What is left, in
priority order:

1. **Fix the traffic engine, then re-ask every staleness question.**
   Only about 18% of background payments settle, so our churn is roughly
   five times weaker than configured (§3). Size the amounts to what the
   network can carry or let the traffic retry, aim a share of it at the
   corridors the scored payments use, and only then re-run the staleness
   sweep and exp-008's decay question underneath it. Everything this
   repo says about time is scoped by this number.
2. **`--import-weights`.** Inject beliefs from a file with no payments
   sent. Every exp-012 arm buys knowledge with liquidity, so the
   experiment cannot construct the one thing the proposed API actually
   delivers: knowledge that costs its consumer nothing (§6).
3. **Draw mainnet liquidity from something we did not write.** The
   evolved priors fit `sim_liquidity.go`'s 5% constant, and the mainnet
   tier overwrites real balances with that same generator, so the
   headline number is real topology and real policies over synthetic
   liquidity (§0.2). This is the top pre-upstream fix.
4. **Degraded attribution.** Delay, drop, or misattribute a fraction of
   failure sources. Both the champions' bounds and atomic1's
   suspicion-spreading are calibrated to a noiseless channel, and this
   is the advisor program's nominated decisive pre-upstream test (§7).

One cheap control also remains unrun: reset mission control between
payments on the mainnet warmup curve, which decides whether exp-012's
11.9× is accumulated blacklisting or plain liquidity depletion (§6).

---

## 9. What is actually portable

Stripping out everything that is simulator-shaped, five ideas survive
and are small enough to argue about upstream:

**Read the bound you already store.** Mission control knows `FailAmt`
for every pair this payment has touched. `paymentSession.RequestRoute`
halves a number instead. Deriving the next shard size from the observed
failure amounts — `(failedAt - 1) / k` — is a change to one function
and it is the mechanism behind most of the attempt reduction (§1, §5).

**Displace the interval on settlement, and credit the reverse
direction.** A settled HTLC moves money. Debiting the forward interval
and crediting the reverse one is a few lines in `setLastPairResult`,
requires no new state, and is the thing that makes the *second* shard
of an MPP payment well-informed (§2).

**Put a floor under learned evidence; never store a hard zero.** This is
the most directly actionable finding the program has, and it is the one
that cost a champion a paired test. Under stale knowledge, mx_c3's hard
`upperFail` zero loses 56% of its objective by declaring live channels
dead, while atomic1's 0.012 floor loses 2% because one retry can
overturn a discouraged channel but nothing can overturn a forbidden one
(§3). lnd reaches the same trapdoor by a different route: a fresh
`FailAmt` makes `nodeProbability * (1 - weight)` exactly zero, and it
stays zero for as long as no clock advances. A floor is a clamp on an
existing estimator, not a new paradigm, and it is what makes aged or
imported evidence safe to hold at all.

**If you serve weights, serve remote pairs only.** A stranger's
observations help lnd and cost the champions nothing, because a failure
at a remote relay records `(relay, target)` and names no observer. What
does not transfer is the consumer's own local channels. Every payment it
sends crosses one of them, so importing stale beliefs about them poisons
the hop it can least afford to lose — which tripled lnd's attempt count
in the measurement (§2). The API rule falls straight out: serve and
import the remote pairs, and let each node measure its own first hop.

**Make the prior capacity-relative.** `DefaultBimodalScaleMsat` is an
absolute 300,000 sat, which means very different things on a 1M-sat
channel and a 16M-sat one. Every evolved prior expresses its scale as a
fraction of capacity, and the corpus that punished scale-dependent
tuning (`corpus-mix`, mixing small-channel and scale-free topologies)
is exactly what selected for it (§0.2, §1).

Everything else — the label-setting search, the 24-hop routes, the
dropped CLTV term, the permanent global blocks, the several dozen
constants selected by an objective rather than by argument — is a
specification of an idea, not a patch.

---

## See also

- `simulation/lab/NOTEBOOK.md` — the narrative arc, newest last.
- `simulation/champions/router_hb1_v1.md`,
  `router_mx3_generalist_v1.md` — the champions, walked through.
- `simulation/lab/experiments/exp-010b-atomic1-best-candidate.md` — the
  no-collapse hybrid and the two-timescale evidence design.
- `exp-002b-bimodal-knob.md` — lnd's own bimodal estimator at seven
  scales, and the §1 control it settles.
- `exp-012-cold-cache.md` — the warmup curves, the stale-cache split,
  the probe arm, the vantage sweep, and the churn defect they exposed.
- `exp-006` (breakthrough), `exp-008` (drift), `exp-009` (mainnet),
  `exp-010`/`exp-010b` (splitting pressure, atomic arena), `exp-011`
  (insight transfer).
- `routing/sim_router.go` — the `SimRouter` contract and what the
  sealed view does and does not expose.
