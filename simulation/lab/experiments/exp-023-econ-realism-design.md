# EXP-023: economic realism, the design spec

**Date:** 2026-07-27
**Status:** DESIGN ONLY. Nothing here is implemented. The tree is held by
a live `code_deg1` run that recompiles `routing/` and `cmd/routesim/` on
every eval, so this document is written from reading alone and lands as
a single new file.
**Position in the roadmap:** item 2 of three. Item 1 (breeding under
degraded attribution) is running. Item 3 (offline replay) is parked on
node access.

---

## What this is

The simulator charges a candidate for three things: whether the payment
completed, how many htlc attempts it took, and what it paid in fees per
delivered millisatoshi. Everything else about the economics of a
Lightning payment is either absent or present but inert. This spec
proposes five mechanisms that put a price on things the arena currently
gives away, and it pre-registers what each one is expected to select
for, so that a null result is a finding rather than a disappointment.

The five, in the order this document argues they should land:

| stage | mechanism | touches the objective? |
|---|---|---|
| A | min/max HTLC pressure | no |
| B | inbound fees | no |
| C | fees as a first-class cost | yes, guarded |
| D | concurrent payments | reporting only |
| E | latency as a cost | offline re-scoring arm only |

The sequencing argument is in its own section near the end. Read it
before reading the stages as a to-do list, because the ordering is the
part of this spec most likely to be wrong.

## Five rules carried over from prior experiments

These are the constraints every stage below is written to satisfy. Each
one was bought with an experiment.

1. **Flag off means byte identical.** `atomic_mpp` (exp-010b),
   `attribution` (exp-019) and the `patch` knobs (exp-021) all shipped
   with a proof that the absent section reproduces the previous
   behavior exactly. Every stage here inherits that requirement, and
   `sim_liquidity_test.go:110` (`TestAssignLiquidityLegacyGolden`) is
   the model for how it is proven: a checked-in table of expected
   values, with the comment that a change to those numbers silently
   moves every published result.
2. **Degradation and constraint live at the shared delivery point, not
   in a router.** `SimRunner.deliverAttempt` (`routing/sim_run.go:474`)
   is the construction that makes exp-019 paired: the lnd stack and an
   evolved candidate get the same damaged result from the same draw.
   Any new constraint (a fee ceiling, a scheduling decision) belongs in
   the runner for the same reason.
3. **Candidates see only what a real sender sees.** The sealed view
   (`routing/sim_router.go:99`, which deliberately passes the wrapper
   and not the concrete graph) plus the evaluator's banned-identifier
   regex (`simulation/evaluate_code.py:32`) are the sandbox. New
   information reaching a candidate has to be information a real node
   could read off gossip, its own channels, or its own attempt results.
4. **Read success and attempts separately, always.** exp-013's winner
   bought an attempt record by abandoning hard payments, and exp-017
   found `give_up_rate == 1 - success_rate` identically for candidate
   routers, so abandonment is only readable jointly. The evaluator hint
   (`simulation/evaluate_code.py:181`) already states the rule
   unconditionally.
5. **The contract changes only under duress.** exp-010b considered
   wave-batched feedback and rejected it because it would have broken
   comparability with every existing router. Stage D reopens this
   question and answers it the same way, additively.

---

## Mechanism A: min/max HTLC pressure

### What exists today

This is the surprise of the survey. Min and max HTLC are fully
implemented on every surface and are inert on every synthetic tier.

Forwarding enforces both. `checkPolicy` rejects an amount below
`MinHTLCMsat` with `AmountBelowMinimum` (`routing/sim_graph.go:585`) and
an amount above a non-zero `MaxHTLCMsat` with `TemporaryChannelFailure`
(`routing/sim_graph.go:589`), which matches what lnd's own link returns
for a max-HTLC violation. Gossip exposes both, with `HasMaxHTLC` derived
from the zero check (`routing/sim_graph.go:293`). The background traffic
engine filters on both (`routing/sim_traffic.go:427`). Every candidate
reads both: the seed's `usable` does
(`cmd/routesim/candidate_impl.go:49`), and so do mx_c3
(`simulation/champions/router_mx3_generalist_v1.go:489`), hb1
(`simulation/champions/router_hb1_v1.go:300`) and atomic1
(`simulation/lab/experiments/exp-010b-atomic1-best-candidate.go:192`).
The describegraph loader parses both (`routing/sim_load.go:71`).

What is missing is any pressure. `defaultSimPolicy` assigns
`MinHTLCMsat: 1000` and leaves `MaxHTLCMsat` at zero, meaning no maximum
(`routing/sim_topology.go:44`). `corridorPolicy` and
`corridorFillerPolicy` do the same (`routing/sim_topology.go:576` and
`:590`). So on every synthetic tier in the program's history, max HTLC
has never bound once, and min HTLC has bound only for shards under 1000
msat. The mainnet tier is the sole exception, and there the real values
are not trivial. Counting the 62,798 directed policies in
`~/codez/data/mainnet_graph.json`:

| statistic | value |
|---|---|
| median `max_htlc / capacity` | 0.99 |
| 5th percentile `max_htlc / capacity` | 0.20 |
| policies with `max_htlc < 0.5 * capacity` | 13% |
| `min_htlc == 1000` msat | 78% of policies |
| `min_htlc >= 100_000` msat | 5% of policies |

So one directed edge in eight on the real graph announces a hard ceiling
below half its capacity, and one in twenty announces a floor at or above
100 sats. The evolved routers have been reading fields that only ever
carried a constant.

### Proposed change

No simulator semantics change at all. This is a generator stage.

A new `htlc_limits` section on the scenario file, parsed like the
`attribution` section is (stamped on by `gen_scenarios.py` with no rng
draw when absent, so the default corpus stays byte identical):

```json
"htlc_limits": {
  "max_htlc_frac_family": "mainnet_empirical",
  "min_htlc_family": "mainnet_empirical",
  "seed": 0
}
```

The two families draw per directed policy at graph construction time.
`mainnet_empirical` samples from the marginal distributions measured
above rather than from an authored shape, which is the exp-017 lesson
applied early: a family we invent is a family a router can only be
overfit to by us. An authored `tight` family (max HTLC uniform in
`[0.1, 0.4]` of capacity, min HTLC drawn from the observed ladder) is
worth having as a stress rung, clearly labelled as authored.

Nothing new flows to candidates. The fields are already on the gossip
struct and already read by every arm.

### Objective change

None. Min and max HTLC change what routes exist, and the cost of getting
them wrong is already priced as failed attempts.

### Falsifiable hypothesis

Two, and they point in opposite directions, which is what makes the
stage worth running.

**H-A1 (pressure selects for cap-aware shard sizing).** A binding max
HTLC forces MPP even where liquidity is ample, and an announced max HTLC
is a *public* upper bound on what an edge can carry. A router that sizes
its first shard against the announced cap spends fewer attempts than one
that discovers the cap by failing. Prediction: on the `htlc-limits` tier
an evolved router will size shards against `MaxHTLC` and show lower
attempts than its own ancestor at equal success. Falsified if evolution
produces no cap-reading mechanism and attempts do not move.

**H-A2 (announced caps narrow the champion margin).** The champions'
edge is per-directed-channel liquidity intervals learned from failures.
A max HTLC is a bound handed over for free in gossip, which is precisely
the kind of knowledge their apparatus exists to acquire by probing.
Prediction: the champion minus lnd margin is *smaller* on the
htlc-limits tier than on its untouched control, because part of what the
champions used to have to learn is now announced to everybody.
Falsified if the paired margin is unchanged, which would say their edge
is about amount *selection* rather than bound discovery.

H-A2 is a narrowing hypothesis, and the program should want it tested
early: if free public bounds do not narrow the gap, the exp-021
conclusion that the edge lives at plan time gets independent support.

### One inconsistency this stage exposes

`walkHtlc` skips policy checks entirely for the first hop, on the
correct ground that a node does not charge or constrain itself
(`routing/sim_graph.go:524`). lnd's pathfinder does not agree: local
edge selection runs `amtInRange` on the source's own announced policy
(`routing/unified_edges.go:268`, calling `amtInRange` at
`routing/unified_edges.go:180`). Today the disagreement is invisible
because no source channel has a binding limit. Under stage A, a
generator that assigns a low max HTLC to a source channel makes lnd
refuse to build a route the simulator would have carried, which is a
self-handicap for the lnd arm alone.

Both readings are defensible. lnd's behavior is real lnd behavior, so
letting it self-handicap is honest. The simulator's behavior is closer
to BOLT semantics, since a sender's own announced policy does not bind
its own sends. This is question 1 for the lead in the final section.

### Implementation scope

Files: `routing/sim_topology.go` (policy generators), `routing/sim_load.go`
(nothing, already parses real values), `routing/sim_run.go` or a new
`routing/sim_htlc_limits.go` for the family parsing and application,
`cmd/routesim/main.go` (one struct field), `simulation/gen_scenarios.py`
(one flag, one stamped section). Roughly 250 to 350 lines of Go plus 60
of Python.

Test surface: a legacy golden in the style of
`TestAssignLiquidityLegacyGolden`, asserting that the absent section
leaves every generated policy byte identical to today's; a determinism
test (same seed, same limits); a binding test that a payment above a
generated max HTLC actually fails with the right code; and a first-hop
test pinning whichever answer the lead gives to the inconsistency above.

---

## Mechanism B: inbound fees

### What exists today

Nothing in the simulator, and a complete implementation in lnd sitting
one field away.

`SimPolicy` has `BaseFeeMsat` and `FeeRatePPM` and no inbound fields
(`routing/sim_graph.go:20`). `SimPolicy.fee` is outbound only
(`routing/sim_graph.go:47`). `checkPolicy` charges the outbound fee
alone (`routing/sim_graph.go:593`). The describegraph loader does not
parse the inbound fields (`routing/sim_load.go:37`).

On lnd's side, everything is live. `graphdb.DirectedChannel` carries an
`InboundFee lnwire.Fee` field documented as "Inbound fees of this node"
(`graph/db/graph_cache.go:41`), populated from the node's own *outgoing*
channel update (`graph/db/graph_cache.go:215`). Pathfinding enables
inbound fees for every hop except the exit hop
(`routing/pathfind.go:1068`), reads them off the directed channel
(`routing/unified_edges.go:133`), computes them with
`models.InboundFee.CalcFee` (`graph/db/models/inbound_fee.go:35`), and
floors the total node fee at zero
(`routing/pathfind.go:800`, `routing/unified_edges.go:232`). The link
enforces the same arithmetic at forwarding time:
`inFee := inboundFee.CalcFee(amtToForward + outFee)` and then
`expectedFee := inFee + int64(outFee)` compared against the actual fee,
with a separate guard that the incoming amount is not below the outgoing
one (`htlcswitch/link.go:2507` through `:2526`).

Because `SimGraph.ForEachNodeDirectedChannel` never sets `InboundFee`
when it builds the directed channel (`routing/sim_graph.go:283`), lnd's
inbound-fee machinery has been running against a constant zero for the
entire program.

This is not a hypothetical part of the network. From the same mainnet
snapshot:

| statistic | value |
|---|---|
| directed policies carrying a non-zero inbound fee | 4,783 of 62,798 (7.6%) |
| of those, discounts (negative) | 4,660 |
| of those, surcharges (positive) | 123 |
| distinct nodes advertising one | 284 |
| median inbound rate | -200 ppm |
| 5th percentile inbound rate | -2,000 ppm |
| most negative inbound rate | -18,800 ppm |
| base component | almost always 0, down to -10,000 msat |

For scale, the simulator's own synthetic outbound rates run 0 to 1000
ppm (`routing/sim_topology.go:44`). A median mainnet inbound discount of
-200 ppm is therefore the same order as a whole outbound fee, and the
tail is an order of magnitude larger. Loading the snapshot today
silently discards all of it.

### Proposed change

**Schema.** Two fields on `SimPolicy`, typed to match lnd's wire model
(`graph/db/models/inbound_fee.go:11` uses `int32` for both):

```go
// InboundBaseMsat is the flat fee this end's owner charges for htlcs
// ARRIVING over this channel. It is negative when the node offers a
// discount for inbound flow, which is how the field is used in
// practice.
InboundBaseMsat int32

// InboundRatePPM is the proportional inbound fee in parts per million,
// applied to the outgoing amount plus the outgoing fee.
InboundRatePPM int32
```

The loader gains two parses of `inbound_fee_base_msat` and
`inbound_fee_rate_milli_msat`, which recovers the 4,783 real policies.
Synthetic tiers gain an `inbound_fees` section that draws from the
measured empirical distribution (share of policies, sign, magnitude)
rather than an authored one.

**Forwarding.** `checkPolicy` gains the incoming end's policy as an
argument and replaces its fee-sufficiency line with lnd's link
arithmetic, floored at zero exactly as the link effectively floors it:

```go
// The forwarding node's total fee is what it charges on the way out
// plus what it charges on the way in, and a node that would end up
// paying to forward simply charges nothing. This mirrors
// htlcswitch/link.go's CheckHtlcForward.
outFee := outPolicy.fee(amtOut)
inFee := inPolicy.inboundFee(amtOut + outFee)
total := int64(outFee) + inFee
if total < 0 {
        total = 0
}
if amtIn < amtOut+lnwire.MilliSatoshi(total) {
        return lnwire.NewFeeInsufficient(amtIn, emptyUpdate)
}
```

The incoming end is available in `walkHtlc` already: at hop `i`, the
forwarding node is `routeHop.PubKeyBytes` at index `i-1`, and its
incoming channel is `rt.Hops[i-1].ChannelID`. The exit hop charges no
inbound fee, matching `!isExitHop` at `routing/pathfind.go:1068`.

**Gossip.** Set `InboundFee` on the directed channel from the iterated
node's own end policy, which is exactly what
`graph/db/graph_cache.go:215` does. That single line gives the lnd arm
its full production behavior with no other change.

There is a second, uglier decision. `models.CachedEdgePolicy` also
carries an `InboundFee fn.Option[lnwire.Fee]`
(`graph/db/models/cached_edge_policy.go:54`), and on a
`DirectedChannel.InPolicy` that field describes the *other* node's
inbound fee, not the iterated node's. A candidate that reaches for
`ch.InPolicy.InboundFee` instead of `ch.InboundFee` gets the wrong
node's number and mis-prices systematically. Two options:

- Populate both faithfully and document the distinction loudly in the
  candidate contract comment and the harness background prompt. This
  keeps the sealed view an exact replica of what lnd's own cache
  presents, which is the program's fidelity invariant.
- Populate only `DirectedChannel.InboundFee` and leave the policy option
  as `fn.None`, removing the trap at the cost of a deliberate
  divergence from lnd's cache.

Recommendation: populate both. The whole point of the sealed view is
that it is the real surface, and a documented sharp edge is better than
an undocumented simplification. But this is question 2 for the lead,
because it spends evolution budget on a footgun.

### Objective change

None. Inbound fees change what a route costs, and fees are already
scored.

### Falsifiable hypothesis

**H-B1 (inbound fees select for reading both directions of a channel).**
Every candidate in the program builds a directed adjacency list from one
policy per edge (`cmd/routesim/candidate_impl.go:128` is the pattern
every descendant inherited). An inbound fee is attached to the receiving
node and is therefore invisible in that representation. Prediction: an
evolution arm on an inbound-fee tier will produce a router that carries
a per-node inbound term into its edge cost, and that router will show a
lower realized `fee_ppm_on_success` than its ancestor at equal success.
Falsified if the evolved router still scores edges from the outbound
policy alone and loses nothing by it.

**H-B2 (inbound fees are worth more to lnd than to the champions).**
lnd's pathfinding already prices inbound fees correctly and caps them
correctly. The champions price fees as a fixed-weight proportional
penalty and nothing else: mx_c3 uses `5.0 * fee / deliver`
(`simulation/champions/router_mx3_generalist_v1.go:929`) and hb1 uses
`15 * fee / deliver` (`simulation/champions/router_hb1_v1.go:521`).
Prediction: switching inbound fees on narrows the champion minus lnd fee
gap and does not change the success ordering. Falsified if the ordering
moves, which would be a much larger result and would belong in the
"what could kill this" ledger below.

Because 4,660 of the 4,783 real inbound fees are discounts, the
mainnet-loaded arm mostly makes certain hubs *cheaper* than the current
sim believes. The first-order prediction is therefore that realized fees
fall for everyone and that the routers which can see the discount
capture more of the fall.

### Implementation scope

Files: `routing/sim_graph.go` (two fields, one method, `checkPolicy`
signature and body, one gossip line), `routing/sim_load.go` (two
parses), `routing/sim_topology.go` (synthetic draws),
`simulation/gen_scenarios.py` (one section). Roughly 200 lines of Go, 60
of Python.

Test surface: a golden proving that with both inbound fields zero,
`checkPolicy` accepts and rejects exactly the amounts it accepts and
rejects today (this is the load-bearing identity test, because
`checkPolicy` is on the hot path of every payment ever run); a table
test transcribed from `htlcswitch/link_test.go:653`
(`TestChannelLinkInboundFee`) so the sim's arithmetic is checked against
lnd's own cases; a negative-fee floor test; an exit-hop test; and a
gossip test asserting that the iterated node's own inbound fee, not its
peer's, lands on `DirectedChannel.InboundFee`.

---

## Mechanism C: fees as a first-class cost

### What exists today

Fees are charged, conserved, and scored, but nothing binds.

Charging and conservation are correct. `walkHtlc` moves `amtOut` across
each hop (`routing/sim_graph.go:554`), so a forwarding node nets the
difference between what it received and what it sent, which is its fee.
Fee sufficiency is enforced (`routing/sim_graph.go:593`).

Scoring is where the gaps are, and there are three.

First, **there is no fee ceiling anywhere.** The lnd arm is constructed
with `FeeLimit: lnwire.MaxMilliSatoshi`
(`routing/sim_router.go:224`), so pathfinding's fee-limit check
(`routing/pathfind.go:829`) never fires. `SimPaymentSpec`
(`routing/sim_router.go:39`) has no fee field to give a candidate, so no
candidate has ever been told a budget. A fee-blind router is never
punished with a failure, only with a small subtraction.

Second, **fees paid on payments that failed are dropped from every
metric.** `RunScenarioFrom` accumulates `result.FeeMsat` on each settled
shard (`routing/sim_run.go:828`), and the aggregate adds it only when
the payment succeeded (`cmd/routesim/main.go:459`). On a non-atomic
tier, which is most of them, a partially settled MPP that then fails has
genuinely moved liquidity and genuinely paid forwarding nodes, and no
number in the output records it. Under `atomic_mpp` the money is
correctly returned (`routing/sim_run.go:738`), which is why the flag
exists, but the flag is off on the hard, OOD and mainnet tiers.

Third, **the fee metric is a ratio over successes only.**
`FeePPMOnSuccess = 1e6 * TotalFeeMsat / AmtSuccessMsat`
(`cmd/routesim/main.go:494`). Abandoning the most expensive payment in a
file lowers the numerator's share and raises nothing, so the metric is
mechanically improvable by giving up.

The objective weights are `FEE_WEIGHT = 0.00002` with
`FEE_PPM_CAP = 5_000` (`simulation/evaluate.py:27`), applied at
`simulation/evaluate_code.py:130`, for a maximum fee penalty of 0.1
against a maximum attempt penalty of 0.15.

### Proposed change

**Put the pressure in the environment, not in the weight.** A new
per-scenario field and a file-level default:

```json
{"target": "...", "amt_msat": 1000000000, "max_parts": 4,
 "fee_limit_ppm": 3000}
```

Enforcement lives in the runner, at the same shared point that
`deliverAttempt` occupies. Before dispatching a route the runner checks
whether `rt.TotalFees()` plus the fees already committed by settled or
held shards would exceed the budget. If it would, the route is not sent:
the runner records the attempt, and reports to the router a synthetic
failure attributed to the source (index 0) with a new
`SimFeeLimitFailure` type, built on the `SimUnknownFailure` pattern
(`routing/sim_attribution.go:94`), so that a router which switches on
failure codes lands somewhere sensible rather than being told something
untrue.

Putting it in the runner rather than in each router is what makes the
comparison paired, and it also makes the constraint real for the lnd arm
without touching lnd's code: `newSimLightningPayment` sets
`FeeLimit` from the spec instead of `MaxMilliSatoshi`, so lnd's
pathfinder prunes on it natively, and the runner's check is then a
belt-and-braces backstop that should never fire for lnd. For candidates,
`SimPaymentSpec` gains a `FeeLimitMsat` field, which is legitimate
information: a real sender knows its own budget.

**Fix the accounting regardless of the rest of the stage.** Three new
aggregate fields:

- `total_fee_msat_spent`: every millisatoshi that actually left the
  sender, including on payments that failed. On atomic tiers this equals
  the current number; on non-atomic tiers it exposes the leak.
- `fee_ppm_attempted`: spent fees over attempted amount, the version of
  the ratio that abandonment cannot launder.
- `fee_limit_failures`: attempts the runner refused to dispatch.

These are reporting only, in the tradition of `give_up_rate` and
`bg_settle_rate` (`cmd/routesim/main.go:116` and `:136`), which exist so
that a defect is visible in every run's output instead of needing a
manipulation check to find.

### Objective change, and the give-up arithmetic

The directive asks whether "first class" means raising `FEE_WEIGHT` or
removing the cap. It should mean neither, at least not first, and the
arithmetic says why.

The corpus files hold between 6 and 10 scored payments each (checked
across `simulation/lab/scenarios/hard-test`, `ood-test` and `mainnet`;
`gen_scenarios.py:gen_example` draws `randint(6, 10)`). Abandoning one
payment in the smallest file costs `1/6 = 0.167` of objective. The
entire fee term is worth at most `0.00002 * 5000 = 0.100`. So today the
fee term is *structurally incapable* of paying for abandonment: even
dropping the single most expensive payment and reducing the fee penalty
to zero loses money. The margin is a factor of 1.67, and it is the only
thing standing between the fee term and the exp-013 attractor.

That gives a design rule with a number in it:

> **The fee term's maximum value must stay strictly below `1/N`, where
> `N` is the payment count of the smallest scored file.** With `N = 6`,
> `FEE_PPM_CAP * FEE_WEIGHT < 0.167`. The current 0.100 satisfies it
> with 1.67x of headroom. Raising `FEE_WEIGHT` by 2x breaks it. Removing
> the cap breaks it unconditionally.

If the lead nonetheless wants a stronger fee term, the safe form is not
a bigger weight on the same metric. It is a *different metric*:
`fee_ppm_attempted` instead of `fee_ppm_on_success`. Abandonment cannot
improve that ratio (the abandoned amount stays in the denominator and
the attempt's fees, if any, stay in the numerator), so the 1/N argument
no longer binds and the weight can rise. That is the change worth
running, and it should run as its own pre-registered arm, scored
side by side with the current objective on the same runs.

Guards, in addition to the rule above:

- The evaluator hint gains one sentence in the same unconditional style
  the exp-017 rewrite established: fees fall for two reasons, cheaper
  routes and fewer completed payments, and only the first is an
  improvement, so read `fee_ppm` against `success_rate` exactly the way
  attempts are read against it.
- The validation protocol gains a hard gate: **a candidate whose success
  rate falls below its own seed's on any tier is disqualified,
  regardless of composite objective.** exp-013's winner would have been
  caught by this at the point of proposal rather than at the point of
  the five-tier sweep.
- `fee_limit_failures` and `num_give_ups` are read together. A router
  that stops sending because everything is over budget looks identical
  in the objective to one that never found a route, and only these two
  counters separate them.

### Falsifiable hypothesis

**H-C1 (a binding fee limit converts fee blindness into failures).**
Prediction: at a fee limit set to the current median realized
`fee_ppm_on_success` across routers, every router's success falls, and
lnd's falls *least*, because its pathfinding minimizes a calibrated
combination of fee, timelock and risk while the champions apply a
fixed-weight fee penalty with no budget tracking at all. Falsified if
the champions' success falls no faster than lnd's, which would say their
route selection is already fee-efficient as a side effect of being
risk-efficient.

**H-C2 (fee budgets select for budget-aware search, not cheaper
search).** Prediction: an evolution arm on a fee-limited tier produces a
router that tracks remaining budget across shards and re-plans when a
shard's fee would consume it, rather than one that simply lowers its fee
weight. This is the MPP-specific version of the constraint and it has no
analogue in lnd's splitter. Falsified if the evolved router's only
change is a larger fee penalty coefficient.

### Implementation scope

Files: `routing/sim_router.go` (`SimPaymentSpec` field, `FeeLimit`
plumbing), `routing/sim_run.go` (budget tracking in the attempt loop,
the refusal path, new result fields), `routing/sim_attribution.go` or a
sibling for the new failure type, `cmd/routesim/main.go` (aggregate
fields, scenario field), `simulation/evaluate.py` and
`simulation/evaluate_code.py` (the alternative metric and the hint
sentence), `simulation/gen_scenarios.py`. Roughly 350 lines of Go, 100
of Python.

Test surface: identity test that an absent `fee_limit_ppm` reproduces
today's traces exactly; a test that lnd's own pruning and the runner's
backstop agree (the backstop must never fire on the lnd arm); a test
that a candidate returning an over-budget route gets the refusal rather
than a settlement; an accounting test that `total_fee_msat_spent` equals
the sum of the sender's balance decrease minus the amount delivered, on
both atomic and non-atomic tiers, which is the property the current
metric quietly violates.

---

## Mechanism D: concurrent payments

### What exists today

Everything is strictly sequential, and three different kinds of
contention already exist that this mechanism must be distinguished from.

`runBatch` walks the scenario list one at a time
(`cmd/routesim/main.go:449`). `RunScenarioFrom` runs one payment from
the first route request to its resolution
(`routing/sim_run.go:669`), building the router for that payment alone
(`routing/sim_run.go:703`) from a `LocalBalances` snapshot taken at
construction (`routing/sim_graph.go:247`). The attempt loop
(`routing/sim_run.go:749`) is a single sequence.

The three existing contention mechanisms, none of which is this one:

1. **Intra-payment shard contention (exp-010b).** Under `atomic_mpp`, a
   shard that reaches the destination reserves rather than settles
   (`routing/sim_graph.go:406`), reservations reduce `available()`
   (`routing/sim_graph.go:66`), and the liquidity check reads
   `available()` (`routing/sim_graph.go:542`). Sibling shards of the
   *same* payment therefore contend. One router, one belief stream, one
   planning context.
2. **Exogenous background traffic (exp-014).** Other people's payments
   move liquidity in the gaps (`routing/sim_traffic.go`), degree
   weighted (`:158`), with a `focus_fraction` share aimed at the
   scenario's own corridors (`:189`). This is the environment, not a
   player: it never uses `SimRouter` and it consults hidden balances by
   privilege (`:420`).
3. **Attempt-boundary drift.** Time passes per attempt and traffic runs
   in it under atomic mpp (`routing/sim_run.go:572`).

What none of these provide is the vantage node running several of its
*own* payments at once, racing for its own outbound liquidity, with
results interleaved. That is the mechanism.

### Proposed change

**Scheduling model.** A `concurrency` section:

```json
"concurrency": {
  "max_in_flight": 4,
  "arrival": "window",
  "inter_arrival_sec": 30
}
```

`arrival: "window"` keeps at most `max_in_flight` payments live and
starts the next as soon as one resolves. `arrival: "poisson"` is
specified but deferred to a later stage, because an arrival process
interacts with the traffic prorating carry
(`routing/sim_run.go:593`) in ways that need their own calibration run.

**Execution model: a deterministic virtual-time event loop, not
goroutines.** This is not negotiable and the reasons are concrete.
`SimGraph` has no locking on balances or on the holds map
(`routing/sim_graph.go:130`). The traffic rng is a single stream whose
draw order defines the exogenous process
(`routing/sim_traffic.go:150`). The attribution degrader consumes a
fixed number of draws per attempt *specifically* so that two routers
face the same sequence (`routing/sim_attribution.go:118`). Real
parallelism would destroy every one of those invariants and with them
the reproducibility that all eleven sealed tiers depend on.

So: each live payment becomes a small state machine with a `nextEventAt`
virtual timestamp. The scheduler repeatedly picks the payment with the
earliest timestamp, breaking ties by scenario index, advances the clock
to it, runs the background traffic owed for that interval through the
existing prorating path, and executes exactly one step of that payment
(one `RequestRoute`, one dispatch, one `ReportAttempt`). At
`max_in_flight = 1` the loop must reduce to today's behavior exactly,
and that is the identity test.

**Router instances: one per payment, sharing the belief store.** This is
the real design decision, and the argument for it has three legs.

The first is the contract. `SimRouterFactory` is called once per payment
today (`routing/sim_router.go:113`). Keeping that means all seven
existing routers run on the new tier unchanged and remain comparable to
every prior tier. A single shared router instance would need
`RequestRoute` and `ReportAttempt` to carry a payment identifier, which
breaks every candidate ever evolved and makes the arm incomparable, and
exp-010b already established that the contract changes only under
duress.

The second is that the sharing already exists. Every evolved router
keeps its beliefs in package-level state that outlives the instance, and
every one of them already guards it with a mutex:
`simulation/champions/router_mx3_generalist_v1.go:73`,
`router_hb1_v1.go:62`, `router_hb2_v1.go:76`,
`exp-010b-atomic1-best-candidate.go:68`,
`exp-018-omni1-best-candidate.go:57`, and
`exp-008-drift1-best-candidate.go:72` (an `RWMutex`). `sim_weights.go:336`
already relies on this to deliver imports. So "per-payment instance over
a shared store" is not a new architecture, it is the one that evolved.

The third is that the interesting pressure survives the choice. What
concurrency adds is that a payment's view of its own outbound liquidity
is stale the moment another of its own payments takes some. And
`LocalBalances` already returns `end.available()`
(`routing/sim_graph.go:258`), which is net of holds, so a payment
starting while an earlier one holds shards already sees the reduced
balance. That is exactly right and it comes for free.

What does not come for free is mid-payment refresh: the router holds the
map it was handed at construction and nothing updates it. The proposal
is an *optional* interface, in the exact shape of
`SimObservationImporter` (`routing/sim_weights.go:64`):

```go
// SimBalanceRefresher is the optional half of the contract a router
// implements if it wants to be told that its own outbound liquidity
// changed under it. A router that does not implement it keeps the
// snapshot it was built with, which is what every router does today.
type SimBalanceRefresher interface {
        RefreshLocalBalances(balances map[uint64]lnwire.MilliSatoshi)
}
```

with an aggregate field `router_accepts_balance_refresh`, mirroring
`import_router_accepts` (`cmd/routesim/main.go:126`), so that "refresh
did not help" is distinguishable from "refresh was never delivered".
exp-016 had to add importer variants of two champions after the fact
because nothing in the contract had ever asked for the capability, and
this stage should not repeat that.

**Holds.** Contention between concurrent payments is only interesting
when in-flight htlcs reserve. The concurrency tier should therefore set
`atomic_mpp`. A concurrency tier without holds is a strictly weaker
arena and is available for free as a control.

**Scoring aggregation.** Per-scenario metrics are unaffected: success,
attempts and fees are all per-payment sums and stay comparable to the
sequential control file for file and payment for payment. New reporting:

- `max_concurrent` and `mean_concurrent`, the manipulation check. If a
  file's payments do not actually overlap, the tier is not testing
  anything, and exp-012's staleness null is the cautionary tale for
  shipping a knob without one.
- `self_contention_failures`: attempts that failed on a channel where
  the sender's own other payment held liquidity at that moment. The
  runner owns the holds map and can attribute this exactly. This is the
  number the whole stage exists to produce.
- `makespan_sec`: virtual time to clear the batch.

`makespan_sec` does **not** enter the objective in this stage. It is a
new axis trading against success in an unmeasured way, and the program's
rule is one change at a time.

### Falsifiable hypothesis

**H-D1 (concurrency selects for a self-versus-world belief split).**
This is exp-018's idea ledger entry, dual belief ledgers separating
own-shard contention from standing balance, and concurrency is the
environment that would pay for it. Prediction: an evolved router on the
concurrency tier will maintain a per-local-channel in-flight count and
will *not* write a persistent liquidity bound when a first-hop failure
is explained by its own reservation. Measurable directly by counting
persistent bound writes on source-local channels. Falsified if the
evolved router writes the same bounds and loses nothing.

**H-D2 (the champions poison their own first hop).** The champions
cannot distinguish self-inflicted first-hop failure from a depleted
channel, so under concurrency they should write false `upperFail` bounds
on their own channels and show rising attempts on later payments in the
file. This is the same failure shape exp-012 found when importing
observations about a consumer's own channels, arrived at from the
opposite direction, and if it reproduces it is a strong result: the
damage from local-channel beliefs is not an artifact of importing, it is
a property of the belief representation.

**H-D3 (lnd degrades gracefully here).** Mission control is shared
across payments by construction (`routing/sim_run.go:132`) and lnd's
production router really does run many payments against one mission
control, so lnd's arm gets its real behavior for free. Prediction: lnd
loses less from concurrency than the champions do. If H-D2 holds and
H-D3 holds, this stage narrows the gap, which belongs in the
pre-registered outcomes below.

### Implementation scope

The largest stage by a wide margin. Files: `routing/sim_run.go` (the
attempt loop becomes a resumable step function, the clock and traffic
plumbing moves from per-payment to scheduler-owned, roughly a rewrite of
`RunScenarioFrom` into a state machine plus a scheduler), a new
`routing/sim_concurrency.go` for the scheduler, `routing/sim_router.go`
(the optional refresher interface), `cmd/routesim/main.go` (`runBatch`
delegates to the scheduler, new aggregate fields),
`simulation/gen_scenarios.py`. Roughly 700 to 900 lines of Go including
tests, and this is the stage where the estimate is least trustworthy.

Test surface, and this is where the golden discipline earns its keep:

- **The `max_in_flight = 1` identity test is the load-bearing one.** Run
  every checked-in sealed tier through the scheduler at concurrency 1
  and assert the full result JSON is byte identical to the pre-change
  binary's. If that does not hold, nothing published survives.
- A determinism test: same file, same seed, same schedule, twice.
- A contention test in the style of `TestSimAtomicMppShardContention`
  (`routing/sim_atomic_test.go:377`) but across two payments.
- A test that the traffic prorating carry (`routing/sim_run.go:593`)
  produces the same total background volume for a given virtual duration
  under the scheduler as under the sequential loop.
- A refresher test asserting `router_accepts_balance_refresh` is false
  for a plain router, in the style of
  `TestRouterAcceptsImportsFalseForPlainRouter`
  (`routing/sim_weights_test.go:273`).

---

## Mechanism E: latency as a cost

### What exists today

Virtual time advances by a flat `AttemptSec` per attempt
(`routing/sim_run.go:572`), independent of route length, and runs the
background traffic for that slice only when `atomic_mpp` is set
(`routing/sim_run.go:582`). `SimClockParams` has exactly two step sizes,
`PaymentGapSec` and `AttemptSec` (`routing/sim_run.go:230`). The
attribution knob can hold a result back by whole attempt-sized slices
(`routing/sim_attribution.go:47`, applied at
`routing/sim_run.go:494`). The objective's attempt term is documented as
a latency proxy (`cmd/routesim/main.go:104`).

So latency exists as a uniform tick with no dependence on the route and
no direct price.

### Proposed change

A `latency` section replacing the flat tick:

```json
"latency": {
  "per_hop_ms": 300,
  "attempt_overhead_ms": 250,
  "hold_carry": true
}
```

Attempt duration becomes `attempt_overhead + 2 * per_hop * k`, where `k`
is the number of hops the htlc actually traversed before resolving: the
full route on a settle, and the index of the failing hop on a failure.
That asymmetry is the point and it is real. A failure at the sender's
own first hop comes back in one round trip; a failure at hop eight comes
back in eight. A router that probes near before probing far learns
faster in wall time even when it learns the same amount per attempt.

Time then flows into everything that already reads the clock: background
traffic through the existing prorating (`routing/sim_run.go:593`), held
liquidity duration under `atomic_mpp`, and, under stage D, the
scheduler's ordering key, so a slow router's payments overlap more and
self-contend more.

Reporting: `mean_payment_latency_sec` and per-attempt latency in the
trace, which is what makes the objective question answerable offline.

### Does it enter the objective?

Not in this stage. Report it, and then run **one pre-registered
re-scoring arm** on the same run outputs, with no re-execution needed:

> **Alternative objective L:** replace
> `0.01 * min(extra_attempts, 15)` with
> `w_t * min(payment_latency_sec, cap)`, calibrated so the mean penalty
> on the current champion equals the mean attempt penalty it pays today.

That substitution is the deepest reading of "latency as a cost", and it
matters because the attempt axis is doing work it should not. Three
parallel shards cost one unit of time and three units of attempt
penalty. A nine-hop route and a two-hop route cost the same attempt
penalty. exp-019 already retired the 8.6x attempt headline as a
perfect-channel artifact; objective L is the question of whether the
attempt axis itself was ever measuring the thing it claimed to.

### The exp-019 delay finding, and why this is not obviously the same null

exp-019 found that pure delay is free for everyone, and exp-015 found
that decay is a tie at eighteen times the churn. A reasonable prior says
latency is another null. The design has to take that seriously.

Delay in exp-019 is **uniform and absolute**. It holds every result back
by the same number of slices regardless of the route, and no evolved
router's behavior depends on absolute time, since they all dropped time
decay. The only channel through which uniform delay could bite is
background-traffic drift, and exp-015 already measured drift as a tie.
A null was overdetermined.

Latency as proposed differs in two ways that the delay knob structurally
could not have. It is **differential**: cost depends on the route, so it
changes the *ranking* of routes, not just the clock. A knob that shifts
absolute time cannot change route choice; a knob that changes relative
time can. And it is **coupled to a scarce resource** once stage D
exists: time on the clock is time holding liquidity and time overlapping
with your own other payments.

The experiment is therefore designed to separate these:

- **E-a, latency alone, no concurrency, no objective term.** Prediction:
  null, confirming exp-015 and exp-019. If it is not null, that is the
  more interesting outcome and it means route-length-dependent drift
  bites where uniform drift did not.
- **E-b, latency on top of concurrency.** Prediction: not null, because
  latency now buys contention.
- **E-c, objective L re-scoring, offline.** Prediction: the champion
  ordering is unchanged but the *margin* against lnd changes sign on at
  least one tier, because lnd's many-attempt style is penalized less by
  a time metric than by an attempt count when its attempts are short and
  parallel.

E-a returning a null is a **reportable confirmation**, not a failure,
and the spec says so in advance so that nobody is tempted to keep
turning knobs until it stops being null.

### Implementation scope

Files: `routing/sim_run.go` (attempt duration computation, latency
reporting), `cmd/routesim/main.go` (aggregate and trace fields),
`simulation/evaluate.py` (objective L as a separate scoring function,
not a change to the default), `simulation/gen_scenarios.py`. Roughly 200
lines of Go, 80 of Python.

Test surface: identity test that an absent `latency` section reproduces
the flat `AttemptSec` tick exactly; a test that a failure at hop `k`
advances the clock by the `k`-hop round trip and not the full route; a
test that total background volume over a fixed virtual window is
unchanged by how that window was divided.

---

## Corpus and validation design

### Tiers

Every stage adds paired tiers built by transforming an existing control
rather than by generating a fresh world, so per-file pairing stays exact.
This is the exp-017 method (thirteen paired tiers, untouched control
reproducing exp-009 to three decimals before the sweep was allowed to
proceed) and the `simulation/lab/scenarios/README.md` rule that anything
derived from a sealed tier is a transformation of the original.

| tier | derived from | knob |
|---|---|---|
| `htlc-limits-empirical` | sealed hard tier | stage A, empirical family |
| `htlc-limits-tight` | sealed hard tier | stage A, authored stress rung |
| `inbound-mainnet` | mainnet tier | stage B, real policies loaded |
| `inbound-synth` | sealed hard tier | stage B, empirical draws |
| `feelimit-{loose,median,tight}` | sealed hard tier + mainnet | stage C ladder |
| `concurrent-{2,4,8}` | atomic tier | stage D ladder |
| `latency-only` | atomic tier | stage E-a |
| `latency-concurrent` | `concurrent-4` | stage E-b |

Controls: the untouched sealed hard tier, OOD tier, mainnet tier and
atomic tier, run in the same sweep, with the exp-020 gate applied
first. **No stage's results count until its control run reproduces the
published numbers to three decimals.** exp-020 exists because that gate
was not always applied, and it found a sealed tier that had been
silently overwritten.

### Routers

The standing five, unchanged across all stages: lnd (defaults), the
hand-written seed, hb1, mx_c3, atomic1. The seed matters more here than
usual: it was never fit to anything, so if margins compress on an
economic-realism tier and the seed compresses with the same shape, that
is a difficulty ceiling rather than the champions' constants failing.
That inference is exp-017's and it is the single most useful control
this program has.

### Verdict criteria, pre-registered

For each tier, per file, paired:

1. Mean composite objective with a 95% percentile bootstrap CI
   (`simulation/sweep_validate.py:60`).
2. Mean paired delta against the named baseline, its bootstrap CI, and a
   two-sided sign test (`simulation/sweep_validate.py:73`).
3. **Success rate and attempts per payment reported and read
   separately**, always, with `num_give_ups` and (stage C)
   `fee_limit_failures` alongside.
4. A directional claim requires the CI to exclude zero *and* the sign
   test to reach p < 0.05. A claim that a mechanism was elicited
   additionally requires the mechanism to be visible in the evolved
   source, not merely in the score.
5. **Champion swaps follow the exp-020 rule unchanged:** no title
   changes without a paired sweep on the original tier set. Two
   independent significant signals pointed at hb1 and neither
   replicated, and that is the whole reason the rule exists.
6. **Abandonment gate (new, stage C onward):** a candidate whose success
   rate falls below its own seed's on any tier is disqualified
   regardless of composite objective.

### Power

exp-017's hubdrain tier was underpowered at n=10 and yielded no
verdicts. Stage D raises per-file variance by construction, since a
payment's fate now depends on its siblings. Budget for n=20 files on the
concurrency tiers, and pre-register that a straddling CI at n=20 on a
concurrency tier is reported as underpowered rather than as a null.

---

## Sequencing: why five stages and why this order

They should land as five separately flag-gated stages, not one release.
Three reasons. Each stage's off state must be provably byte identical,
and five simultaneous flags make that proof a product rather than a sum.
Each stage has its own null hypothesis and its own paired sweep, and a
combined change cannot attribute a movement. And stage D is a runner
rewrite whose blast radius covers every other stage, so it should rebase
onto settled ground rather than the reverse.

The order argued: **A, B, C, D, E.**

A first because it is generator-only, changes no simulator semantics,
needs no objective decision, and immediately fixes the most embarrassing
finding of this survey, which is that four fields every candidate reads
have carried constants on every synthetic tier for the whole program. It
also de-risks C, because a fee ceiling and a shard-size floor interact.

B second because it is a small, exactly specified semantics change with
lnd's own code as the reference implementation and lnd's own test table
as the oracle, and because it recovers 4,783 real policies the mainnet
loader has been discarding. It is the cheapest real fidelity win
available.

C third because it is the first stage that touches scoring, and it
should not do that until the two constraint stages have shown what the
realized fee distribution actually looks like. The calibration run (all
five routers, all tiers, reporting `fee_ppm_on_success` and
`fee_ppm_attempted`) is a prerequisite for choosing the ladder, and it
can run during A and B.

D fourth because it is the runner rewrite. Putting it earlier means
every later stage rebases onto a moving loop, and putting it last means
E has nothing sharp to ride on.

E last because its only non-null form rides on D, and because the
objective-L re-scoring arm needs D's latency reporting to be
meaningful.

The counter-argument worth recording: D is the highest-information
stage, it is the one whose hypotheses (dual belief ledgers, first-hop
self-poisoning) connect directly to open ideas in the ledger, and a
program with limited budget might reasonably do D first and treat A, B
and C as fidelity chores. If the lead wants one stage only, it should be
D. This is question 3 in the final section.

---

## What could kill this

Pre-registered outcomes. Each of these is a result, not a failure, and
naming them now is what keeps them from being explained away later.

**Economic realism could close the champion gap rather than widen it,
and that is the most likely single surprise.** lnd's pathfinding prices
fees, inbound fees, timelocks, min and max HTLC and success probability
in one calibrated weight function, and it has done so for years. The
champions price fees as a single fixed-weight proportional penalty
(`router_mx3_generalist_v1.go:929` uses `5.0 * fee / deliver`;
`router_hb1_v1.go:521` uses `15 * fee / deliver`), have no fee budget,
have no concept of an inbound fee, and cannot distinguish their own
in-flight reservations from a depleted channel. Every one of the five
mechanisms above adds pressure on an axis where lnd is well engineered
and the champions are, at best, incidentally competent. If the champion
minus lnd margin narrows monotonically across stages A through D, the
honest reading is that the champions' edge is *informational*, a better
model of hidden liquidity, and that it does not extend to *pricing*,
and that a production router wants both. That is a publishable finding
and arguably a more useful one for upstream than another win, because it
tells you which half of the evolved design to port.

**A stage could be a pure null for a reason that is about our
generators, not about the network.** Stage A is null if we assign max
HTLC values that never bind. Stage B is null if the synthetic inbound
family is too small to matter, though the mainnet arm cannot be null by
construction since it recovers real data. Stage E-a is *expected* to be
null. The guard is that each stage's tier ships with a manipulation
check (`max_concurrent`, `fee_limit_failures`, count of binding max-HTLC
edges per file) so that "the mechanism did not matter" is always
distinguishable from "the mechanism never fired", which is exactly the
distinction `import_router_accepts` was added for in exp-016.

**Any strengthened cost term creates a new cheap direction.** This is
exp-013, and the section on stage C gives the arithmetic and the guards.
The specific danger here is subtler than exp-013's: a fee ceiling makes
*giving up early* look like *respecting the budget*, and both produce
low fees and low attempts. `fee_limit_failures` next to `num_give_ups`
is the only thing that separates them.

**Concurrency could destroy reproducibility.** Every sealed tier and
every published number depends on a fixed draw order. The deterministic
event loop is designed to preserve it, but the failure mode is silent:
a scheduler that reorders traffic draws produces plausible numbers that
do not match anything. The `max_in_flight = 1` byte-identity test across
all sealed tiers is the only real defense, and it should gate the merge.

**Variance could swamp the concurrency verdicts.** See the power note.

**Scope creep into a fee market.** Nodes here do not adjust their fees
in response to flow, do not rebalance, and do not respond strategically.
`IDEAS.md:44` already flags that a real fee-market simulation is a
separate project. Everything in this spec takes policies as exogenous.
Saying so now is what keeps stage C from turning into one.

**We could be authoring worlds again.** exp-017 closed the
generator-family question with the caveat that every world tested was
still one we chose. Stages A and B partly escape it, because both draw
from measured mainnet marginals and stage B's mainnet arm uses the real
values directly. Stages C, D and E do not escape it at all: a fee limit,
an arrival rate and a per-hop latency are all authored. The full escape
is still offline replay, and nothing here changes that.

---

## Budget

Implementation, in engineer-days, assuming the tree is free:

| stage | Go | Python | tests | total |
|---|---|---|---|---|
| A | 0.5 | 0.25 | 0.5 | ~1.25 |
| B | 0.5 | 0.25 | 0.75 | ~1.5 |
| C | 0.75 | 0.5 | 0.75 | ~2 |
| D | 2 | 0.5 | 1.5 | ~4 |
| E | 0.5 | 0.25 | 0.5 | ~1.25 |
| corpus and sweep tooling | | 1 | | ~1 |

Total roughly 11 days of implementation, of which stage D is a third and
carries all of the schedule risk.

Evolution budget. One arm per stage at the program's standard 400 evals
is the honest cost, because validating routers evolved on old tiers
against new economics tests robustness rather than the paradigm under the
new economics. Historical reference points: exp-018 ran three arms at 150
evals each, with the xhigh arm stretching to nine hours after timeouts,
and the searcher defaults have since been retuned to high/900s. A 400-eval
codex arm has historically run overnight. So budget one overnight run per
stage, five total, plus one continuation arm if any stage produces a
challenger worth continuing. Stage D's arm should be the most expensive
and is the one most likely to need a second swing, since it is the only
stage that also adds an optional contract surface.

Sweep sizes, per stage: 5 routers x 10 to 20 files x 2 to 4 tiers, so 100
to 400 paired runs. At routesim's throughput (well under a second per
synthetic scenario file, longer on mainnet) each sweep is minutes to about
an hour. Sweeps are not the cost; evolution is. exp-017 ran 650 runs and
exp-019 ran 520, both comfortably within a session.

Reserve one day for a calibration run before stage C: all five routers,
all controls, reporting the realized fee distribution, which decides the
fee-limit ladder. Reserve one further day for the stage D identity
sweep across every sealed tier, because that is the gate that protects
everything already published.

---

## Open questions left for the lead

1. **First-hop policy enforcement (stage A).** The simulator exempts the
   source from its own announced policy (`routing/sim_graph.go:524`)
   while lnd's pathfinder filters local edges on it
   (`routing/unified_edges.go:268`). Under stage A this becomes a real
   asymmetry: lnd will refuse routes the simulator would have carried,
   handicapping the lnd arm alone. Options are to keep the simulator's
   BOLT-faithful behavior and let lnd self-handicap (honest, but a gift
   to candidates), to make the simulator enforce the source's own
   announced limits (matches lnd, diverges from the protocol), or to
   forbid the generator from assigning binding limits to source
   channels (avoids the question, at the cost of removing a realistic
   pressure). I lean toward the first with an explicit note in the
   writeup, but this changes the arm's meaning and is the lead's call.

2. **Inbound fee exposure shape (stage B).** Populate both
   `DirectedChannel.InboundFee` and `InPolicy.InboundFee` faithfully,
   reproducing lnd's cache exactly including the trap that the two
   describe different nodes, or populate only the unambiguous one and
   accept a documented divergence from the real gossip surface. Fidelity
   argues for the first; not spending evolution budget on a footgun
   argues for the second.

3. **Stage ordering under a constrained budget.** The spec argues A, B,
   C, D, E on dependency and blast-radius grounds. If only one stage can
   run, it should be D, on information grounds: its hypotheses connect
   directly to the open idea ledger (dual belief ledgers, first-hop
   self-poisoning) and it is the only stage that tests something no
   prior experiment has touched. If only two, D and B. The lead should
   decide whether this is a fidelity program (do them all, in order) or
   a hypothesis program (do D, and treat the rest as chores).

Two smaller ones, recorded but not blocking. Whether the fee objective
should migrate from `fee_ppm_on_success` to `fee_ppm_attempted`
unconditionally, since the current metric is launderable by abandonment
regardless of what weight it carries. And whether `makespan_sec` should
ever enter the objective, or whether the attempt term should be
*replaced* by objective L rather than supplemented by it, which is the
strongest form of "latency as a cost" and the one that would most change
what the program has been optimizing.

---

## Lead decisions (2026-07-27, at spec review)

1. **First-hop policy: enforce uniformly.** Under the stage A flag the
   simulator applies the source's announced policy at forwarding time
   like every other hop's. This is the option the spec called "matches
   lnd," chosen because it removes the special case rather than adding
   one: a single rule for all hops, the same constraint visible in
   gossip to every arm, and no divergence between what lnd's pathfinder
   refuses and what the wire would carry. The protocol-fidelity cost is
   noted and accepted; the generator keeps binding limits on source
   channels rare (matching the real graph, where operators rarely
   constrain their own send path), so the rule binds at the margin
   instead of dominating tiers.

2. **Inbound fees: one unambiguous field.** The sealed gossip view is a
   protocol surface, not a replica of lnd's cache, and evolution budget
   spent learning an lnd implementation footgun teaches nothing about
   routing. Expose the inbound fee on the policy of the node that
   charges it, with the direction convention documented in the
   SimRouter contract comment. Fixing the lnd arm's own plumbing so
   `DirectedChannel.InboundFee` is actually populated is in scope for
   stage B: the discovery that lnd's inbound-fee machinery has only
   ever run against a hardcoded zero here makes that fix half the
   point of the stage.

3. **This is a fidelity program.** The directive named all five
   mechanisms, so all five run, in the argued order A, B, C, D, E. If
   the budget tightens mid-program, stage D is the protected stage and
   the re-sequencing conversation happens then, not preemptively.

On the two smaller questions: `fee_ppm_attempted` runs as the
side-by-side pre-registered arm exactly as the spec proposes, and the
migration question is decided by that arm's data, not now. And
`makespan_sec` stays out of the objective for this whole program;
objective L remains an offline re-scoring only, until an experiment
earns it a place.
