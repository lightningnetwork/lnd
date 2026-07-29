# EXP-026 — The compose world holds: the difficulty ladder reaches the floor

**Date:** 2026-07-29
**Status:** complete. No candidate produced; frontier unchanged
(hb1/mx_c3 champions, atomic1 and econ2 specialists). No verdict
sweep was needed: the run returned its own seed.

## Why this ran

Every prior world yielded to evolution at the standard budget:
the clean corpora bred the champions, the lying channel bred deg1's
attribution confidence (+0.044 on its world), the economic world bred
econ2's budget discipline (+0.022 and the first beat-lnd). The
compose world is both at once — the five economic mechanisms AND the
exp-019 realistic-mix attribution — built by injecting the one-field
attribution stanza into the sealed corpus-econ (byte-round-trip
proven on all 88 files), with a two-variant held-out test (composed,
and the sealed econ test unchanged for delta reading). The question:
can budget machinery and attribution confidence co-evolve, or does
the search have to sacrifice one to afford the other?

## Result: neither. The seed won.

code_full1 ran its full budget (385 evals, 39 proposals, no
hijacks, gate reproduced to six decimals) and returned the
hand-written seed unchanged: best val 0.3162, held-out composed test
0.2157, both the seed's own numbers to the last digit.

This is an honest defeat, not an artifact, and the diagnosis
discipline from code_econ1 was applied before believing it. The
proposal stream was healthy: only 13 of 39 proposals were broken,
zero used reflection (the econ1 prompt fix held), and EIGHT were
accepted into the pool on genuine subsample wins. Every accepted
member attempted the full synthesis — all eight read the fee budget
and priced inbound fees, all eight built reservation tracking, three
added suspect-bound quarantine — and every one of them lost to the
seed on the full validation set. The machinery is expressible in one
router. At this budget, it is not yet profitable: the candidates
paid complexity for three machineries at once and could not recoup
it across a mixed world that punishes every mistake two ways.

## The ladder, in one table

| world | run | gain over seed (val) |
|---|---|---|
| clean (corpus-mix) | exp-011/018 era | +0.05 to +0.06 |
| lying channel | exp-022 | +0.044 |
| economic | exp-025 | +0.022 |
| compose | this run | **+0.000** |

Monotone compression to zero as the environment stacks pressures,
with an identical optimizer, budget, seed and recipe at every rung.
Read with exp-024 (the alternative engine converges below gepa at
ten times the budget), this puts the current stall at the
environment, not the optimizer configuration — the compose world is
the first world that the seed-plus-insights recipe cannot improve on
at 400 evals.

## What it is not

Not a statement that the compose world is unlearnable: one run, one
seed, one budget. The two obvious escapes are pre-registered as the
next arms: seed FROM a specialist (econ2 already carries the budget
machinery paid for; the open question is whether continuation
acquires attribution confidence on top — noting econ2 sits at the
attempt-heavy end, so the exp-013 give-up direction is the one to
watch from the other side), and budget scaling (800 evals would say
whether the wall is real or merely slow). Not a challenger entry
either: nothing was produced, so the ledger stays at nine.

## Artifacts

`exp-026-full1-run.log.gz`, `exp-026-full1-summary.json`. The corpus
(corpus-full) regenerates from the sealed corpus-econ plus the
documented one-field injection; the composed prompt hash and the
gate number (seed val 0.316237) are recorded in the run log and the
scratch gate file.
