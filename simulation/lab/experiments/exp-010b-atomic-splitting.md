# EXP-010B — Atomic commitment: joint planning's honest arena

**Date:** 2026-07-25 (started)
**Status:** in flight — sim change under implementation, corpus design
pre-registered

## Question

exp-010 elicited joint route-set planning from three proposer lineages
and none of it beat the champion's reactive ladder. The suspected
reason is that the simulator made sequential adaptivity free: a shard
settles instantly, its outcome arrives before the next route request,
and the world freezes while the payment runs. Probe-learn-resize
therefore enjoys all of joint planning's information advantages at
none of its costs. If the arena charges honestly for sequential
probing, does joint planning finally win — and specifically, does the
persistent-plan machinery opus-default evolved in exp-010 become
load-bearing?

## Pre-registered design (before any results)

The sim change (scenario flag `atomic_mpp`) alters three couplings and
deliberately does NOT alter the feedback channel:

1. **Hold-and-release shards.** A shard that traverses successfully
   holds liquidity along its path instead of settling. All held shards
   settle together when the payment completes; all release (no fees,
   balances restored) when it fails. Failed MPP becomes atomic — this
   also removes a known fidelity distortion flagged by the Fable
   simulator advisor.
2. **Held-liquidity contention.** Sibling shards and background
   traffic see availability net of holds, so a probing router
   physically reserves what it probes and two shards can no longer
   double-count one corridor.
3. **The world keeps turning.** Background traffic runs on attempt
   boundaries (30 virtual seconds each in the new corpus), not just
   between payments. A twenty-attempt reactive ladder watches ten
   minutes of corridor churn; a joint plan that fills MaxParts up
   front commits before the world moves.

Per-attempt `ReportAttempt` feedback is unchanged. We considered
wave-batched feedback (router learns nothing until a whole shard set
resolves) and rejected it: on mainnet each shard's failure IS observed
as it happens, so information denial would be less realistic, and it
would require breaking the SimRouter contract that keeps all seven
existing routers comparable. The cost of sequential probing here is
time and reservation, which is the real mainnet cost.

**Corpus:** `gen_scenarios --split --split-leads 5 --atomic`
(corridors topology, ~7 graded payments per file, descending lead
ladder). This composes both exp-010 follow-ups at once: the
resolution fix (near-binary per-file scores quantized minibatch
selection below the signal being selected for) and the honest arena.
Traffic amounts stay within the thinnest corridor rung so churn
perturbs tiers without wiping them.

**Success criteria, in order of interest:**
1. Does the baseline ORDERING change? If mx_c3's reactive ladder loses
   ground to lnd or to exp-010's joint planners (split2/opus1) merely
   from the arena change, sequential adaptivity was indeed being
   subsidized.
2. Does evolution on this arena produce a router that beats mx_c3 on
   the held-out atomic split tier — and does the winner plan route
   sets up front?
3. Does anything evolved here hold up on the four legacy tiers
   (regression guard: hard, v2, mainnet, non-atomic split)?

**Pre-registered caveats.** (a) The 30 s/attempt and 8-payments-per-gap
churn parameters are design choices, not measurements; if baseline
success collapses below ~0.3 or stays above ~0.9 across routers, the
arena is mis-tempered and gets re-tempered BEFORE evolution (parameter
changes after seeing evolution results would be p-hacking; changing
them after only baseline results is calibration). (b) Held-shard
contention plus attempt-time churn makes per-file variance higher than
exp-010's corpus; the paired sweep and sign tests remain the decision
standard.

## Baseline (pending sim change)

## Evolution run (pending)

## Verdict (pending)
