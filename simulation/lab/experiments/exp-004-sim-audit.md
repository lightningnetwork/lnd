# EXP-004 — Adversarial audit of the simulator (Fable reviewer)

**Date:** 2026-07-24 (night)
**Status:** critical fix landed; fidelity batch queued

## What was audited
A Fable code-reviewer agent audited the sim for BOLT forwarding
correctness, evaluation integrity (reward hacking), runner fidelity and
determinism, with a working exploit attempt.

## Result
- **BOLT forwarding math confirmed correct** (amount/expiry accounting,
  fee/CLTV check direction, MC failure attribution).
- **CRITICAL (C1, fixed immediately):** `simGossipView.GraphSession`
  delegated to `SimGraph.GraphSession`, which passed the raw `*SimGraph`
  to the callback. A candidate could type-assert it back and read hidden
  balances (`LocalBalances(anyNode)`) or rewrite liquidity
  (`AssignLiquidity`) — a total sandbox escape, demonstrated end to end
  by the reviewer without tripping the banned-token guard. Fix: the view
  now passes itself (it implements `NodeTraverser`); regression test
  `TestSimViewSealed` added. Audited all in-flight run candidates
  (code1, omni1): zero used the escape, so no results were tainted.
- **Also landed:** revert-before-error in `SendHtlc` (malformed routes no
  longer corrupt balances), malformed routes now fail the payment instead
  of killing the whole batch, over-del