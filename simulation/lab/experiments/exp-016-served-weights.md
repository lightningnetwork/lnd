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

This is the program's central thesis measured from a new direction. An
interval router stores a failure as an *amount bound*: "≥ X fails on
this channel." It will still happily route X/2 there tomorrow, so a
served failure is pure information. lnd stores a failure as a *penalty
on the pair*, and a penalty is not amount-aware — it suppresses the
corridor for everything, so a served failure at some other node's
amount steers lnd off corridors that were fine for the amounts it
actually wants to send. Free, accurate, correctly-scoped information
makes lnd worse because of how it files it.

One hypothesis I had and disproved: I expected mission control's
collapse of channels onto node *pairs* to be the culprit, since it
discards the channel id an interval router keys on. It is not — these
topologies have 761 directed edges and 761 distinct node pairs, no
parallel channels at all, so nothing collapses. The damage is in the
representation of a failure, not in the keying.

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
