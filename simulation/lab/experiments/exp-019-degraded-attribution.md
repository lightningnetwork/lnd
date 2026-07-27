# EXP-019 — Degraded attribution: the 8.6× dies, the margin survives

**Date:** 2026-07-27
**Status:** complete — the advisor's nominated decisive pre-upstream
test, run. Champions unchanged.

## Why this ran

Every attempt-efficiency number this program has published was
measured on a perfect failure channel: instant, truthful, exactly
attributed. Mainnet's channel is none of those. A BOLT4 onion error
can be unreadable — the sender learns only THAT the payment failed —
a buggy or adversarial hop can blame the wrong place, and every
result arrives after delay during which the network moves. The 8.6×
attempt reduction was therefore an upper bound, flagged as such since
the advisor review, and this experiment is the measurement that
replaces the flag.

## Design

The instrument (landed at `d47283c34`): an `attribution` section on
the scenario file degrades results at the single `ReportAttempt`
delivery point both consumer paths share — `unknown_prob` strips
source and code (the lnd path converts this to a nil failure message,
exactly what the switch hands mission control on
`ErrUnreadableFailureMessage`, so lnd runs its real
`processPaymentOutcomeUnknown` logic), `shift_prob` blames an
adjacent hop with the code intact (a well-formed, plausible, wrong
answer), and `delay_slices` holds every result back through slices of
background-traffic time. Three uniforms are drawn per attempt
regardless of outcome, so every router faces the identical
degradation sequence. Absent config is proven byte-identical against
a pre-change binary.

The ladder: the sealed hard-test tier at six levels (control, unknown
0.1/0.3, shift 0.1/0.3, and a realistic mix of unknown 0.2 + shift
0.1), the mainnet tier at control + mix, and the drift tier isolating
delay (control, delay=4 alone, mix + delay). 520 paired runs, five
routers, with the rebuilt binaries gated on reproducing the exp-020
undegraded scores to three decimals.

## Result

Hard tier, objective (Δ vs each router's own control):

| router | control | unk .1 | unk .3 | shift .1 | shift .3 | mix |
|---|---|---|---|---|---|---|
| lnd | 0.309 | 0.202 (−.107) | 0.162 (−.147) | 0.394 (+.085) | 0.431 (+.122, p=.002) | 0.188 (−.121) |
| seed | 0.530 | −.001 | −.004 | −.013 | −.040 | −.016 |
| hb1 | 0.586 | −.008 | −.029 | −.008 | −.093 | −.061 |
| mx_c3 | 0.583 | −.012 | −.064 | −.021 | −.042 | −.067 |
| atomic1 | 0.510 | −.023 | −.098 | −.024 | −.085 | −.042 |

Champion−lnd margin by level: hb1 +0.277 → +0.377 (unk .1) → +0.395
(unk .3) → +0.185 (shift .1) → **+0.062, CI straddling zero (shift
.3)** → +0.336 (mix). Mainnet at the realistic mix: hb1 +0.080,
mx_c3 +0.077, atomic1 +0.079 (p=.021) — everyone still clears lnd.

## Finding 1: the ordering survives the realistic channel

At the realistic mix and on degraded mainnet, every champion beats
lnd, and on the hard tier the margin *widens* under unreadable
errors. The single level that erases the margin is shift=0.3 — 30% of
failures plausibly misattributed — and it does so not by hurting the
champions but by helping lnd (finding 3). Nothing resembling the
feared "champions are calibrated to a clean channel and collapse
without it" appears anywhere on the ladder.

Why the champions barely move: none of the evolved routers writes a
liquidity bound from an unattributed failure. The seed ignores it
outright; hb1 and mx_c3 apply only soft session penalties with no
interval update; atomic1 marks the route suspect. By evolution or by
accident, they all treat no-information as no-information. (mx_c3
even evolved an explicit anonymous-failure contingency,
`recordAnonymousFailure`, that fires on a readable code with an
unknown source — a case the protocol-faithful unreadable marker
routes past. Nobody had noticed it until this audit.)

## Finding 2: lnd's unknown-failure handling is a give-up spiral, from 10% unreadable errors

lnd's `processPaymentOutcomeUnknown` penalizes every pair of the
failed route in BOTH directions. On the hard tier that turns a 10%
unreadable-error rate into: give-ups 0.31 → 0.71, attempts 45.5 →
6.3, success 0.49 → 0.29. At 30% four of ten files pin to exactly
zero success — lnd blacklists routes until pathfinding returns "no
path" and quits. The same signature appears on mainnet at the
realistic mix: success 0.790 → 0.730, give-ups doubling, attempts
19.8 → 2.8. No other router shows anything like it.

This is a concrete, self-contained upstream finding independent of
everything evolved: **the response to an unreadable error is so
aggressive that a modest rate of them exhausts the route set.** It
joins the exp-016/exp-002b convergence as a third input to the
distillation patch — the failure-information handling needs work in
both directions (bounds too weak when attributed, penalties too
strong when not).

## Finding 3: plausible lies help lnd (an anomaly, mechanism unproven)

shift=0.3 is lnd's best hard-tier configuration in this program's
history: +0.122, CI [+0.067,+0.182], ten files of ten, p=.002 —
success genuinely rises (the attempt-cap term is only +0.038 of it).
Being lied to a third of the time beats being told the truth. The
candidate mechanism — on short small-world routes, "one hop off" is
often the same bottleneck seen from the other side, so a coarser
wrong penalty pushes lnd out of a bad region faster than the precise
correct one — is flagged unproven: it is tier-A-only evidence, and a
shift-isolated mainnet arm should run before any mechanism story is
published. After exp-016's three wrong guesses, the anomaly ships
labelled as an anomaly.

## Finding 4: delay is free; misattribution is the binding constraint

Holding every result back four attempt-slices on a live drifting
network moves nobody: deltas of −0.002 to +0.026, every CI straddling
zero, with the counters confirming 100% of results were delayed. All
of the combined level's damage is its misattribution component. This
extends exp-012 and exp-015's pattern — evidence staleness keeps
failing to matter in this environment — to the delivery channel
itself.

## Finding 5: the 8.6× is dead; what replaces it is stronger

Under any unreadable-error rate the attempt ratio *inverts*: lnd uses
fewer attempts than the champions (mix: 3.0 vs 16.3) — because it has
stopped paying for hard payments. Quoting attempt ratios on a
degraded channel is therefore meaningless in both directions, and the
8.6× was always a perfect-channel artifact.

The replacement claim is better. On degraded mainnet the champions
hold success at *exactly* their undegraded values (0.810 → 0.810,
0.800 → 0.800) for +0.2–0.45 attempts, while lnd trades 6 points of
success and a doubled give-up rate for its attempt drop. **Realistic
degradation converts the champions' edge from an attempt edge into a
success edge.** The efficiency was a fair-weather bonus; the
robustness is structural.

## Abandonment watch

Clean for all three interval routers: attempts and success move
together (spend more, get less), which is degradation, not the
exp-013 attractor. atomic1 sits closest to the line (give-ups 0.37 →
0.45 at unknown 0.3, attempts near-flat while success falls),
consistent with its exp-012 shrug-under-uncertainty policy.

## Caveats

n=8–10 per tier. hb1's shift-level magnitudes lean on one file (61%
of the delta; direction is 9/10). Two hard-tier files are
near-degenerate for lnd under unknown (success pinned at zero),
inflating champion margins at those levels. The mainnet arm inherits
the synthetic-liquidity caveat as always. And the shift-helps-lnd
anomaly has no mainnet or mechanism-isolating arm yet — it is a
measured fact with an unproven story attached.


## Follow-up (exp-019b): the anomaly is hard-tier-only, and the story I told about it was wrong twice

The shift-isolated mainnet arm ran the same night: shift 0.1 and 0.3
with no unknown and no delay, paired against the exp-019 controls
(which reproduce bit for bit), lnd and mx_c3, n=10.

**Shift does not help lnd on mainnet.** +0.014 [−0.034,+0.060] at
0.1, −0.026 [−0.097,+0.044] at 0.3 — both CIs straddle zero, the
sign flips between levels, and the per-file deltas are heterogeneous
in both directions. Decomposing the objective shows the two tiers are
different animals: the hard-tier gain was success-led (+0.069 of the
+0.122), while on mainnet success FALLS (−0.040, −0.100) and
give-ups rise on 8 of 8 files — that is finding 2's give-up spiral
wearing a smaller mask, not finding 3's anomaly. mx_c3 is immune on
both tiers (success and give-ups exactly unchanged; the whole effect
is +0.3–0.4 attempts).

The route-geometry story fails in a way worth recording. It predicted
the effect would shrink on mainnet, and the effect vanished — but the
prediction rested on mainnet routes being LONGER, and they are three
times SHORTER (first-attempt mean 1.9 hops vs 5.4; the exp-009 hub
source reaches 29–62% of targets in a single hop). A single-hop
route is the case where "one hop off" most often lands on the
sender's own channel, so the story predicts full-or-greater strength
exactly where the data shows none. An outcome can match a prediction
while refuting its premise; this one did. The anomaly now carries two
facts and no mechanism: it is real on the hard tier (10/10, p=.002)
and it does not travel to a different graph.
