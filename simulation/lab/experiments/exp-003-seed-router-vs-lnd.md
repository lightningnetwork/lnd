# EXP-003 — Seed candidate router vs lnd stack

**Date:** 2026-07-24
**Status:** complete — corpus-wide confirmed

## Setup
One hard val example (`corpus/val/example_001.json`, bimodal liquidity),
identical scenarios, two routers:
- `--router=lnd`: production pathfinding + mission control, default params.
- `--router=candidate`: the ~300-line seed — cheapest-path Dijkstra,
  per-payment failure blacklist (lowest failed amount per channel),
  halving MPP splits.

## Result

| metric | lnd stack | seed router |
|---|---|---|
| success rate | 0.222 | **0.333** |
| attempts/payment | 112.6 | **7.8** |
| fee ppm (on success) | 440 | 405 |
| value delivered (msat) | 1.5G | **6.5G** |

## Reading
- The lnd defaults spend enormous retry budgets in bimodal small-channel
  regimes (112 attempts/payment; the apriori penalty half-life keeps
  resurrecting doomed channels within a batch).
- A trivial "never retry above a failed amount" memory is very strong in
  a bimodal world: one failure ≈ the channel is empty in that direction.
- Caveats: single example; the seed router's blacklist resets per
  payment while mission control persists across the batch (helps *and*
  hurts lnd here); lnd's MinProbability floor forces splitting that the
  seed avoids.

## Corpus-wide confirmation (all 16 val+test examples)

| example | liquidity | lnd sr / att | seed sr / att |
|---|---|---|---|
| val/000 | uniform | 0.38 / 125.4 | 0.50 / 9.1 |
| val/001 | uniform | 0.22 / 112.6 | 0.33 / 7.8 |
| val/002 | bimodal | 0.25 / 78.5 | 0.62 / 4.6 |
| val/003 | bimodal | 0.25 / 103.1 | 0.75 / 75.2 |
| val/004 | bimodal | 0.40 / 82.0 | 0.70 / 7.2 |
| val/005 | bimodal | 0.00 / 56.1 | 0.12 / 16.5 |
| val/006 | bimodal | 0.33 / 95.8 | 0.78 / 50.4 |
| val/007 | uniform | 0.00 / 57.1 | 0.00 / 9.9 |
| test/000 | bimodal | 0.57 / 61.7 | 0.86 / 39.0 |
| test/001 | bimodal | 0.10 / 30.7 | 0.10 / 26.0 |
| test/002 | uniform | 0.67 / 69.1 | 1.00 / 15.2 |
| test/003 | bimodal | 0.43 / 54.9 | 1.00 / 62.6 |
| test/004 | bimodal | 0.29 / 123.1 | 0.43 / 91.0 |
| test/005 | bimodal | 0.40 / 102.6 | 0.80 / 48.1 |
| test/006 | bimodal | 0.00 / 30.8 | 0.00 / 61.1 |
| test/007 | uniform | 0.33 / 133.8 | 0.83 / 38.2 |
| **mean** | | **0.289 / 82.3** | **0.552 / 35.1** |

The seed router wins or ties on 16/16 examples: mean success rate 1.9×
higher at 2.3× fewer attempts. The gap holds on uniform liquidity too, so
this is not purely a bimodal artifact.

## Caveats
- These are small-channel synthetic topologies (2–10M sat channels,
  50–500 nodes); lnd's defaults were tuned against mainnet scale. The
  claim is regime-specific until a mainnet-snapshot corpus exists.
- simMaxAttempts=200 lets the lnd stack keep digging where a real
  payment would have timed out; attempts/payment differences partly
  reflect its willingness to keep trying.

## Corpus v2 (with mainnet-like scale-free nets) — caveat addressed

Corpus v2 adds Barabási-Albert scale-free topologies (800/1500 nodes,
log-normal capacities, hub-dominated) alongside the earlier families.
Across all 20 v2 val+test examples:

| regime | lnd sr / att | seed sr / att |
|---|---|---|
| ALL (20) | 0.559 / 58.4 | 0.681 / 26.4 |
| scale-free only (7) | 0.819 / 20.4 | 0.860 / 16.0 |

On mainnet-like graphs the lnd defaults are near parity — consistent with
them having been tuned for mainnet scale. The seed's edge concentrates in
small-channel/hard-liquidity regimes, but it never loses on average in
any regime tested.

## Follow-ups
- Real mainnet describegraph snapshot as the final validation tier.
- This is the headroom story for Phase 3: if 300 naive lines beat the
  stack in this regime, an evolved router should beat both decisively.
