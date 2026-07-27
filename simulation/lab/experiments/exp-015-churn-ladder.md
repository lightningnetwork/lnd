# EXP-015 — Decay under real churn: exp-008 called a tie a loss

**Date:** 2026-07-26
**Status:** complete — corrects exp-008 and the harness prompt.

## Why this ran

exp-014 fixed a traffic engine that had been running several times
weaker than configured, and the before/after check produced a
directional hint: on the drift tier, stronger churn helped lnd and
slightly hurt all three interval routers. That is exactly the
direction that would undermine exp-008, whose headline is "time-decay
re-evolved under drift and LOST to the time-less champions." exp-008's
own caveat anticipated this: *"A heavier drift regime or a longer run
could still tip the balance toward time-awareness."*

Since exp-008's conclusion is fed to every evolution run through the
harness background prompt — "Spend your complexity budget elsewhere" —
getting it wrong is not a passive error. It steers the search.

## Design

`drift1`, exp-008's evolved time-aware winner, against the time-less
champions, on ONE fixed corpus with only the churn rate varying:
`payments_per_gap` ∈ {0, 20, 80, 240}, everything else — topology,
liquidity seed, amounts, clock, payment set — held identical. With the
fixed engine settling ~0.9 of background payments, 240/gap is roughly
**eighteen times** the effective churn exp-008 actually ran under
(30/gap at a 0.41 settle rate).

Holding the corpus fixed matters: my first attempt compared the old
drift corpus against a regenerated one and the levels moved by 0.25,
which says nothing about churn and everything about topology.

## Result: a tie, everywhere

Paired deltas against mx_c3, n=8 per tier:

| churn/gap | drift1 | hb1 | lnd |
|---|---|---|---|
| 0 | −0.016 (p=.73) | +0.018 | −0.216 |
| 20 | −0.005 (p=.29) | +0.015 | −0.158 |
| 80 | −0.007 (p=1.0) | +0.028 | −0.154 |
| 240 | −0.003 (p=.29) | +0.037 | −0.148 (p=.008) |

Absolute objectives at 240/gap: lnd 0.498, drift1 0.643, mx_c3 0.646,
hb1 0.684.

**drift1 is statistically indistinguishable from mx_c3 at every churn
level, including none.** The deltas are −0.003 to −0.016 with every
CI straddling zero — an order of magnitude smaller than the 0.04 gap
exp-008 reported, and they do not trend with churn.

## What exp-008 got wrong, and how

Not the measurement — the reading. exp-008 saw drift1 at 0.417 against
mx_c3 at 0.457 on drift-test, n=8, and wrote "does not beat the
time-less champions." There was no paired comparison and no interval
on that gap. Re-scoring that same original corpus under the fixed
engine gives −0.033 with p=0.453; the "loss" was never significant.

So the correction is not "the fixed traffic engine changed the
answer." It is that **the answer was a tie in the first place, and the
churn ladder confirms it stays a tie at eighteen times the churn.**
exp-014's directional hint did not survive contact with a controlled
test — which is why it was worth running rather than repeating.

## Consequences

1. **The harness prompt is corrected.** It told every candidate that
   decay "LOST to plain hard bounds" and to "spend your complexity
   budget elsewhere." It now says the result was a tie at every churn
   level, describes the evolved form that tied (confidence softening
   toward the prior with bound expiry, not lnd-style penalty fading),
   and frames it as open. An unsupported negative in that prompt is a
   search-space restriction we imposed on ourselves.
2. **exp-008's substantive findings stand.** Time-awareness genuinely
   re-evolved; its evolved form is structurally unlike lnd's; it costs
   nothing on static tiers. Only the "and lost" clause was wrong.
3. **Decay still hasn't earned anything.** A tie is not a win. The
   honest position is that decay is unproven here, not disproven —
   the complexity has never bought a measurable gain, and nothing
   rules out a form that would.
4. **The hb1-gains-with-churn hypothesis is dead; something smaller
   and more durable replaced it.** See the follow-up below.

## Methodology note

Comparing two differently-generated corpora tells you about the
corpora. The first version of this experiment put the old drift corpus
next to a regenerated one and produced a 0.25 level shift with no
causal meaning. Only within-tier paired deltas survive that, which is
why the ladder holds everything but `payments_per_gap` fixed.


## Follow-up at n=40: the churn trend was noise, but hb1 does edge mx_c3

The ladder showed hb1 gaining on mx_c3 monotonically with churn
(+0.018, +0.015, +0.028, +0.037). Four noisy points at n=8 is exactly
the shape a trend takes when there is no trend, so it got its own test:
a FRESH 40-file drift corpus (seed 7717, scenarios neither router nor
I had seen), at churn 0 and 240.

| tier | hb1 | mx_c3 | atomic1 | lnd | hb1 vs mx_c3 |
|---|---|---|---|---|---|
| n=40, churn 0 | 0.533 | 0.524 | 0.530 | 0.329 | **+0.009** [−0.000,+0.018] p=.014 |
| n=40, churn 240 | 0.553 | 0.540 | 0.519 | 0.386 | +0.013 [−0.005,+0.031] p=.143 |

**No churn effect.** hb1's edge is +0.009 at zero churn and +0.013 at
240/gap — statistically the same number, and the high-churn one is the
*less* significant of the two. The monotone ladder was noise, as
suspected. Whatever separates hb1 from mx_c3 here has nothing to do
with liquidity movement.

What survives is smaller and more interesting: **hb1 beats mx_c3 on
this corpus family at n=40 with a sign test at p=0.014**, at zero
churn, by +0.009. The bootstrap CI barely excludes zero and the effect
is trivial in size, so this is not a champion swap — the rule needs a
held-out paired sweep across the full tier set, and +0.009 on one
corpus family is not that. It is consistent with the long-standing
observation that hb1 leads on the hard tier while mx_c3 leads on OOD
and mainnet: these two are closer than the "generalist champion"
framing suggests, and which one wins is a property of the corpus.

Two lessons carried forward. A monotone trend across four points at
n=8 is worth exactly one follow-up and no writeup. And lnd's deficit
shrinking under churn (−0.195 → −0.154 here, −0.216 → −0.148 on the
ladder) reproduces at n=40 — the one directional claim from exp-014
that has now survived two independent tests.
