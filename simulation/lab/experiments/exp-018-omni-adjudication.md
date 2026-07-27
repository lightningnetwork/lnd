# EXP-018 — The omni adjudication: the band is not a gepa artifact

**Date:** 2026-07-27 (overnight)
**Status:** complete — the exp-011 confound resolved at practical
budgets. Champions unchanged.

## Why this ran

exp-011 found three evolved lineages converging on a ~0.64 combined
band and called it a paradigm ceiling. The claim was confounded from
the start: every run in this program used `engine="gepa"`, so three
lineages agreeing said as much about one optimizer's attractor as
about the problem. The GEPA team's own omni results — no engine
dominant, each winning about a third of problems — made the
alternative live. This run gives three engines the identical seed
(the in-tree candidate slot), the identical corpus (corpus-mix, the
exp-011 ground), and the identical eval budget (150, enforced
centrally in the eval server), and compares what each produces.

## Result

| engine | val | held-out test | evals | wall | proposer $ | produced |
|---|---|---|---|---|---|---|
| **gepa** | **0.510** | **0.556** | 150 | 9.0h | $0 | a real 947-line candidate |
| meta_harness | 0.454 | 0.508 | 136 | 19m | $1.95 | the seed, byte-identical |
| autoresearch | 0.454 | 0.508 | 150 | 13m | $2.50 | the seed, modulo comments |

(Seed held-out test: 0.508. The claude arms ran under the sterile
config home with the durable JSON fix; neither crashed, leaked, nor
misbehaved — the containment held. They simply produced nothing.)

**Why the claude arms produced nothing is the finding.** meta_harness
benchmarks each proposed candidate against the full example set — 68
evals per candidate — so 150 evals bought it exactly one iteration:
its first real proposal scored 0.058 (broken), its second ran out of
budget mid-benchmark, and its "best" is the seed it started from.
autoresearch consumed its entire budget in 13 minutes without ever
beating the seed. gepa's minibatch loop stretched the same allowance
across 13 iterations. The budget-unit asymmetry the harness records
per arm (cache-miss accounting, proposal caps) is not a footnote; at
this scale it is the whole outcome. **gepa's moat is eval
efficiency, not proposal quality.**

The adjudication answer, stated with its limits: **at practical
budgets, the ~0.64 band is not a gepa artifact — the alternative
engines do not break it, or reach it, or leave the starting line.**
What this does not establish is that the band is a true problem
ceiling: a meta_harness run at roughly 10× the eval budget (or with
minibatch benchmarking) is the arm that would test that, and it now
comes with measured cost expectations — about $2 and 19 proposer
minutes per swing.

One operational cost surfaced too: the gepa arm ran its reflections
at xhigh effort per the standing directive, and 4 of 13 iterations
lost their proposal to the 600s timeout, stretching the arm to nine
hours. The searcher defaults were retuned the same night
(`820d06d01`): high effort, 900s timeout, xhigh one flag away.

## The candidate: omni1 displaces nobody

gepa's candidate looked real internally (held-out test 0.556 vs the
seed's 0.508) and the inflated-metric caveat held again: on the
adjudication tier set it beats no champion anywhere. Its only
CI-positive delta is +0.011 over hb1 on split_test — the tier where
hb1 is the known weak twin — with a sign test at p=0.219, landing on
exactly atomic1's second-place shelf. Against mx_c3 it is negative on
all six tiers, twice with CIs excluding zero. It is, however, no
exp-010-style collapse: above lnd on five of six tiers (hard_test
+0.271 at 10/0), and the highest success of any router on
atomic_test.

Its shape is the exact inverse of the give-up attractor: **omni1 is
the most attempt-expensive evolved router on every tier** (85.8 per
payment on atomic_test, against mx_c3's 12.9), buying
champion-or-better success with attempts the composite then taxes.
The source audit found the cause is likely an absence, not a
strategy: omni1 carries none of the champions' guardrails — no
attempt limit, no hop cap, no search budget. And since the objective
caps the attempt penalty at 15, its atomic_test 0.427 actually
flatters it.

Two evolved mechanisms are worth the idea ledger even though the
router fails:

1. **Dual belief ledgers.** omni1 keeps two books per directed
   channel: one that learns amounts inflated by its own in-flight
   shard reservations ("blocked right now by my own MPP contention")
   and one that learns raw attempt amounts ("this channel's standing
   balance"), blended at different strengths. No champion separates
   those two kinds of failure, and in the atomic arena they are
   genuinely different facts.
2. **Contradiction-triggered confidence decay.** Instead of
   time-decay, evidence that contradicts a bound clears it and halves
   the confidence. Evidence-keyed forgetting sidesteps the
   exp-008/015 decay question rather than answering it — and costs
   nothing on static tiers by construction.

Incidental for the §0.2 discussion: omni1's low-mode prior constant
is 0.025 — not the generator's 0.05, consistent with exp-017's
finding that the fitted constants are not where the performance
lives.

## Consequences

1. The engine question is closed at this budget scale, in gepa's
   favor, and the ceiling question is sharpened rather than settled:
   the one arm that would test it (meta_harness at ~10× evals) is
   specified and costed.
2. omni1 joins the challenger ledger as failure number six (exp-010's
   three, exp-010b's atomic1-as-challenger, exp-013's continuation,
   now exp-018). Champions: hb1 + mx_c3, unchanged.
3. The distillation patch moves to the top of the queue — no
   optimizer at practical budget is producing a new champion, and
   three experiments (exp-002b, exp-016, exp-019) now converge on the
   same lnd-side fixes.

## Caveats

One run per engine, one seed, one corpus — engine variance is
unmeasured (exp-010b showed proposer A/Bs flip between environments).
The claude arms' effort knobs were left at their engine defaults, not
swept. And the gepa arm's 4 lost reflections mean even the winning
arm ran below its potential; all three arms were handicapped in
different ways, which is the honest description of comparing engines
that disagree about what a budget is.
