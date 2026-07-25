# EXP-010 — Splitting pressure: does joint route-set planning emerge?

**Date:** 2026-07-25 (started)
**Status:** in flight — baseline done, evolution run `code_split1` live

## Question
Every winner so far splits reactively: try an amount, and when it
fails, carve the next shard from a ladder of halves and
evidence-derived sizes. Nobody has evolved joint route-set planning —
choosing a set of routes AND their shard amounts together,
min-cost-flow style. Does it emerge when the environment makes
deliberate unequal splits the difference between success and failure?

## Environment
`corpus-split` (seed 4041), built on the new corridors topology
(commit 11f4ccc65): K = 8–16 parallel corridors of deliberately
unequal capacity tiers (one fat corridor, then rungs each at most half
its size) between one source and one target, with the tier enforced
structurally by the target-inbound channel capacity — the fattest tier
is a hard ceiling on any single shard, the tier sum a hard ceiling on
the payment. Bimodal liquidity, no drift (one variable at a time).
Each file: two cheap probes that seed corridor knowledge, then one
ambitious payment above the fattest tier. A forced max_parts=1 control
fails 40/40 files: splitting is mandatory by construction, and the
uneven ladder makes the right split unequal — halving an above-tier
payment yields shards only the fat corridor can carry.

## Baseline (before evolution)

| router | split-val obj | split-test obj | test succ | test att |
|---|---|---|---|---|
| lnd stack | 0.782 | 0.837 | 0.958 | 23.4 |
| seed | 0.594 | 0.644 | 0.750 | 20.1 |
| hb1 | 0.814 | 0.814 | 0.917 | 12.1 |
| **mx_c3** | **0.835** | **0.876** | 0.958 | 10.2 |
| gen2 | 0.801 | 0.770 | 0.875 | 10.7 |
| drift1 | 0.826 | 0.829 | 0.917 | 9.3 |

Findings before evolution starts:
- **This corpus reverses the usual ordering for lnd.** Its production
  divide-and-conquer MPP is genuinely good at completing these
  payments (0.958 success on test, second-best objective) — it just
  pays 23.4 attempts/payment for it. On corpus-building verification
  it beat the naive seed outright (0.79 vs 0.67 mean objective), the
  first environment where lnd tops any evolved-lineage member.
- **mx_c3's halving-plus leads**, consistent with its
  evidence-derived shard ladder, but at ~10 attempts/payment there is
  clear headroom: an efficient joint planner should complete these
  payments in roughly half the attempts (the probes reveal corridor
  tiers; sizing shards to tiers up front should rarely miss).
- The seed's 0.594/0.644 shows the gradient the run gets to climb.

## Evolution run
`code_split1`: pure gepa, codex/gpt-5.6-sol reflection, small seed +
insights prompt, 400 evals, corpus-split. The prompt names joint
route-set planning as unexplored design space (commit 12276e6cf) and
carries the exp-008 lesson so budget is not wasted rediscovering
decay. Success criterion: beat mx_c3 on held-out split-test, then
check the winner's structure — does it plan a route SET up front
(min-cost-flow shape) or refine the reactive ladder further? Either
answer is informative; a win by ladder refinement would suggest
sequential adaptivity beats up-front planning even under maximal
splitting pressure.

## Verdict
(pending run completion)
