# EXP-011 — code_gen2: insight transfer reaches champion level, doesn't pass it

**Date:** 2026-07-24
**Status:** complete
(exp-008 and exp-010 numbers are reserved for the designed
background-traffic and splitting-pressure experiments.)

## Question
Can a fresh evolution run seeded from the SMALL original router (~380
lines), with the champions' discovered insights supplied only as prose
in the background prompt, reach or beat the champions bred through the
long hb1 → mx_c3 lineage? This tests whether the *ideas* transfer
without dragging an 872-line body through every reflection prompt.

## Setup
Pure gepa engine (`--no-adaptive`), codex/gpt-5.6-sol reflection,
mixed corpus (corpus-mix), 400 evals, seed = in-tree candidate slot.
The background prompt's "Insights from prior successful runs" section
named the bimodal prior, per-directed-channel liquidity bounds over
time decay, retry-at-lower-amount, and the <800-line lean guidance.
Run completed cleanly: 400/400 evals, 31 accepted candidates across 31
iterations (vs code_mix1's 17 accepts in 500 evals — the small-seed
design iterates much faster, confirming the exp-007 hypothesis).

## Result (held-out three-way validation, composite objective)

| tier | lnd | seed | hb1 | mx_c3 | **gen2** |
|---|---|---|---|---|---|
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.583 | 0.565 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | **0.581** | 0.563 |
| mainnet (12,161 nodes) | 0.694 | 0.762 | 0.790 | **0.791** | 0.787 |
| combined average | 0.453 | 0.593 | 0.640 | **0.652** | 0.638 |

Attempts/payment track the champions everywhere (hard 10.1, v2 8.8,
mainnet 2.3 — identical to mx_c3 on mainnet). Exploit-grep clean; 931
lines (it blew through the lean guidance and kept accepting anyway).

## What it evolved
Same paradigm family as the champions — explicit bimodal prior,
per-directed-channel lowerOK/upperFail beliefs with evidence counts,
risk-adjusted Dijkstra, retry-at-lower-amount, zero time-based logic —
plus two mechanisms the champions don't have:

- **Local liquidity reservation:** `reserveRoute`/`releaseRoute` track
  amounts reserved on its own first-hop channels by in-flight shards
  and roll settled amounts into a spent ledger, so concurrent MPP
  shards can't double-book outbound liquidity.
- **Weakest-edge failure attribution:** on an ambiguous
  TemporaryChannelFailure it blames only the lowest-probability
  (least-evidenced) edge on the route rather than penalizing every
  hop, preserving evidence about innocent channels.

## Reading
- **Insight transfer works.** From a small seed plus four sentences of
  prose, 400 evals rebuilt a router within ~1–2% of champions that
  took a 400-eval breakthrough run PLUS a 500-eval continuation to
  breed. The knowledge compresses into the prompt; the lineage doesn't
  need to be replayed.
- **But it plateaued at the same ceiling.** gen2 lands between hb1 and
  mx_c3 on OOD, slightly under both on the hard test, and a hair under
  on mainnet. Three independent lineages (hb1, mx_c3, gen2) now
  converge on the same performance band with the same paradigm —
  strong evidence the interval-belief design is at a local optimum
  *for these environments*. The two genuinely new mechanisms didn't
  move the aggregate: nothing in the current sim (sequential shard
  settlement, no background traffic, no non-binary split pressure)
  rewards them.
- **Implication:** more evals of the same regime buy nothing. The next
  lever is changing the environment, exactly what exp-008 (background
  traffic / virtual clock) and exp-010 (splitting pressure) are
  designed to do. Champions of record remain **hb1 + mx_c3**.

## Artifacts
- Best candidate source: `exp-011-gen2-best-candidate.go` (this dir) —
  kept for reference, not promoted to `champions/`.
- Full sweep numbers: `gen2-validation.json` in the session scratch
  dir (regenerable via `sweep_gen2.py` there; corpora and mainnet
  scenario files regenerate from fixed seeds).


## Caveat added 2026-07-27: the ceiling is confounded with the engine

Every lineage in this comparison — and every run in the program to date
— used ONE optimizer: `optimize_anything` with `engine="gepa"` and
`--no-adaptive`. Three lineages converging on one band is therefore
evidence about that engine's attractor exactly as much as it is
evidence about the problem, and nothing here distinguishes the two.

The GEPA team's own "omni" results make the alternative live rather
than hypothetical: across their benchmark no engine dominated, each of
gepa / autoresearch / meta_harness won roughly a third of problems
unpredictably, each plateaued at a different point, and switching
engines is what broke through. Standalone gepa was the *weakest* of
the three there and gained the most from composition.

So the "paradigm ceiling" reading is **underdetermined, not wrong** —
we never had the data to separate a problem ceiling from an engine
ceiling. The adjudicating experiment is cheap and specified: re-run
this corpus and seed under `meta_harness` or `autoresearch`. If the
band breaks, the ceiling was the engine and this writeup needs a
retraction with data behind it. If the band holds, the claim returns
stronger than it has ever been.