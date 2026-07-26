# atomic1 — the challenger with no collapse tier

`exp-010b-atomic1-best-candidate.go` (1,031 lines) is the winner of the
`code_atomic1` run and the first router in the program's history to challenge
mx_c3 without paying for it somewhere. Every previous challenger bought its
home-corpus strength with an off-corpus cliff. This one is statistically
indistinguishable from the champion on the sealed hard test, on the
out-of-distribution corpus, and on the 12,161-node mainnet snapshot, where it
scores 0.790 against the champion's 0.791 at **1.6 attempts per payment** —
below the champions' 2.3, and the most attempt-frugal router this project has
ever measured.

It still loses. On the held-out atomic tier it was bred for, the tier that
decides the experiment, it trails mx_c3 by 0.044 at p=.07. So the champion
survives its fifth direct challenge, and this time the arena was built
expressly to tax its reactive ladder.

Structurally atomic1 is a hybrid, and that is the reason to read it. The codex
lineage has always carried cross-payment network memory; exp-010's Opus arms
produced up-front route-set planning and no memory at all. atomic1 fuses them:
a package-level belief map keyed by a hash of the gossip graph, feeding a
planner that lays out a whole shard set against a per-edge reservation ledger
before it sends anything.

Read this document next to the source. Every constant quoted below appears
verbatim in the file.

## Provenance

| field | value |
|---|---|
| run | `code_atomic1` (GEPA code mode, reflection LM `codex:gpt-5.6-sol`) |
| seed program | the small in-tree router, `cmd/routesim/candidate_impl.go`, with the discovered insights and the atomic arena's economics supplied as prose in the background prompt |
| training corpus | `corpus-splitatomic` (seed 6061, `--split --split-leads 5 --atomic`): the corridors topology under atomic MPP, ~7 graded payments per file on a descending lead ladder, background traffic advancing one slice per attempt |
| budget | 400 evaluations, zero degraded reflections, proposal canary zero |
| sibling | `exp-010b-atomicopus1-best-candidate.go` (987 lines, Opus-5-default arm) |
| writeups | `exp-010b-atomic-splitting.md`, `exp-010-splitting-pressure.md` |
| status | not promoted; kept as the first no-collapse challenger and the attempt-economy record holder |

The run completed clean, which is worth one sentence of its own: both exp-010b
arms ran after the `CODEX_HOME` and `CLAUDE_CONFIG_DIR` seals landed, so
neither carries the leaked-instruction caveat that the exp-010 arms do.

## Validated scores

Every tier is held out from the run. Objective =
`success − 0.01·min(extra_attempts, 15) − 0.00002·min(fee_ppm, 5000)`. Paired
deltas are against mx_c3 as baseline, with bootstrap 95% intervals and sign
tests. All routers were rebuilt on the current tree for this sweep, and the
scratch legacy corpora were regenerated after a reboot, so compare deltas
within the table rather than levels against older writeups.

| tier | **mx_c3** | atomic1 | delta [p] | atomicopus1 | opus1 (unevolved) |
|---|---|---|---|---|---|
| atomic val | **0.442** | 0.426 | −0.016 [.29] | 0.374 | 0.429 |
| atomic test | **0.444** | 0.400 | −0.044 [.07] | 0.391 | 0.425 |
| corridors split-test | **0.876** | 0.825 | −0.051 [.07] | 0.711 | 0.841 |
| hard sealed test | **0.479** | 0.417 | −0.062 [.75] | 0.247 | 0.284 |
| OOD corpus-v2 | **0.581** | 0.544 | −0.036 [.75] | 0.367 | 0.483 |
| mainnet, 12,161 nodes | **0.791** | 0.790 | −0.001 [.039] | 0.738 | 0.757 |

Two rows carry the story. On hard and OOD the sign test returns p=.75, which
is the plainest way the sweep has ever said "these two routers are the same
router as far as this corpus can tell." And on mainnet the delta is 0.001 —
the p=.039 there reflects consistent hair-width per-file losses, not a
meaningful gap, and atomic1 buys those hair-widths at 1.6 attempts per payment
against 2.3.

For scale on the atomic tiers, lnd's production stack scores 0.286 and 0.338
at 104.8 attempts per payment in the same arena. The arena reordered the whole
field before evolution ran; the baseline section of `exp-010b-atomic-splitting.md`
has that table.

The uncomfortable column is the last one. opus1, bred on the static corridors
corpus and never shown atomic semantics, beats atomic1 on both atomic tiers
(0.429 and 0.425 against 0.426 and 0.400) while losing to it by 0.133 on the
hard test and 0.061 on OOD. Four hundred evaluations of evolution *on* the
arena produced a better generalist and a worse atomic specialist than an
artifact that had never seen the arena. Hold that thought for "Why it lost."

## Running it

```bash
cd $LND_REPO
cat > /tmp/overlay.json <<EOF
{"Replace": {"$PWD/cmd/routesim/candidate_impl.go":
             "$PWD/simulation/lab/experiments/exp-010b-atomic1-best-candidate.go"}}
EOF
go build -overlay /tmp/overlay.json -o /tmp/routesim_atomic1 ./cmd/routesim

# Regenerate corpus-splitatomic (fixed seed, so it reproduces exactly).
python3 simulation/gen_scenarios.py --out /tmp/corpus-splitatomic \
    --split --split-leads 5 --atomic --seed 6061

/tmp/routesim_atomic1 \
    --scenarios /tmp/corpus-splitatomic/test/example_000.json \
    --router=candidate --traces=false
```

## The hybrid, in one table

Three routers, three answers to the same three questions.

| | mx_c3 (champion) | opus1 (exp-010) | **atomic1** |
|---|---|---|---|
| how a payment is planned | one shard at a time, chosen from a priced ladder after each failure | a full residual decomposition over disjoint corridors | a full-coverage shard set, re-planned from scratch whenever one fails |
| how siblings avoid contending | they cannot; only one shard is ever in mind | a residual budget per first-hop channel | a per-edge reservation ledger folded into the price of every edge |
| what survives the payment | a package-level belief map, global | nothing; the router is built fresh per payment | a package-level belief map, keyed by a hash of the graph |

atomic1 is the middle row's answer taken further and the bottom row's answer
made careful. What it did *not* take from opus1 is persistence: a failure
voids the whole remaining plan (`r.planned = nil`) rather than pruning the
shards the failure actually implicates. That is the exact behaviour opus1's
own design comment identifies as the bug it fixed, and it is the sharpest
thing to hold against atomic1 on the atomic tier.

## Architecture

Start with what is absent, because two absences decide the generalization
story.

There is one named constant in the entire file:

```go
const finalCltvDelta = 40
```

**There is no hop limit.** `findRoute` runs a backward Dijkstra over the whole
reachable graph with a flat `hopPenalty = 220` per edge and no cap on path
length. mx_c3 allows 24 hops; opus1 allowed 7, and the exp-010 follow-up
measurement showed that single constant cost it about half the hard-test gap,
because a 600-node small-world graph at 25% of channel capacity needs routes
of 9 to 23 hops. atomic1 can express those routes. That, plus the memory it
carries between payments, is the most economical explanation for why this is
the first challenger with no collapse tier: the two things opus1 lacked on the
hard corpus are the two things atomic1 has.

**There is no clock.** No `time` import, no `view.Now()`, no half-life, no
decay term. The background prompt states exp-008's verdict as a premise and
the candidate took it. Staleness is handled, but by scope rather than by time;
see the two-timescale section below.

The attempt budget is the only give-up test in the file:

```go
r.attemptLimit = int(maxParts)*3 + 8
if r.attemptLimit < 24 {
	r.attemptLimit = 24
}
if r.attemptLimit > 64 {
	r.attemptLimit = 64
}
```

Three attempts per allowed shard plus eight, clamped to `[24, 64]`. opus1 had
three separate ways to quit, one of which could fire before the first attempt.
atomic1 has one, and it scales with how many shards the payment is allowed.

### Memory keyed by the graph itself

Every codex-lineage router keeps a mutex-guarded package-level belief map.
atomic1 is the first to worry about *which network* those beliefs describe:

```go
type candidateNetworkKey struct {
	source      route.Vertex
	fingerprint uint64
}
```

The fingerprint is accumulated during the construction BFS, one XOR per
directed edge:

```go
fingerprint ^= candidateEdgeHash(edge)
```

where `candidateEdgeHash` mixes the channel ID, both endpoints, the capacity,
and every policy field through splitmix64. XOR makes the result independent of
traversal order, so the same graph always hashes the same way, and any change
to a policy or the channel set produces a different key and a fresh belief
map. Payments over one scenario's graph share knowledge; payments over a
different graph cannot contaminate each other.

Construction copies the shared map into a per-payment snapshot, and every
write goes to both:

```go
func (r *candidateRouter) storeBelief(key candidateEdgeKey,
	belief candidateBelief) {

	r.beliefs[key] = belief
	...
	mem.beliefs[key] = belief
```

One detail here is a genuine correctness property rather than an
optimization. Both `learnSuccess` and `learnFailure` return early on
`key.from == r.source`, so nothing about the router's own channels is ever
persisted. Local balances are exact, snapshotted per payment, and change when
money moves; publishing them into a map that outlives the payment would poison
the next one. The champions' global map does not draw this distinction.

### The prior kept its shape

```go
x := float64(amt) / float64(capacity)
lowMode := math.Exp(-x / 0.055)
highMode := 1 / (1 + math.Exp((x-0.93)/0.035))
p := 0.5*lowMode + 0.5*highMode
```

clamped to `[0.005, 0.985]`. Evaluate it: 0.985 at dust, 0.58 at 10% of
capacity, 0.50 flat across the middle, 0.35 at 90%, 0.06 at capacity. That is
mx_c3's curve — a coin flip in the middle with a wall near the top — arrived
at independently, with the cliff at 93% of capacity against mx_c3's 96.5%.
Contrast opus1, whose "bimodal" prior degenerated into a monotone pessimism
slide with its cliff at 42%. The bimodal hypothesis is now four lineages deep.

### Two timescales of evidence, and neither is a clock

This is the design idea worth stealing. atomic1 keeps two kinds of failure
record, with deliberately different severities and lifetimes.

The durable one is the belief, persisted across payments:

```go
type candidateBelief struct {
	lowerOK   lnwire.MilliSatoshi
	upperFail lnwire.MilliSatoshi
	estimate  lnwire.MilliSatoshi
	successes uint32
	failures  uint32
}
```

and the amount at or above a persisted `upperFail` is **not** vetoed:

```go
if belief.upperFail > 0 && total >= belief.upperFail {
	if p > 0.012 {
		p = 0.012
	}
	return p * retryScale
}
```

A ceiling of 0.012 rather than zero. mx_c3 returns a hard zero here. The
difference is that mx_c3's bound was learned in a world that stood still,
while atomic1's may be several payments and several minutes of background
traffic old, so the router keeps a sliver of hope alive and lets the search
buy it if nothing better exists.

The ephemeral one is scoped to the payment and is savage:

```go
type candidateCurrentFailure struct {
	upper lnwire.MilliSatoshi
	count uint32
}
```

```go
if failure.count >= 2 {
	return 0
}
if failure.upper > 0 {
	if total >= failure.upper {
		return 0
	}

	retryCeiling := failure.upper * 2 / 3
	...
	if total > retryCeiling {
		return 0
	}
	retryScale = 0.35
}
```

Two strikes on one directed channel and it is dead for the rest of this
payment. One strike, and the only amounts still considered are those below two
thirds of what just failed, priced at 35% of whatever the rest of the model
says. That is mx_c3's six-rung `candidateLowerRetryFactor` compressed into a
gate and two constants — and unlike mx_c3's, it expires when the payment does,
because `currentFails` lives on the router and the router is rebuilt per
payment.

Fresh evidence is treated as certain; old evidence is treated as a strong
prior. No half-life computes that, and exp-008 said no half-life should have
to.

### Reservation pricing

The reservation ledger is the arena-native mechanism, and it is applied in the
one place that makes it impossible to route around:

```go
func (r *candidateRouter) probability(edge *candidateEdge,
	amt lnwire.MilliSatoshi) float64 {

	reserved := r.reserved[edge.key]
	total := amt + reserved

	if !edge.policyAllows(amt) || total > edge.capacity {
		return 0
	}
```

Every subsequent test in the function — capacity, the session failure bound,
the local balance check, the prior, `lowerOK`, `upperFail`, the interval
interpolation — reads `total`, not `amt`. A shard being priced against an edge
that already carries one of our own shards is priced as though the edge must
carry both, because under atomic MPP it must. Note the asymmetry on the first
line: `policyAllows` tests `amt`, since minHTLC and maxHTLC apply per HTLC,
while capacity is tested against the sum. That is the correct reading of the
protocol and it is not the kind of thing a careless mutation gets right.

The search adds a second, softer discouragement on top:

```go
edgeScore += float64(r.edgeUses[edge.key]) * 22_000
edgeScore += float64(r.suspect[edge.key]) * 260_000

if r.reserved[edge.key] > 0 {
	edgeScore += 260_000
}
```

A reserved edge costs the same surcharge as one unit of suspicion. This is a
soft exclusion, and it is the interesting choice: atomicopus1 hard-excludes the
entire edge set of a placed shard, so its second shard *cannot* reuse a fat
corridor even when doing so is right. atomic1 can, at a price, and the price
is paid twice over — once in the fee-equivalent surcharge and once in the
honest probability of carrying both amounts.

The ledger is reconciled against the runner rather than trusted:

```go
func (r *candidateRouter) syncReservations(inFlight uint32) {
	r.reserved = make(map[candidateEdgeKey]lnwire.MilliSatoshi)

	if inFlight == 0 {
		r.held = nil
		return
	}

	count := int(inFlight)
	if count > len(r.held) {
		count = len(r.held)
	}
	start := len(r.held) - count

	for _, rt := range r.held[start:] {
		r.reserveRoute(rt)
	}
}
```

`RequestRoute` calls this first, every time. The router keeps a list of routes
that came back without a failure, and rebuilds the whole ledger from the last
`inFlightHtlcs` of them — the count the runner reports. It never accumulates
drift between its own bookkeeping and the simulator's, and when the payment
resolves and `inFlight` drops to zero, the holds vanish in one line. Under
atomic MPP, where held shards are exactly the shards that have not failed and
have not settled, taking the last `count` entries is right by construction.

### The plan loop

`planOnce` builds one candidate plan for a given appetite for unequal shards.
It saves and restores the ledger around itself, so trial plans never leak
reservations:

```go
savedReservations := candidateCopyReservations(r.reserved)
defer func() {
	r.reserved = savedReservations
}()
```

Then, per slot, it enumerates shard sizes anchored on the equal split:

```go
base := (remaining + lnwire.MilliSatoshi(slots) - 1) /
	lnwire.MilliSatoshi(slots)

candidateAddAmount(&sizes, seen, base, remaining)
candidateAddAmount(&sizes, seen, base*5/4, remaining)
candidateAddAmount(&sizes, seen, base*3/2, remaining)
candidateAddAmount(&sizes, seen, base*2, remaining)
candidateAddAmount(&sizes, seen, base*3, remaining)
candidateAddAmount(&sizes, seen, remaining, remaining)

if r.lastFailedShard > 0 {
	candidateAddAmount(
		&sizes, seen, r.lastFailedShard*5/8, remaining,
	)
}
```

Every rung is at or above the equal split, up to three times it and up to the
whole remainder. Unequal splitting therefore falls out of the interaction
between this ladder and reservation pricing: a fat corridor takes a 2× or 3×
rung because its probability barely moves, the next slot re-derives its `base`
from what is left, and the thin corridors get what they can bear. The last
rung is mx_c3's evidence-derived idea in miniature — five eighths of the shard
size that most recently failed.

Each size is routed and scored, and the winner is the one that best trades
end-to-end log-probability against how much of the payment it moves:

```go
sizeReward := math.Log(float64(size) / float64(base))
utility := logProb + sizeBias*sizeReward - float64(fees)/4_000_000
```

`sizeBias` is the appetite. The shard is then reserved, the remainder drops,
and the loop continues. When one slot is left it must carry the entire
residue, and a plan that cannot cover the full amount is thrown away:

```go
if remaining != 0 || len(plan) == 0 {
	return nil, 0, false
}
```

Partial coverage is not a plan. That is a defensible rule in an atomic arena,
where a payment that never reaches its full amount settles nothing and returns
nothing but information.

### Three appetites, and the gate that skips them

`makePlan` is where the attempt economy lives:

```go
if inFlight == 0 && r.lastFailedShard == 0 {
	full, logProb, err := r.findRoute(total)
	if err == nil && logProb >= math.Log(0.22) {
		return []*route.Route{full}, nil
	}
}

biases := []float64{0.28, 0.48, 0.72}
```

At the very start of a payment, before anything is in flight and before
anything has failed, a single route carrying the whole amount at 22% or better
believed success is sent immediately, with no planning at all. Otherwise the
router runs `planOnce` three times at increasing appetite for unequal shards
and keeps the plan with the best joint score, where each shard contributes its
log-probability minus its fee minus a flat 0.025 per shard — an explicit price
on the attempt each shard will cost.

That gate is most of the mainnet result. The mainnet snapshot is a graph where
most payments fit down one corridor; atomic1 recognizes that in one search and
spends one attempt, and its cross-payment memory means the search gets sharper
with every payment in the file. Hence 1.6 attempts per payment. The champions
reach for their ladder first and average 2.3; lnd averages 19.8.

### Learning, amplified by its own reservations

`ReportAttempt` credits every hop that demonstrably forwarded, and it credits
them with more than the shard carried:

```go
r.learnSuccess(key, amt+r.reserved[key])
```

The ledger at that moment holds what the router's *other* in-flight shards are
sitting on, so a hop that just forwarded 1M while already holding 3M of ours
has proven it can carry 4M, and `lowerOK` records 4M. The same amplification
applies on the failure side: `totalRequired = amtOver + r.reserved[key]` is
what gets recorded as `upperFail`. Reservations are not only a planning
constraint, they are a measurement instrument. Nothing else in the project
does this, because nothing else in the project had a reason to before shards
started holding liquidity.

Attribution itself is conventional and cheap. The prefix before the failing
hop is credited; a `TemporaryChannelFailure` at the failing hop records a
bound and one unit of suspicion; anything else — fee, CLTV, or an unrecognized
code — sets `policyBlocked[key]` for the rest of the payment and adds two.
An unattributable failure touches no bounds at all and instead spreads
suspicion over the route:

```go
if failIdx < 0 {
	r.markRouteSuspect(rt, 2)
	r.lastFailedShard = candidateFinalAmount(rt)
	return nil
}
```

Compare mx_c3's `recordAnonymousFailure`, which reasons by elimination and
escalates a repeat suspect into a hard bound. atomic1 does none of that
reasoning; it just makes the route expensive and moves on. Given the sim's
precise attribution this costs almost nothing, and it is one of the places
where the degraded-attribution experiment would hurt this router more than the
champion.

## Why it lost

The verdict is one tier wide: −0.044 on the held-out atomic test at p=.07.
Reading the code, two candidates explain it, and the sweep tells us which one
matters.

**The plan does not survive its own failure.** On any failure,
`ReportAttempt` sets `r.planned = nil`, and `RequestRoute` also discards the
plan whenever the leading shard no longer fits the remaining amount. So a
four-shard plan that loses its third shard to one busy corridor throws away
the two shards that had nothing to do with that corridor, and the next call
re-derives everything from scratch — three fresh `planOnce` sweeps, each one
running a Dijkstra per size rung. In an arena that charges 30 virtual seconds
of background traffic per attempt, re-planning is not free.

This is precisely the mechanism exp-010b was built to reward, and precisely
the one exp-010's opus1 had. The tier ordering agrees: opus1's persistent
queue scores 0.425 on atomic-test against atomic1's 0.400, and 0.429 against
0.426 on atomic-val, despite opus1 never having seen the arena. Selection on
the atomic corpus produced a better router overall and re-derived less of the
mechanism the corpus was designed to select for.

**The arena's selection signal is noisy.** That is the pre-registered caveat
in the writeup, and it now looks binding. Per-file scores on the atomic corpus
swing with churn even at seven graded payments per file, so minibatch
acceptance is noisy, and both arms show the symptom: the Opus arm's winner is
*worse on the atomic tier* than exp-010's opus1 (0.391 against 0.425), and the
codex arm's winner is worse there too. Four hundred evaluations in a
high-variance environment select for robustness — a router that does
tolerably everywhere is a router that survives noisy minibatches — which is a
neat explanation for why the arm produced the program's first generalist
challenger and not an atomic specialist.

Read those two together and the honest verdict is that the environment change
worked and the selection budget did not keep up. The arena reordered the
baseline exactly as hypothesized, elicited up-front planning from both arms,
and then handed the trophy to whichever candidate was least punished by
variance.

## What it says about proposers

exp-010 ran three proposer lineages on one static corpus and found that the
strongest one, Opus 5 at default effort, produced the deepest planner and the
best on-corpus score. exp-010b ran two of them on a churn-noisy corpus and
flipped that: codex wins every tier here, and the Opus arm's winner is the
weakest artifact of the family. The consistent story is that deliberate,
large-step proposals pay in a low-noise environment, where a big architectural
jump is measured accurately enough to be accepted for the right reason, and
misfire in a noisy one, where a big jump is accepted or rejected largely on
churn. Small steps ride noise better.

Proposer choice, in other words, interacts with environment *variance*, not
just with budget. That is a new axis for the program's law, and it is
actionable: match the proposer to the arena's signal-to-noise, or fix the
arena's resolution first.

## Shortcomings

**No plan persistence.** Covered above; it is the leading candidate for the
atomic-tier loss and the one thing the exp-010 lineage already knew how to do.

**No reverse-direction inference.** The edge key carries `from` and `to`, so
the reverse direction is addressable, and nothing uses it. mx_c3 moves both
sides of a channel when a shard settles; opus1 goes further, inferring a
ceiling on this side from proven liquidity on the other (`compBound`) and an
optimistic center from a dry reverse side (`provenCenter`). atomic1 learns one
direction at a time and leaves the free inference on the table.

**Design-level weaknesses visible in the code.**

- `belief.successes` and `belief.failures` are incremented, persisted across
  payments, and never read. They are the confidence counters the champions use
  to weight evidence; here they are dead weight in a map that never evicts.
- `buildRoute` can fail with "route contains cycle". The parent pointers in
  `next` are written at relaxation time while the required amount varies along
  each path, so the reconstructed chain is not guaranteed acyclic. The router
  detects it and errors out, which discards the whole plan attempt rather than
  the one bad path.
- The whole file has exactly one named constant. `riskWeight = 420_000` and
  `hopPenalty = 220` are at least local to `findRoute`; `22_000`, `260_000`
  twice, `0.025`, `4_000_000`, `0.012`, `0.35`, `5/8`, and `2/3` are literals
  at their point of use, so the router's economics cannot be read off a
  constant block the way mx_c3's can.
- A `FeeInsufficient` or `IncorrectCltvExpiry` reply sets `policyBlocked` for
  the rest of the payment. That is a stale gossip policy, not a liquidity
  problem, and both lnd's second-chance logic and opus1's policy repair treat
  it as recoverable. atomic1 discards the channel instead. Cheap, and wrong in
  the one case where a single re-quote would have worked.
- The fingerprint XORs per-edge hashes, so two byte-identical directed edges
  cancel each other out. Parallel channels between one pair would have to
  differ in `chanID`, which they do, so this is safe in the simulator and
  worth remembering anywhere else.
- `r.attempts++` counts a queued shard handed out from an existing plan the
  same as a fresh search. That is the right accounting against the runner and
  it means a wide plan spends its budget fast: at `MaxParts = 8` the limit is
  32, and eight of those go to dispatching the first plan.

**The usual simulator caveats.** No fee market, no non-strict forwarding, no
parallel channels between a pair, one source node per scenario, local balances
snapshotted once per payment, and a composite objective that caps the fee
penalty at 5,000 ppm. The atomic arena lifts the sequential-settlement caveat
and adds its own: hold-and-release, contention, and 30 virtual seconds of
traffic per attempt are design choices calibrated against the baseline, not
measurements of mainnet.

**Precise attribution is a gift.** Every failure in this arena names its
source. atomic1's unattributable-failure path does no elimination reasoning at
all, so it has more to lose than mx_c3 from the degraded-attribution
experiment the advisor program flagged as the decisive pre-upstream test.

**Not production code.** The contract is `routing.SimRouter`, not lnd's
`Router`. The package-level memory map is unbounded and never evicted, keyed
by a hash that assumes a static graph over a scenario. There is no
persistence, no namespacing, no RPC surface, no belief import or export. Treat
the file as a specification of an idea.

## When to read atomic1

Read it for the hybrid. It is the first artifact in the project that carries
knowledge between payments *and* commits a whole shard set up front, and the
two halves interlock better than either does alone: the memory makes the
first plan of a payment good, and the reservation ledger makes a good plan
survive contact with its own siblings.

Read it also for two mechanisms that deserve to outlive it. Reservations
priced into the probability function, so a plan cannot lean twice on one
corridor and so a successful hop proves more than its own shard. And evidence
scoped by lifetime instead of decayed by a clock: savage within the payment,
merely persuasive across payments. exp-008 concluded that time decay buys
nothing at realistic churn. atomic1 shows what you build instead.

Do not pick it for scoring. mx_c3 matches or beats it on all six tiers, and
the two tiers where the gap is real are the two it was bred for.

## See also

- `exp-010b-atomic-splitting.md` — the atomic arena, its pre-registered
  design, the baseline that reorders the field, and both arms' verdicts.
- `exp-010b-atomicopus1-best-candidate.md` — the Opus-arm sibling, its
  bound-relaxation re-probe, and the losing economy it produced.
- `exp-010-opus1-best-candidate.md` — the persistent-plan challenger this
  router is measured against on the atomic tiers, walked through the same way.
- `exp-010-splitting-pressure.md` — the corridors corpus and the original
  three-way proposer A/B this experiment inverted.
- `simulation/champions/router_mx3_generalist_v1.md` — the champion, its
  reactive ladder, and the full comparison against lnd's production stack.
- `routing/sim_router.go` and `routing/sim_run.go` — the `SimRouter`
  contract, the atomic-MPP hold ledger, and the per-attempt traffic advance.
