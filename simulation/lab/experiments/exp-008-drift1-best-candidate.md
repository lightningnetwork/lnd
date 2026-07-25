# drift1 — the first evolved router with a clock

`exp-008-drift1-best-candidate.go` (1,147 lines) is the winner of the
`code_drift1` run, and it is the only router in this project that evolution
gave time-based logic. It carries an `updatedAt` stamp on every belief, a
35-minute confidence half-life, and hard bounds that expire after twenty
minutes. It also loses to the time-less champions on every held-out tier,
including the drift corpus it was bred on.

That combination is why the file is kept and why this document exists. It is
not a champion and it is not promoted to `simulation/champions/`, the same
disposition as the `code_gen2` best candidate. Read it as the answer to the
question that motivated exp-008: is the champions' total absence of time
decay a real design property, or an artifact of a simulator where nothing
moved liquidity between our own payments?

Read this document next to the source. Every constant quoted below appears
verbatim in the file.

## Provenance

| field | value |
|---|---|
| run | `code_drift1` (GEPA code mode, reflection LM `codex:gpt-5.6-sol`) |
| seed program | the small in-tree router, `cmd/routesim/candidate_impl.go` (~380 lines), with the discovered insights supplied as prose in the background prompt |
| training corpus | `corpus-drift` (seed 3031): hard bimodal small-channel topologies, ten virtual minutes between payments, one virtual second per attempt, background senders per gap scaled to network size (at least 10, or `num_nodes/10`) |
| budget | 400 evaluations, 400 consumed, 51 accepted candidates |
| writeup | `exp-008-drift-evolution.md` (this directory) |
| status | not promoted; kept as the record of an evolved time mechanism |

The reflection prompt described drift neutrally and flagged the hard-bounds
insight as something learned in a static world. It did not prescribe decay,
and it did not forbid it.

## Validated scores

All four tiers are held out from the run. Objective =
`success − 0.01·min(extra_attempts, 15) − 0.00002·min(fee_ppm, 5000)`.

| tier | lnd stack | seed | hb1 | mx_c3 | gen2 | **drift1** |
|---|---|---|---|---|---|---|
| drift test | 0.203 | 0.377 | 0.455 | **0.457** | 0.456 | 0.417 |
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.583 | 0.565 | 0.580 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | **0.581** | 0.563 | 0.544 |
| mainnet, 12,161 nodes | 0.694 | 0.762 | 0.790 | **0.791** | 0.787 | 0.790 |

On the drift test drift1 settles 60% of payments at 12.2 attempts each,
against mx_c3's 64% at 12.3 and lnd's 39% at 34.5. On the mainnet
snapshot it reaches 0.810 success at 2.4 attempts per payment, which is
champion-class and 8× better than lnd's 19.8. So the time machinery cost it
almost nothing where nothing drifts, and it did not pay where things do.

## Running it

The candidate slot is a single file, swapped at build time so the router
under test compiles into the real `routesim` binary:

```bash
cd $LND_REPO
cat > /tmp/overlay.json <<EOF
{"Replace": {"$PWD/cmd/routesim/candidate_impl.go":
             "$PWD/simulation/lab/experiments/exp-008-drift1-best-candidate.go"}}
EOF
go build -overlay /tmp/overlay.json -o /tmp/routesim_drift1 ./cmd/routesim

# Regenerate corpus-drift (fixed seed, so it reproduces exactly).
python3 simulation/gen_scenarios.py --out /tmp/corpus-drift \
    --hard --drift --seed 3031

/tmp/routesim_drift1 --scenarios /tmp/corpus-drift/test/example_000.json \
    --router=candidate --traces=false
```

## Architecture

drift1 shares its skeleton with the champions: a per-payment router over a
package-level belief map, a backward search from the target, and a shard
ladder the router prices itself. Three constants set the scale, and they are
smaller than mx_c3's:

```go
candidateFinalCltvDelta = 40
candidateAttemptLimit   = 48
candidateMaxRouteHops   = 20
```

Everything below centers on the machinery that is new, because that is the
whole reason to read this file.

### The belief state, now stamped

```go
type candidateLiquidityBelief struct {
        capacity  lnwire.MilliSatoshi
        lowerOK   lnwire.MilliSatoshi
        upperFail lnwire.MilliSatoshi
        estimate  lnwire.MilliSatoshi
        conf      float64
        updatedAt time.Time
}
```

The first five fields are the lineage's interval: `lowerOK` is the largest
amount this direction has been proven to carry, `upperFail` the smallest
amount proven to fail, `estimate` a point guess between them. The sixth
field is new, and it is the whole experiment. Beliefs live in the usual
package-level map, keyed by `candidateEdgeKey{chanID, from, to}` — a
directed channel, not a node pair — guarded here by a `sync.RWMutex` rather
than the champions' plain `Mutex`, so the read path in `candidateSnapshot`
does not serialize.

`candidateNormalizeBelief` runs on every write and enforces the interval's
invariants: bounds clamped to capacity, an `upperFail` that contradicts
`lowerOK` dropped, `estimate` pinned inside the interval, and `conf` capped
at 0.99. That reconciliation is mx_c3's idea, not hb1's.

### Confidence that decays, and bounds that expire

Two functions carry all of the time logic.

```go
func candidateBeliefConfidence(
        b candidateLiquidityBelief, now time.Time) float64 {
        ...
        age := now.Sub(b.updatedAt).Minutes()
        ...
        const halfLifeMinutes = 35.0
        conf := b.conf * math.Exp(-math.Ln2*age/halfLifeMinutes)

        if conf < 0.01 {
                return 0
        }

        return conf
}
```

Confidence in a belief halves every 35 virtual minutes. The comment above
the constant says why: "Background traffic can invalidate old directional
evidence quickly." With ten minutes between payments, a fresh observation
retains 82% of its weight by the next payment and roughly two-thirds by the
third, so evidence survives two or three payments near full strength. The
`0.01` floor is a cliff, not an asymptote: below it the function returns
zero, and a zero confidence makes callers discard the belief entirely and
fall back to the prior. Since observations latch `conf` at 0.92 to 0.98, a
belief that is never refreshed lives about four hours — twenty-odd payments
at the corpus gap — and then ceases to exist.

```go
func candidatePrepareObservation(
        b candidateLiquidityBelief, capacity lnwire.MilliSatoshi,
        now time.Time) candidateLiquidityBelief {

        if b.capacity != capacity {
                return candidateLiquidityBelief{capacity: capacity}
        }

        conf := candidateBeliefConfidence(b, now)
        if conf == 0 {
                return candidateLiquidityBelief{capacity: capacity}
        }

        b.conf = conf

        // Bounds become hints rather than permanent facts after
        // substantial age.
        if now.Sub(b.updatedAt) > 20*time.Minute {
                b.lowerOK = 0
                b.upperFail = 0
        }

        return candidateNormalizeBelief(b, capacity)
}
```

Every writer funnels through this function before recording anything, so
every observation first ages the belief it is about to update. The comment
is the design in one line. After twenty virtual minutes — two payment gaps —
the two hard bounds are zeroed outright, and only the softer `estimate`
survives, weighted by whatever confidence is left (about 0.66 of the latched
value at exactly twenty minutes). The bounds do not shrink, widen, or fade.
They expire.

### The probability model: sliding back to the prior

The prior is the lineage's rediscovered bimodal shape, an exponential low
mode plus a logistic cliff:

```go
lowMode := 0.48 * math.Exp(-ratio/0.025)
highMode := 0.50 / (1 + math.Exp((ratio-0.92)/0.045))

return candidateClampProbability(0.005 + lowMode + highMode)
```

`candidateLearnedProbability` is what the evidence says: `0.995` at or below
`lowerOK`, `0.005` at or above `upperFail`, the prior when there is no
`estimate`, and otherwise a logistic centered on the estimate with width
`3.5%` of capacity. When an `upperFail` is known it blends in a
position-within-the-interval term, `0.65*bounded + 0.35*probability` with
`bounded = 0.005 + 0.99*(1-fraction)^2.4`.

Then comes the line that makes drift1 different from every other router
here:

```go
b := candidateSnapshot(edge)
conf := candidateBeliefConfidence(b, r.view.Now())
if conf == 0 {
        return prior
}

learned := candidateLearnedProbability(b, amt, edge.capacity)
probability := conf*learned + (1-conf)*prior
```

The router reads the clock (`r.view.Now()`, the simulator's virtual clock,
which no champion touches) and interpolates: fresh evidence dominates, aging
evidence slides back toward the bimodal prior, dead evidence disappears and
leaves the prior alone. This is a single mechanism doing what lnd needs two
estimators and several constants to express — and, notably, it recovers
toward drift1's *own learned prior*, not toward a configured scalar.

Everything before that line is untimed. Session-blocked edges return `0`,
the router's own channels return `0.9995` when the local balance covers the
amount and `0` when it does not, and per-payment session evidence takes
precedence over the global belief: an amount at or above a session failure
returns `0`, an amount above 75% of one returns `0.006`, and an amount at or
below a session-proven pass returns `0.998`. After the interpolation, the
session-failure ratio ladder multiplies the result down — `0.08` above 55%
of a failed amount, `0.30` above 30%, `0.65` above 12%. That ladder is
drift1's version of mx_c3's `candidateLowerRetryFactor`, and it has no clock
either.

### What it kept from the lineage

Four inherited mechanisms survived the drift regime intact, which is itself
a result.

- **Bidirectional evidence.** `candidateStorePair` writes the forward belief
  and then mirrors it through `candidateReverseKey`: the reverse
  `estimate` becomes `capacity - forward.estimate`, a forward `upperFail`
  becomes a reverse `lowerOK` at `capacity - upperFail + 1`, a forward
  `lowerOK` becomes a reverse `upperFail`, and the reverse confidence
  latches at `forward.conf * 0.88`. Liquidity sitting on one side of a
  channel cannot also sit on the other, so a failure one way is evidence of
  room the other way. The reverse belief is stamped with the forward's
  timestamp, so both ages advance together.
- **Settlement displacement.** `candidateRecordSettlement` does not merely
  record that the channel worked; it moves the money. `estimate`, `lowerOK`,
  and `upperFail` all drop by the settled amount, and the mirror credits the
  reverse direction. Nothing in lnd does this.
- **The ceil-division shard ladder.** `candidateShardAmounts` enumerates
  `ceil(amt/parts)` for `parts = 2 .. min(partsLeft, 16)` plus the
  full amount and the minimum, deduplicated. `RequestRoute` prices a route
  for every rung and maximizes
  `log P + 0.48*progress - 6*fee/shard`, taking an early exit when a shard
  covering at least half the amount clears probability `0.55`. It can split
  before it has ever failed.
- **Retry at a lower amount rather than blacklisting.** No global block
  exists anywhere in the file. Policy failures
  (`CodeFeeInsufficient`, `CodeIncorrectCltvExpiry`) and unrecognized codes
  set `sessionBlocked`, which dies with the payment, and everything else is
  a bound the shard ladder can simply fit under.

The route search is a plain backward Dijkstra over incoming edges with
per-edge cost

```go
riskCost := -math.Log(probability)
feeCost := 8 * float64(fee) / feeScale
hopCost := 0.055
useCost := 0.035 * math.Min(float64(r.edgeUses[edge.key]), 8)
```

plus the session penalty, where `feeScale = max(deliver, 1_000_000)`. The
`useCost` term is drift1's own small idea: an edge the current payment has
already routed over gets progressively more expensive, which spreads
retries across the graph instead of hammering one corridor.

Failure attribution follows hb1's shape. Every hop before the failure index
demonstrably forwarded, so each one records a pass — its own first hop
excepted, since the router already knows that balance. The failing hop
records a liquidity failure on `CodeTemporaryChannelFailure`. An
unattributable failure spreads `0.45` of session penalty over every non-local
hop, or `0.60` over hops in the second half of the route, capped at `4`.

## The contrast with lnd's decay

This is the interesting part of the file, and it is easy to state wrongly.
lnd decays. drift1 decays. They are not doing the same thing.

lnd's apriori estimator — the default, `DefaultEstimator =
AprioriEstimatorName` — stores per node pair the last
`TimedPairResult{SuccessAmt, SuccessTime, FailAmt, FailTime}` and then, in
`probability_apriori.go`:

```go
weight := p.getWeight(timeSinceLastFailure)
probability := nodeProbability * (1 - weight)
```

with `getWeight(age) = 2^(-age/PenaltyHalfLife)` and a default half-life of
one hour. A fresh failure weighs 1 and yields probability zero; as it ages
the *penalty* fades and the pair recovers toward `nodeProbability`, the
apriori scalar `0.6` blended with the node's other results at
`DefaultAprioriWeight = 0.5`. Successes do not decay at all — the code says
so outright: "Weigh success with a constant high weight of 1. There is no
decay." The bimodal estimator takes a different route to the same instinct,
decaying the stored amounts themselves over `DefaultBimodalDecayTime` of one
week, shrinking `SuccessAmt` toward zero and growing `FailAmt` toward
capacity.

Line up the two designs and every axis differs:

| axis | lnd (apriori) | drift1 |
|---|---|---|
| what ages | the weight of a failure penalty | confidence in evidence, whatever the evidence says |
| symmetry | failures fade, successes never do | passes and failures age on one clock |
| what it recovers toward | `nodeProbability`: a configured scalar plus node-level history | its own bimodal prior, shaped by capacity |
| how it recovers | continuously and asymptotically | continuously for `estimate`, as a cliff for the bounds |
| granularity | node pair | directed channel |
| timescale | 1 hour (penalty), 1 week (bimodal amounts) | 35 minutes (confidence), 20 minutes (bounds) |

The deepest difference is the first one. lnd fades a *judgment* it made
about a channel back toward neutrality. drift1 keeps the judgment intact and
fades its *trust* in the observation that produced it, then interpolates
between what it learned and what it would have assumed knowing nothing.
Structurally that is a Bayesian move — a posterior collapsing toward a prior
as its likelihood loses force — while lnd's is a penalty on a timer. Neither
form was in the prompt. Selection under genuine staleness pressure produced
the softening form, twice over: once as continuous interpolation, once as
the twenty-minute expiry that demotes proven bounds to hints.

So lnd's *rationale* is validated by this run. Stale knowledge should indeed
lose force, and an evolutionary search that has never been told about decay
will invent something to that effect once its evidence can actually go
stale. lnd's specific *mechanism* is not validated: the evolved answer looks
nothing like a penalty half-life, and the production stack still finishes
last on the drift corpus (0.203) with its half-lives finally operating over
meaningful spans.

## The verdict, and what it means

| tier | lnd | seed | hb1 | mx_c3 | gen2 | drift1 |
|---|---|---|---|---|---|---|
| drift test | 0.203 | 0.377 | 0.455 | **0.457** | 0.456 | 0.417 |
| hard test | 0.309 | 0.530 | **0.586** | 0.583 | 0.565 | 0.580 |
| OOD v2 | 0.357 | 0.487 | 0.545 | **0.581** | 0.563 | 0.544 |
| mainnet | 0.694 | 0.762 | 0.790 | **0.791** | 0.787 | 0.790 |

Time-awareness re-evolved, and it still lost every tier.

The sharpest comparison in the table is drift1 against gen2. Same small
seed, same insights-in-the-prompt design, same 400-evaluation budget — and
gen2 never saw drift during evolution, because the virtual clock did not
exist yet. The static-bred, time-less router scores 0.456 on the drift
corpus; the drift-bred, time-aware one scores 0.417. Whatever the time
machinery bought, it did not buy enough to cover what a mutation spent
building it.

Two readings, and they pull in opposite directions.

**Staleness pressure is real.** The time logic won selection inside its own
lineage: 51 candidates were accepted over 400 evaluations, and the winner is
the one carrying the clock. Within `code_drift1` the timed candidates beat
their time-less ancestors on the drift minibatches, so the mechanism was
selected *for*, not merely tolerated the way hb2's unexercised in-flight
reservation was. Something in the drift regime genuinely rewards noticing
that evidence gets old.

**At realistic churn, a stale bound costs one retry.** That is cheaper than
what decay discards. When drift1 zeroes `lowerOK` and `upperFail` at twenty
minutes it throws away information that is usually still approximately
right, and it pays for that on every subsequent route it prices. hb1 and
mx_c3 keep the bound, are occasionally wrong, and pay one extra attempt when
they are — an attempt that also refreshes the belief. The objective charges
`0.01` per extra attempt, capped at fifteen; that is a small bill next to
planning against a prior when you had a measurement. Failure evidence in
this environment is cheap to re-acquire, so decay is insurance against a
cost the interval design barely pays.

The champions' timelessness is therefore a validated design property at
this level of churn, not a simulator artifact. That was the open question
from exp-006 through exp-011, and it is closed — with the residual caveats
below.

## Shortcomings

**One drift regime.** `corpus-drift` has ten-minute gaps and roughly
`num_nodes/10` background payments per gap, all from naive fee-optimizing
senders. That is one intensity and one traffic model. A heavier churn rate,
bursty traffic, or adversarial rebalancing could all tip the balance back
toward decay, and nothing here rules that out. What the run settles is the
behavior at this churn, not at every churn.

**A 400-evaluation budget against a two-run lineage.** hb1 and mx_c3
accumulated their Pareto label-setting search, bidirectional evidence, and
evidence-derived shard ladders across two runs budgeted at 400 and 500
evaluations. drift1 spent 400 evaluations rebuilding a large part of that
from a small seed *and* inventing the time machinery. Its deficit is
plausibly a budget deficit as much as a mechanism deficit; the honest claim
is that time logic did not pay for itself within this budget, not that it can
never pay.

**The usual simulator caveats, all still in force.** MPP shards settle
sequentially, so drift1 never races its own HTLCs. There is no fee market,
no non-strict forwarding, no parallel channels between a pair, one source
node per scenario, and local balances snapshotted once per payment. The
composite objective caps the fee penalty at 5,000 ppm and very likely
undervalues fees relative to a real routing node.

**Design-level weaknesses visible in the code.**

- `conf` is a latch, not a posterior width. Observations set it to 0.92
  (pass), 0.94 (settlement), or 0.98 (failure) via `math.Max`, and there are
  no success or failure counters anywhere in the file. So drift1's
  "confidence" measures recency alone — how long ago it looked, never how
  many times.
- The two time constants are undefended and only loosely coupled. Bounds
  expire at 20 minutes; confidence halves every 35. Nothing derives either
  number, and nothing ties them to the corpus gap of 10 minutes except
  selection pressure.
- The expiry is all-or-nothing. Zeroing both bounds discards strictly more
  than widening them would, and interval widening with elapsed time was the
  outcome we hypothesized and did not get.
- `candidateRecordFailure` sets `b.lowerOK = amt - 1` on a contradiction
  even when the previous `lowerOK` was legitimately higher, discarding
  proven evidence rather than reconciling it. hb1 has the same flaw;
  mx_c3's `candidateNormalizeState` does better.
- The policy-failure branch sets `sessionBlocked[key] = true` *and*
  `sessionPenalty[key] = 20`. The first already returns probability zero
  from `edgeProbability`, so the penalty is dead weight — a vestigial branch
  of the kind code evolution leaves behind.
- Still single-path: one route per shard, found independently. Joint
  route-set planning never evolved here either (exp-010).

**Not production code.** The contract is `routing.SimRouter`, not lnd's
`Router`. There is no persistence, no namespacing, no RPC surface, no
belief import or export. The global `candidateKnowledge` map is
mutex-guarded but unbounded and never evicted, where lnd caps history at
`DefaultMaxMcHistory = 1000`. Treat the file as a specification of an idea.

## When to read drift1

Read it if you are arguing about time decay in mission control, because it
is the only artifact in the project where an evolutionary search invented
decay on its own and you can see exactly which form it chose. Read it also
for the `useCost` term, which is the cheapest good idea in the file and
appears in no champion.

Do not pick it for scoring: hb1 and mx_c3 beat it on all four tiers,
including drift.

## See also

- `exp-008-drift-evolution.md` — the run, the baseline, and the verdict.
- `exp-011-code-gen2.md` and `exp-011-gen2-best-candidate.go` — the
  time-less sibling with the same seed style and budget that beats drift1
  on drift.
- `simulation/champions/router_hb1_v1.md` — the full comparison against
  lnd's production stack; everything there applies here too.
- `simulation/champions/README.md` — the results tables and the lineage.
- `routing/probability_apriori.go` — `getWeight` and
  `calculateProbability`, the decay drift1 was measured against.
- `routing/sim_router.go` — the `SimRouter` contract, and `Now()` on the
  network view.
