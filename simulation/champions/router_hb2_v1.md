# hb2 — archived lineage member

`router_hb2_v1.go` (1,166 lines) came out of the same run as hb1 and was
briefly a genuine Pareto sibling: worse on the hard corpus, better
out-of-distribution. mx_c3 then beat it on both, so hb2 is retired. The
file stays in this directory because two of its ideas are worth keeping on
the record — one that the later champions dropped, and one that arrived at
lnd's own bimodal derivation by a different route.

This document is deliberately shorter than the other two. For the full
comparison against lnd's production stack, read `router_hb1_v1.md`;
everything there applies here.

## Provenance

| field | value |
|---|---|
| run | `code_hard1`, frontier candidate 2 (the same run that produced hb1) |
| seed program | the in-tree hand-written router, `cmd/routesim/candidate_impl.go` |
| training corpus | the `--hard` regime |
| writeups | `simulation/lab/experiments/exp-006-breakthrough.md` (introduced), `exp-007-mix-followup.md` (retired) |
| status | **archived**; strictly dominated by mx_c3 |

## Validated scores

| tier | lnd | seed | hb1 | hb2 | mx_c3 |
|---|---|---|---|---|---|
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.545 | 0.583 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | 0.577 | **0.581** |
| average of those two | — | — | 0.565 | 0.561 | **0.582** |

hb2 was retired before the mainnet and drift tiers existed, so it was never
measured on them.

In exp-006 hb2 looked like a real trade-off against hb1: give up 0.041 on
the hard test to gain 0.032 out of distribution. exp-007's frontier sweep
removed the choice. mx_c3 scores 0.583 and 0.581, winning both columns, so
there is no scenario mix in which hb2 is the right pick.

## What it does differently

hb2 shares the paradigm with hb1 and mx_c3: a per-directed-channel liquidity
interval (`lowerOK`, `upperFail`, `estimate`, `conf`) in a package-level
`candidateKnowledge` map, no time decay anywhere, backward search priced by
`-log P` plus normalized fees, and a shard ladder the router chooses from
itself. Four things are specific to hb2.

### It rediscovered conditional renormalization

hb1 blends its prior with evidence using hand-shaped formulas. hb2 instead
computes a *survival function* and conditions on the known interval:

```go
dry    := 0.47 * math.Exp(-ratio/0.035)
rich   := 0.50 / (1 + math.Exp((ratio-0.94)/0.025))
middle := 0.03 * math.Max(1-ratio, 0)
probability := dry + rich + middle   // candidatePriorSurvival
```

and then, in `candidateBaseProbability`:

```go
denominator := lowerSurvival - upperSurvival
numerator   := amountSurvival - upperSurvival
probability := numerator / denominator
```

That is Bayes' rule applied to a truncated distribution: the probability
mass above the amount, divided by the mass remaining between the proven
bounds. It is structurally the same operation lnd's bimodal estimator
performs analytically in `probability_bimodal.go`:

```go
prob := p.integral(capacity, amount, failAmount)
reNorm := p.integral(capacity, successAmount, failAmount)
prob /= reNorm
```

lnd derives that renormalization from the Pickhardt et al. formalism. hb2
arrived at the same posterior structure from failure traces and a scalar
objective, which is the strongest single piece of evidence in the project
that the search is finding real structure rather than overfitting a corpus.
The remaining difference is the same one as everywhere else: lnd decays
`successAmount` and `failAmount` with the clock before renormalizing, and
hb2 does not.

### It reserved in-flight liquidity

hb2 is the only champion that models its own concurrent HTLCs:

```go
func (r *candidateRouter) reserveRoute(rt *route.Route)
func (r *candidateRouter) releaseRoute(rt *route.Route)
```

`RequestRoute` reserves the chosen route's per-hop amounts, `ReportAttempt`
releases them, and `edgeProbability` prices the *conditional* probability of
fitting `amt` on top of what is already reserved:

```go
probability := candidateBaseProbability(edge, reserved+amt) /
        candidateBaseProbability(edge, reserved)
```

This is the correct thing to do when shards fly in parallel, and neither hb1
nor mx_c3 has it. Both dropped it, which is what you would expect given the
simulator: `SimRunner.RunScenario` increments `inFlightHtlcs` only after a
part settles, so no two of a candidate's HTLCs are ever actually in flight
together. The mechanism is dead code under the current harness, which is
precisely why it is worth remembering — it is the piece a
concurrency-faithful simulator would immediately need.

### It escalates the hop limit instead of fixing one

```go
hopLimits := [...]int{20, 32, 64}
```

`findRoute` retries the label-setting search with a wider hop bound each
time a narrower one fails, so easy payments pay for a small search and hard
ones get a deep one. hb1 has no hop limit at all; mx_c3 fixes one at
`candidateMaxRouteHops = 24`.

### Its search and utility constants differ

Per-edge cost is `-log P + sessionPenalty + 6*fee/deliver +
0.04*sqrt(utilization) + 0.018`, with a smaller label budget than mx_c3
(`candidateLabelLimit = 8`) and a lower attempt cap
(`candidateAttemptLimit = 48`). The shard ladder mixes ceil-division rungs
for `parts = 1..12` with the fractions 7/8, 3/4, 2/3, 3/5, and 2/5 — a
hand-shaped set rather than mx_c3's evidence-derived one. Shard selection
maximizes

```go
utility := -logRisk + 0.50*progress - feePenalty - 0.35*nonRiskCost
```

where `nonRiskCost` is the part of the search score that was *not* risk, so
hb2 explicitly discounts routes whose cheapness came from fees and hop
tolls rather than from probability. It also keys its belief state on
capacity and discards it when a channel's capacity changes, which neither
sibling does.

## Shortcomings

Everything in `router_hb1_v1.md` applies. Specific to hb2:

- **It is dominated.** There is no measured regime where you should choose
  it over mx_c3, and that is the whole reason it is archived.
- **Its central mechanism is unexercised.** The reservation logic cannot pay
  off under sequential shard settlement, so it was never selected *for*; it
  survived neutrally and then got dropped downstream.
- **1,166 lines** for a worse result than hb1's 872, with the same
  complexity-wall problems as mx_c3 and less to show for them.
- `candidateRecordFailure` zeroes `lowerOK` outright on a contradiction
  (`state.lowerOK = 0`), throwing away more proven evidence than hb1's
  `amt - 1` and much more than mx_c3's `candidateNormalizeState`
  reconciliation.

## When to pick hb2

Do not, for scoring. Read it for two reasons: to see conditional
renormalization evolve independently of lnd's analytical derivation, and to
lift the in-flight reservation mechanism if and when the simulator learns to
race MPP shards.

## See also

- `router_hb1_v1.md` — the sibling from the same run, with the full lnd
  comparison.
- `router_mx3_generalist_v1.md` — the champion that retired hb2.
- `simulation/lab/experiments/exp-007-mix-followup.md` — the frontier sweep.
- `routing/probability_bimodal.go` — lnd's analytical version of hb2's
  renormalization.
