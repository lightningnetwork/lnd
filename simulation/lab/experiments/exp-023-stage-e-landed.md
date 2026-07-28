# EXP-023 stage E — landed, with six spec-vs-reality findings

**Date:** 2026-07-28
**Status:** implemented (five commits on `econ-realism`, ending ef93b4bf1
plus this writeup); the evolution arm and the paired tier sweep remain
to run.

Stage E of the economic-realism program is in the tree, and with it the
program's fifth and last mechanism. An htlc attempt is no longer priced
by a flat tick: it costs an overhead plus a round trip to the hop that
resolved it, so a failure at the sender's own first hop comes back in
one round trip and a failure at hop eight comes back in eight. The
scheduler's resolve step splits into a dispatch and a report with that
round trip between them, `--latency` stamps the section onto generated
corpora, and objective L re-scores archived runs on time instead of
attempts.

The objective is UNCHANGED. `makespan_sec` and the payment latencies are
reported and stay out of it, per the lead's decision at spec review;
objective L is an offline re-scoring of archived output and lives in
`simulation/evaluate.py` as a separate function, not as a change to what
the optimizer maximizes.

## Commits

| commit | what |
|---|---|
| e6cf5659c | the `latency` section, its arithmetic, the hop count, two refusals |
| ac3234d76 | the resolve/report split; the round trip charged on the hops crossed |
| 0a3abd6cf | routesim reads the section and reports three timing keys |
| f8c558ef1 | `--latency` on `gen_scenarios.py`, refusal for refusal |
| ef93b4bf1 | objective L, offline, with the 1/N rule enforced rather than stated |

## Schema as landed

```json
"latency": {
  "per_hop_ms": 300,
  "attempt_overhead_ms": 250,
  "hold_carry": true
}
```

An attempt costs `attempt_overhead + 2 * per_hop * k`, where `k` is how
many hops the htlc traversed before it resolved: the whole route on a
settle, and the failing hop on a failure. `per_hop_ms` is the ONE WAY
crossing time, so a hop is charged twice; both step sizes may be zero
individually but not together, and a section that charges nothing is
refused with the instruction to omit it. `hold_carry` is accepted for
schema fidelity and only `true` is honored (finding 2).

The section REQUIRES a clock section. That is stage D's rule arrived at
again for the same reason: with no virtual time nothing the simulator
does moves a clock, so a per-hop price could not be charged anywhere.
It also satisfies the concurrency window's own clock requirement on its
own, since an attempt that costs a round trip is an attempt that takes
time.

`--latency 300` on `gen_scenarios.py`, or
`--latency per_hop_ms=300,attempt_overhead_ms=250` for the long form.
The flag refuses a corpus with no clock rather than emitting a section
the simulator will refuse at the first run, and names the two recipes
that write one: `--drift`, or `--split --atomic`.

Reported fields, all pointers, emitted only when a file asks for the
mechanism and then emitted in full including zeroes:
`mean_payment_latency_sec`, `mean_attempt_latency_sec`, and
`makespan_sec` (which a latency file now gets whether or not it also
asks for concurrency). Per attempt and per payment, `latency_sec` in the
results array.

## How latency composes with the stage D loop and with `delay_slices`

**With the stage D loop.** The scheduler had one step for "the htlc is
in the air": it sent the route, learned what the network did and told
the router, all at one instant a flat `attempt_sec` after the route was
chosen. That step is now two. `resolve` dispatches and learns; `report`
tells the router; the gap between them is the round trip. With no
latency section the gap is zero and the two run back to back inside one
step, so nothing can slip between them and the timeline is the one every
file before this stage produced.

The split falls along the line between what the NETWORK did and what the
SENDER knows. Money moves and liquidity is reserved at dispatch, because
that is when it happens and because a sibling racing this payment has to
contend with a hold that exists from the moment the shard arrives, not
from the moment the news gets home. The router's belief, the amount
still owed, and the settle of a completed hold all wait for the report.

Everything else follows for free, which is what stage D was built to
make possible. Background traffic runs through the existing per-interval
prorating, so a slow attempt watches more of other people's payments. An
atomic shard reserves its liquidity for the whole round trip. Under a
concurrency section the scheduler's ordering key moves, so a slow
router's payments overlap more and self-contend more (finding 4).

**With `delay_slices`.** They compose rather than collide, and they
measure different things:

| | `attribution.delay_slices` (exp-019) | `latency` (stage E) |
|---|---|---|
| shape | uniform, absolute | differential, per hop |
| unit | whole `attempt_sec` slices | milliseconds per hop, doubled |
| depends on the route | no | yes |
| depends on the outcome | no | yes: settle pays for the route, failure for the failing hop |
| applied at | the report step, inside `deliverAttempt` | between dispatch and report |
| can change route ranking | no | yes |

Latency decides when the report step comes due; the delay knob then ages
the network further at the moment it runs, exactly as before this stage
existed. Its draw order is untouched, which is what keeps exp-019
paired. A file may set both, and if it does, `delay_slices` stays
denominated in the clock's `attempt_sec` rather than in the latency
step sizes: the delay knob is exp-019's and this stage did not
re-denominate it.

One asymmetry worth stating because it is the reason to expect anything
here at all. A knob that shifts absolute time cannot change which route
is better, and no evolved router's behavior depends on absolute time
since all of them dropped time decay. A knob that changes RELATIVE time
can. That is the whole difference between exp-019's null and this stage,
and finding 1 is what happened when it was tested.

## Byte identity, proven

Flag off is byte identical, literally, with no halves needed: every new
key is a pointer that is nil when the section is absent, so a file
without one emits exactly the JSON it emitted before this stage.

Against a binary built at the stage D merge (69ee4e727):

- **272 paired whole-output runs, zero diffs.** Sealed hard tier (10
  files), sealed OOD tier (10), the sealed `corpus-mix` train and val
  splits (68), and regenerated default, hard, drift, split, atomic and
  concurrency corpora (48), each on both the lnd and candidate arms.
- **132 mainnet runs, no real mismatch.** 11 files x 2 arms x 3 runs x 2
  binaries, compared as sets of aggregates because of stage B's finding
  4. Two files showed a second variant on the old binary and not the new
  at n=3, `mn_11_uniform` and `mn_55_uniform` (lnd arm); at n=20 both
  binaries produce the same two variants for both. Those are the same
  two files stage B and stage C named independently, so this is the
  known lnd map-iteration wobble reproducing a third time and not a
  change.
- **Generator output tree diff-identical** at a fixed seed either side of
  the change, across nine modes: default, `--hard`, `--drift`,
  `--split`, `--split --atomic`, `--drift --atomic`, and the three that
  already carry a stage section (`--concurrency 4`,
  `--fee-limit-ppm 4000`, `--attribution unknown=0.3,delay=4`).
- **A stamped NO-OP latency section against its own control**, which is
  the load-bearing one and the analogue of stage D's `max_in_flight: 1`.
  A section with `per_hop_ms: 0` and
  `attempt_overhead_ms: attempt_sec * 1000` charges exactly the flat
  tick it replaces, so the machinery is on and the arithmetic is the old
  one. 48 pairs across the drift, atomic and concurrency corpora, both
  arms, zero mismatches, checked four ways: the results array byte for
  byte with `latency_sec` projected out; the aggregate byte for byte
  INCLUDING KEY ORDER with the three timing keys projected out; a key
  census asserting those three are the only additions and nothing
  disappeared; and `mean_attempt_latency_sec` reading back exactly the
  clock's `attempt_sec`. The concurrency corpus is in there deliberately:
  it is what proves the resolve/report split does not change the
  interleaving of a `max_in_flight: 4` batch when the round trip is zero.
- Goldens in the tree: `TestSimLatencyAbsentGolden` pins that with no
  section a one hop route and a two hop route cost the same flat tick and
  no timing key is emitted anywhere;
  `TestSimLatencyLongerRoutesResolveLater` and
  `TestSimLatencyFailureCostsTheHopsCrossed` are the mechanism and its
  asymmetry, read off the sender's own clock;
  `TestSimLatencyHops` pins the hop count including the two caps;
  `TestSimLatencyValidate` pins the refusals;
  `TestSimLatencyDeterminism` runs the same concurrent latency batch
  twice; `TestSimLatencySlowAttemptsChurnMore` pins the indirect channel.

`go test ./routing/...` and `./cmd/routesim/...` are green, and all five
commits build, vet and test standalone.

## Six things implementation taught the spec

### 1. E-a is null in SCORE for both arms and NOT null in TRACE for lnd, and the cause is time decay

The spec pre-registers E-a (latency alone, no traffic, no concurrency) as
a null and asks that it be reported as a confirmation. It is a
confirmation, and it is sharper than the spec asked for.

The check is stronger than comparing scores: with the timing keys
projected out, the whole output of a latency run should be byte identical
to the control's. Ten files, both scales, no traffic, no concurrency:

| arm | rung | outputs identical to control | success | attempts |
|---|---|---|---|---|
| candidate | fast | **10 / 10** | 0.795 -> 0.795 | 34.1 -> 34.1 |
| candidate | slow | **10 / 10** | 0.795 -> 0.795 | 34.1 -> 34.1 |
| lnd | fast | 7 / 10 | 0.583 -> 0.583 | 50.6 -> 50.6 |
| lnd | slow | 6 / 10 | 0.583 -> 0.583 | 50.8 -> 50.8 |

The candidate arm is byte identical at both scales: latency alone moves
literally nothing, which is the strongest form the null could take. The
lnd arm differs on three or four files of ten while its aggregate does
not move at all.

The cause is identified rather than guessed. Re-running the lnd arm with
`penalty_half_life_sec` set to effectively infinite makes it byte
identical too, **10 / 10 at both scales**. So the only thing in the whole
simulator that notices latency-alone is mission control's penalty decay,
and the difference it makes is a handful of route choices that wash out
of every reported number.

That is worth its own sentence, because it is the same shape as several
earlier findings pointing the other way: the one mechanism that responds
to absolute virtual time is the one mechanism every evolved router
DROPPED (exp-008, exp-015). E-a is therefore null for the champions by
construction, and near-null for lnd by accident of a half-life longer
than an attempt.

### 2. `hold_carry` is already unconditionally true, so `false` is refused by name

The spec's schema carries a third field and the spec's prose never says
what it does. The most defensible reading is "charge the time an htlc
spends in the air to everything else that reads the clock", and the
scheduler already does exactly that, by the same rule it applies to
every other stretch of virtual time: traffic runs when nothing is live
or when anything live is atomic (stage D).

So the field is accepted for schema fidelity and only `true` is honored.
`false` is REFUSED by name at both ends, with the reason in the message,
which is the treatment `poisson` got in stage D. Opting the latency
window out of the exogenous process would mean a latency section quietly
editing exp-014's traffic engine, and that is not a decision a timing
knob gets to make.

### 3. A first-hop failure costs one round trip, which is the spec's reading and not obviously the right one

The spec says `k` is "the index of the failing hop" and glosses it: "A
failure at the sender's own first hop comes back in one round trip; a
failure at hop eight comes back in eight." `walkHtlc` blames the SENDING
end of the hop it refused, so the node at index `i` reporting a failure
means hop `i+1` did not carry the htlc, and `k = i + 1`. A liquidity
shortfall on the sender's own channel is blamed on the sender, index 0,
so it costs `k = 1`.

That is implemented as written and pinned by test, and it is question 1
for the lead. A real sender learns its own link is short without any
network round trip, so the protocol-faithful reading is `k = i`, which
makes a first-hop failure free in time. The spec's reading is the one
that keeps first-hop probing from being free, which is the same instinct
exp-010b's holds came from. Both are defensible; the difference is
small in aggregate and large for a router that probes its own channels
first.

Two caps fall out of the same arithmetic and are pinned too. A failure
reported by the FINAL node (the htlc reached the far end) would give
`k = len + 1` and is capped at the route length. A failure from a node
that is not on the route at all is charged the whole route rather than
credited with stopping early, which is the conservative reading.

### 4. The round trip is priced off the TRUE result, which makes it un-launderable

Same construction as stage D's self-contention counter, arrived at
independently. The hop count is computed before `deliverAttempt` runs,
so an attribution section that blames a neighbour of the node that
really failed moves what the ROUTER is told and not what the CLOCK is
charged. The time an htlc took is a fact about the network, not about
what came back over it.

It matters more here than it did for a counter: the clock feeds the
background traffic, the hold durations and the scheduler's ordering key,
so a degraded failure message that could edit the clock would let the
attribution knob move the exogenous process. It cannot.

### 5. The differential structure is enormous in practice, and it points at lnd

This is the finding to carry into the sweep. Smoke, slow rung
(`per_hop_ms: 1000`, `attempt_overhead_ms: 250`), ten files, both arms,
reading the failure depth straight off the per-attempt latencies:

| tier | arm | failures | mean depth | of a route of | share |
|---|---|---|---|---|---|
| slow, sequential | lnd | 4,075 | 1.48 hops | 7.1 hops | **21%** |
| slow, sequential | candidate | 2,140 | 5.63 hops | 11.5 hops | **49%** |
| slow, window 4 | lnd | 4,982 | 1.28 hops | 6.7 hops | 19% |
| slow, window 4 | candidate | 2,429 | 4.93 hops | 11.7 hops | 42% |

lnd's failures come back from the first hop or two of every route it
tries. The seed candidate's come back from halfway down a route half
again as long. So under a time metric lnd's many attempts are CHEAP and
the candidate's few attempts are EXPENSIVE, and the realized payment
latencies say so: 173s for lnd against 375s for the candidate on the
same tier, while the candidate uses 42% fewer attempts.

That is exp-019's retirement of the 8.6x attempt headline arriving from
a third direction, and it is the mechanism behind the spec's E-c
prediction, observed directly rather than inferred. Whether it flips a
margin is a sweep question; that the attempt axis and the time axis
disagree about which arm is efficient is visible on smoke.

### 6. The 1/N rule broke objective L on its first calibration, and the guard caught it

The spec's objective L is calibrated rather than typed: the weight is
chosen so the mean time penalty on the reference arm equals the mean
attempt penalty it pays today. That is exactly how the 1/N rule gets
broken without anybody choosing a number. A reference arm whose attempt
penalty is saturated (which the seed candidate's is, at 30 attempts per
payment) and whose latencies are modest produces a weight that, against
a generous cap, saturates past `1/N`.

It fired on the first use. At a 600 second cap the calibrated weight
saturates at **0.226**, past the **0.167** an abandoned payment costs in
a six-payment file, and comfortably past the **0.150** the attempt term
it replaces saturates at. `check_latency_budget` refuses that
combination rather than reporting it.

`simulation/evaluate.py` therefore ships the rule as a check and not as
a comment, and a caller picks the cap against it. On this smoke the
largest admissible caps are 260s (sequential) and 240s (window 4).

## Smoke, labelled as smoke

Single runs, ten files, no pairing statistics, and the "candidate" arm is
the in-tree SEED router, not hb1, mx_c3 or atomic1. NOT results.

Tier: `--drift --atomic`, seed 8081, six to nine atomic payments per
file, flat tick `attempt_sec: 1`. Two latency scales, one faster than
the tick and one much slower:

- **fast:** `per_hop_ms: 50`, `attempt_overhead_ms: 100`
- **slow:** `per_hop_ms: 1000`, `attempt_overhead_ms: 250`

| rung | window | arm | succ | att/pmt | give-ups | obj | bg sent | makespan | pmt lat | att lat | mean conc | self cont |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| none | seq | lnd | 0.613 | 52.1 | 9 | 0.471 | 4787 | 5219 | - | 1.00 | - | - |
| fast | seq | lnd | 0.603 | 53.6 | 9 | 0.460 | 4635 | 4910 | 13.6 | 0.34 | - | - |
| slow | seq | lnd | 0.623 | 52.1 | 9 | 0.479 | 5421 | 6208 | 173.1 | 4.74 | - | - |
| none | seq | cand | 0.825 | 32.5 | 10 | 0.648 | 4736 | 5047 | - | 1.00 | - | - |
| fast | seq | cand | 0.825 | 31.9 | 10 | 0.649 | 4669 | 4989 | 25.5 | 0.69 | - | - |
| slow | seq | cand | 0.812 | 30.3 | 13 | 0.632 | 6317 | 7739 | 375.4 | 11.13 | - | - |
| none | 4 | lnd | 0.569 | 64.8 | 9 | 0.433 | 102 | 218 | - | 1.00 | 2.06 | 1167 |
| fast | 4 | lnd | 0.554 | 68.2 | 7 | 0.421 | 26 | 53 | 15.0 | 0.30 | 2.19 | 1912 |
| slow | 4 | lnd | 0.564 | 63.4 | 9 | 0.430 | 306 | 572 | 184.8 | 4.46 | 2.40 | 1275 |
| none | 4 | cand | 0.748 | 34.6 | 16 | 0.581 | 71 | 128 | - | 1.00 | 2.04 | 468 |
| fast | 4 | cand | 0.762 | 33.5 | 14 | 0.595 | 48 | 93 | 24.3 | 0.63 | 1.92 | 518 |
| slow | 4 | cand | 0.764 | 37.1 | 15 | 0.594 | 749 | 1340 | 458.2 | 10.94 | 2.26 | 679 |

The `none` rows report no latency by design; their attempt latency is
the flat tick and their makespan is read from a
`max_in_flight: 1` control, which stage D proved identical to no section
at all. Five things to read out of it, all provisional.

**(a) Longer routes resolve later, exactly.** A settle costs
`overhead + 2 * per_hop * hops` to the millisecond, and a failure over
the same length of route costs far less. Slow rung, candidate arm,
window 4:

| route | settle | n | failure | n |
|---|---|---|---|---|
| 2 hops | 4.25s | 9 | 4.25s | 1 |
| 5 hops | 10.25s | 26 | 5.77s | 75 |
| 11 hops | 22.25s | 54 | 9.01s | 282 |
| 15 hops | 30.25s | 45 | 12.77s | 173 |
| 23 hops | 46.25s | 1 | 21.43s | 22 |

The settle column is the formula; the failure column is the mechanism.
On the lnd arm the failure column is nearly FLAT at 2.4 to 3.2 seconds
across every route length, which is finding 5 in its rawest form.

**(b) Slow attempts churn more, fast attempts churn less.** Sequential,
lnd: 4,787 background payments at the flat tick, 4,635 at the fast rung
(attempts now cost 0.34s against the tick's 1.0s) and 5,421 at the slow
one. The candidate goes 4,736 / 4,669 / 6,317. Makespan moves with it in
both directions. The indirect cost channel is live and it is signed:
latency is not only a tax, it is a rate, and a router that resolves
faster than the tick sees a QUIETER world.

**(c) Under concurrency, slow attempts buy overlap.** `mean_concurrent`
goes 2.06 / 2.19 / 2.40 on the lnd arm and 2.04 / 1.92 / 2.26 on the
candidate's. Self contention is not monotone (lnd 1167 / 1912 / 1275),
which at n=10 with one run per file is exactly what "underpowered" looks
like. E-b's setup is real; E-b's verdict needs the sweep.

**(d) The objective barely moves, which is the point.** Composite scores
stay inside 0.02 of the control on every rung and both arms. Latency
does not enter the objective, so the only paths in are drift and
contention, and neither is large here. Read this as the confirmation
that the stage changed the clock and not the scoring.

**(e) Objective L, re-scored offline from these same outputs.** At the
largest admissible cap the guard allows:

| tier | cap | weight | arm | att/pmt | pmt lat | objective | objective L |
|---|---|---|---|---|---|---|---|
| slow, seq | 260s | 0.000633/s | lnd | 52.1 | 173.1s | 0.479 | 0.488 |
| slow, seq | 260s | 0.000633/s | cand | 30.3 | 375.4s | 0.632 | 0.632 |
| slow, w4 | 240s | 0.000694/s | lnd | 63.4 | 184.8s | 0.430 | 0.426 |
| slow, w4 | 240s | 0.000694/s | cand | 37.1 | 458.2s | 0.594 | 0.594 |

The candidate minus lnd margin goes +0.153 to +0.144 sequentially and
+0.164 to +0.168 at window 4: it moves, in the direction E-c predicts on
one tier, and nowhere near enough to change a verdict. Both arms sit at
or near the cap, which is where a calibrated weight puts them, and that
is a property of the calibration rather than of the routers. The
re-scoring tooling is what this stage owed; the arm itself is the
sweep's.

## Spec-vs-reality deltas

1. **`hold_carry` false is refused, not implemented.** The scheduler
   already does what the field asks, unconditionally. Finding 2.
2. **A latency section requires a clock section.** Not in the spec; the
   same rule stage D found for concurrency, for the same reason.
3. **The attempt duration is charged in two pieces, not one.** The spec
   gives a formula for an attempt's duration; the implementation splits
   it at the wire. `attempt_overhead` is the dispatch step (route chosen
   to htlc on the wire) and `2 * per_hop * k` is the return step (htlc
   resolved out there to the sender being told). That split is what lets
   the round trip be priced on the outcome, which the formula requires
   and a single step could not deliver: `k` is only knowable after the
   send.
4. **`k` is one past the failure index, not the index.** The spec's
   formula says "the index of the failing hop" and its prose says a
   first-hop failure costs one round trip. The two disagree by one
   against `walkHtlc`'s blame convention; the prose wins. Finding 3 and
   open question 1.
5. **`mean_attempt_latency_sec` is not in the spec.** The spec names
   `mean_payment_latency_sec` and the per-attempt trace field. The
   attempt mean is the manipulation check every stage since A has needed:
   read against the flat `attempt_sec` it replaces, it says whether the
   mechanism fired at all.
6. **`makespan_sec` is emitted for a latency file with no concurrency
   section.** The spec puts makespan in stage D. Gating it behind the
   other section would have hidden the headline number from every tier
   this stage is run on.
7. **Objective L ships with a budget check, not just a formula.** The
   spec gives the substitution and the calibration rule and stops there.
   The calibration breaks the 1/N rule on its first real use. Finding 6.
8. **No evaluator hint was added, deliberately.** Stages C and D added
   unconditional sentences to the evaluator hints. Stage E adds none: the
   objective is unchanged, so there is nothing new for a candidate to
   misread, and a sentence telling a candidate that probing near is
   cheaper in TIME would hand it the mechanism the pre-registered
   hypothesis exists to test. exp-015 found the background prompt had
   been restricting the search by telling candidates what had already
   lost; this is the same hazard with the sign flipped.
9. **Byte identity is literal, not halved.** Every new key is nil when
   the section is absent, so stage C's halves method was not needed.

## Open for the lead

1. **`k = i + 1` or `k = i` for a first-hop failure.** The spec's prose
   is implemented: a liquidity shortfall on the sender's own channel
   costs one round trip. A real sender learns that without any network
   round trip, so the protocol-faithful reading charges zero. The choice
   decides whether probing one's own channels is free in time, which is
   precisely the kind of subsidy exp-010b existed to remove. Small in
   aggregate, large for a router that probes near first, and it is one
   line plus one test.
2. **The rung ladder is authored and needs a data-driven replacement,
   the way stage C's did.** The two scales here were chosen to straddle
   the tier's flat tick (0.34s and 4.74s of realized attempt latency
   against a 1.0s tick), which makes the drift channel readable in both
   directions but is taste rather than measurement. Real per-hop
   latencies on mainnet are the honest anchor, and nobody has measured
   them into this program. Same shape as stage A's `tight`, stage B's
   `heavy` and stage D's `inter_arrival_sec`: the honest empirical rung
   is the realism anchor and an authored one is the power source, and
   this is the fifth time in a row.
3. **Objective L's cap is a free parameter that the calibration makes
   load bearing.** Finding 6 shows the weight and the cap cannot be
   chosen independently. Options: pin the cap so objective L saturates
   at exactly the `0.150` the attempt term does, which makes the two
   objectives comparable by construction; or pin it at a quantile of the
   measured payment-latency distribution and let the guard reject what
   it rejects. The first is a decision, the second is a measurement, and
   the pre-registered E-c arm needs one of them before it runs.
4. **E-b is underpowered at n=10 and its counter is not monotone.**
   Self-contention goes 1167 / 1912 / 1275 across the lnd rungs. Stage D
   already asked for n=20 on concurrency tiers; a latency ladder ON a
   concurrency tier multiplies the variance again. Pre-register that a
   straddling CI here is reported as underpowered rather than as a null.
5. **No champion has ever been given a reason to probe near first.**
   Fifth appearance of the shape that opened stage B's, C's and D's
   ledgers: hb1, mx_c3 and atomic1 were all evolved when every attempt
   cost the same regardless of route, so a stage E sweep over them
   measures their indifference rather than their design. Unlike the
   earlier four this one needs no new contract surface at all, which
   makes it the cheapest of the five to answer with an evolution arm
   rather than a hand-written variant.
6. **Finding 1 says the champions cannot lose E-a and cannot win it
   either.** Their outputs are byte identical under latency alone, at
   both scales, because none of them reads absolute time. Every
   non-trivial stage E result therefore has to come through traffic or
   contention, which means the E-a control's real job in the sweep is to
   prove the other tiers' movements are not artifacts.
