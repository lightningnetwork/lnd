# EXP-024 — The ceiling arm: meta_harness at 10x converges, far below the band

**Date:** 2026-07-27
**Status:** complete — the one run exp-018 said would separate "problem
ceiling" from "every practical optimizer stalls at the starting line."
Champions unchanged.

## Why this ran

exp-018 gave three engines the same seed, corpus and 150-eval budget,
and only gepa produced anything: meta_harness benchmarks each candidate
against the full 68-example set, so 150 evals bought exactly one
iteration and its "best" was the seed it started from. That left the
adjudication answer with an explicit hole: at practical budgets the
~0.64 band is not a gepa artifact, but nothing had tested whether an
alternative engine given real room would break it, reach it, or
converge somewhere else entirely. The costed fix was meta_harness at
roughly 10x, and this is that run: same seed, same corpus-mix, 1,500
evals, $60 proposer cap, launched alongside code_deg1 on an otherwise
idle tree.

## Result

| arm | val | held-out test | evals | iters | wall | proposer $ |
|---|---|---|---|---|---|---|
| meta_harness @ 10x | 0.4677 | 0.5136 | 1,496 | 8 | 157m | $15.68 |
| (exp-018) meta_harness @ 1x | 0.4539 | 0.5082 | 136 | 1 | 19m | $1.95 |
| (exp-018) gepa @ 1x | 0.5102 | 0.5565 | 150 | 13 | 9.0h | $0 |
| (exp-018) seed | 0.4539 | 0.5082 | — | — | — | — |

The starting-line story is dead: given room, meta_harness iterates and
improves. Eight iterations, five of them finding a new best, a real
422-line candidate at the end (`log_bimodal_cost`, exploit-grep clean),
and the first improvement over the seed any claude-proposer engine has
produced in this program.

The trajectory is the finding. The improvements land almost entirely in
the first hour: +0.0041 (iter 1), +0.0046 (iter 2), +0.0049 (iter 3),
then +0.0001, +0.0001, zero. By iteration 3 the arm sat at 0.4675 and
the remaining 950 evals bought +0.0002. That is convergence, not
starvation — and the shelf it converges to (0.4677 val, 0.5136 test) is
below gepa's own result at ONE TENTH the eval budget (0.5102 val,
0.5565 test), which is itself well below the champions' band.

## Reading

1. **The band survives a second engine at an order of magnitude more
   budget.** exp-018's caveat is retired: the ~0.64 band is not an
   artifact of starving the alternatives. meta_harness, given 10x,
   converges to a shelf a full 0.043 of held-out test below where gepa
   lands on 150 evals. Whatever the band is, it is not "the only
   optimizer we tried."
2. **gepa's moat compounds instead of closing.** The full-set benchmark
   costs meta_harness 68 evals per candidate, so 1,500 evals bought 22
   candidate evaluations; gepa's minibatch loop got 13 accept/reject
   decisions out of 150. Eval efficiency is not a small-budget quirk of
   the comparison, it is the mechanism, and scaling the budget scales
   the gap.
3. **The proposer plateaus on quality, not on budget.** Iterations 4
   through 8 kept proposing (three candidates each, $1.85 to $1.99 per
   session, every session exited clean) and could not find anything
   past 0.4677. The candidate names tell the story of a search circling
   the same ideas: `directional_belief`, `capacity_penalty`,
   `log_bimodal_cost`, `failure_code_filter`, `widest_path_routing`.
   It found the general region (bimodal-ish cost shaping) and could not
   find the interval apparatus from there.
4. Cost accounting for the ledger: the whole arm was $15.68 and 157
   minutes. The exp-018 costing predicted ~$2 and ~19 minutes per
   swing; eight swings landed within 3% of that.

## Consequences

1. The exp-018 open question is closed on both halves: not a gepa
   artifact (exp-018), and not budget starvation (this run). The ~0.64
   band now looks like a property of the problem or of the paradigm
   class gepa reaches, and the remaining escape hatches are environment
   changes (exp-023) rather than optimizer changes.
2. CLAUDE.md queue item 4 (the 10x ceiling arm) is done. No further
   engine adjudications are planned at any budget.
3. `log_bimodal_cost` joins the challenger ledger as failure number
   seven, the first from a claude proposer: no tier sweep needed at
   0.5136 held-out test against a seed of 0.5082 and champions far
   above both.

## Caveats

One run, one seed, one corpus, as always for the engine arms; exp-010b
showed proposer variance can flip orderings between environments. The
arm ran concurrently with code_deg1's evals on the same machine, which
affects wall-clock only (the eval server serializes nothing across
runs; scores are deterministic per candidate). And 10x is not infinity:
nothing here bounds what meta_harness would do with minibatch
benchmarking bolted on, but that engine change is upstream work
(gepa-ai/gepa), not an experiment this program owes.

## Artifacts

`exp-024-ceiling1-best-candidate.go` (the winner, exploit-grep clean),
`exp-024-ceiling1-adjudication.json` (per-arm verdict),
`exp-024-ceiling1-run.log.gz` (full run log). All harvested from
scratch before it could be reboot-wiped.
