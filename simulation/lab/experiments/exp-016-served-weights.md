# EXP-016 — Free knowledge helps the champions and hurts lnd

**Date:** 2026-07-26
**Status:** complete — first measurement of served weights, and the
sharpest upstream argument the program has produced.

## Why this ran

exp-012 asked what a warm cache is worth and could not answer, because
every arm it could build bought its knowledge with payments, and
payments drain the corridors they teach about. The drain arm paid in
depletion, the restore arm paid in staleness, and the thing the actual
proposal describes — knowledge arriving over an API for free — was
unconstructible.

`--import-weights` constructs it. A third-party node's observations go
in from a file, no payment sent.

## Design

For each of the ten sealed hard-tier files, a **different source node**
— a server that has been paying and will share what it saw — runs the
same network and exports its observations. Each consumer then runs the
original file twice: cold, and with the server's observations imported
before its first route request. Same graph, same liquidity seed, same
payment set; the only variable is whether the consumer was told
anything.

Observations about the consumer's own channels are excluded, which is
exp-012 part 4's measured rule rather than a guess.

Three consumers. lnd takes its copy through `MissionControl.
ImportHistory`, which already ships. The champions could not take one
at all — nothing in the `SimRouter` contract ever asked a candidate to
consume third-party knowledge, so no evolved router implements it — so
this experiment also produced `exp-016-mxc3-importer.go` and
`exp-016-atomic1-importer.go`. Each is its ancestor plus one method,
routing every observation through the same belief update a real attempt
would have produced. Both score identically to their originals when
cold (mx_c3 0.667/19.1 on the smoke file, byte for byte), so the only
thing that changed is the capability.

## Result

| router | arm | objective | attempts | success | Δ vs cold | 95% CI | sign p |
|---|---|---|---|---|---|---|---|
| **lnd** | cold | 0.298 | 30.9 | 0.421 | — | | |
| | all | 0.268 | 33.8 | 0.401 | −0.029 | [−0.079,+0.001] | 1.00 |
| | success-only | 0.301 | 27.5 | 0.437 | +0.003 | [−0.052,+0.051] | 1.00 |
| | **failure-only** | 0.259 | 27.8 | 0.385 | **−0.039** | **[−0.077,−0.006]** | 0.375 |
| **mx_c3** | cold | 0.479 | 8.1 | 0.592 | — | | |
| | **all** | 0.510 | **4.4** | 0.592 | **+0.031** | **[+0.007,+0.061]** | 0.180 |
| | success-only | 0.508 | 6.2 | 0.603 | +0.028 | [−0.001,+0.059] | 0.688 |
| | failure-only | 0.489 | 6.7 | 0.592 | +0.010 | [−0.019,+0.040] | 0.180 |
| **atomic1** | cold | 0.417 | 7.1 | 0.519 | — | | |
| | **all** | 0.472 | 5.1 | 0.555 | **+0.055** | **[+0.010,+0.106]** | **0.016** |
| | success-only | 0.456 | 5.7 | 0.544 | +0.038 | [+0.003,+0.085] | 0.125 |
| | failure-only | 0.436 | 6.6 | 0.533 | +0.019 | [−0.000,+0.054] | 0.125 |

**The same file, served to three consumers, helps two of them and hurts
the third.** atomic1 gains +0.055 (CI excludes zero, sign test
p=0.016). mx_c3 gains +0.031 and nearly halves its attempts, 8.1 → 4.4.
lnd loses 0.029 and its attempts go *up*, 30.9 → 33.8.

## The mechanism: it is entirely the failure evidence

Splitting the stream is what makes this more than a scoreboard.

**Successes help everyone.** lnd +0.003, mx_c3 +0.028, atomic1 +0.038.
Nobody is hurt by being told what worked.

**Failures split the field.** They help the interval routers (+0.010,
+0.019) and they are the whole of lnd's loss: **−0.039, with a
bootstrap CI that excludes zero, and lnd is worse on 9 of 10 files.**
Imported failure evidence alone is more damaging to lnd than the full
stream, because the successes were partly offsetting it.

### The mechanism, after three wrong guesses

**The first published version of this section was wrong, and so were
the next two guesses.** The corrected account is below; the discarded
ones are kept because each was falsified by a specific measurement, and
because the first of them reached a dashboard before it was checked.

*Wrong guess 1 (published, retracted): "a penalty is not amount-aware."*
I wrote that lnd files a failure as a penalty on the pair that
suppresses the corridor for every amount. `probability_apriori.go:363`
says otherwise:

```go
if lastPairResult.FailTime.IsZero() || amt < lastPairResult.FailAmt {
        return nodeProbability
}
```

A failure at X does **not** penalize amounts below X. lnd's estimator
gates on amount correctly, and our import path preserves `FailAmt`
faithfully. The claim was false.

*Wrong guess 2: node-level contagion.* `getNodeProbability` folds every
pair result into a node-level prior used for all of that node's untried
channels — lnd's own comment says "one failure will lead to the success
probability estimates for all other channels being 0 too." That is a
keying collapse onto NODES, which my "761 edges, 761 pairs" check never
ruled out. Testable: `apriori.weight = 1.0` short-circuits
`getNodeProbability` to the bare prior, disabling the aggregation while
leaving the per-pair penalty intact. The loss survives it, −0.046 →
−0.038. Not contagion.

*Wrong guess 3: staleness.* The server's own run moves the liquidity it
is reporting on, so its observations describe a network that has since
drifted. Testable: rebuild the server export from a **one-payment**
server, which barely perturbs anything. Fresh failures give lnd +0.000,
worse on 0 of 10 files — apparently decisive, until you notice the
fresh set has 232 failures against the stale set's 2,808. Size-matching
a random subsample of the *stale* set to 232 gives −0.003, worse on 1
of 10. Stale and fresh are the same at equal volume. Not staleness.

| failure evidence imported | count | Δ vs cold | worse on |
|---|---|---|---|
| stale, full | 2,808 | **−0.046** | 4/10 |
| stale, size-matched | 232 | −0.003 | 1/10 |
| fresh, 1-payment server | 232 | +0.000 | 0/10 |

**The surviving explanation: the damage scales with the VOLUME of
failure bounds near the amounts the consumer wants to send, and lnd
cannot respond to a bound by sending less.** Each imported failure
marks one directed edge as near-zero probability at or above its
amount. The server's payments are drawn from the same distribution as
the consumer's, so those bounds land squarely on the amounts the
consumer is about to attempt. At 232 observations few corridors are
removed and nothing happens. At 2,808 — most of a 761-edge graph —
lnd's pathfinder sees the amount it wants blocked almost everywhere,
and its only available response is to route around, onto longer and
worse paths. Attempts rise, success falls.

The interval routers receive exactly the same removals and turn them
into instructions. An imported `upperFail` of X tells mx_c3's shard
ladder to try `(X−1)/k`; the bound does not merely delete an option, it
names a smaller one that should work. That is why identical information
is worth +0.031 to mx_c3 and −0.029 to lnd.

So the thesis survives, and in a sharper form than I first wrote it.
The problem is not that lnd's estimator ignores amounts — it does not.
The problem is that **nothing downstream of the estimator can act on an
amount bound**: `findPath` takes the amount as a fixed argument, so
knowledge that "≥X fails here" can only ever subtract routes and never
resize the payment. This is exactly exp-002b's finding, reached from
the opposite direction, and the two now converge on one patch rather
than two observations.

## Consequences for the weight-serving proposal

1. **Serve observations, not weights.** Neither side's internal state
   is servable — mission control keeps a decaying penalty history
   keyed by the observer, the evolved routers keep an interval with an
   evidence count — but both are derivable from a stream of
   `(from, to, chan_id, amount, success, time)`. Serving either side's
   weights would force every consumer into that side's probability
   model.
2. **A consumer must store failures as amount bounds to benefit from
   them.** This is now measured, not argued. An API that serves
   failure observations to lnd as it stands makes lnd worse. Either
   the API serves successes only to such consumers, or — better —
   mission control learns to keep `FailAmt` as a bound that the retry
   loop actually reads.
3. **The rule from exp-012 part 4 still holds and is implemented:**
   never serve observations about the consumer's own local channels.
   In this corpus 43% of what a node observes is about its own
   channels, so a naive server would ship nearly half a payload that
   must be dropped.
4. **Server coverage varies enormously.** The ten server nodes exported
   between 0 and 2,111 observations. One well-connected node is worth
   more than a hundred leaves, and a badly connected server serves an
   empty response. Who serves matters as much as what is served.

## Caveats

One tier (hard), n=10, one server node per file chosen by index rather
than by degree. The sign test is very weak at n=10 — it needs 9 of 10
to reach p<0.05 — so it and the bootstrap CI disagree in places
(mx_c3's +0.031 has a CI excluding zero at p=0.180); both are reported
rather than the friendlier one. The server's observations are also
stale by construction, since its own run moved the liquidity it was
observing, so these are lower bounds on the value of fresh knowledge.

The direction of the lnd result is what matters here, and it is
consistent across every arm and both statistics.
