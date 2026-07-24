# EXP-004 — Code-mode evolution (whole routing algorithm)

**Date:** 2026-07-24 (night), ongoing
**Status:** first runs done; breakthrough run (code_hard1) in flight

## The bar: the hand-written seed already beats lnd

Three-way on corpus v2 (20 val+test, composite objective =
success − 0.01·min(extra_attempts,15) − 0.00002·min(fee_ppm,5000)):

| router | objective | success | attempts/pmt |
|---|---|---|---|
| lnd stack (defaults) | 0.393 | 0.559 | 58.4 |
| seed router (~300 lines) | **0.547** | 0.681 | 26.4 |
| evolved (code1, 4 iters) | 0.533 | 0.641 | 9.1 |

The paradigm-different seed router beats lnd's production stack by +39%
on the objective. So the bar for *evolution* is the seed, not lnd — a
high bar, since the seed is already strong.

## code1 (adaptive gepa↔meta_harness, corpus v1) — crashed early

The adaptive scheduler rotated to the meta_harness engine on iteration 4
and died in gepa's `_parse_proposer_result` (`'list' object has no
attribute 'get'` — the claude CLI's JSON shape doesn't match the
installed parser). Only 4 iterations ran. Despite that, GEPA accepted a
frontier candidate: a **967-line rewrite** that adds per-channel
`liquidityKnowledge` (lower/upper liquidity bounds + confidence), well
beyond the seed's blacklist. Validated OOD on corpus v2: beats lnd
(0.533 vs 0.393) and is dramatically more efficient (9.1 attempts vs
lnd's 58.4), just shy of the seed on the composite. Clean — no sandbox
exploit. A genuine novel algorithm from 4 iterations; promising.

## meta_harness bug → pivot to pure gepa (bug now patched)

meta_harness (and thus the adaptive/omni composers that include it) was
unusable with this gepa build + claude CLI version: the CLI emits a JSON
*array* of stream events, but `_parse_proposer_result` assumed a single
object and did `payload.get(...)` on a list.

**Patched** in the venv's `gepa/oa/engines/meta_harness.py`: the parser
now picks the terminal `type=="result"` event (or last dict) from an
array. This unblocks omni/adaptive ensembles for a follow-up run. (Not
yet end-to-end validated — running meta_harness now would compete with
code_hard1 for codex/CPU, so deferred.) For a persistent fix, carry a
small monkeypatch in the harness rather than editing site-packages.

gepa is the strongest backend for rich feedback anyway (per the skill),
so the breakthrough attempt is pure gepa regardless.

## code_hard1 (pure gepa, 400 evals, HARD corpus) — in flight

Hard corpus: bimodal-only, small-channel smallworld/grid/hubspoke where
lnd scores 0.1–0.29 (real headroom). Seeded from the seed router. This is
the overnight breakthrough attempt. Results + three-way validation →
exp-006.
