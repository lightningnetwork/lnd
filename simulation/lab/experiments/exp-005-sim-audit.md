# EXP-005 — Simulator correctness audit (Fable code-reviewer)

**Date:** 2026-07-24 (night)
**Status:** critical fix applied; fidelity fixes batched

## Verdict
BOLT forwarding math (`SendHtlc`/`checkPolicy`) confirmed **correct**:
per-hop amount/expiry accounting, fee and CLTV check direction, min/max
HTLC, and mission-control failure-index attribution all match production
semantics. The problems were in sandbox sealing and runner fidelity.

## C1 (critical) — sandbox was not sealed — FIXED
`simGossipView.GraphSession` delegated to `SimGraph.GraphSession`, which
calls `cb(g)` with the concrete `*SimGraph`. A candidate could
type-assert the session graph back to `*routing.SimGraph` (exported) and
call `LocalBalances` (read any hidden balance) or `AssignLiquidity` /
`BalanceNodeChannels` (rewrite ground-truth liquidity) — a perfect-score
reward hack using no banned tokens. Reviewer demonstrated a working
escape.

**Fix:** `GraphSession` now passes the sealed view itself (`cb(v)`), never
the graph. Added `TestSimViewSealed` regression test asserting neither the
view nor the session graph can be asserted to `*SimGraph`.

**Exposure check:** audited all in-flight eval candidates (code1 + omni1
phase 1) for use of `GraphSession`/`LocalBalances`/`AssignLiquidity` —
**zero hits**. The optimizer had not discovered the hole; no result was
corrupted. Code-mode evals recompile against repo source per eval, so runs
picked up the seal immediately.

## Also fixed now (no contract change, safe mid-run)
- `SendHtlc` malformed-route errors now `revert()` balances and terminate
  only that payment (recorded as an error), not the whole batch — one bad
  edge case no longer zeroes a functional candidate (was M4).
- `amtRemaining` over-delivery guard against unsigned underflow (m4).
- `NodeByAlias` resolves ties to the lexicographically smallest pubkey —
  determinism on alias-collided real graphs (m2).

## Batched for after current runs (contract-affecting — defer for
consistent mid-run comparisons)
- **M1 determinism:** mission control uses a real-time clock; wall-clock
  jitter between recording a failure and the next probability query can
  flip a routing decision at a tie or the MinProbability cutoff. Inject a
  deterministic/virtual clock via `MissionControlConfig.clock`. High
  priority — must land before we publish head-to-head numbers.
- **M2 fidelity:** first-hop bandwidth hints are snapshotted once per
  payment; rebuild from live balances each `RequestRoute` so sequential
  MPP shards over one source channel behave like a real node.
- **M3 symmetry:** lnd baseline gets persistent mission control across
  payments; candidate routers are rebuilt per payment with no persistent
  state handle. Either add a batch-scoped state object to the
  `SimRouterFactory` contract or reset MC per payment — make the contest
  symmetric and explicit.
- Minor: `OutPolicySet` vs `!Disabled` (m3), source-hop min/max/disabled
  checks (m5), weak banned-token guard now that C1 is sealed at the Go
  level (m1).

## Tests added / to add
- Added: `TestSimViewSealed`.
- To add: determinism-under-decay (`-count=20`), `checkPolicy` table
  tests + liquidity-conservation on mid-route failure, MPP same-first-hop
  attempt count, lnd-vs-candidate parity.
