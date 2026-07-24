# EXP-009 — Mainnet-graph validation: champions win on lnd's home turf

**Date:** 2026-07-24
**Status:** complete — the closing validation

## Setup
Real mainnet `describegraph` snapshot (12,161 nodes / 39,659 channels,
durable copy at `~/codez/data/mainnet_graph.json`) loaded via the
describegraph loader. Source = the network's highest-degree node (2,015
channels, rebalanced 50/50 per the sim's sender model). 10 scenario
files: 5 liquidity seeds × {bimodal, uniform} hidden-balance regimes, 10
payments each (100k–2M sats, up to 8 MPP parts) — 100 payments per
router. Composite objective as in exp-006.

## Result

| router | objective | success | attempts/pmt |
|---|---|---|---|
| lnd production stack | 0.694 | 0.790 | 19.8 |
| hand-written seed | 0.762 | **0.820** | 6.1 |
| hb1 (evolved) | 0.790 | 0.810 | 2.3 |
| **mx_c3 (evolved)** | **0.791** | 0.810 | **2.3** |

## Reading
- This settles exp-003's standing caveat. The champions were bred
  entirely on synthetic topologies; lnd's defaults were tuned for
  exactly this real graph — and the evolved routers still win, with
  comparable success at **8.6× fewer attempts** (2.3 vs 19.8/payment).
- The gap compresses on success rate (lnd is much stronger here than on
  synthetic corpora: 0.79 vs ~0.3–0.5), but the attempt efficiency gap
  *widens* — the interval-belief memory pays off most where the graph
  offers many alternative paths to burn retries on.
- mx_c3 edges hb1 by a hair, consistent with its generalist profile;
  either is a credible basis for an upstream proposal.
- Caveats: one snapshot, one (well-connected) source, no background
  traffic (see exp-008 design), and the sim's other fidelity gaps
  (batch-2). But every validation tier — sealed synthetic test, OOD
  topology, and now the real graph — points the same direction.
