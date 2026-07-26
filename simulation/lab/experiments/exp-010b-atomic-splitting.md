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

## Baseline — the ordering changes, confirming the subsidy

Sim change landed (d0f062747; hold ledger on the graph, shared hop
walk with a commit mode, prorated attempt-boundary traffic with
fractional carry; flag-off byte-identity verified over two corpora).
Corpus: `corpus-splitatomic` (seed 6061, --split --split-leads 5
--atomic). All routers rebuilt against the new tree.

| router | atomic-val obj | atomic-test obj | test succ | test att |
|---|---|---|---|---|
| lnd stack | 0.286 | 0.338 | 0.500 | **104.8** |
| seed | 0.389 | 0.385 | 0.536 | 56.5 |
| hb1 | 0.430 | 0.444 | 0.554 | 10.7 |
| **mx_c3** | **0.442** | **0.444** | 0.571 | 12.6 |
| split2 | 0.356 | 0.391 | 0.554 | 26.9 |
| opus1 | 0.429 | 0.425 | 0.571 | 23.5 |
| opusmed1 | 0.357 | 0.373 | 0.536 | 28.0 |

Success criterion 1 answered YES before any evolution: **the arena
change alone reorders the field.** lnd, second-best on the non-atomic
corpus (0.837), collapses to last (0.338) at 105-113 attempts/payment
— its divide-and-conquer probe ladder is precisely what the arena now
taxes, so sequential adaptivity was indeed being subsidized. The
champions hold the top (hb1 ties mx_c3 exactly on test, +0.001
p=.73), but the gap to the joint planners compresses: opus1's
persistent-plan router is statistically indistinguishable from mx_c3
on BOTH atomic tiers (val −0.013 p=.73, test −0.019 p=.29) despite
never having seen atomic semantics, while the shallower planners
(split2, opusmed1) fall significantly behind. Deeper joint planning
already pays under honest pricing.

Tempering check (pre-registered): mean success sits at 0.45-0.57 and
objectives at 0.29-0.44 — hard, not collapsed, real headroom. The
churn parameters stand; evolution proceeds unchanged.

## Evolution runs (in flight)

`code_atomic1` (codex/gpt-5.6-sol) and `code_atomic_opus1` (Opus 5
default effort — the proposer that built the deepest planner in
exp-010), both 400 evals on corpus-splitatomic, launched 2026-07-25
evening. The background prompt now states the atomic arena's
economics explicitly (holds, contention, drift per attempt, atomic
release) and the exploit grep bans the hold-ledger API. Success
criterion 2: beat mx_c3 on held-out atomic-test; criterion 3: no
collapse on the legacy tiers.

## Verdict — Opus arm (code_atomic_opus1; codex arm still running)

The Opus-default arm completed cleanly (400 evals, 51 iterations,
zero degraded reflections — first fully sealed run). Its winner
(987 lines, exploit-grep clean, archived as
`exp-010b-atomicopus1-best-candidate.go`) re-evolved exactly the
mechanism family the arena targets — genuinely min-cost-flow-ish
up-front planning: corridors enumerated once per plan with per-edge
reservations, excluded by whole edge set so shards cannot silently
contend, shard sizes from believed capacity, residual planning that
reuses known bounds — plus one mechanism new to the family, bred by
drift: repeated whole-plan failure RELAXES hard bounds slightly so a
churning network can be re-probed instead of going permanently
unroutable.

**And it still loses.** Paired sweep (all routers rebuilt on the
current tree; the scratch legacy corpora were regenerated after a
reboot, so compare deltas within this table, not levels across
writeups):

| tier | mx_c3 | opus1 (unevolved challenger) | atomicopus1 |
|---|---|---|---|
| atomic-val | 0.442 | 0.429 (−0.013, p=.73) | 0.374 (−0.067, p=.29) |
| atomic-test | 0.444 | 0.425 (−0.019, p=.29) | **0.391 (−0.053, p=.008)** |
| split-test | 0.876 | 0.841 (−0.035, p=.07) | 0.711 (−0.165, p=.008) |
| hard test | 0.479 | 0.284 (−0.195, p=.18) | 0.247 (−0.232, p=.109) |
| OOD v2 | 0.581 | 0.483 (−0.098, p=.34) | 0.367 (−0.214, p=.109) |
| mainnet | 0.791 | 0.757 (−0.033, p=.18) | 0.738 (−0.053, p=.18) |

The failure mode is legible in one column: 57.5 attempts/payment on
atomic-test vs mx_c3's 12.6 and the challenger's 23.5. The
relax-and-re-probe loop converts drift-tolerance into attempt burn,
and the objective's attempt penalty (plus the drift each extra
attempt invites) eats the success gains. Evolution polished the
right architecture into the wrong economy.

Notable negative result within the A/B family: 400 evals of evolution
ON the atomic arena produced a router WORSE there than exp-010's
opus1, which never saw atomic semantics (0.391 vs 0.425). Selection
inside a high-variance arena is noisy enough that the pre-registered
resolution caveat looks binding again — per-file scores on this
corpus swing with churn, so minibatch acceptance is noisy even with
7 graded payments per file. The codex arm will tell us whether this
is proposer-specific or the arena's selection signal is the binding
constraint.
