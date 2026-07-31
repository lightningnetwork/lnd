# EXP-030 — The unknown×shift interaction: misattribution manufactures innocence

**Date:** 2026-07-30.
**Status:** complete. Mechanism CONFIRMED by ablation and ground-truth
counters; fix (V3) measured and dispatched to the integration branch.

## The question

exp-027 round 6 left one anomaly on the integrated router's record:
deg_hard_mix (unknown 0.2 + shift 0.1) costs it 0.034 of objective
(z=−11.1 against replicate noise) while unknown-only, shift-only, and
unknown-at-0.3 all sit at or ABOVE the pre-quarantine reference. The
two attribution failures together cost five times the sum of their
parts. With the ship target set, the mechanism had to be found before
an upstream reviewer found it.

## The mechanism, in three rules

The ablation is decisive: disable the quarantine and change nothing
else, and the mix recovers +0.0355 while every other tier stays put.
The whole loss is the quarantine — but not because the quarantine's
own logic is wrong. It is disarmed by evidence the rest of the stack
manufactures:

1. A NAMED failure writes a hard `LowerOK` on every hop before the
   reported index — a hop that forwarded has proven it can carry the
   amount. Sound only if the report is honest.
2. A SHIFTED attribution names the wrong hop. Blame shifted
   downstream puts the true culprit before the reported index, so the
   guilty channel collects a lower bound saying it can carry the
   amount it just refused.
3. That false bound disarms the quarantine on the guilty channel
   through all three of its `LowerOK`-keyed suppression rules (the
   suspect-list filter, `recordSuspect`'s early return, `normalize`'s
   contradiction clearing) — and removing the culprit from the
   suspect list concentrates the `1/sqrt(n)` weight on the innocent
   survivors.

Unknown alone never fires it: no shift, no false bound (0.0% of
promoted bounds land on innocent channels). Shift alone barely runs
the quarantine (zero suspects recorded — every failure names a
channel). Together: 9.6% of the mix tier's promoted hard bounds land
on channels that NEVER failed, more promotions than unknown-at-0.3
despite fewer unreadable failures, and the suppression rules firing
39-73% more often. The counters come from the simulator's ground
truth (the true failing channel pre-degradation), not from inference.

## The fix ladder

Four variants measured; V3 wins and is principled:

| variant | mix vs tip | unk20 | unk30 | clean |
|---|---|---|---|---|
| V3 proven-only | +0.0406 | −0.0073 | −0.0092 | −0.000009 |
| V1 no-quarantine | +0.0355 | −0.0050 | −0.0063 | +0.0000 |
| V2 no-probe-bounds | +0.0283 | −0.0593 | −0.0854 | −0.0005 |
| V4 corroborate | +0.0212 | −0.0452 | −0.0454 | −0.0000 |

V3: a `ProvenOK` field written only by settlements; the quarantine's
three suppression rules read it instead of `LowerOK`. `LowerOK` keeps
its full pathfinding role — it just stops counting as proof of
innocence, because a hop before a named index is only proven if the
naming is honest, which is precisely the assumption misattribution
breaks. A settlement is ground truth. V3 takes the mix to 0.5105,
ABOVE the pre-quarantine reference (0.5039), leaves the clean tier
identical to seven decimals, and gives back only an insignificant
sliver of the unknown-only gains. No variant found recovers the mix
AND keeps the full unk30 gain; V3 is the best point on the trade.

This also resolves the quarantine keep/drop question the data had
left open: with V3 the quarantine is positive or neutral on every
degraded tier, so it KEEPS (still severable via the exp-027-era gate).

## Caveats

The mechanism is established (ablation + ground-truth counters + the
replicate-noise z). The MAGNITUDE is pinned on one 10-file corpus
with large between-file variance — at n=10 the paired bootstrap
straddles zero for every variant including the original loss. A wider
degraded corpus should re-pin the numbers before the upstream PR
quotes them.

## Artifacts

`exp-030-results-summary.json` (ablation + fix tables, verdict),
`exp-030-counters.json` (the ground-truth attribution counters),
`exp-030-tables.txt.gz`, `exp-030-commands.md`. Throwaway variant
builds were never committed anywhere; V3 proper is being implemented
on interval-router with tests and doc updates.
