# EXP-023 stage C — landed, with five spec-vs-reality findings

**Date:** 2026-07-28
**Status:** implemented (seven commits on `econ-realism`, ending
813ada9f7 plus this writeup); the evolution arm and the paired tier
sweep remain to run.

Stage C of the economic-realism program is in the tree: a payment
carries a fee budget, its sender is told what it is, and a route that
would overrun it is refused instead of sent. `SimPaymentSpec` grows
`FeeLimitMsat`, the lnd arm stops being constructed with an unlimited
ceiling and prunes on the budget natively, the runner enforces the same
number at its shared delivery point, the fee accounting stops dropping
every millisatoshi spent on payments that failed, and
`--fee-limit-ppm` stamps the knob onto generated corpora.

The objective is unchanged. That was the point of the stage: the
pressure went into the environment, not into the weight.

## Schema and flags as landed

```json
{"fee_limit_ppm": 2000,
 "scenarios": [{"target": "...", "amt_msat": 1000000000, "max_parts": 4,
                "fee_limit_ppm": 3000}]}
```

Parts per million of each payment's own amount, summed over every shard
the payment uses. A payment that pins its own limit keeps it; the
file-level value is the default for the ones that do not, and it
applies to warmup payments too, since a warmup exempt from the
constraint the scored batch runs under would be a different network.
Zero means no limit at both levels, which is what every scenario file
written before stage C says by omission.

`--fee-limit-ppm N` on `gen_scenarios.py`, validated at both ends: zero
and negative are rejected with the instruction to omit the flag, and
anything past 1,000,000 ppm is rejected as a units mistake.

Reported fields: `fee_limit_payments` and `fee_limit_failures` (the
static half and the alarm, both `omitempty`), `total_fee_msat_spent`
and `fee_ppm_attempted` (the accounting fix, always emitted). New
failure type `SimFeeLimitFailure`, which a trace names
`FeeLimitExceeded`.

## Five things implementation taught the spec

### 1. The alternative fee metric is NOT abandonment proof, and the 1/N rule governs both metrics

This is the correction that matters, because the spec's own give-up
arithmetic is the heart of the stage and the spec drew the wrong
conclusion from it.

The rule itself stands and is now recorded next to the constants that
would break it (`simulation/evaluate.py`): a scored file holds 6 to 10
payments, so abandoning one payment in the smallest file costs
`1/6 = 0.167` of objective, while the whole fee term is worth at most
`FEE_PPM_CAP * FEE_WEIGHT = 0.100`. **The fee term's maximum value must
stay strictly below `1/N`, where `N` is the payment count of the
smallest scored file.** The current margin is a factor of 1.67.
Doubling the weight breaks it; removing the cap breaks it
unconditionally.

Where the spec goes wrong is the escape hatch. It argues that
`fee_ppm_attempted` escapes the rule, because the abandoned amount
stays in the denominator, so the weight could safely rise once the term
was charged against it. Fixing the denominator only stops abandonment
from shrinking it. The numerator falls anyway, because a payment nobody
completes pays no fee: abandoning a payment that would have cost `f`
and spent `s` on partial shards moves the ratio from `(F+f)/A` to
`(F+s)/A`, a weak improvement for **every** payment that pays a fee.

`fee_ppm_on_success` is the partly self-limiting one by comparison. It
improves only when the abandoned payment was dearer than its file's
average, and abandoning a cheap payment makes it worse. On this axis
the substitution the spec proposed as the safe way to raise the weight
is the less safe of the two.

Re-scored on the sealed hard tier with no re-execution: switching the
fee term onto `fee_ppm_attempted` raises the lnd arm's mean objective
by +0.036 and the seed candidate's by +0.014, and the arm that gains
more is the arm that abandoned more (21 give-ups against 20, on a much
lower spent-fee base).

What the metric is genuinely for survives, and it is worth having on
its own: it counts money that LEFT THE SENDER.

### 2. Two fifths of the fees these tiers pay have never been counted

`total_fee_msat_spent` was specified as an accounting fix and read like
a housekeeping item. It is not:

| tier | fees reported | fees actually spent | invisible |
|---|---|---|---|
| sealed hard (10 files, both arms) | 91,619,606 | 154,805,664 | **40.8%** |
| sealed OOD (10 files, both arms) | 143,413,475 | 245,321,870 | **41.5%** |
| mainnet (11 files, both arms) | 61,751,981 | 105,854,408 | **41.7%** |

These are non-atomic tiers, so a partially settled MPP that later fails
has genuinely paid its forwarding nodes, and `total_fee_msat` counts
only the payments that completed. Every fee number this program has
published is the 59%.

The leak was only ever in the aggregate. Each scenario result has
carried its own spent fee in `fee_msat` all along, so **every archived
run can be re-totalled from its results array** with no re-execution.

### 3. A constraint the arms can see binds at plan time. Third confirmation, now a rule

Stage A learned it from announced htlc limits, stage B from inbound
fees, and stage C did not have to learn it at all: the counters were
declared with the lesson already applied, and the sweep then confirmed
it. `fee_limit_failures` is an ALARM, not a measurement.

| tier | rung (ppm) | lnd refusals | seed candidate refusals |
|---|---|---|---|
| sealed hard | none | 0 | 0 |
| sealed hard | 4000 | 0 | 107 |
| sealed hard | 2000 | 0 | 791 |
| sealed hard | 650 | 0 | 1645 |
| mainnet | 100 | 0 | 5744 |

lnd's path finding prunes any partial path whose accumulated fee
exceeds the budget it was handed, so the arm that prices its own routes
never offers the runner a route it has to refuse, at any rung, on
either tier. The seed candidate cannot see the budget at all and its
refusals grow monotonically with the pressure. That is H-C2's starting
gun rather than a defect, exactly as stage B's inbound fee refusals
were H-B1's.

Bindingness therefore has to be read off `success_rate` and the
realized fees against the unlimited control, which is the same place
stage B's discounts had to be read from.

### 4. The rung ladder cannot be global. Mainnet and the synthetic tiers are two orders of magnitude apart

The spec proposes one data-driven ladder. The data says one ladder per
tier family. Realized fee distribution with no limit, per file, both
arms (n = 20 synthetic, 22 mainnet):

| tier | metric | p10 | p25 | p50 | p75 | p90 | mean |
|---|---|---|---|---|---|---|---|
| hard + OOD | `fee_ppm_on_success` | 335 | 642 | 1983 | 3895 | 4424 | 2284 |
| hard + OOD | `fee_ppm_attempted` | 115 | 252 | 1127 | 2133 | 2813 | 1324 |
| mainnet | `fee_ppm_on_success` | 2 | 12 | 82 | 235 | 371 | 411 |
| mainnet | `fee_ppm_attempted` | 5 | 52 | 108 | 187 | 1337 | 499 |

Per tier and arm, for the sweep that has to choose rungs:

| tier | arm | feeS p25/p50/p75 | feeA p25/p50/p75 |
|---|---|---|---|
| hard | lnd | 965 / 3273 / 4263 | 162 / 889 / 1740 |
| hard | candidate | 1125 / 3528 / 4090 | 616 / 2326 / 3255 |
| OOD | lnd | 980 / 1262 / 1933 | 404 / 911 / 1290 |
| OOD | candidate | 764 / 1101 / 3069 | 669 / 1029 / 1709 |
| mainnet | lnd | 13 / 109 / 227 | 53 / 109 / 175 |
| mainnet | candidate | 9 / 56 / 201 | 48 / 108 / 218 |

A rung of 2000 ppm is the median of the synthetic tiers and roughly
twenty times the whole mainnet distribution: the same number would be a
control on one tier and a wipeout on the other. Suggested ladders,
from the measured quantiles: synthetic `{5000, 4000, 2000}` for
loose/median/tight against a `none` control, mainnet `{400, 100, 25}`.

### 5. The zero value of `SimPaymentSpec` stopped being inert

Absent means `lnwire.MaxMilliSatoshi` and not zero, deliberately: zero
is a real budget that forbids paying any fee at all, and a router
reading zero as "unlimited" would have the sign of the constraint
backwards. The consequence is that a hand-built spec that leaves
`FeeLimitMsat` alone now says the payment may pay nothing, and lnd duly
refuses to build any route that charges a fee. It bit immediately:
`TestSimAttributionUnknownLndPath` went red the moment the lnd arm was
wired to the field.

Both places in the tree that construct a spec directly now say
`lnwire.MaxMilliSatoshi`. The failure is loud rather than silent, which
is the failure worth having, and the alternative convention would have
made a genuinely tiny budget unexpressible.

## Byte identity, proven — and the one place it is not literal

**Stage C is the first stage whose off state is not byte identical**,
and the deviation is deliberate and bounded. The design spec orders the
accounting fix "regardless of the rest of the stage", so
`total_fee_msat_spent` and `fee_ppm_attempted` are emitted on every
run, budget or no budget. Two new keys in the aggregate mean a raw
`cmp` fails on every file for a purely additive reason.

The proof was therefore made in two strict halves, against a binary
built at the stage B landing (d92e7f2fc):

1. the `results` array compared byte for byte;
2. the aggregate with exactly the four stage C keys projected out,
   compared byte for byte **including key order**;
3. plus a key census asserting that the only keys added are
   `total_fee_msat_spent` and `fee_ppm_attempted`, and that no key
   disappeared.

Results:

- **216 paired whole-output runs, zero diffs.** Sealed hard tier (10
  files), sealed OOD tier (10), the sealed `corpus-mix` train and val
  splits (68), and regenerated hard, drift, split, atomic and default
  corpora (20), each on both the lnd and candidate arms.
- **220 mainnet runs, no real mismatch.** 11 files x 2 arms x 5 runs x
  2 binaries, compared as sets of aggregates because of stage B's
  finding 4. One file (`mn_55_uniform`, lnd arm) showed a second
  variant on the new binary and not the old at n=5; at n=20 both
  binaries produce the same two variants. That is stage B's finding 4
  reproduced independently, and by the same file it named.
- **Generator output tree diff-identical** at a fixed seed either side
  of the change, with the flag absent.
- **Objective arithmetic unchanged to the last bit**, checked by
  scoring the pre-change binary's own output with the old inline
  formula and comparing the repr to `composite_score`.
- Goldens in the tree: `TestSimFeeLimitAbsentGolden` pins the unlimited
  sentinel a scenario with no limit hands its router, with
  `TestSimFeeLimitReachesTheSpec` proving the sentinel is not dead
  plumbing, and `TestSimFeeLimitAbsentSendsEverything` pins that the
  route a tight budget refuses is dispatched and settles when no budget
  is named. `TestSimFeeLimitLndPrunesInsteadOfBeingRefused` is the
  agreement test: zero refusals on the lnd arm at four rungs, with the
  tight rung required to have cost the arm real payments so the zeroes
  are not the zeroes of a limit that never bound.

## Smoke, labelled as smoke

Single runs, n=10 or 11 files, no pairing statistics, and the
"candidate" arm is the in-tree SEED router, not hb1, mx_c3 or atomic1.
NOT results.

| tier | rung | arm | succ | att | feeS | feeA | give-ups | refused | obj | obj(att) |
|---|---|---|---|---|---|---|---|---|---|---|
| hard | none | lnd | 0.493 | 45.5 | 2778 | 997 | 21 | 0 | 0.309 | 0.345 |
| hard | none | cand | 0.704 | 34.7 | 2736 | 2041 | 20 | 0 | **0.530** | 0.544 |
| hard | 4000 | lnd | 0.434 | 44.6 | 2297 | 953 | 26 | 0 | 0.261 | 0.288 |
| hard | 4000 | cand | 0.626 | 35.0 | 2410 | 1862 | 26 | 107 | **0.459** | 0.470 |
| hard | 2000 | lnd | 0.258 | 30.1 | 680 | 785 | 47 | 0 | 0.128 | 0.126 |
| hard | 2000 | cand | 0.323 | 34.4 | 691 | 1090 | 50 | 791 | **0.189** | 0.182 |
| hard | 650 | lnd | 0.110 | 20.8 | 61 | 312 | 63 | 0 | **0.027** | 0.022 |
| hard | 650 | cand | 0.155 | 31.6 | 73 | 417 | 66 | 1645 | 0.015 | 0.008 |
| mainnet | none | lnd | 0.786 | 18.4 | 205 | 312 | 15 | 0 | 0.695 | 0.693 |
| mainnet | none | cand | 0.814 | 6.1 | 616 | 685 | 20 | 0 | **0.756** | 0.753 |
| mainnet | 100 | lnd | 0.675 | 22.6 | 7 | 22 | 25 | 0 | **0.577** | 0.576 |
| mainnet | 100 | cand | 0.627 | 55.9 | 6 | 7 | 11 | 5744 | 0.503 | 0.503 |

Three things to read out of it, all provisional.

**The mechanism fires and it fires hard.** Success falls monotonically
with the budget on both arms and on both tiers, and the fee limit is
doing it: the median synthetic rung halves success. H-C1's setup is
real.

**The gap narrows monotonically with fee pressure, and then inverts.**
The seed candidate leads lnd by +0.221 of objective on the clean hard
tier, +0.198 at 4000, +0.061 at 2000, and **-0.012 at 650**; on mainnet
at 100 ppm it is -0.074. This is the pre-registered "economic realism
could close the champion gap rather than widen it" outcome appearing on
the first stage that prices anything. Two loud caveats: this is the
seed router, whose fee model is a plain cheapest-path Dijkstra, not a
champion; and the champions have no budget-aware variant either, so a
sweep on them measures blindness rather than design (see the open
questions).

**Attempts and give-ups move in opposite directions on the two arms.**
Under pressure lnd's attempts fall (45.5 to 20.8) as it prices routes
out and stops trying; the candidate's stay flat or climb (34.7 to 31.6
on hard, 6.1 to 55.9 on mainnet) as it re-proposes routes it cannot
afford. `fee_limit_failures` next to `num_give_ups` is exactly what
separates those two stories, which is what the spec asked for.

## Give-up arithmetic, as implemented

- The 1/N rule is stated at the constants that would break it
  (`simulation/evaluate.py`), with the spec's escape hatch corrected in
  place.
- The evaluator hint gains the fee sentence unconditionally, in both
  evaluators: fees fall for two reasons, cheaper routes and fewer
  completed payments, and only the first is an improvement, so read
  `fee_ppm` against `success_rate` exactly the way attempts are read
  against it. The code evaluator additionally tells a candidate that a
  budget exists, where to read it, and that a refused route costs an
  attempt and teaches nothing.
- `fee_limit_failures` and `num_give_ups` are documented at the
  declaration as a pair to be read together.
- `composite_score(agg, fee_metric)` is one function taking either
  metric, so the pre-registered side-by-side arm re-scores archived
  runs offline with no re-execution.
- **Not implemented, and it belongs to the sweep rather than the
  simulator:** the abandonment gate ("a candidate whose success rate
  falls below its own seed's on any tier is disqualified regardless of
  composite objective"). It is a verdict criterion, so it belongs in
  `simulation/sweep_validate.py` and in the validation protocol, not in
  the evaluator that evolution optimizes against.
- One hazard found while looking for the gate's home and deliberately
  left alone: `sweep_validate.py` carries its OWN copy of
  `ATTEMPT_WEIGHT`, `FEE_WEIGHT`, `FEE_PPM_CAP` and the objective
  formula (`:28` through `:56`). It is the tool every published verdict
  was measured with, so the 1/N rule now recorded in `evaluate.py`
  guards a constant that exists in two places. Pointing it at
  `evaluate.composite_score` would also give the side-by-side arm a
  `--fee-metric` flag for free, but changing the adjudication tool was
  not this stage's business.

## Open for the lead

1. **The two always-emitted keys are a judgement call.** The spec
   ordered the accounting fix unconditionally and the lead's contract
   asked for both metrics emitted, which is incompatible with a literal
   byte-identity proof. The keys can be gated behind
   `fee_limit_ppm` at any time if the lead prefers the literal gate;
   the cost is that the 41% invisible-fee finding becomes invisible
   again on every tier that does not set a budget, which is every tier
   published so far.
2. **No champion carries a budget-aware variant.** This is stage B's
   open question 3 arriving again on schedule: hb1, mx_c3 and atomic1
   were all evolved before a payment had a budget, so a stage C sweep
   over them measures their blindness. exp-016 solved the same shape by
   hand-writing importer variants after the fact. Whether to hand-write
   budget-aware variants or let the evolution arm answer it is a budget
   question.
3. **Rung choice.** The measured ladders are in finding 4. The median
   synthetic rung is already severe (success halves), so the
   informative rung on the hard tier is likely 4000 rather than 2000,
   with 2000 as the stress rung and mainnet run at 400/100.
4. **Whether the objective should migrate at all.** With the corrected
   arithmetic, neither metric is abandonment proof and the 1/N rule
   binds both, so "migrate to `fee_ppm_attempted`" is no longer a way
   to buy a bigger fee weight. It is still a better measurement of what
   was spent. The pre-registered arm should therefore be read as a
   measurement question, not as a licence to raise `FEE_WEIGHT`.
