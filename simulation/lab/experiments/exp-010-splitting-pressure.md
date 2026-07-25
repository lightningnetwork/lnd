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

## Pre-registered caveat: corpus resolution (added mid-run)

An Opus 5 advisor review flagged a measurement problem in this corpus
before the runs finished, and we log it before seeing the verdicts.
Each file carries two easy probes plus ONE ambitious payment, so
two-thirds of the success term is free and per-file scores are nearly
binary; at reflection_minibatch_size=3 the acceptance signal
quantizes at ~0.111 while the spread we are selecting for (attempt
efficiency between good routers) is worth at most 0.15. Selection
noise likely exceeds signal, so weak or null verdicts here — including
an indistinguishable codex-vs-Opus A/B — would not be evidence that
joint planning cannot evolve. The follow-up corpus (one probe pair +
8–10 ambitious payments per file) raises per-file resolution ~8x; and
the deeper fix — simultaneous shard commitment, so sequential
adaptivity stops being free — is designed as the successor experiment.

## Verdict — codex arm (code_split2; Opus arm still running)

Run history first: code_split1 was killed mid-run after ~70% of its
reflections were hijacked by global-instruction leakage (see the
CodexLM hardening commits); code_split2 is the clean rerun, 400/400
evals on the isolated CODEX_HOME.

**The mechanism emerged.** The winner (976 lines, exploit-grep clean,
archived as `exp-010-split2-best-candidate.go`) is the first evolved
router that plans route SETS rather than shards in isolation:

- `candidateAmounts` derives UNEQUAL split candidates from known
  bounds and estimated corridor sizes ("each shard sized to a
  differently sized parallel corridor"), not halves.
- `RequestRoute` does one-step-lookahead joint planning: for each
  candidate shard it reserves the route, plans the NEXT shard against
  the remainder (sized so the rest still fits the remaining parts),
  and scores the pair jointly — the utility includes the next shard's
  probability and fee. Reservation during planning
  (`reserveChoice`/`releaseChoice`) is the in-flight machinery two
  earlier lineages invented speculatively, now actually load-bearing.
- Repeat-attempt suppression via route-set hashing replaces
  blacklisting.

Not full min-cost-flow, but structurally past reactive laddering:
selection pressure produced exactly the design family the corpus was
built to elicit.

**It still does not beat the champion.** First sweep with paired
statistics (bootstrap 95% CIs, sign tests, baseline mx_c3):

| tier | mx_c3 | split2 | paired delta [CI] | p |
|---|---|---|---|---|
| split-val | 0.835 | 0.809 | −0.025 [−0.041,−0.011] | 0.008 |
| split-test | 0.876 | 0.810 | −0.067 [−0.145,−0.020] | 0.008 |
| hard test | 0.583 | 0.536 | −0.048 [−0.081,−0.017] | 0.021 |
| OOD v2 | 0.581 | 0.494 | −0.086 [−0.139,−0.043] | 0.021 |
| mainnet | 0.791 | 0.743 | −0.048 [−0.069,−0.026] | 0.039 |

Consistently and significantly behind mx_c3 on every tier — including
the splitting corpus itself, where its lookahead matches mx_c3's
success (0.917 vs 0.958) at similar attempts but pays more in the
penalty terms. Off-corpus it generalizes worse (17-20 attempts on the
static tiers vs mx_c3's 8-11). The paired statistics also settle an
old informal claim: hb1 and mx_c3 are genuinely indistinguishable on
mainnet (delta −0.000 [−0.003,+0.002]).

**Reading.** Same shape as exp-008: the environment reliably elicits
the *mechanism* (there: time-decay; here: joint planning), but a
400-eval lineage carrying new machinery cannot out-refine a 900-eval
champion, and the pre-registered resolution caveat stands — the
corpus's near-binary per-file scores gave selection little gradient
to polish the lookahead with. Champions of record remain hb1 + mx_c3.
The successor experiment is already designed: the higher-resolution
corpus (--split-leads) plus simultaneous shard commitment, which
makes sequential adaptivity stop being free and gives joint planning
its honest arena.

## Verdict — Opus 5 arm (code_split_opus1)
(pending run completion — first frontier-model reflection A/B)
