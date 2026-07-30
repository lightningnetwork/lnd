# EXP-028 — Compose escape arm 1: the specialist seed meets the give-up attractor

**Date:** 2026-07-29/30 (overnight run, code_full2).
**Status:** complete. No challenger produced; frontier unchanged. The
compose wall (exp-026) holds against the first pre-registered escape.

## The arm

exp-026 pre-registered two escapes from the compose world's +0.000:
seed from a specialist that already carries part of the synthesis
paid for, and budget scaling. This is the first. code_full2 seeded
gepa with econ2 itself (1,230 lines, the budget machinery already
built) on the identical composed corpus, optimizer, and 400-eval
budget. The exp-013 give-up direction was pre-registered as the thing
to watch: econ2 sits at the attempt-heavy end (22.6 att/pmt on this
world), so the cheapest objective direction available to a
continuation is shedding attempts by abandoning payments.

## Result: the attractor, reproduced from a second seed family

The run was healthy (400/400 evals, 35 proposals, zero hijacks, ten
pool accepts) and its best-val candidate is real machinery: it keeps
econ2's budget ledger and inbound pricing and adds an edge-penalty /
blocked-set attribution layer with suspect handling. It also loses to
its own seed on held-out, and the decomposition is exactly the
registered pattern:

| arm | obj | success | att/pmt | give-up |
|---|---|---|---|---|
| econ2 (seed) | 0.2373 | 0.3779 | 22.6 | 0.6221 |
| code_full2 best | 0.2071 | 0.3469 | 18.4 | 0.6531 |

Paired: objective −0.030 (9W/10L/1T), success −0.031, attempts
−4.2/pmt. Every point of objective lost is success; every attempt
saved is a payment abandoned. `give_up_rate == 1 − success`
identically on both arms. This is exp-013's finding with atomic1
replaced by econ2 and the atomic arena replaced by the compose world:
**continuing a seed that sits at the attempt frontier converts the
optimizer into an abandonment machine**, independent of seed lineage
and environment. Two independent reproductions make it a rule, not
an anecdote.

The selection layer failed honestly too: gepa's best-val pick (val
0.2833) undershoots its own seed on held-out by 0.030 — minibatch
val selected a val-shaped candidate. The runner's printed numbers
were reproduced exactly by an independent overlay rebuild and
20-file re-run before writing any of this down (0.2071/0.2373 to
four digits).

## What the arm did buy

econ2's machinery transfers into the compose world for free: the
seed's own composed held-out is 0.2373 against the hand-written
seed's 0.2157 (exp-026, same files). The +0.022 that budget
discipline was worth in the clean economic world survives the
addition of the lying channel intact. The wall is not that the
compose world rejects economic machinery — it is that 400 evals of
reflective search cannot find anything on TOP of either seed that
survives held-out.

## Ladder update

| world | seed | gain over seed (val) | held-out vs seed |
|---|---|---|---|
| clean | hand | +0.05 to +0.06 | + |
| lying channel | hand | +0.044 | + |
| economic | hand | +0.022 | + |
| compose | hand | +0.000 (seed returned) | 0 |
| compose | econ2 | best-val BELOW seed | **−0.030** |

The remaining pre-registered escape is budget scaling (800 evals).
With both seed families now measured, that arm carries the whole
question: if it also fails, the compose world is the first
environment where the seed-plus-insights recipe is DONE at any seed,
and the interesting frontier moves fully to the integration branch
and the foreign-balance-sheet tier.

## Artifacts

`exp-028-full2-summary.json`, `exp-028-full2-best-candidate.go` (pool
#7, byte-matched to the runner's final print, exploit-grep clean),
`exp-028-heldout-rerun.json` (the independent 2-arm re-run),
`exp-028-full2-run.log.gz`. Corpus = corpus-full (regenerates from
sealed corpus-econ + the documented attribution injection).
