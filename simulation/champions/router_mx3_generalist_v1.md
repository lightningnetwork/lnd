# mx_c3 — the champion of record

`router_mx3_generalist_v1.go` (1,525 lines) is the best generalist router
the project has produced and the winner on the real mainnet graph. Where
hb1 is a specialist tuned by a hard-regime corpus, mx_c3 was evolved
against a mixture of regimes and gives up almost nothing on either.

Read this document next to the source. Every constant quoted below appears
verbatim in the file.

## Provenance

| field | value |
|---|---|
| run | `code_mix1` (GEPA code mode, reflection LM `codex:gpt-5.6-sol`) |
| seed program | **hb1**, passed in with `--seed-file`, so this is a direct continuation of the hb1 lineage |
| training corpus | `corpus-mix`: hard bimodal small-channel scenarios plus corpus-v2 scale-free scenarios, split 48/20/20 |
| budget | 500 evaluations, 462 consumed, 17 accepted candidates, 6-member final frontier |
| siblings | mb1 (1,306 lines), mx_c4 (1,134), mx_c5 (1,107) |
| writeup | `simulation/lab/experiments/exp-007-mix-followup.md` |
| status | **champion of record**: best combined score, mainnet winner |

`code_mix1`'s `summary.json` reported `best_score` 0.9962. Ignore that
number; it is a per-minibatch training metric and it is inflated. mx_c3 was
selected by held-out three-way validation against lnd and the seed, the
same way every other champion was.

## Validated scores

Objective = `success − 0.01·min(extra_attempts, 15) − 0.00002·min(fee_ppm, 5000)`.

| tier | lnd stack | seed | hb1 | **mx_c3** |
|---|---|---|---|---|
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.583 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | **0.581** |
| mainnet, 12,161 nodes | 0.694 | 0.762 | 0.790 | **0.791** |
| average of the three | 0.453 | 0.593 | 0.640 | **0.652** |
| drift test (exp-008) | 0.203 | 0.377 | 0.455 | **0.457** |

mx_c3 costs 0.003 objective on the hard test and buys 0.036 on the
out-of-distribution test, which is why it is the champion of record rather
than hb1. On the mainnet snapshot it reaches 0.810 success at **2.3
attempts per payment**, against lnd's 0.790 at 19.8. exp-010's paired
sweep put statistics on the hb1 comparison: on mainnet the two are
genuinely indistinguishable, paired delta −0.000 [−0.003, +0.002].

### The closest anything has come: exp-010

exp-010 bred routers against a corridors corpus built to make unequal
splitting mandatory — exactly the regime where mx_c3's evidence-derived
shard ladder should be at its weakest — and it produced the first candidate
in the program's history to catch mx_c3 on any tier. The winner of the
Opus-5-default arm, 1,931 lines of persistent parallel flow plans,
concurrency-first dispatch, and residual-aware shard budgeting, scored
0.839 on split-val against mx_c3's 0.835: a paired delta of +0.005 at
p=.07, with a higher raw success rate (0.958 versus 0.917). On the corpus
it was bred for, the gap to the champion closed to noise.

Generalization is what protects mx_c3. That same challenger scores 0.303 on
the sealed hard test against mx_c3's 0.583, because its corridor-tuned
adaptive fail budget gives up after roughly seven attempts where mx_c3
spends 10.8 and succeeds at 2.4× the rate. Two other proposer lineages in
the same experiment — codex/gpt-5.6-sol with one-step lookahead, Opus 5 at
medium effort with up-front corridor-sized shard sets — evolved shallower
joint planners and lost to mx_c3 on all five tiers. The honest reading is
that mx_c3's splitting is beatable on a corpus designed to punish it, and
that nothing yet beats it everywhere at once. Detail in
`simulation/lab/experiments/exp-010-splitting-pressure.md`.

exp-010b then produced a challenger that shares the title from the other
side. Bred on an atomic-commitment arena built expressly to tax mx_c3's
reactive ladder, the codex arm's `atomic1` is the first candidate the program
has measured with no collapse tier at all: statistically indistinguishable
from mx_c3 on the sealed hard test (0.417 vs 0.479, p=.75), on the OOD corpus
(0.544 vs 0.581, p=.75), and on mainnet (0.790 vs 0.791), where it settles
payments at 1.6 attempts each against the champion's 2.3 — the best attempt
economy on record here. What still separates them is the tier that arena was
built to decide: mx_c3 wins the held-out atomic test 0.444 to 0.400 at p=.07,
and holds every other tier as well, so the champion's margin is now a
home-tier edge plus a fifth straight survival rather than a challenger's
off-corpus cliff. Detail in
`simulation/lab/experiments/exp-010b-atomic-splitting.md` and
`exp-010b-atomic1-best-candidate.md`.

## Running it

```bash
cd $LND_REPO
cat > /tmp/overlay.json <<EOF
{"Replace": {"$PWD/cmd/routesim/candidate_impl.go":
             "$PWD/simulation/champions/router_mx3_generalist_v1.go"}}
EOF
go build -overlay /tmp/overlay.json -o /tmp/routesim_mx3 ./cmd/routesim
/tmp/routesim_mx3 --scenarios /tmp/corpus/test/example_000.json \
    --router=candidate --traces=false
```

## Architecture

mx_c3 inherits hb1's skeleton — a per-payment router over a package-level
belief map, backward search from the target, a shard ladder priced by
utility — and rebuilds nearly every component. Start from the constants,
which announce the change of scale:

```go
candidateFinalCltvDelta = 40
candidateMaxRouteHops   = 24
candidateMaxLabels      = 24
candidateSearchLimit    = 120000
candidateAttemptLimit   = 80
```

### The belief state, with invariants and a mode latch

```go
type candidateLiquidityState struct {
        lowerOK   lnwire.MilliSatoshi
        upperFail lnwire.MilliSatoshi
        estimate  lnwire.MilliSatoshi
        confidence float64
        failures   uint32
        successes  uint32
        mode       int8
        known      bool
}
```

Two additions matter. First, `candidateNormalizeState` runs on every read
and every write and enforces the interval's invariants:
`0 <= lowerOK <= estimate < upperFail <= capacity`, dropping an `upperFail`
that contradicts `lowerOK` rather than clamping evidence away. hb1 had no
such reconciliation, and its failure handler could silently discard a
proven `lowerOK`.

Second, `mode` is a three-valued latch that names which side of the bimodal
distribution the channel appears to sit on:

```go
case state.estimate <= capacity/50:  state.mode = -1   // depleted
case state.estimate >= capacity*49/50: state.mode = 1  // rich
```

The model then branches on `mode` instead of treating every channel with
one formula. That is the single biggest structural difference from hb1: the
bimodal prior is no longer just a curve, it is a *classification* the router
commits to and then reasons inside.

`candidateStrongObservation` gates whether an observation is allowed to move
the latch at all: the amount must be at least `capacity/200`. A dust probe
proves almost nothing about a channel's mode, so it updates the bounds but
not the classification.

### Bidirectional bookkeeping, done properly

hb1 credited the reverse direction only on settlement. mx_c3 does it on
every observation, and it does it with the right sign:

- `candidateRecordProbe`: forward `lowerOK` rises to the forwarded amount;
  the **reverse** direction's `upperFail` drops to `capacity - amt + 1`,
  because liquidity sitting on this side cannot also be sitting on the other
  side. Strong observations additionally set forward `mode = 1` and reverse
  `mode = -1`, and set reverse `estimate = capacity - forward.estimate`.
- `candidateRecordFailure`: forward `upperFail` drops to the failing amount
  and, on a strong observation, `estimate` collapses to
  `min(amt/32, capacity/1000)`; the reverse direction's `lowerOK` rises to
  `capacity - amt + 1` and it records a *success*.
- `candidateRecordSettlement`: the forward interval shifts down by the
  settled amount, the reverse interval shifts up, and both confidences
  latch at 0.96.

A failure in one direction is therefore evidence of *available* liquidity in
the other. Nothing in lnd's mission control makes that inference.

### The probability model

The prior sharpens hb1's, with both modes compressed into 1.8% of capacity
and the cliff moved out to 96.5%:

```go
lowSide  := 0.495 * math.Exp(-ratio/0.018)
highSide := 0.495 / (1 + math.Exp((ratio-0.965)/0.018))
probability := 0.005 + lowSide + highSide  // clamped to [0.0005, 0.999]
```

`edgeProbability` is a branch table, ordered by how much the evidence
proves:

| condition | probability |
|---|---|
| session-blocked, or amount at/above a session failure | `0` |
| own channel with sufficient balance | `0.9995` |
| `lowerOK >= amt` | `0.9985` |
| `amt >= upperFail` | `0` |
| no history | prior |
| `mode < 0` | `candidateLowModeProbability` |
| `mode > 0 && estimate >= amt` | `0.975 + 0.022*conf + 0.002*min(margin*8,1)` |
| interval known (`upperFail != 0`) | `0.01 + 0.94*(1-position)^2.8`, then blended `0.90*p + 0.10*prior` |
| `estimate >= amt` | `0.90 + 0.075*conf + 0.02*min(margin*5,1)` |
| otherwise | `prior * 0.12 * exp(-over/0.035)` |

`position` is where the amount falls inside `[lowerOK, upperFail]`, so the
interval branch is a smooth interpolation between the two certainties rather
than hb1's ratio-to-`upperFail` heuristic. `candidateLowModeProbability`
handles the depleted mode with an exponential tail from `lowerOK` at scale
`0.018 * capacity`, truncated and renormalized at `upperFail` when one is
known.

Two multipliers then apply on top of whichever branch fired:

```go
probability *= retryFactor                              // see below
probability *= math.Exp(-0.70 * math.Min(penalty, 8))   // session penalty
```

Note `0.9995` rather than `1` for the router's own channels. That small
haircut means a local hop is not free, so the search prefers shorter routes
without needing a separate term. It is the kind of detail evolution finds
and a human would not bother to write.

### Retry at a lower amount, as a first-class mechanism

```go
func candidateLowerRetryFactor(amt, failedAt lnwire.MilliSatoshi) float64 {
        if amt >= failedAt { return 0 }
        ratio := float64(amt) / float64(failedAt)
        switch {
        case ratio > 0.75: return 0.004
        case ratio > 0.40: return 0.018
        case ratio > 0.15: return 0.075
        case ratio > 0.04: return 0.30
        case ratio > 0.01: return 0.62
        default:           return 0.88
        }
}
```

Read this as the answer to "the channel just refused X; what do I believe
about 0.3X?" It is a calibrated six-step ladder, and it replaces both hb1's
blunt session penalty and lnd's blacklist-then-decay. Retrying at 76% of a
failed amount is nearly hopeless (0.004); retrying at 1% of it is nearly
fine (0.88).

### Route search: label-setting, not Dijkstra

hb1 kept one best distance per node. mx_c3 keeps up to
`candidateMaxLabels = 24` **Pareto-incomparable labels** per node, where a
label dominates another only if it is no worse on all three of score,
amount, and hop count:

```go
if old.active &&
        old.score <= label.score+1e-12 &&
        old.amount <= label.amount &&
        old.hops <= label.hops {
        return false
}
```

This matters because the amount grows backwards along the path as fees
accrue, so a route that is cheaper in score but carries a larger amount is
genuinely not comparable: the larger amount may be refused further
upstream. A single-distance Dijkstra cannot express that. When a node
exceeds 24 labels, `candidateLabelRank` evicts the worst by
`score + 0.10*log(amountRatio) + 0.014*hops`.

The per-edge cost adds two terms hb1 lacked:

```go
edgeRisk := -math.Log(probability)
feePenalty := 5.0 * float64(fee) / math.Max(float64(deliver), 1)
hopPenalty := 0.045 + 0.003*float64(item.hops)
capacityPenalty := 0.0
if ratio > 0.70 {
        x := (ratio - 0.70) / 0.30
        capacityPenalty = 0.30 * x * x
}
```

The hop penalty *grows* with depth, so long routes get progressively more
expensive rather than paying a flat toll. The capacity penalty is a
quadratic ramp that starts at 70% channel utilization: it steers away from
channels the payment would nearly fill even when the probability model has
no evidence against them. lnd expresses the same instinct through
`capacityFactor`, but as a probability multiplier rather than a cost term,
and centered at 99.99% of capacity rather than 70%.

Search is bounded by `candidateSearchLimit = 120000` expansions and
`candidateMaxRouteHops = 24`, and `routeRejected` discards any rebuilt route
that this payment has already seen fail at this amount or higher, keyed by
the full route string.

### MPP splitting: the halving-plus ladder

`candidateShardAmounts` unions four sources:

1. the ceil-division ladder, `ceil(amt/parts)` for `parts = 2 .. min(partsLeft, 64)`;
2. the halving chain, `amt/2, amt/4, ...` down to the minimum;
3. **evidence-derived rungs**: for every per-channel session failure bound,
   `(failedAt - 1) / {2, 4, 8, 16, 32}` — shard sizes fitted just under
   amounts this payment has already proven do not fit;
4. small multiples of the minimum, `minimum * {2, 3, 4, 6, 8}`.

Source 3 is the substantive invention. hb1's ladder was a function of the
amount alone; mx_c3's ladder is a function of the amount *and the beliefs*,
so failures reshape the set of shard sizes it will even consider.

`RequestRoute` then prices every rung and maximizes utility, with a
split-appetite weight that responds to how the payment is going:

```go
progressWeight := 0.72
switch {
case r.successfulParts > 0:    progressWeight = 0.94  // committed, finish it
case r.failedAttempts >= 3:    progressWeight = 0.50  // struggling, split smaller
case r.failedAttempts > 0:     progressWeight = 0.60
}

utility := -risk + progressWeight*progress + completionBonus -
        feePenalty - hopPenalty
```

`completionBonus = 0.08` when the shard covers the whole remaining amount,
`feePenalty = 4.0*fee/shard`, `hopPenalty = 0.006*len(hops)`. Ties break
toward the larger shard. Unlike hb1, there is no probability threshold and
no early return: mx_c3 always evaluates the full ladder.

### Failure attribution

When the failure source identifies a hop, mx_c3 behaves much like hb1:
probe the prefix, record a liquidity failure at the failing hop, and decay
the session penalties on hops that proved themselves. Two additions:

- `shiftSessionLiquidity` moves the *session* bounds when a part settles, in
  both directions, mirroring what the global map does.
- a `TemporaryChannelFailure` on the router's **own** channel clamps
  `localBalances[chanID] = amount - 1`. The router treats its own balance
  view as fallible and corrects it, which matters because the simulator
  snapshots local balances once per payment.

When the failure source is unidentifiable, `recordAnonymousFailure` does the
attribution work that lnd hands to `failPairRange`:

```go
// Only hops not already proven at this amount are suspects.
if state.lowerOK >= amount { continue }
```

- **exactly one suspect** → treat it as a definite failure, hard bound and
  all. Elimination gives certainty for free.
- **zero suspects** → contradiction; add a flat `0.35` penalty everywhere
  and move on.
- **many suspects** → spread `2.2/sqrt(n)` of penalty across them, and
  count how often each hop appears in an ambiguous failure. At four
  appearances add another 0.30; at eight, promote suspicion to a hard
  `sessionFailedAt` bound.

That escalation ladder is a Bayesian argument in the shape of a counter: a
channel that keeps showing up in unexplained failures eventually gets
treated as the cause.

## How this differs from lnd's production algorithm

Everything in hb1's comparison section applies to mx_c3 as well. What
follows are the differences that are specific to mx_c3, or that mx_c3
sharpens.

### Interval versus decayed range

lnd does keep a range. `missioncontrol_state.go` stores per node pair the
last `TimedPairResult{SuccessAmt, SuccessTime, FailAmt, FailTime}`, moves
`FailAmt` up to `successAmt + 1` when a success lands inside the failure
range, and refuses to relax a failure amount upward inside
`DefaultMinFailureRelaxInterval` (one minute). So the *data structure* is
not the difference. The difference is what happens to it next:

- **lnd dissolves the range with time.** The bimodal estimator feeds
  `canSend(SuccessAmt, ...)` and `cannotSend(FailAmt, ...)` into the
  probability formula, both exponentially decayed over
  `DefaultBimodalDecayTime` (one week). The apriori estimator ignores the
  range shape entirely and returns `nodeProbability * (1 - 2^(-age/halflife))`
  with a one-hour half-life.
- **mx_c3 treats the range as a hard constraint and moves it by arithmetic.**
  `amt >= upperFail` is exactly zero and `lowerOK >= amt` is 0.9985, with no
  clock anywhere in the file. The bounds change only when new evidence
  arrives or when a settlement displaces liquidity.
- **mx_c3 propagates across directions.** Every observation writes both the
  forward and the reverse state. lnd's directed pair results are entirely
  independent of each other.
- **mx_c3 keys on the directed channel.** `(chanID, from, to)` versus lnd's
  `(fromNode, toNode)`.

The hard constraint is the better trade while the evidence is mx_c3's own and
fresh, which is every tier it was bred and validated on. It is the worse
trade once the evidence has aged, because a bound with no floor has no way
back: exp-012 priced that at 56% of the objective. See the shortcomings
below.

### Two priors, and a classification lnd does not have

lnd's default estimator is apriori: a scalar
`DefaultAprioriHopProbability = 0.6` times a `capacityFactor` logistic, with
`DefaultAprioriWeight = 0.5` blending in the node's other results. Its
bimodal alternative integrates the two-exponential density
`P(x) ~ exp(-x/s) + exp((x-c)/s) + 1/c` over `[amount, failAmount]` and
renormalizes by `[successAmount, failAmount]`, with `BimodalScaleMsat`
defaulting to 300,000 sat.

mx_c3's prior is that same bimodal shape, written directly as a probability,
with scale as a *fraction of capacity* (1.8%) rather than an absolute
millisatoshi constant. That difference is why it transfers across
topologies: lnd's fixed 300,000 sat scale means very different things on a
1M sat channel and a 16M sat channel, whereas 1.8% of capacity means the
same thing everywhere. The `corpus-mix` training set mixed small-channel
and scale-free topologies precisely to punish scale-dependent tuning, and
this is what came out.

On top of the prior, mx_c3's `mode` latch has no counterpart anywhere in
lnd. lnd's estimators are stateless functions of the stored amounts and the
clock; they never commit to a hypothesis about which regime a channel is in.

### Cost function

lnd minimizes `fee + timeLockPenalty + attemptCost/P(route)`. mx_c3
minimizes

```
-log P + 5*fee/deliver + (0.045 + 0.003*hops) + capacityPenalty
```

per hop. Four contrasts:

- **`-log P` versus `c/P`.** Additive over hops, which is what makes
  label-setting search tractable, and far gentler on low-probability routes.
- **Proportional fees.** lnd's `weight` is in absolute millisatoshis and the
  attempt cost has both a fixed part (100 sat) and a proportional part
  (1000 ppm). mx_c3 normalizes fees by the delivered amount only, so its
  fee sensitivity is scale-free.
- **A depth-increasing hop penalty**, where lnd has none; lnd discourages
  long routes indirectly through accumulated fees, the CLTV risk factor
  (`RiskFactorBillionths = 15`), and the `MinProbability` floor multiplying
  down over hops.
- **No CLTV term at all.** mx_c3 tracks `timeLockDelta` only to build a
  valid route. It has no reason to care: the simulator charges nothing for
  locked-up capital.

### Splitting

lnd's splitting is a fallback inside `paymentSession.RequestRoute`: when
`findPath` returns `errNoPathFound`, halve `maxAmt`, stop at
`DefaultShardMinAmt` (10,000 sat) or `MaxParts`. The trigger is indirect,
arriving through the `MinProbability = 0.01` pruning inside pathfinding.

mx_c3 plans splits. It enumerates a ladder that includes evidence-derived
rungs, prices a full route for each, and picks by explicit utility with an
appetite weight that reads the payment's history. Concretely, it can:

- split before any failure, when beliefs already rule out the full amount;
- pick a shard that is not any power-of-two fraction of the amount;
- pick a shard sized to fit *just under* a bound it discovered two attempts
  ago;
- grow more conservative after three failures and more aggressive once one
  part has settled.

None of that is expressible in lnd's halving loop.

### Penalization and retry

lnd penalizes nodes as well as pairs, on purpose:
`getNodeProbability` blends every result for the sending node with weight
0.5, and `result_interpretation.go`'s `failNode` fails all pairs adjacent to
a node on amount-independent errors. Policy failures get a second chance
(`minSecondChanceInterval = time.Minute`) because stale gossip is common.

mx_c3 has no node-level state. Its escalation is per directed channel and
per payment:

| lnd | mx_c3 |
|---|---|
| `failPair` / `failPairRange` with a decaying penalty | hard `upperFail` bound, no decay |
| `failNode`, dragging down a node's other channels | nothing; per-channel only |
| second chance for policy failures | `sessionBlocked` plus `+6` penalty, expiring with the payment |
| blacklist and wait for the half-life | `candidateLowerRetryFactor`, a six-step retry-at-lower-amount ladder |
| `failPairRange` blames a whole prefix on ambiguity | suspect elimination: one suspect becomes a certainty, many get `2.2/sqrt(n)` each plus a counter that escalates at 4 and 8 |

Note one improvement over hb1: mx_c3 has no permanent global block. hb1's
`candidateBlockEdge` marked a channel dead for the rest of the process on a
single policy failure. mx_c3 confines every policy and unknown failure to
`sessionBlocked`, which dies with the payment. That is closer to lnd's
second-chance instinct, arrived at independently.

## Shortcomings

**Simulator fidelity gaps that plausibly shaped the design.**

- No background traffic and no virtual clock during evolution, so nothing
  moved liquidity between the sender's own payments. Zero time logic is
  trivially the right answer in that world, which made it suspect. exp-008
  built the drift corpus and re-ran evolution on it. Time awareness did come
  back — a 35-minute confidence half-life, hard bounds expiring at 20
  minutes, probability interpolated between aging evidence and the prior —
  and the router carrying it scores 0.417 on the drift test against mx_c3's
  0.457, losing on all four tiers. mx_c3's timelessness is a validated
  design property at this level of churn rather than a simulator artifact,
  with the residual caveat of one drift intensity, one traffic model, and a
  400-eval budget. See
  `simulation/lab/experiments/exp-008-drift-evolution.md` and
  `simulation/lab/experiments/exp-008-drift1-best-candidate.md`.
- **Sequential shard settlement.** The runner increments `inFlightHtlcs`
  only after a part settles, so mx_c3 never races shards and never has to
  reason about two of its own HTLCs contending for one channel. Its whole
  "observe shard *k*, then choose shard *k+1*" design depends on that.
  Notably, hb2 in the same lineage *did* evolve in-flight reservation
  (`reserveRoute`/`releaseRoute`) and mx_c3 dropped it, which is what you
  would expect when concurrency does not exist.
- No non-strict forwarding and no parallel channels, so the directed-channel
  key is unambiguous in a way it is not on mainnet.
- No fee market. Fees are static, nobody reprices, and the objective caps
  the fee penalty at 5,000 ppm. The `5.0*fee/deliver` and `4.0*fee/shard`
  weights were tuned in that world; the composite objective very likely
  undervalues fees relative to a real routing node's preferences.
- A single source node per scenario, so no contention with its own
  concurrent payments and no interaction with a real HTLC switch.
- Local balances are snapshotted once per payment (exp-005's M2 note).
  mx_c3 partly compensates by clamping `localBalances` on a first-hop
  failure, but that is a workaround for a simulator artifact.

**Design-level weaknesses visible in the code.**

- **Size and vestigial code.** 1,525 lines with several near-duplicate
  probability branches, a `confidence` field that is a saturating latch
  rather than a real posterior width, and `sessionSuspect` thresholds (4 and
  8) that are asserted rather than derived. exp-011 found that code
  evolution hits a complexity wall past roughly 800 lines, and mx_c3 is
  well past it.
- **Magic constants everywhere.** `2.2/sqrt(n)`, `exp(-0.70*penalty)`, the
  six retry rungs, `capacity/200`, `capacity/50`, `amt/32`, `0.045 +
  0.003*hops`. Each was selected by the objective, none by argument. Expect
  several to be corpus-specific.
- **Search cost.** Up to 24 labels per node and 120,000 expansions per
  shard, multiplied by every rung of a ladder that can hold dozens of
  entries. On the 12,161-node mainnet graph this is fine at 2.3 attempts per
  payment, but the worst case is much larger than lnd's single-distance
  Dijkstra.
- **Stale evidence makes it quit.** `upperFail` is a hard zero, so an amount
  at or above the bound is impossible rather than unlikely, and nothing in the
  file can ever revise that upward. Under evidence mx_c3 gathered itself, in
  the same batch, that is exactly right and it is where the attempt economy
  comes from. Under evidence that has aged — or that arrived from somewhere
  else — it is a trapdoor. exp-012 part 2 warmed the router with unscored
  payments and then restored the network's liquidity, so its bounds described
  a state that no longer existed, and mx_c3 fell from 0.791 to 0.347 on
  mainnet while its attempts fell from 2.3 to 0.6: it declared enough live
  channels dead to make live payments look hopeless, and gave up almost
  before starting. atomic1, whose persisted bounds clamp to a 0.012
  probability floor instead of zero, lost 2% on the same arm and beat mx_c3 by
  +0.233 (p=.002) at 100 warmup payments and +0.428 (p=.002) at 400 — the
  first statistically significant win over a champion in the program. mx_c3
  remains champion of record on the standing tiers, and this is the sharpest
  known weakness of the design: it has no floor, so it cannot be handed
  imported or aged knowledge safely. Fix it by clamping a persisted bound to a
  small probability rather than zero, and keep the hard zero for evidence
  gathered inside the current payment. See
  `simulation/lab/experiments/exp-012-cold-cache.md` and
  `simulation/lab/WHY.md` §3.
- **Still single-path.** Every shard gets its own independently-found route.
  Joint route-set planning — Pickhardt-style min-cost flow choosing a *set*
  of paths together — never evolved here. exp-010 elicited it from three
  independent proposer lineages under a corpus that demanded it, and none of
  the resulting planners beat mx_c3 off that corpus; the deepest one tied it
  on the corpus's own validation tier. The mechanism is available, the
  advantage is not, at least until sequential adaptivity stops being free
  (exp-010b).

**Not production code.** The contract is `routing.SimRouter`, not lnd's
`Router`. There is no persistence, no mission-control namespacing, no RPC
surface, no belief import/export. The global `candidateKnowledge` map is
mutex-guarded but unbounded and never evicted, where lnd caps history at
`DefaultMaxMcHistory = 1000`. Concurrency safety is limited to that one
mutex; nothing here has been reviewed against lnd's real router lifecycle.
Treat the file as a specification of an idea.

## When to pick mx_c3 over the others

Pick mx_c3 by default. It has the best combined score (0.652), it wins on
mainnet and out-of-distribution, and it loses only 0.003 on the hard test.
If you are going to port one of these ideas into lnd, port this one, and
read hb1 first to understand the core without the extra machinery.

Pick hb1 instead when you want the smallest readable expression of the
paradigm, or when the target network genuinely looks like the hard corpus
(small channels, strongly bimodal liquidity, large amounts relative to
capacity) and the 0.003 matters. hb2 is archived; see `router_hb2_v1.md`
for why.

## See also

- `simulation/lab/NOTEBOOK.md` — the full narrative.
- `simulation/lab/WHY.md` — mechanism by mechanism against lnd's production
  code, with §3 on how each design ages its evidence.
- `simulation/lab/experiments/exp-012-cold-cache.md` — the cold-cache and
  hot-load sweep, and the stale-knowledge weakness above.
- `simulation/lab/experiments/exp-007-mix-followup.md` — the `code_mix1`
  run and the frontier sweep that selected mx_c3.
- `simulation/lab/experiments/exp-009-mainnet-validation.md` — the mainnet
  snapshot result.
- `simulation/lab/experiments/exp-010-splitting-pressure.md` — the
  splitting-pressure experiment, the three joint planners it evolved, and
  the five-tier paired sweep that kept mx_c3 champion.
- `simulation/lab/experiments/exp-011-code-gen2.md` — the independent third
  lineage that converged on the same paradigm from a small seed.
- `simulation/lab/experiments/exp-008-drift-evolution.md` and
  `exp-008-drift1-best-candidate.md` — the drift experiment that settled the
  zero-time-logic question, and the time-aware router it produced.
- `router_hb1_v1.md` — the parent, with the full lnd comparison.
- `routing/sim_router.go` — the `SimRouter` contract.
