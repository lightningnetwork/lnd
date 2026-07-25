# hb1 — the hard-regime specialist

`router_hb1_v1.go` (872 lines) is the first evolved router that beat both
lnd's production stack and the hand-written seed on held-out scenarios. It
is the ancestor of every later champion, and it is still the best router we
have on depleted, small-channel networks.

Read this document next to the source. Every constant quoted below appears
verbatim in the file.

## Provenance

| field | value |
|---|---|
| run | `code_hard1` (GEPA code mode, reflection LM `codex:gpt-5.6-sol`) |
| seed program | the in-tree hand-written router, `cmd/routesim/candidate_impl.go` (384 lines) |
| training corpus | the `--hard` regime: bimodal liquidity only, small-channel smallworld/grid/hubspoke topologies |
| budget | 400 evaluations requested, terminated at 135 when a sibling candidate hung the evaluator |
| writeup | `simulation/lab/experiments/exp-006-breakthrough.md` |
| status | champion of record for the hard regime; runner-up generalist |

GEPA's own validation-set selection picked this candidate (val aggregate
0.31654), which matched the independent hard-val measurement of 0.3165.
Two Pareto siblings came out of the same run: hb2 (`router_hb2_v1.go`,
better out-of-distribution, later dominated by mx_c3) and hb3 (discarded).

## Validated scores

All three tiers are held out from the run that produced hb1. Objective =
`success − 0.01·min(extra_attempts, 15) − 0.00002·min(fee_ppm, 5000)`.

| tier | lnd stack | seed | **hb1** | mx_c3 |
|---|---|---|---|---|
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.583 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | **0.581** |
| mainnet, 12,161 nodes | 0.694 | 0.762 | 0.790 | **0.791** |
| drift test (exp-008) | 0.203 | 0.377 | 0.455 | **0.457** |

On the mainnet snapshot hb1 reaches 0.810 success at **2.3 attempts per
payment**, against lnd's 0.790 success at 19.8 attempts. The success rates
are close; the 8.6× reduction in attempts is where the objective gap comes
from, and it is the headline result of the whole project.

## Running it

The candidate slot is a single file, swapped at build time so that the
router under test compiles into the real `routesim` binary:

```bash
cd $LND_REPO
cat > /tmp/overlay.json <<EOF
{"Replace": {"$PWD/cmd/routesim/candidate_impl.go":
             "$PWD/simulation/champions/router_hb1_v1.go"}}
EOF
go build -overlay /tmp/overlay.json -o /tmp/routesim_hb1 ./cmd/routesim
/tmp/routesim_hb1 --scenarios /tmp/corpus/test/example_000.json \
    --router=candidate --traces=false
```

The only contract the file must satisfy is a package-level
`newCandidateRouter` matching `routing.SimRouterFactory`;
`cmd/routesim/main.go` hands it to `runner.SetRouterFactory` when
`--router=candidate`.

## Architecture

### Lifecycle and where the memory lives

The simulator builds a fresh router per payment
(`SimRunner.RunScenario` calls the factory once), so anything hb1 keeps on
the `candidateRouter` struct dies when the payment ends. What survives is
the package-level global:

```go
var candidateKnowledge = struct {
        sync.Mutex
        states map[candidateEdgeKey]*candidateLiquidityState
}{...}
```

That map is hb1's mission control. It is keyed by
`candidateEdgeKey{chanID, from, to}` — a **directed channel**, not a node
pair — and it is never cleared, so beliefs accumulate across every payment
in a `routesim` process. The per-payment struct holds only the short-lived
state: `sessionPenalty`, `sessionBlocked`, `localBalances`, and an attempt
counter.

The constructor walks the sealed gossip view breadth-first from the source
and materializes one `candidateEdge` per directed channel, keyed on the
*incoming* policy, because all pathfinding runs backwards from the target.

### The belief state: a liquidity interval

```go
type candidateLiquidityState struct {
        upperFail lnwire.MilliSatoshi
        lowerOK   lnwire.MilliSatoshi
        estimate  lnwire.MilliSatoshi
        known     bool
        conf      float64
        failures  uint32
        successes uint32
        blocked   bool
}
```

`lowerOK` is the largest amount this direction has been *proven* to carry;
`upperFail` is the smallest amount proven to fail. Three writers maintain
the pair:

- `candidateRecordProbe` fires for every hop *upstream* of a failure, which
  the wire proves worked: it raises `lowerOK` to the forwarded amount,
  clears a now-contradicted `upperFail`, and pushes `estimate` up to 90% of
  capacity (`edge.capacity * 9 / 10`).
- `candidateRecordFailure` lowers `upperFail` to the failing amount, drags
  `lowerOK` below it, and collapses `estimate` to `amt / 8` — the depleted
  mode of the bimodal prior.
- `candidateRecordSettlement` is the interesting one. A settled HTLC does
  not just prove the channel worked, it *moves the money*. So hb1
  decrements the forward direction's `estimate`, `lowerOK`, and `upperFail`
  by the settled amount, and credits the **reverse** direction by the same
  amount, capped at capacity. Nothing in lnd does this.

`conf` is a monotone latch (`math.Max(state.conf, 0.85)` on a probe, 0.95
on a failure, 0.9 on a reverse credit), not a real count-based confidence.
The `failures`/`successes` counters do the count-sensitive work, and they
only feed two adjustments in the probability model.

### The probability model

`candidatePriorProbability` is the prior for a channel with no history:

```go
lowMode  := 0.45 * math.Exp(-ratio/0.025)
highMode := 0.50 / (1 + math.Exp((ratio-0.92)/0.04))
probability := 0.025 + lowMode + highMode   // clamped to [0.005, 0.985]
```

`ratio` is `amt/capacity`. The first term is an exponential that decays
over 2.5% of capacity, the second a logistic cliff centered at 92% of
capacity with a 4%-wide transition, and 0.025 is a uniform floor. That
shape — an exponential at each end of the channel plus a constant — is the
same hypothesis lnd's bimodal estimator is derived from
(`P(x) ~ exp(-x/s) + exp((x-c)/s) + 1/c`, `probability_bimodal.go`).
Nobody told the evolutionary search about it. It fell out of failure
traces.

`edgeProbability` then layers evidence on top, in strict precedence:

1. session-blocked or globally blocked → `0`.
2. own channel → `1` if the exact local balance covers the amount, else `0`.
3. `amt >= upperFail` → `0`. A proven failure is treated as certainty, not
   as a decayed penalty.
4. `lowerOK >= amt` → `0.995`. Likewise for a proven success.
5. no history → the prior.
6. `estimate >= amt` → `0.78 + 0.17*conf + 0.04*min(margin, 1)`, capped at
   0.995, where `margin` is the headroom over the amount as a fraction of
   capacity.
7. inside a known interval (`upperFail != 0`) →
   `0.03 + 0.35*(1-relative)^3 + 0.15*prior`, where `relative = amt/upperFail`;
   multiplied by 0.75 when `failures > successes+1`; floored at 0.01.
8. otherwise → `0.35*prior`, plus 0.15 when successes outnumber failures,
   capped at 0.75.

### Route search

`findRoute` is a backward Dijkstra from `spec.Target` to the source over
`incomingEdges`, with the amount growing hop by hop as fees accrue. The
per-edge cost is:

```go
logRisk := -math.Log(probability) + r.sessionPenalty[edge.key]
feePenalty := 15 * float64(fee) / math.Max(float64(deliver), 1)
edgeScore := logRisk + feePenalty + 0.012
```

Three things to notice. The risk term is `−log P`, so the path cost is the
negative log of the *product* of hop probabilities: minimizing it maximizes
route probability directly. The fee term is normalized by the delivered
amount, so it reads as a proportional-fee budget rather than absolute
millisatoshis. And `0.012` is a flat per-hop toll that biases toward short
routes. `findRoute` returns the accumulated `sourceRisk` alongside the
route, so the caller knows the route's probability without recomputing it.

### MPP splitting

hb1 owns splitting end to end; the simulator never splits for it.
`candidateShardAmounts` builds a ladder of `ceil(amt/parts)` for
`parts = 1 .. min(partsLeft, 24)`, deduplicated, largest first.

`RequestRoute` walks that ladder from the largest shard down and returns
the **first** shard whose route probability clears a threshold:

```go
threshold := 0.20
if partsLeft <= 2 {
        threshold = 0.08
}
```

The threshold drops when few parts remain, because a low-probability
attempt beats giving up. If no shard clears the bar, hb1 falls back to the
shard with the best utility:

```go
utility := -logRisk + 0.22*progress - feePenalty
```

where `progress = log(shard/minimum)` rewards making more headway per
attempt and `feePenalty = 10*fee/shard`. The whole payment is capped at 96
attempts.

### Learning from an attempt

`ReportAttempt` splits on whether the failure could be attributed:

- **Settled**: record a settlement on every hop (moving liquidity in both
  directions), clear session penalties, and debit the first hop from
  `localBalances`.
- **Failure with a source**: probe every hop *before* the failure index
  (they demonstrably forwarded), then act on the code.
  `CodeTemporaryChannelFailure` records a liquidity failure.
  `CodeFeeInsufficient` and `CodeIncorrectCltvExpiry` call
  `candidateBlockEdge`, which sets `blocked` in the **global** map.
  Anything else sets `sessionBlocked`, which expires with the payment.
- **Unattributable failure**: add `0.45` to `sessionPenalty` for every
  non-local edge on the route, which the search adds directly to path cost.

## How this differs from lnd's production algorithm

### Time-decayed penalties versus hard intervals

lnd's mission control keeps, per **node pair**, the last
`TimedPairResult{SuccessTime, SuccessAmt, FailTime, FailAmt}`
(`missioncontrol_state.go`), so it does track a success/failure amount
range. The estimators then dissolve that range back into a soft
probability using elapsed time. The apriori estimator weights a failure by
`2^(-age/PenaltyHalfLife)` with a default half-life of one hour, so a
channel that failed recovers to the node probability on a clock. The
bimodal estimator decays the amounts themselves: `canSend` shrinks
`SuccessAmt` toward zero and `cannotSend` grows `FailAmt` toward capacity,
both over `DefaultBimodalDecayTime` of one week.

hb1 has **no time logic at all**. Grep it: no `time.Now`, no half-life, no
decay constant. Three differences follow:

- **Granularity.** lnd keys on `(fromNode, toNode)`; hb1 keys on
  `(chanID, from, to)`. Parallel channels between the same pair share one
  belief in lnd and get separate beliefs in hb1.
- **Certainty.** `amt >= upperFail` returns exactly `0` and
  `lowerOK >= amt` returns `0.995`, forever. lnd never returns a hard zero
  from a stale failure; it returns `nodeProbability * (1 - weight)`, which
  climbs back as the failure ages.
- **Displacement.** hb1's settlement handler moves the interval by the
  settled amount and credits the reverse direction. lnd's success path
  raises `SuccessAmt` and, if the success lands in the failure range, sets
  `FailAmt = successAmt + 1`; it never debits the direction it just used,
  and never touches the reverse direction.

The last point is why hb1 gets so much out of so few attempts. Once it has
pinned an interval, it plans the *next* shard against a belief that already
accounts for the liquidity the previous shard consumed.

### A rediscovered prior versus a configured one

lnd offers two priors and you pick one with `--routerrpc.estimator`. The
default is apriori (`DefaultEstimator = AprioriEstimatorName`), whose prior
is a single scalar, `DefaultAprioriHopProbability = 0.6`, multiplied by a
`capacityFactor` — a logistic centered at `CapacityFraction = 0.9999` of
capacity, smeared over 2.5%, bottoming out at `minCapacityFactor = 0.5`.
The bimodal estimator instead integrates the two-exponential liquidity
density and renormalizes over the known interval.

hb1's `candidatePriorProbability` sits between the two. It is shaped like
the bimodal density (exponential low mode plus a cliff near capacity), but
it is written directly as a probability rather than derived by integration,
and the cliff is at 92% of capacity with a 4% transition rather than
99.99% with 2.5%. Because the low mode decays over just 2.5% of capacity,
hb1 assumes far more depletion than lnd's default 0.6-flat prior on the
small-channel networks it was trained on. That is exactly the regime where
lnd's defaults lose: exp-002 established that no amount of parameter tuning
on lnd's knobs beat lnd's own defaults, so the prior's *shape*, not its
constants, is the lever.

### Cost function

lnd's Dijkstra minimizes
`weight + attemptCost/P(route)` (`getProbabilityBasedDist`), where
`weight = fee + timeLockPenalty` in millisatoshis and `attemptCost` is
`DefaultAttemptCost` (100 sat) plus `DefaultAttemptCostPPM` (1000 ppm) of
the amount, adjusted by the time preference. The derivation in
`pathfind.go` is exact: `F + c/P` is the correct ordering for "try A then
B". Probability enters as a reciprocal, and it is the *whole-route*
probability, recomputed at each expansion.

hb1 minimizes `−log P + normalized_fee + hop_toll` instead. The
substitution of `−log P` for `c/P` makes the cost additive over hops, which
is what lets the risk term compose cleanly inside a plain Dijkstra
relaxation. It also changes the trade-off: `c/P` punishes a low-probability
route brutally (a 1% route costs 100c), while `−log P` punishes it
logarithmically. hb1 compensates with a hard `MinProbability`-equivalent of
its own — the `threshold` in `RequestRoute` — applied to the *route*, not
to each partial path.

lnd also carries a CLTV risk term, `RiskFactorBillionths = 15` scaled by
amount and time-lock delta. hb1 drops it entirely; it tracks
`timeLockDelta` only to build a valid route. In a simulator with no
capital-lockup cost this term is free to discard, and evolution discarded
it. On a real node it is not free.

### Splitting

lnd splits reactively. `paymentSession.RequestRoute` calls `findPath`, and
only when pathfinding returns `errNoPathFound` does it halve: `maxAmt /= 2`,
floored at `DefaultShardMinAmt` (10,000 sat) and capped by
`payment.MaxParts`. Because `findPath` prunes any partial path whose
running probability drops below `cfg.MinProbability` (default 0.01), the
halving is triggered *indirectly* by the probability floor.

hb1 splits deliberately. It enumerates the whole shard ladder up front,
prices a route for each rung, and picks by threshold-then-utility. Two
consequences: it can choose a shard size that is not a power-of-two
fraction of the amount, and it can decide to split on the *first* attempt,
before any failure, when its beliefs already say the full amount will not
fit. lnd cannot split before it has failed to find a path.

### Penalization and retry

lnd penalizes at the node level by design.
`AprioriEstimator.getNodeProbability` blends the apriori probability with
every result for the *from* node, weighted by `DefaultAprioriWeight = 0.5`,
so a failure on one channel drags down the estimate for that node's other
channels. `result_interpretation.go` reinforces this with `failNode`, which
fails every pair adjacent to a node for amount-independent errors, and with
the second-chance logic for policy failures
(`minSecondChanceInterval = time.Minute`).

hb1 has no node-level machinery whatsoever. Its penalties are per directed
channel, and they come in three flavors that map onto how much the failure
proved:

- proven liquidity limit → `upperFail`, permanent, global.
- proven policy problem (`FeeInsufficient`, `IncorrectCltvExpiry`) →
  `candidateBlockEdge`, permanent, global. This one is a real hazard: a
  single stale channel update blocks that direction for the rest of the
  process, with no expiry and no second chance. lnd deliberately grants a
  second chance here because policy failures are usually stale-gossip
  artifacts.
- unattributed → additive `sessionPenalty`, per payment, spread over all
  non-local hops.

Where lnd blacklists, hb1 mostly *retries at a lower amount*: the interval
model says nothing about `amt/2` when `amt` failed, so the shard ladder
simply prices the smaller shard and finds it viable.

## Shortcomings

Be blunt about what this is: a routing algorithm evolved against a
simulator, validated on that simulator plus one mainnet graph snapshot.

**Simulator fidelity gaps that plausibly shaped the design.**

- Until exp-008 the simulator had no virtual clock and no background
  traffic, so *nothing changed liquidity between a sender's own payments*.
  In that world time-decay logic can only hurt, which made hb1's
  zero-time-logic look like a simulator artifact. exp-008 tested it
  directly: the drift corpus (ten virtual minutes between payments,
  background senders moving balances) plus a fresh evolution run on that
  corpus. Time awareness did re-evolve — a 35-minute confidence half-life,
  hard bounds expiring at 20 minutes, probability interpolated between
  aging evidence and the prior — and the resulting router scores 0.417 on
  the drift test against hb1's 0.455. hb1's timelessness is a validated
  design property at this level of churn, not an artifact; a stale hard
  bound costs about one retry, which is cheaper than what decay discards.
  The residual caveat is one drift intensity, one traffic model, and a
  400-eval budget. See
  `simulation/lab/experiments/exp-008-drift-evolution.md` and
  `simulation/lab/experiments/exp-008-drift1-best-candidate.md`.
- MPP shards settle **sequentially** in the runner: `inFlightHtlcs` only
  increments after a part settles, so hb1 never races two shards against
  the same channel and never learns to. Its whole splitting design assumes
  it observes the outcome of shard *k* before choosing shard *k+1*.
- No non-strict forwarding and no parallel channels between a pair, so the
  directed-channel key is unambiguous in the simulator in a way it is not
  on mainnet.
- No fee market: fees are static policy, nobody reprices, and the objective
  caps the fee penalty at 5,000 ppm. hb1's `15 * fee / deliver` weight was
  tuned in that world and probably undervalues fees.
- Each scenario has a single source node, so hb1 never contends with its
  own concurrent payments.

**Design-level weaknesses visible in the code.**

- `conf` is a latch that saturates at 0.85–0.95 after one observation, so
  "evidence-count confidence" overstates what it does. Real count
  sensitivity lives in two coarse adjustments (`*0.75`, `+0.15`).
- `candidateBlockEdge` is permanent and process-global with no expiry.
- `candidateRecordFailure` sets `state.lowerOK = amt - 1` even when the
  previous `lowerOK` was legitimately higher, discarding proven evidence on
  a contradiction rather than reconciling it.
- The 96-attempt cap and the `0.20`/`0.08` thresholds are magic numbers
  with no derivation behind them.
- Single-path search: hb1 finds one route per shard. Joint route-set
  planning (Pickhardt-style min-cost flow over a set of paths) never
  evolved.

**Not production code.** The contract is `routing.SimRouter`, not lnd's
`Router`. There is no persistence, no namespacing, no RPC surface, no
import/export of beliefs, and no coordination with lnd's real mission
control. The global `candidateKnowledge` map is mutex-guarded but grows
without bound and has no eviction policy, where lnd caps mission control at
`DefaultMaxMcHistory = 1000` entries. Treat the file as a specification of
an idea, not as a patch.

## When to pick hb1 over the others

Pick hb1 when the network looks like the hard corpus: small channels,
strongly bimodal liquidity, amounts that are a large fraction of channel
capacity. It is the best router we have measured on the sealed hard test
(0.586 against mx_c3's 0.583), and it gets there in 872 lines instead of
1,525, which makes it far easier to read, port, or argue with.

Pick mx_c3 instead when the topology is unknown or scale-free, where hb1
gives up 0.036 objective (0.545 versus 0.581) on the out-of-distribution
corpus. On the mainnet snapshot the two are within noise of each other
(0.790 versus 0.791), so on mainnet-like graphs read hb1 first and reach
for mx_c3 only if you want the extra machinery.

## See also

- `simulation/lab/NOTEBOOK.md` — the full narrative.
- `simulation/lab/experiments/exp-006-breakthrough.md` — the run that
  produced hb1 and the three discoveries.
- `simulation/lab/experiments/exp-009-mainnet-validation.md` — the mainnet
  snapshot result.
- `simulation/lab/experiments/exp-008-drift-evolution.md` — the drift
  experiment that settled the zero-time-logic question.
- `simulation/lab/experiments/exp-008-drift1-best-candidate.md` — the
  time-aware router that experiment produced, walked through in full.
- `routing/sim_router.go` — the `SimRouter` contract hb1 implements.
