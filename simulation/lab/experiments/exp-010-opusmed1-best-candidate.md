# opusmed1 — the val-set overfit

`exp-010-opusmed1-best-candidate.go` (883 lines) is the winner of
`code_split_opusmed1`, the Opus-5-at-medium-effort arm of exp-010's
three-way proposer A/B. It posted the best validation score of the whole
family and the worst held-out score, which is the only reason to keep it.

Read it next to `exp-010-opus1-best-candidate.md`: same proposer model, same
corpus, same seed, same 400-evaluation budget, one knob changed.

## Provenance and scores

| field | value |
|---|---|
| run | `code_split_opusmed1` (reflection LM `claude:claude-opus-5`, medium reasoning effort, 1–2 minutes per proposal) |
| corpus | `corpus-split` (seed 4041), the corridors topology |
| budget | 400 evaluations, matched with the codex and Opus-default arms |
| status | not promoted; kept as the cautionary arm |

| tier | mx_c3 | opus1 | **opusmed1** | delta vs mx_c3 [p] |
|---|---|---|---|---|
| best validation score during the run | — | 0.798 | **0.874** | — |
| corridors split-val | 0.835 | 0.839 | 0.782 | −0.053 [.008] |
| corridors split-test | 0.876 | 0.841 | 0.743 | −0.133 [.008] |
| hard sealed test | 0.583 | 0.303 | 0.299 | −0.285 [.002] |
| OOD corpus-v2 | 0.581 | 0.483 | 0.420 | −0.161 [.021] |
| mainnet, 12,161 nodes | 0.791 | 0.757 | 0.766 | −0.025 [.109] |
| atomic test (exp-010b) | 0.444 | 0.425 | 0.373 | — |

The first two rows are the whole story. During the run opusmed1 looked like
the best candidate anyone had produced on this corpus, by a margin of 0.076
over the arm that actually won. The sealed sweep reversed the ordering
completely. This is exactly the failure the project's selection rule exists to
catch: `summary.json`'s `best_score` and the val score are training metrics,
and champions are decided only by held-out validation.

## The design: an up-front corridor shard planner

`planShard` is the joint planner, and it plans once per call from scratch.

1. Price the full remaining amount over the best corridor. Return immediately
   if it clears probability 0.55, on the argument that a single full-amount
   route is the cheapest outcome in both fees and attempts.
2. Otherwise ask that corridor what it can bear. `routeCapacity` walks the
   route's hops taking the minimum of exact local balance, belief-derived
   usable amount, and `maxHTLC`, net of in-flight holds, and discounts the
   first hop by 0.5% for fees. Re-price the shard at exactly that number
   rather than at a blind half.
3. Add a small ladder of deliberate fractions — `amt/2, amt/3`, widening to
   `/4, /6` and then `/10, /16` as `partsLeft` grows or failures accumulate,
   plus `2*amt/3`.
4. Choose. A part slot is treated as scarce, so a partial shard must be
   meaningfully more likely than the full-amount route to win one:

```go
if bestPart.prob > bestFull.prob*1.6 &&
	bestPart.prob >= 0.30 {

	return bestPart.rt, bestPart.prob, nil
}
return bestFull.rt, bestFull.prob, nil
```

`commit`/`release` reserve each in-flight route's amount per directed channel
so parallel shards do not plan over the same corridor twice. Beliefs are the
lineage's `okAmt`/`failAmt` pair with no time decay, and — like the
Opus-default arm and unlike the champions — they live on the router, so
nothing is remembered between payments.

That is a real joint planner: the route tells the planner the bottleneck and
the planner cuts the shard to fit. It is also the shallowest of the three
exp-010 planners, because nothing survives the call. Each `RequestRoute`
rebuilds its shard set from the current beliefs, so the router never holds a
multi-shard decomposition in mind the way opus1's persistent queue does.

## Where it overfitted

The constants are corridor-shaped, and more tightly than opus1's:

- `maxHops = 6`. Corridors are three hops; a 600-node small-world graph needs
  9 to 23. This is the same trap the Opus-default arm fell into with
  `maxRouteHops = 7`, one hop tighter.
- `minShard = 1_000_000` msat, a floor of 1,000 sat — a thousand times
  opus1's `minShard`. Salvaging a payment with small shards is not
  representable.
- `maxAttempts = 48` and a flat `attemptCost = 9_000` msat, where the
  corridors corpus rewards finishing in a handful of attempts.
- The prior's cliff sits at 55% of capacity with slope 12
  (`0.30*exp(-6x) + 0.70/(1+exp(12*(x-0.55)))`), sharper and lower than any
  champion's.
- `nodeFail` multiplies edge probability by `0.55^n` per non-liquidity failure
  at the destination node. This is the only node-level penalty in the exp-010
  family, and it is the mechanism lnd's `failNode` implements and every
  champion dropped.

Together these describe a router that expects short routes, large shards, few
attempts, and mostly-full corridors. The corridors corpus is exactly that
world. Nothing else is.

## The throughput-versus-quality lesson

The A/B was run at a fixed evaluation budget, and at fixed evaluations
reflection quality wins. Medium effort turned proposals around roughly four
times faster than default effort and finished hours earlier, and it produced
the weakest held-out router of the three arms while topping the validation
metric. Faster proposals were not worse at climbing the training signal; they
were worse at climbing anything else.

The complementary experiment was deliberately not run: at fixed *wall clock*,
medium's iteration rate would buy roughly twice the evaluations, and whether
that trade pays is open. Budget was the reason, and it remains on the backlog.

The sharper form of the finding, drawn across all three arms: proposer
strength moves a candidate along the specialist–generalist axis rather than
lifting the whole curve. The strongest proposer produced the deepest planner,
the best on-corpus score, and near-worst generalization; the fastest produced
the shallowest planner and the worst of both. The champions were untouched by
either.

## See also

- `exp-010-splitting-pressure.md` — the corpus, the A/B, and the five-tier
  sweep.
- `exp-010-opus1-best-candidate.md` — the default-effort sibling, walked
  through in full.
- `exp-010b-atomic-splitting.md` — the arena where the depth ordering of the
  three planners survives and the penalty for depth does not.
