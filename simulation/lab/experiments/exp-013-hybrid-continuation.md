# EXP-013 — code_hybrid1: the give-up attractor

**Date:** 2026-07-26
**Status:** complete — negative. Champions unchanged (hb1 + mx_c3).

## Question

The recipe that produced champion mx_c3 was lineage continuation:
take the best router so far, seed a fresh evolution run with it, let
the proposer refine rather than reinvent. exp-010b produced atomic1,
the first challenger with no collapse tier and the program record for
attempts on mainnet (1.6/payment). Apply the same recipe to it: does
continuation from the most attempt-efficient router in the program
produce another champion?

## Setup

Pure gepa engine (`--no-adaptive`), codex/gpt-5.6-sol reflection,
corpus-mix, seed = `exp-010b-atomic1-best-candidate.go` (1,031 lines).
Ran 12:22–17:22 on 2026-07-26, terminating at 352 cache-missing evals
and 30 iterations. Zero stub reflections; hijack canary 0. Winner is
1,436 lines, exploit-grep clean.

In-run trajectory: seed 0.4407 on the valset, best 0.4662 by iteration
19, then flat for the last eleven iterations. All three objective
Pareto axes (success, retry_efficiency, fee_efficiency) were held by a
single program for the entire run — the hybrid frontier had no
specialists to keep alive.

**gepa's own held-out test called it before we did: 0.5124 for the
evolved winner against 0.5274 for the seed it grew from.**

## Result (held-out paired sweep, composite objective, baseline mx_c3)

| tier | n | lnd | hb1 | mx_c3 | atomic1 | **hybrid1** | vs mx_c3 |
|---|---|---|---|---|---|---|---|
| mix_test | 20 | 0.333 | 0.565 | **0.582** | 0.527 | 0.512 | −0.070 (p=.82) |
| hard_test | 10 | 0.298 | **0.498** | 0.479 | 0.417 | 0.427 | −0.053 (p=.75) |
| v2_test | 10 | 0.357 | 0.545 | **0.581** | 0.544 | 0.495 | −0.086 (p=1.0) |
| atomic_test | 8 | 0.338 | 0.444 | **0.444** | 0.400 | 0.427 | −0.017 (p=.73) |
| split_test | 8 | 0.837 | 0.814 | **0.876** | 0.825 | 0.730 | −0.146 (p=.07) |
| mainnet hub | 1 | 0.711 | 0.729 | **0.733** | 0.732 | 0.611 | −0.122 |

No tier is a significant loss on its own — every CI crosses zero. But
the champion rule requires a *win*, and hybrid1 does not win anywhere:
it is below mx_c3 on all six tiers, and below its own seed on the
run's own held-out test. Five independent sealed tiers agreeing on
the sign is the finding; the per-tier p-values are underpowered at
n=8–20 and should not be read as "no difference."

## What actually happened: it learned to give up

The attempt column is where this run explains itself.

| tier | lnd | hb1 | mx_c3 | atomic1 | **hybrid1** |
|---|---|---|---|---|---|
| mix_test | 52.1 | 8.7 | 9.6 | 7.9 | **5.5** |
| hard_test | 30.9 | 7.6 | 8.1 | 7.1 | **4.9** |
| v2_test | 58.8 | 8.2 | 8.4 | 8.1 | **5.8** |
| split_test | 23.4 | 12.1 | 10.2 | 9.2 | **2.2** |

hybrid1 has the fewest attempts on every tier, by a wide margin, and
lower success on every tier. On split_test it converges to 2.2
attempts and 0.750 success while everyone else sits at 0.917–0.958.
On the mainnet hub it drops one payment outright (0.750 → 0.625) at
2.2 attempts. It did not get more efficient; **it stopped trying.**

The objective is `success − 0.01·min(extra_attempts,15) −
0.00002·min(fee_ppm,5000)`. Abandoning a payment costs 1.0 and saves
at most 0.15, so quitting is supposed to be a terrible trade. It
isn't, conditionally: on a payment the router has good reason to
believe is unroutable, the expected value of grinding to the cap is
below the 0.15 it costs. A router that can tell those apart *should*
quit. One that mis-calibrates the belief quits on payments that would
have settled — and that is a smooth, gradient-friendly direction to
walk in, because each step trades a large certain attempt saving
against a small probabilistic success loss.

**This is the program's first observed give-up attractor, and the
seed is what exposed it.** atomic1 entered this run already at the
attempt frontier (1.6/payment on mainnet). Continuation evolution
looks for the nearest improvement to its seed; with nothing left to
win on routing quality, the only cheap direction left was abandonment.
The same recipe applied to hb1 — which had attempts to spare — walked
uphill instead and produced mx_c3.

## Consequences

1. **Continuation from an attempt-frontier seed is a trap.** Before
   the next lineage run, check where the seed sits on the attempt
   axis. If it is already at the frontier, the recipe that produced
   mx_c3 does not apply; it needs a different environment or a
   different objective, not another 400 evals.
2. **Our objective under-punishes abandonment.** The 0.01/attempt
   penalty with a cap of 15 means the worst case for grinding is
   −0.15 while a false abandon is −1.0, but the *variance* makes
   quitting attractive to a search that optimizes the mean. Any
   future run in this regime should report success and attempts
   separately, not just the composite, and a give-up rate belongs in
   the eval output.
3. **The exp-010b "no collapse tier" claim survives.** Checking
   atomic1 against mx_c3 in this same sweep: −0.055 (p=.50), −0.062
   (p=.75), −0.036 (p=.75), −0.044 (p=.07), −0.051 (p=.07), and a tie
   on the mainnet hub. Point estimates below, no significant loss on
   any tier — which is what "no collapse" meant.
4. **hybrid1 did beat its own seed where the seed was bred.** On
   atomic_test, hybrid1 vs atomic1 is +0.027 [+0.018,+0.037] p=0.008
   — the run's only significant result, and in the right direction.
   It improved on the atomic arena and paid for it everywhere else.

## Methodology note: two mainnet tiers, one of them useless

The first mainnet tier tried here was the exp-012 multivantage set
(11 source nodes spanning degree 2024 down to 2). It is not a valid
discriminator: **every router scores identically at 0.227 success**,
because at low-degree vantages the reachable fraction of targets is
fixed by the graph and policies, not by routing skill. Only attempts
differ, so the tier scores an attempt-cost contest and reports it as
an objective difference — atomic1 and hybrid1 "beat" mx_c3 there at
p=0.03/0.02 purely by attempting less on payments nobody can
complete. The number reported above is the exp-009 hub-vantage
scenario (`scen-mainnet.json`), where success does vary and the
comparison means something.

Anyone reusing the multivantage set for champion validation will get
a spurious result. It was built for vantage transfer, and that is all
it is good for.
