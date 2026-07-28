# EXP-023 stage D — landed, with six spec-vs-reality findings

**Date:** 2026-07-28
**Status:** implemented (seven commits on `econ-realism`, ending
a27f657d5 plus this writeup); the evolution arm and the paired tier
sweep remain to run.

Stage D of the economic-realism program is in the tree: the sender can
run several of its own payments at once, racing itself for its own
outbound liquidity, on a deterministic virtual-time event loop that
reduces to the sequential batch exactly. `SimRunner` grows a batch
entry point and a scheduler behind it, `RunScenarioFrom` becomes a batch
of one on that same loop, `SimBalanceRefresher` is the optional half of
the contract a router implements if it wants to be told its own
liquidity moved under it, and `--concurrency` stamps the section onto
generated corpora.

The objective is unchanged. `makespan_sec` is reported and stays out of
it, per the lead's decision at spec review.

## Commits

| commit | what |
|---|---|
| 01a01b7e2 | the `concurrency` section and its validation, poisson rejected by name |
| 3af6ed316 | a hold can say which directed edges it reserves; `endLiquidity` |
| 08dc442b6 | the scheduler, the state machine, `SimBalanceRefresher`, self contention |
| 380d17ffa | routesim reads the section and reports five fields |
| 217f4788e | `--concurrency` on `gen_scenarios.py` |
| 90f0e3bdb | the reported keys back inside eighty columns |
| a27f657d5 | a batch that never started names no scenario |

## Schema as landed

```json
"concurrency": {
  "max_in_flight": 4,
  "arrival": "window",
  "inter_arrival_sec": 5
}
```

`arrival` defaults to `window` and is the only process implemented;
`poisson` is rejected by name at both ends, in the simulator and in the
generator, with the deferral's reason in the message. `inter_arrival_sec`
defaults to the clock section's `payment_gap_sec`, which is what makes
`max_in_flight: 1` identical to a file with no section at all. An absent
section is the sequential batch, which is what every scenario file
written before stage D says by omission.

`--concurrency 4` on `gen_scenarios.py`, or
`--concurrency max_in_flight=4,inter_arrival_sec=5,arrival=window` for
the long form. Generate it alongside `--drift --atomic`: see finding 2.

Reported fields, all five emitted only when a file asks for the
mechanism and then emitted in full, zeroes included:
`max_concurrent`, `mean_concurrent`, `self_contention_failures`,
`makespan_sec`, `router_accepts_balance_refresh`.

## The event loop, as built

**States.** A live payment is in one of three: `request` is about to ask
its router for the next route and has nothing in the air; `resolve` has
an htlc on the wire and will dispatch it and report the outcome; `done`
has resolved and released whatever it still held. The split between the
first two is the whole point. The window between choosing a route and
hearing what happened to it is the only window two of the sender's
payments can share, so a step that did a whole attempt at a time would
have serialized the batch under a new name.

**The loop.** Pick the earliest of (the next admission, the next live
payment's event). Advance the clock to it, running the background
traffic owed for the interval. Execute exactly one step: one
`RequestRoute`, or one dispatch and one `ReportAttempt`.

**Tie breaking.** Two payments due at the same instant run in scenario
order, always. An admission due at the same instant as a payment event
goes first, which is the only reading of "keep max_in_flight payments
live" that does not leave a slot idle while a payment that could have
started waits.

**Admission.** Each free slot remembers when it became free. The next
waiting payment is admitted `inter_arrival_sec` after the EARLIEST of
them. At `max_in_flight: 1` there is one slot, it frees when the
previous payment resolved, and the gap is measured from exactly there,
which is the sequential batch. All slots are free when the batch starts,
so the whole window fills at once.

**Traffic accounting.** The scheduler runs the exogenous process per
INTERVAL, through the existing prorating carry, rather than per payment.
The sequential loop answered "does traffic run for this stretch" two
different ways: the gap between payments always churned, and the time
inside a payment churned only under atomic mpp. With one payment live
those two cases are exactly "no payment is live" and "the live payment
is atomic", so the generalized rule is: run it if nothing is live, or if
anything live is atomic. One case is kept verbatim rather than
generalized: a file with background traffic and no clock has no duration
to prorate, and the sequential loop churned a whole gap's worth there
anyway, so admission still does.

**Time inside a step.** An attribution section can age the network in
the middle of a resolve step, by whole attempt-sized slices. The
scheduler re-reads the clock after every step and only ever advances
forward, so events that were due inside that window are late rather than
run backwards. That is unchanged behavior, deliberately: the delay knob
is exp-019's and its draw order is what makes exp-019 paired.

## Byte identity, proven

Flag off is byte identical, literally, with no halves needed: every new
key is a pointer that is nil when the section is absent, so a file
without one emits exactly the JSON it emitted before this stage.

Against a binary built at the stage C merge (6b7229d78):

- **256 paired whole-output runs, zero diffs.** Sealed hard tier (10
  files), sealed OOD tier (10), the sealed `corpus-mix` train and val
  splits (68), and regenerated default, hard, drift, split and atomic
  corpora (40), each on both the lnd and candidate arms.
- **132 mainnet runs, zero aggregate mismatches.** 11 files x 2 arms x 3
  runs x 2 binaries, compared as sets of aggregates because of stage B's
  finding 4.
- **Generator output tree diff-identical** at a fixed seed either side of
  the change, across six modes: default, `--hard`, `--drift`, `--split`,
  `--split --atomic` and `--drift --atomic`.
- **A stamped `max_in_flight: 1` against a no-section control**, 20
  paired runs on the concurrency tier, both arms: results byte for byte,
  aggregate byte for byte with the five new keys projected out, a key
  census asserting those five are the only additions and nothing
  disappeared, and the five reading `max_concurrent = 1`,
  `mean_concurrent = 1.0`, `self_contention_failures = 0`,
  `router_accepts_balance_refresh = false`.
- Goldens in the tree: `TestSimSchedulerSequentialTimeline` pins the
  timeline the sequential batch has always produced (a payment starts one
  gap after the previous one resolved, each attempt takes one step,
  nothing overlaps, and the makespan is two gaps plus three attempts);
  `TestSimSchedulerTrafficCarryIsSliceInvariant` pins that a window's
  background volume does not depend on how finely it is cut;
  `TestSimSchedulerDeterminism` runs the same concurrent batch twice;
  `TestSimSchedulerSelfContention` is the exp-010b contention test across
  two payments, with the sequential batch and the no-holds batch as its
  two controls; `TestRouterAcceptsBalanceRefreshFalseForPlainRouter` and
  `TestSimSchedulerBalanceRefreshIsDelivered` are the pair that prove the
  optional half is neither mandatory nor dead plumbing.

The whole `go test ./routing/...` suite is green, and the pre-existing
suite is itself a large part of the identity proof: every one of the
several dozen `RunScenario` tests now runs through the scheduler.

## Six things implementation taught the spec

### 1. The manipulation check needs a corpus, not just a counter

`mean_concurrent` does what the spec asked and immediately earns its
keep, because the obvious tier does not overlap.

| tier | window | mean_concurrent | max | self contention |
|---|---|---|---|---|
| `--split --atomic`, file's own 600s gap | 1 | 1.00 | 1 | 0 |
| `--split --atomic`, file's own 600s gap | 2 | 1.03 | 2 | 0 |
| `--split --atomic`, file's own 600s gap | 4 | 1.12 | 3 | 1 |
| `--drift --atomic`, `inter_arrival_sec: 5` | 2 | 1.56 | 2 | 202 |
| `--drift --atomic`, `inter_arrival_sec: 5` | 4 | 2.04 | 4 | 468 |

The spec's tier table derives `concurrent-{2,4,8}` from the atomic tier.
The atomic tier is `--split --atomic`, which holds three payments per
file of wildly unequal length and spaces them by a 600 second gap, so
the window empties before it fills and a four-payment window never has
more than 1.12 payments live. Nothing is broken there; there is just
nothing to measure.

Two knobs fix it and both matter. `inter_arrival_sec` has to be at or
below the time a payment takes on the tier, and the tier needs enough
payments of comparable length for the window to stay full.
`--drift --atomic` gives six to nine atomic payments per file on a clock,
and at `inter_arrival_sec: 5` the window fills and stays filled. This is
the third stage in a row where the honest empirical rung is the realism
anchor and an authored one is the power source, arrived at
independently each time.

### 2. Concurrency needs a clock, and the sealed tiers do not have one

Found while writing the identity sweep: **no sealed tier configures a
virtual clock at all.** `hard-test`, `ood-test`, `corpus-mix` and the
whole mainnet tier are static worlds where no simulator action moves
time. With no virtual time every scheduler event ties at the same
instant, the loop degenerates to running the payments in index order,
nothing overlaps, and the counters would report a window that never
opened.

So a concurrency section on a file with no clock is REFUSED rather than
measured, with the reason in the message. The consequence for the sweep
is that a concurrency tier cannot be derived from the sealed hard, OOD or
mainnet tiers as they stand: it has to be derived from a tier that has a
clock, or those tiers have to gain a clock section, which would be a
transformation of a sealed tier and therefore its own decision. This is
open question 1.

### 3. Running the traffic per interval makes a concurrent tier a quieter world

This is the confound a sweep has to know about, and it falls straight out
of the design the spec asked for.

Background traffic is a function of elapsed virtual time. A concurrent
batch clears the same payments in less virtual time. So it elapses less
of the exogenous process: on the concurrency tier the lnd arm sends 255
background payments at `max_in_flight: 1`, 134 at 2, and 102 at 4.

The alternative would have been to run a gap's worth of churn per
payment regardless of the clock, which is what the sequential loop
effectively did because its payments were serial. That would have kept
the churn constant across the ladder at the cost of making the exogenous
process depend on the scheduler, which is exactly backwards: other
people's payments do not know how many of ours are in flight.

The honest reading is that a concurrency rung differs from its
sequential control in two ways at once, more self-contention and less
background drift, and that the second is small (exp-015 measured decay as
a tie at eighteen times this churn) but not zero. A sweep that wants them
separated can hold the churn fixed by scaling `payments_per_gap` with the
measured makespan ratio, which is a corpus decision rather than a
simulator one.

### 4. Self contention is measurable exactly, and the smoke points the OPPOSITE way to H-D3

`self_contention_failures` is causal rather than coincidental. At the
moment an attempt fails for want of liquidity the runner reads the
failing edge's true balance and its total reservation, and asks whether
the edge would have carried the amount with the siblings' share of that
reservation removed. An edge that was short anyway is not contention, and
an edge with room to spare did not fail for liquidity at all. It is
computed from the TRUE result rather than the one the router is told, so
an attribution section cannot launder it.

Smoke, on the concurrency tier, ten files, both arms:

| window | arm | self contention | per attempt |
|---|---|---|---|
| 1 | lnd | 0 | 0.000 |
| 1 | candidate | 0 | 0.000 |
| 2 | lnd | 933 | 0.208 |
| 2 | candidate | 202 | 0.076 |
| 4 | lnd | 1167 | 0.223 |
| 4 | candidate | 468 | 0.174 |

The spec pre-registered H-D2, that the champions poison their own first
hop and show rising attempts, and H-D3, that lnd degrades gracefully
because mission control is shared across payments by construction. The
smoke says lnd absorbs two to three times the self-contention rate of the
in-tree seed candidate, per attempt, at both windows. Read it as a
starting gun and not a result: this is a single run per file, the
"candidate" arm is the SEED router rather than hb1, mx_c3 or atomic1, and
the counter counts attempts rather than payments, so an arm that retries
more has more chances to collide with itself. But it is the opposite sign
to the prediction, and it is the first thing the paired sweep should
check.

The counter is structurally ZERO without `atomic_mpp`, since a shard that
settles on arrival reserves nothing. That is the stage's free control and
it is declared at the field, next to the reminder that a zero can also
mean the payments never overlapped, which is what `mean_concurrent` is
for.

### 5. `router_accepts_balance_refresh` is false for every arm, and that is the point

`SimBalanceRefresher` ships with the capability rather than after it,
which is exp-016's lesson applied before the fact. No router in this
program implements it, the lnd stack included: `newLndStackRouter` closes
over the balances it was handed and builds its bandwidth hints from that
map, so it takes no refresh either. Unlike served observations there is
no second path in, so the flag reads false everywhere.

That is a finding rather than a gap, and it is the finding the flag
exists to make visible. `TestSimSchedulerBalanceRefreshIsDelivered`
proves the plumbing is live: a router that does implement it is told,
before every route request, what its own outbound liquidity is now, and
the number is net of what is currently held, short by exactly the shard
in the air.

### 6. Two scheduling decisions the spec left open, decided here

**The window fills all at once.** Every slot is free when the batch
starts, so the first `max_in_flight` payments are all admitted at the
same instant and all plan against identical balances. The alternative,
staggering the initial fill by `inter_arrival_sec`, serializes the
arrivals at that spacing and, on a tier whose gap exceeds its payment
duration, produces no overlap at all. It also breaks the monotonicity the
slot list relies on. Simultaneous fill is the reading that makes the knob
do what it says.

**A refused route costs no time.** The stage C fee-limit refusal
consumes an attempt and no virtual time, which is what the sequential
loop did, so a router that keeps proposing routes it cannot afford spins
the request step without advancing the clock. It cannot livelock: the
attempt cap is checked at the top of the request step, which is where the
sequential loop checked it.

## Smoke, labelled as smoke

Single runs, ten files, no pairing statistics, and the "candidate" arm is
the in-tree SEED router, not hb1, mx_c3 or atomic1. NOT results.

Tier: `--drift --atomic --concurrency max_in_flight=N,inter_arrival_sec=5`,
seed 8081, six to nine atomic payments per file.

| max_in_flight | arm | succ | att/pmt | give-ups | mean_conc | max_conc | self_cont | self/att | makespan | bg sent |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | lnd | 0.593 | 50.5 | 11 | 1.00 | 1 | 0 | 0.000 | 456 | 255 |
| 1 | cand | 0.795 | 34.7 | 12 | 1.00 | 1 | 0 | 0.000 | 290 | 163 |
| 2 | lnd | 0.593 | 56.3 | 9 | 1.68 | 2 | 933 | 0.208 | 258 | 134 |
| 2 | cand | 0.785 | 34.2 | 14 | 1.56 | 2 | 202 | 0.076 | 179 | 105 |
| 4 | lnd | 0.569 | 64.8 | 9 | 2.06 | 4 | 1167 | 0.223 | 218 | 102 |
| 4 | cand | 0.748 | 34.6 | 16 | 2.04 | 4 | 468 | 0.174 | 128 | 71 |

Four things to read out of it, all provisional.

**The mechanism fires.** `mean_concurrent` goes 1.00 to 1.68 to 2.06 and
`max_concurrent` reaches the window, so the payments genuinely overlap,
and self contention goes from structurally zero to hundreds of attempts.

**Both arms pay, in different currencies.** lnd holds success flat from
window 1 to 2 and buys it with attempts (50.5 to 56.3 to 64.8); the seed
candidate holds attempts flat (34.7 to 34.2 to 34.6) and loses success
(0.795 to 0.785 to 0.748) and give-ups climb (12 to 14 to 16). Read
success and attempts separately, as always: neither arm is simply worse,
they are trading on different axes.

**The gap does not close here.** The seed candidate leads lnd by 0.202 of
success at window 1 and 0.179 at window 4. Concurrency is the first stage
of this program whose pre-registered story was that lnd's shared mission
control would help it; on this smoke it does not.

**Concurrency is cheap in time.** The makespan falls by roughly half at
window 4 on both arms. It is reported and does not enter the objective,
which is the lead's decision for the whole program.

## Spec-vs-reality deltas

1. **`poisson` is rejected, not implemented.** Specified and deferred by
   the spec; rejected by name at both ends with the reason in the
   message.
2. **A concurrency section requires a virtual clock.** Not in the spec.
   Without one the loop cannot order events and the tier would be
   silently inert.
3. **The tier cannot be derived from the atomic tier as the spec's table
   says**, or rather it can, and measures nothing. See finding 1.
4. **The sealed tiers have no clock at all**, so `concurrent-{2,4,8}`
   cannot be derived from `hard-test`, `ood-test` or mainnet without a
   clock section being added to them. Open question 1.
5. **Background churn falls with the window.** A consequence of the
   spec's own "run the traffic owed for the interval". Finding 3.
6. **The warmup phase stays sequential.** The spec does not say; what the
   warmup exists to measure is the value of knowledge a node was handed,
   not the scheduling of the payments that bought it.
7. **`self_contention_failures` is stricter than the spec's wording.**
   The spec says "attempts that failed on a channel where the sender's
   own other payment held liquidity at that moment". That is
   co-occurrence; the implementation additionally requires the siblings'
   share of the reservation to be what made the difference. The residual
   over-count is an attempt refused by an announced max-htlc limit on an
   edge that was ALSO short exactly by its siblings' share, which needs
   both conditions at once.
8. **Byte identity is literal, not halved.** Every new key is nil when
   the section is absent, so stage C's halves method was not needed.
9. **`RunScenarioFrom` is now a batch of one on the scheduler** rather
   than a second implementation of the sequential loop, which is what
   makes the identity proof cover it.

## Open for the lead

1. **The sealed tiers have no clock, and the concurrency tier needs
   one.** Options: derive the tier from `--drift --atomic` as the smoke
   here does and accept that it is a fresh world rather than a
   transformation of a sealed one; or add a clock section to a sealed
   tier, which is a transformation and needs the exp-017 pairing
   discipline applied to it; or run the ladder on both and report the
   fresh tier as the power source and the transformed sealed tier as the
   comparability anchor. This is the corpus decision that gates the
   sweep.
2. **Whether to hold background churn fixed across the ladder.** Finding
   3 says a concurrency rung is a quieter world than its sequential
   control by construction. Scaling `payments_per_gap` by the measured
   makespan ratio would separate the two effects at the cost of making
   the corpus depend on a measurement. Doing nothing is defensible and
   the effect is likely small; saying so in advance is what keeps it from
   being explained away later.
3. **No champion carries a balance-refresh variant.** Third time this
   shape has arrived, after stage B's inbound fees and stage C's
   budgets. hb1, mx_c3 and atomic1 were all evolved before anything could
   tell them their own liquidity had moved, so a stage D sweep over them
   measures their blindness. exp-016 solved it by hand-writing importer
   variants after the fact; whether to do that again or let the evolution
   arm answer it is a budget question.
4. **`max_in_flight: 8` is in the spec's tier table and is untested
   here.** The ladder above stops at 4 because the corpus holds six to
   nine payments per file, so a window of 8 would be the whole file and
   the arrival process would stop being an arrival process. A rung at 8
   wants files with more payments.
5. **H-D3 points the wrong way on smoke.** lnd absorbs two to three times
   the self-contention rate of the seed candidate. If the paired sweep
   holds this up it is a result worth its own section, and it belongs in
   the pre-registered outcomes now rather than after.
