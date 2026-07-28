# EXP-023 atomic1 source audit: why it is fee-robust and contention-immune

**Date:** 2026-07-28
**Status:** read-only audit, no code changed. Written to answer consequence 3
of `exp-023-econ-realism-verdict.md`: atomic1's two second-order wins deserve
a source audit before the economic evolution arm's background prompt is
written.

**Source audited:** `simulation/lab/experiments/exp-010b-atomic1-best-candidate.go`
(the exact overlay source the sweep ran, 1031 lines). Contrasted against
`simulation/champions/router_hb1_v1.go` (872 lines) and
`simulation/champions/router_mx3_generalist_v1.go` (1525 lines). All line
numbers below are absolute in those files.

**The measurements being explained** (from `exp-023-results-summary.json`):

| tier | metric | lnd | seed | hb1 | mx_c3 | atomic1 |
|---|---|---|---|---|---|---|
| c_mn_ctrl | fee_ppm_attempted | 333.4 | 749.4 | 224.8 | 223.8 | **129.8** |
| c_mn_ctrl | attempts | 19.82 | 6.11 | 2.30 | 2.28 | **1.58** |
| c_mn_400 | fee_limit_failures/file | 0 | 276.8 | 151.8 | 100.6 | **27.2** |
| c_mn_400 | attempts | 24.06 | 32.25 | 16.82 | 12.11 | **4.18** |
| c_mn_400 | objective vs lnd | n/a | −0.038 ns | −0.057 | −0.026 ns | **+0.061 CI** |
| c_mn_25 | objective vs lnd | n/a | −0.067 | −0.093 | −0.070 | **−0.002 ns** |
| d_w4 | self-contention/file | 88.5 | 36.5 | 40.5 | 16.6 | **2.85** |
| d_w4 | attempts (w1 → w4) | 39.5→57.3 | 26.1→25.9 | 7.1→21.6 | 7.9→12.6 | **6.1→7.3** |
| d_w4 | makespan sec | 223.9 | 104.5 | 81.0 | 50.5 | **33.8** |
| d_w4 | objective vs d_w1 | −0.074 | −0.067 | −0.112 | −0.077 | **−0.029** |

The per-attempt normalization in the verdict (0.048 for atomic1 against 0.229
for hb1) is the per-file column above divided by attempts times payments per
file; the raw column is what the summary stores.

---

## 1. Fee robustness

### 1a. Where fees enter at all

Three places, and only three:

1. **The edge fee model**, lines 35-37: `baseFeeMsat + amt*feeRatePPM/1e6`,
   with the fee-inflated amount propagated upstream at line 441
   (`sending := amtOver + fee`), so every hop's probability is evaluated at
   the amount that hop will actually be asked to forward. Same as both
   champions (hb1:513-517, mx_c3:919-927).
2. **The path score**, lines 401-404 and 443-444:
   ```
   riskWeight = 420_000.0
   hopPenalty = 220.0
   edgeScore := float64(fee) + riskWeight*(-math.Log(p)) + hopPenalty
   ```
3. **The plan score and the shard-size choice**, lines 663-665, 703-708 and
   722-724: `jointScore += logProb - float64(fees)/4_000_000 - 0.025` and
   `utility := logProb + sizeBias*sizeReward - float64(fees)/4_000_000`.

That is the whole list. There is **no fee cap, no budget tracking, and no
read of `spec.FeeLimitMsat`**. Grepping `spec.` across the file returns six
hits (lines 147, 397, 409, 412, 487, 802) and every one is `spec.Target` or
`spec.MaxParts`. The contract offers the budget (`routing/sim_router.go:112`,
documented there as enforced at dispatch so that "a router that ignores the
budget pays for it in attempts and in failed payments") and atomic1 does not
take it.

### 1b. The mechanism: the score is denominated in money, not in nats

This is the finding. atomic1's path score is in **millisatoshis**, with
probability converted into money at a fixed rate of 420,000 msat per nat of
log-probability (line 443-444). Both champions do the opposite: their score is
in **nats**, with money converted into probability at a rate proportional to
1/amount.

- hb1, lines 521-523: `feePenalty := 15 * float64(fee) / max(deliver,1)`,
  then `edgeScore := logRisk + feePenalty + 0.012`.
- mx_c3, lines 930-931: `feePenalty := 5.0 * float64(fee) / max(deliver,1)`,
  added at 947-949 alongside a hop penalty and a capacity penalty.

Expressed as a single number, the router's willingness to pay for one nat of
reliability:

| router | site | willingness to pay, 1 nat of log-prob | at the mainnet median amount (1e9 msat) |
|---|---|---|---|
| atomic1 | line 443 | 420,000 msat, flat | **420 ppm** |
| hb1 | line 521 | deliver/15 | 66,667 ppm |
| mx_c3 | line 930 | deliver/5 | 200,000 ppm |
| atomic1 | line 707 (shard) | 4,000,000 msat, flat | **4,000 ppm** |
| hb1 | line 715 (shard) | shard/10 | 100,000 ppm |
| mx_c3 | line 1180 (shard) | shard/4 | 250,000 ppm |

Mainnet amounts are 1e8 to 2e9 msat, median 1e9 (`simulation/lab/scenarios/mainnet/*.json`).
At that median atomic1 is 159x more fee-sensitive than hb1 in its path search
and 476x more than mx_c3. hb1 would need a hop fee of 6.7% of the payment
before its fee term matched one nat; mx_c3 20%. Neither term ever binds in
this environment. atomic1's does.

The unit choice has a second consequence that matters specifically for a
**ppm-denominated** budget: because atomic1's exchange rate is a fixed
absolute amount, its implicit ceiling in ppm terms scales as 1/amount. At
1e8 msat it will pay 4,200 ppm per nat; at 2e9 msat only 210 ppm. Larger
payments, where a ppm budget bites hardest in absolute msat, automatically get
the tighter implicit ceiling. The amount-relative routers are scale-free and
so cannot do this.

**Verdict: atomic1 genuinely prices fees.** It does not track a budget, but it
is the only router here whose cost function trades reliability against money
at a rate that any realistic fee budget can reach. That is why its attempted
price on clean mainnet is 129.8 ppm against 224.8 (hb1) and 223.8 (mx_c3), and
why at the 400 ppm rung it is refused 27.2 times per file against hb1's 151.8.

**Caveat, in the house style:** 420,000 and 4,000,000 are evolved constants
fitted in an arena whose amounts are 1e8 to 2e9 msat. On a corpus with
different amounts the same code has different fee sensitivity. The mechanism
(absolute denomination) is portable; those two numbers are not.

### 1c. The accidentally-cheap components, separated out

Three effects reinforce the pricing but are not pricing:

- **Fewer, larger shards.** `makePlan` lines 749-754 short-circuits to a
  single full-amount route whenever it can find one at p >= 0.22, and
  `planOnce` charges a flat 0.025 per shard (lines 665, 724) while rewarding
  larger shards through `sizeBias*log(size/base)` with biases {0.28, 0.48,
  0.72} (lines 704-708, 756). Base fees are per hop per shard, so shard count
  multiplies the base-fee bill. hb1 has an analogous size ladder
  (639-668) but its early return at line 707 takes the first shard clearing a
  fixed probability threshold and **never evaluates its fee at all**; the fee
  term at 715-717 only runs in the fallback branch.
- **A tight attempt budget.** atomic1's limit is `maxParts*3 + 8` clamped to
  [24, 64] (lines 151-157). hb1's is a hard 96 (line 676), mx_c3's a
  constant 80 (line 22). A budget refusal costs an attempt and nothing else
  (`routing/sim_concurrency.go:663-684`), so a router that runs out of attempts
  sooner wastes less on refusals.
- **Corridor rotation.** Line 446 charges 22,000 msat per prior use of an
  edge *in this payment* (`edgeUses`, incremented on dispatch at 781-788) and
  line 447 charges 260,000 msat per unit of suspicion. Neither champion has a
  use counter; they penalize only on failure (hb1 `sessionPenalty`,
  mx_c3 `sessionPenalty`/`sessionSuspect`).

### 1d. What is NOT the differentiator

Worth recording, because it was the first guess. All three routers respond
identically to a budget refusal. `SimFeeLimitFailure.Code()` returns
`lnwire.CodeNone` (`routing/sim_fee_limit.go:56`), the refusal is reported with
`FailureSource == rt.SourcePubKey` so `failIdx = 0`, and every router's
`default:` branch blocks the first-hop edge for the rest of the payment:
atomic1 lines 1025-1027 (`policyBlocked`, read as probability 0 at 259-261),
hb1 lines 857-858 (`sessionBlocked`, read at 350-352), mx_c3 lines 1500-1502.
The reaction to a refusal is the same. The difference is entirely in **how
often the router walks into one**, which is 1b and 1c.

---

## 2. Contention immunity

### 2a. The reservation ledger

`syncReservations` (lines 608-625) is called at the top of every
`RequestRoute` (line 800) with the runner's `inFlightHtlcs` count. It rebuilds
`r.reserved` from scratch each call by replaying the last `inFlight` routes
from `r.held`, which is appended on every attempt that came back settled or
held (line 956). `reserveRoute` (595-606) adds each hop's amount to the
per-edge total. When `inFlight` hits zero the ledger and the held list are
both cleared (611-614), so it never drifts.

Every read of `r.reserved`:

1. **Pricing, lines 253-256.** `total := amt + reserved[edge.key]`, and every
   subsequent test in `probability()` is against `total`, not `amt`: the
   policy and capacity check (256), the current-failure ceiling (268-279), the
   local-balance check (284-289), and all three belief comparisons (298, 302,
   309-330). The router asks "can this edge carry my new shard **on top of
   what I am already holding there**", which is the physically correct
   question in a hold-and-release arena.
2. **A disjointness surcharge, lines 449-451.** `if r.reserved[edge.key] > 0 {
   edgeScore += 260_000 }`. In this cost function 260,000 msat is 0.62 nats,
   an e^-0.62 = 0.54x probability multiplier. Reusing a corridor is
   discouraged but not forbidden.
3. **Joint plan construction, lines 646-649 and 727.** `planOnce` snapshots
   the ledger, then calls `reserveRoute` after each shard is chosen and
   restores the snapshot on exit. So shard k+1 is priced against shards 1..k
   of the same plan. The plan is corridor-disjoint by construction, not by a
   post-hoc filter.
4. **Belief writes, lines 953, 992 and 1011.** This is the subtle one.
   On success the router records `learnSuccess(key, amt + r.reserved[key])`
   and on failure it computes `totalRequired := amtOver + r.reserved[key]`
   before calling `learnFailure`. The bound written is about the **total load
   the channel bore**, not about one shard of it. A success while holding y
   proves the channel carried x+y; a failure while holding y proves nothing
   worse than x+y.

hb1 and mx_c3 have **none of this**. Their only use of `inFlightHtlcs` is
`partsLeft := maxParts - inFlightHtlcs` (hb1:688, mx_c3:1145). Their belief
writes record the shard amount alone: hb1 lines 834 and 849
(`candidateRouteAmount(rt, i)`), mx_c3 lines 1460 and 1484. Both write into a
process-global store shared by every concurrently running payment
(`candidateKnowledge`, hb1:95-160). So a failure caused by the sender's own
concurrent load is recorded as a property of the channel, at an amount lower
than the load that actually failed, and the next payment reads it.

### 2b. Hard ceilings versus a soft floor

Above `upperFail`, hb1 returns a hard zero (line 367-369) and mx_c3 returns a
hard zero (line 645-646). atomic1 floors instead, lines 302-307:

```go
if belief.upperFail > 0 && total >= belief.upperFail {
        if p > 0.012 {
                p = 0.012
        }
        return p * retryScale
}
```

This is the same 0.012 shrug exp-012 identified under staleness, and under
concurrency it does the same job for a different reason: a bound written from
a sibling's transient hold is wrong, and it stops being wrong the moment the
sibling releases. A soft ceiling lets the corridor come back; a hard zero
retires it for the batch. Combined with 2a's total-load attribution, atomic1
both writes fewer wrong bounds and suffers less when it writes one.

### 2c. Why it is immune to *cross-payment* contention

Worth stating precisely, because the ledger alone does not explain it. Routers
are constructed per payment (`routing/sim_concurrency.go:544`), and
`self_contention_failures` counts an attempt that failed for liquidity that a
**sibling payment** was holding (`noteSelfContention`, lines 867-926). atomic1's
ledger tracks only its own payment's shards, so it cannot see the siblings.

What it does instead is minimize its own footprint, in three ways that all
fall out of the mechanisms above:

- **Residency.** Makespan per payment at window 4 is 33.8 sec against hb1's
  81.0 and lnd's 223.9, and attempts stay at 6.1 to 7.3 across windows 1 to 4
  while hb1's go 7.1 to 21.6. A payment that is live for a third as long holds
  liquidity for a third as long, and overlaps fewer siblings. Mean concurrency
  bears this out: 2.09 for atomic1, 2.23 for hb1, at the same window cap.
- **Spread.** The disjointness surcharge (449-451) plus the `edgeUses`
  surcharge (446) push each successive shard onto a fresh corridor, so the
  sender's held liquidity is spread across more edges rather than stacked on
  the cheapest one. A sibling is then less likely to find any given edge short.
- **No poisoning feedback loop.** hb1's tripling of attempts under concurrency
  is the visible half of a loop: a sibling-caused failure writes a spuriously
  tight `upperFail` into the shared store, which returns a hard zero for every
  concurrent payment, which forces longer searches and more shards, which
  holds more liquidity, which causes more sibling-caused failures. atomic1
  breaks the loop at two points (total-load attribution, soft floor). This is
  the one claim here that is a mechanism story rather than a direct reading of
  a counter; no ablation isolates it.

---

## 3. What is absent from hb1 and mx_c3

| concern | atomic1 | hb1 | mx_c3 |
|---|---|---|---|
| fee in path score | absolute msat, 420k msat/nat (443) | 15·fee/deliver (521) | 5·fee/deliver (930) |
| fee in shard choice | −fee/4e6 on every size (707) | 10·fee/shard, fallback branch only (715) | 4·fee/shard (1180) |
| reads `spec.FeeLimitMsat` | no | no | no |
| reservation ledger | yes (595-625) | none | none |
| edge priced at own in-flight load | yes (253-256) | no | no |
| beliefs written at total load | yes (953, 992, 1011) | shard only (834, 849) | shard only (1460, 1484) |
| belief store shared across concurrent payments | yes (841-856) | yes (95-160) | yes |
| above upper-fail | floor 0.012 (302-307) | hard 0 (367) | hard 0 (645) |
| joint plan against own reservations | yes (646-649, 727) | no, one shard at a time | no, one shard at a time |
| anti-reuse counter | `edgeUses`, 22k msat/use (446) | none | none |
| attempt limit | 24-64, scales with maxParts (151-157) | 96 (676) | 80 (const 22) |
| explicit hop cap | none | none | 24 (const 19) |

mx_c3's nearest analogue to own-load accounting is its post-settlement
debit of the shared estimate (1430-1436, and hb1:804-812, plus
`candidateRecordSettlement` at hb1:162-184 which subtracts the settled amount
from `estimate` and `lowerOK`). That fires only after a shard settles, and
under concurrency each of N routers debits the same shared entry, so it moves
in the wrong direction under load. It is not a reservation ledger.

---

## 4. Proposed BACKGROUND insight bullets for the economic evolution arm

Register matched to the existing "Insights from prior successful runs" and
"Insights from prior measurement (exp-019)" blocks in
`simulation/run_gepa_code.py:93-127` and `156-175`: state what was measured and
what the mechanism was, name the open space, do not prescribe. Five bullets,
plus a note on what to leave out.

> - The UNITS of your route cost function turn out to decide whether a fee
>   budget can reach you. Both incumbent champions score a path in
>   log-probability and convert fees into it at a rate proportional to
>   1/amount (a penalty of k·fee/amount, k around 5 to 15), which makes their
>   willingness to pay for one nat of reliability roughly 7% to 20% of the
>   payment no matter how large the payment is; that term never binds, and
>   under a mainnet fee budget both go from a significant lead over lnd to a
>   deficit. The one router that stays ahead (atomic1, +0.061 against lnd at
>   400 ppm with the CI excluding zero, and a tie at 25 ppm) scores the path
>   in millisatoshis instead, buying probability at a flat 420,000 msat per
>   nat, so its implicit price ceiling in ppm terms tightens automatically as
>   the payment grows. Its attempted routes cost 130 ppm where the others'
>   cost 224. Which denomination is right is your design choice; note that the
>   exchange constant is fitted to this corpus's amounts (1e8 to 2e9 msat) and
>   would not transfer unchanged to another.
>
> - NOBODY HAS YET READ THE BUDGET THEY ARE GIVEN. `spec.FeeLimitMsat` is on
>   the contract, it is the total across all shards, fees already committed by
>   settled and held shards count against it, and it is enforced at the point
>   the runner dispatches: a route over budget is refused before it reaches the
>   wire, costing you one attempt and teaching you nothing about the network.
>   No incumbent reads the field. The whole space is open: subtracting
>   committed fees to know what is left, dividing the remaining budget across
>   a planned shard set, tightening the reliability-for-money exchange rate as
>   the budget runs down, or pruning over-budget paths inside the search
>   instead of discovering them at dispatch. Under a 400 ppm mainnet budget the
>   champions are refused 100 to 152 times per file and atomic1 27; every one
>   of those was an attempt spent on a route the sender could have priced
>   itself.
>
> - When your own shards are in flight, an observation about a channel is
>   about the TOTAL load it bore, not about the shard you happened to send.
>   The champions record a success or a failure at the shard amount alone and
>   write it into a belief store shared by every concurrently running payment,
>   so a failure their own concurrent load caused is filed as a fact about the
>   channel, at an amount below the load that really failed. atomic1 keeps a
>   per-edge reservation ledger rebuilt each call from the in-flight count,
>   prices every edge at amount-plus-reserved, and records both bounds at
>   amount-plus-reserved. At four concurrent payments its self-contention
>   failures are 0.048 per attempt against hb1's 0.229, and its attempt count
>   is flat from window 1 to window 4 (6.1 to 7.3) while hb1's triples
>   (7.1 to 21.6).
>
> - A hard ceiling and a soft one behave differently when the evidence might
>   be about a transient. Both champions return probability zero above their
>   upper-fail bound; atomic1 floors at 0.012 instead. A bound written from
>   liquidity that a sibling shard was merely holding stops being true when the
>   sibling releases, and a floor lets the corridor come back while a zero
>   retires it for the batch. This is the same floor that made atomic1 the
>   only router that shrugged under stale knowledge in exp-012. It has never
>   been isolated by an ablation, so read it as a live hypothesis rather than
>   a settled one.
>
> - Committing an up-front shard set costs fewer attempts AND less contention
>   than discovering one by failure. atomic1 plans the whole set in one pass,
>   pricing each shard against the reservations of the shards already placed in
>   the same plan, with a surcharge on any corridor it is already using, so the
>   set comes out corridor-disjoint by construction rather than by filtering.
>   The measured consequence is a smaller and shorter-lived footprint: at four
>   concurrent payments its makespan is 33.8 sec against hb1's 81.0 and lnd's
>   223.9, and it loses 0.029 of objective to concurrency where hb1 loses
>   0.112. It also pays less: fewer, larger shards means the per-hop base fee
>   is multiplied fewer times.

**Left for the search to discover, deliberately not stated:**

- Any concrete budget-handling design. Bullet 2 names the field, the
  enforcement point and the cost of ignoring it, and stops. The four sketched
  approaches are listed as an open space in the exp-019 register's style, not
  as a recipe.
- Inbound fees. exp-023 found the authored rung closes the champion gap but
  the real mainnet policies are exactly null (ten ties of ten). Naming inbound
  fees in the prompt would spend complexity on a mechanism the real network
  does not yet exercise.
- The specific constants (420,000; 4,000,000; 260,000; 0.012; 22,000). Bullet
  1 quotes 420,000 only because the caveat about it being fitted is the point
  of quoting it. The rest are omitted; a candidate that re-derives its own
  exchange rate against this corpus is what we want.
- The attempt-limit and hop-penalty settings. exp-013's give-up attractor says
  attempt economy is one cheap step from abandonment, and the evaluator hint
  already carries the read-success-and-attempts-separately rule.
- Anything about lnd's own fee-aware pathfinding as a model to copy. The
  verdict's own reading is "port the belief system, not the cost model"; the
  prompt should describe the measurement, not the branch decision.
