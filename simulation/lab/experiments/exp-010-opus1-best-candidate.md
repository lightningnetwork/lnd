# opus1 — the challenger that caught the champion at home

`exp-010-opus1-best-candidate.go` (1,931 lines) is the winner of the
`code_split_opus1` run and the largest artifact code evolution has produced
here. It is also the only evolved router in the program's history to reach
mx_c3 on any tier. On the corridors corpus it was bred against it scores 0.839
against the champion's 0.835 — a paired delta of +0.005 at p=.07 — and it
settles 0.958 of those payments where mx_c3 settles 0.917. Off that corpus it
collapses: 0.303 on the sealed hard test, against mx_c3's 0.583.

So the one-line verdict is "tied at home, paid for it everywhere else," and
that is where exp-010 left it. exp-010b then changed the arena rather than the
router. Once shards hold liquidity, siblings contend for it, and background
traffic moves between attempts, opus1 pulls statistically even with mx_c3 on
both atomic tiers (−0.013 at p=.73 and −0.019 at p=.29) while the two
shallower joint planners fall significantly behind and lnd's production stack
drops to last at 105 attempts per payment. It had never seen atomic semantics.
Honest pricing of sequential probing does not make its planner win, but it
stops charging it for depth it cannot use.

Read this document next to the source. Every constant quoted below appears
verbatim in the file.

## Provenance

| field | value |
|---|---|
| run | `code_split_opus1` (GEPA code mode, reflection LM `claude:claude-opus-5` at default reasoning effort, 5–8 minutes per proposal) |
| seed program | the small in-tree router, `cmd/routesim/candidate_impl.go` (~380 lines), with the discovered insights supplied as prose in the background prompt, and joint route-set planning named as unexplored design space |
| training corpus | `corpus-split` (seed 4041): the corridors topology, 8–16 parallel corridors of deliberately unequal capacity tiers between one source and one target, two cheap probes then one payment above the fattest tier per file |
| budget | 400 evaluations, matched with the codex and Opus-medium arms |
| siblings | `exp-010-split2-best-candidate.go` (976 lines, codex arm), `exp-010-opusmed1-best-candidate.go` (883 lines, Opus-medium arm) |
| writeups | `exp-010-splitting-pressure.md`, `exp-010b-atomic-splitting.md` |
| status | not promoted; kept as the deepest evolved planner and the closest anything has come to the champion |

Two operational caveats are logged with the run and apply symmetrically to the
Opus-medium arm, so the A/B stays fair: a user-level Stop hook leaked into
3–4% of iterations before the `CLAUDE_CONFIG_DIR` seal landed, and an
API-limit outage cost the run its last eleven iterations before it was resumed
from GEPA state with the stub-wasted budget refunded.

## Validated scores

Every tier is held out from the run. Objective =
`success − 0.01·min(extra_attempts, 15) − 0.00002·min(fee_ppm, 5000)`.
Paired deltas are against mx_c3 as baseline, with bootstrap 95% intervals and
sign tests.

| tier | lnd stack | **mx_c3** | split2 | opusmed1 | opus1 | opus1 delta [p] |
|---|---|---|---|---|---|---|
| corridors split-val | 0.782 | 0.835 | 0.809 | 0.782 | **0.839** | +0.005 [.07] |
| corridors split-test | 0.837 | **0.876** | 0.810 | 0.743 | 0.841 | −0.035 [.07] |
| hard sealed test | 0.309 | **0.583** | 0.536 | 0.299 | 0.303 | −0.280 [.002] |
| OOD corpus-v2 | 0.357 | **0.581** | 0.494 | 0.420 | 0.483 | −0.098 [.34] |
| mainnet, 12,161 nodes | 0.694 | **0.791** | 0.743 | 0.766 | 0.757 | −0.033 [.18] |
| atomic val (exp-010b) | 0.286 | **0.442** | 0.356 | 0.357 | 0.429 | −0.013 [.73] |
| atomic test (exp-010b) | 0.338 | **0.444** | 0.391 | 0.373 | 0.425 | −0.019 [.29] |

Read the table by rows of three. On the corpus it was bred for, opus1 is the
best non-champion result anyone has posted and is indistinguishable from the
champion on validation. On the static legacy tiers it is a specialist paying
for its specialization: 0.757 on mainnet at 5.9 attempts per payment where
mx_c3 needs 2.3, and 0.303 on the hard test where mx_c3 scores 0.583. On the
atomic tiers, where sequential probing costs time and reserved liquidity, the
gap to the champion is noise again.

## Running it

```bash
cd $LND_REPO
cat > /tmp/overlay.json <<EOF
{"Replace": {"$PWD/cmd/routesim/candidate_impl.go":
             "$PWD/simulation/lab/experiments/exp-010-opus1-best-candidate.go"}}
EOF
go build -overlay /tmp/overlay.json -o /tmp/routesim_opus1 ./cmd/routesim

# Regenerate corpus-split (fixed seed, so it reproduces exactly).
python3 simulation/gen_scenarios.py --out /tmp/corpus-split --split --seed 4041

/tmp/routesim_opus1 --scenarios /tmp/corpus-split/test/example_000.json \
    --router=candidate --traces=false
```

## The mechanism ladder

exp-010 built an environment where a payment above the fattest corridor tier
cannot be halved into shards anything will carry, then ran three proposer
lineages at it on the same corpus, the same budget, and the same seed. All
three produced joint route-set planning. They differ in how long the plan
lives.

| arm | what it plans | how long the plan lives |
|---|---|---|
| split2 (codex) | one shard plus a one-step lookahead at the next shard, scored as a pair with reservation during planning | one call; the pair is re-derived from scratch each time |
| opusmed1 (Opus, medium) | a shard sized to the believed bottleneck of the corridor the search just returned, chosen against a fraction ladder | one call; `planShard` starts over on every request |
| **opus1 (Opus, default)** | a full residual decomposition of the remaining amount over disjoint corridors, each shard sized to its own corridor | across failures; the queue is pruned by evidence, not discarded |

The depth ordering is also the on-corpus ordering (0.810, 0.743, 0.841 on
split-test) and, once the arena charges for probing, the atomic ordering
(0.391, 0.373, 0.425 on atomic-test). It is the reverse of the
generalization ordering on the hard test, where the shallow codex planner
keeps 0.536 and both Opus planners land near 0.30.

Persistence is the whole difference, and the design comment at the top of the
file says where it came from:

> The big observed failure was a large payment (2.06 Gmsat) burning 20
> attempts and dying on "no progress" while every failure was a plain
> TemporaryChannelFailure. The root cause was that the joint flow plan was
> thrown away on EVERY failure (r.queued = nil), so the router collapsed back
> to single-corridor probing and never actually held several unequal shards in
> flight.

An earlier candidate in the same lineage already planned route sets. It threw
the plan away the first time any shard missed, which turned joint planning
into an expensive way of doing reactive laddering. Selection found the fix in
the failure traces.

## Architecture

Start from the constants, because two of them decide the whole
specialist–generalist story later.

```go
finalCltvDelta = 40
maxRouteHops   = 7
maxAttempts    = 90
baseFailStreak = 10
maxFailStreakCap = 34
probeBudget      = 12
flowRounds       = 14
hopelessStreak   = 6
```

mx_c3 allows 24 hops and 80 attempts and has no give-up test at all beyond the
attempt limit. opus1 allows 7 hops, 90 attempts, and quits on three separate
conditions.

### What it inherited

The belief layer is the lineage's, with one field per idea:

```go
type belief struct {
	okAmt   lnwire.MilliSatoshi
	failAmt lnwire.MilliSatoshi
	hasFail bool
	succ    bool
	fails   int
	drained lnwire.MilliSatoshi
	inFlight lnwire.MilliSatoshi
	misses  int
	dead    bool
}
```

`okAmt` is the largest amount proven to pass, `failAmt` the smallest proven to
fail, and there is no timestamp anywhere in the file — no `time` import, no
half-life, no decay constant. The file's own summary states the exp-008
conclusion as a premise: "a stale bound costs one retry to refresh, which is
cheaper than decaying evidence." Insight transfer through the background
prompt is working exactly as `code_gen2` intended.

Beliefs are keyed by `edgeKey{chanID, to}` rather than the champions'
`(chanID, from, to)`. For a two-party channel the destination already names
the direction, so this is the same key with one field less.

Complementary-side reasoning appears twice, and it is the sharpest inherited
idea in the file. `compBound` turns proven liquidity on the reverse side into
a hard ceiling on this side:

```go
held := rb.okAmt * compSlackNum / compSlackDen   // 90%
if held >= e.capacity {
	return 0
}
return e.capacity - held
```

`provenCenter` runs the same inference in the optimistic direction — a reverse
side that came up dry means this side is holding nearly the whole channel:

```go
if rb := r.beliefs[e.revKey()]; rb != nil && rb.hasFail {
	if e.capacity > rb.failAmt {
		inf := (e.capacity - rb.failAmt) *
			revEmptyNum / revEmptyDen           // 80%
		if inf > center {
			center = inf
		}
	}
}
```

`applySettle` then moves the money on both sides of the channel when a shard
settles: the forward direction's bounds drop by the settled amount and gain it
in `drained`, and the reverse direction's `okAmt` rises by exactly that
amount, with a reverse `failAmt` pushed up and dropped entirely once it
exceeds capacity or falls below `okAmt`. Nothing in lnd does this.

`retryLimit` is the retry-at-a-lower-amount mechanism, depth-aware rather than
tabulated: a direction that failed at a small fraction of capacity is treated
as nearly empty, one that failed near capacity leaves room underneath.

```go
f := 0.40 + 0.35*depth
lim := lnwire.MilliSatoshi(float64(b.failAmt) * f)
```

with `depth = failAmt/capacity`, and a hard stop after `maxDryProbes = 3`
liquidity failures on the same direction, after which the limit collapses to
`okAmt`. mx_c3 spends a six-rung constant table on the same question; this is
two constants and a ratio.

**The prior kept the name and lost the shape.** `bimodalPrior` is still
introduced by a comment about liquidity sitting almost entirely on one side,
and the two terms are still labelled "low mode" and "cliff":

```go
low := math.Exp(-x * 3.2)
cliff := 1.0 / (1.0 + math.Exp((x-0.42)*9.0))

return clampProb(0.30*low + 0.70*cliff)
```

The cliff is centred at 42% of capacity, not near it. Evaluate the curve and
it is a monotone slide from 0.985 at dust to 0.43 at 42% of capacity to 0.02
at capacity — a smooth pessimism gradient, not mx_c3's flat coin-flip across
the middle with a wall at 96.5%. The bimodal hypothesis did not disappear from
the router; it moved out of the prior and into `compBound` and `provenCenter`,
where it is applied only when there is evidence to apply it to. That is a
defensible reallocation on a corpus whose corridors are sized so the payment
sits well up the capacity curve, and it is one more reason the router travels
badly.

One accident is worth noticing. `prob` ends in `clampProb(p)`, whose ceiling
is `maxProb = 0.985`, but the local-channel branch returns `knownProb = 0.995`
before reaching it. A proven remote hop is therefore capped a percent below a
local one, which biases the search toward short routes without a hop term.
mx_c3 achieves the same thing deliberately with `0.9995` on its own channels;
opus1 got it from a clamp it did not apply uniformly.

### The persistent plan

Three functions carry it.

`planFlow` builds the decomposition. It walks up to `flowRounds = 14`
corridors, and it keeps a residual ledger so two shards never spend the same
local balance twice:

```go
// Residual local budget per first-hop channel.
residual := make(map[uint64]lnwire.MilliSatoshi, len(r.localEdges))
for _, e := range r.localEdges {
	if _, ok := residual[e.chanID]; !ok {
		residual[e.chanID] = r.availCap(e)
	}
}
```

Each round sizes one shard by the minimum of what its corridor is believed to
bear and what its first hop has left, then charges the shard against that
first hop and retires every channel it used from the rest of the
decomposition:

```go
amtS := want
if fl := residual[path[0].chanID]; fl > 0 && fl < amtS {
	amtS = fl
}
if bn := r.bottleneck(path); bn < amtS {
	amtS = bn
}
```

```go
fh := path[0].chanID
if residual[fh] > pl.amt {
	residual[fh] -= pl.amt
} else {
	residual[fh] = 0
}
for _, e := range path {
	avoid[e.chanID] = true
}
```

The bound that matters is the first-hop one. A shard leaves through exactly
one local channel, so the true ceiling on a single shard is
`maxLocalEdge()`, not the payment amount — and a decomposition that ignores
the residual will happily plan three shards all sized to the same 500k-sat
channel. mx_c3 never has this problem because it never holds more than one
shard in mind at a time.

`pruneQueue` keeps the plan alive across failures:

```go
func (r *router) pruneQueue(chanID uint64) {
	if len(r.queued) == 0 {
		return
	}
	kept := r.queued[:0]
	for _, pl := range r.queued {
		if pathUses(pl.path, chanID) {
			continue
		}
		kept = append(kept, pl)
	}
	r.queued = kept
}
```

What survives depends on what was learned. A liquidity miss prunes only the
shards routed through the failing channel. A permanent failure
(`ChannelDisabled`, `UnknownNextPeer`, `PermanentChannelFailure`) does the
same. An unattributable failure prunes every shard sharing any remote hop of
the failed route, because the failure could have been any of them. A fee,
CLTV, or minimum-HTLC repair prunes nothing at all — the comment in
`ReportAttempt` is explicit that "a fee repair is not a liquidity miss, so it
does not count against the streak and the queued plan stays valid."

`serveQueued` hands the plan out lazily, re-pricing each shard against
beliefs that have moved since it was planned. Three outcomes per shard:

- The corridor is busy with one of our own in-flight HTLCs. Defer it to the
  front of the queue and try the next one; a busy corridor is temporarily
  occupied, not wrong.
- The exact `(path, amount)` pair already failed. Re-price the same corridor
  one notch lower rather than abandoning a corridor that was deliberately
  chosen:

```go
if lower := r.bestOnPath(pl.path, a-a/8, amt); lower !=
	nil && lower.prob >= queueMinProb {

	r.queued = append(deferred, r.queued...)
	return lower
}
```

- Re-pricing the route now fails, or clears less than `queueMinProb = 0.12`.
  Try the corridor at three quarters of the amount, then drop it.

The plan is therefore a hypothesis about the shape of the flow, not a script.
Corridors leave it only when evidence contradicts them.

### Concurrency-first dispatch

`RequestRoute` is four steps, and the second one is the deliberate skip:

```go
// Step 2: concurrency-first dispatch. When the remainder provably
// exceeds any one local channel, ladder search over a single corridor
// cannot succeed, so plan the whole decomposition now.
if split && partsLeft > 1 && single > 0 && amt > single {
	flow := r.planFlow(amt, partsLeft, busy)
	if len(flow) > 0 {
		r.queued = flow[1:]
		r.attempts++
		return flow[0].rt, nil
	}
}
```

`single` is `maxLocalEdge()`. When the remainder exceeds it, no single shard
can carry the payment, so searching the amount ladder for one is a waste of an
attempt by construction. The router plans the whole decomposition and returns
its first shard immediately, filling `MaxParts` with correctly sized unequal
shards instead of discovering the split by failing at a blind half. Every
champion, and both sibling planners, reach for the ladder first.

Step 3 is the ladder search, run twice: a first pass that avoids channels
carrying in-flight shards, then an unrestricted pass. Step 4 compares the best
single shard against a fresh decomposition and prefers the decomposition when
it covers more of the payment, on the argument that "every uncovered millisat
is a failed payment." Step 5 is `salvage`, a 22-rung descent at ratio 2/3 over
both passes, on the argument that a small settled shard reduces the remainder
and refreshes evidence, which beats abandoning the payment.

### The fail budget, and three ways to quit

`failBudget` scales the streak allowance with how many shards the payment
provably needs:

```go
shards := 1
if single := r.rawMaxLocal(); single > 0 && r.firstAmt > single {
	shards = int(r.firstAmt/single) + 1
}
if mp := int(r.spec.MaxParts); shards > mp && mp > 0 {
	shards = mp
}
if shards > 1 {
	budget += 5 * (shards - 1)
}
```

Ten failures for a payment that should fit one corridor, five more per shard
it provably needs, capped at 34. The reasoning is sound: each shard costs at
least one attempt to place and one more to resize, so a four-shard payment
should not die on a streak a single-shard payment would deserve.

The streak that feeds it is charged at half weight when the failure was
informative, and the implementation is the oddest line in the file:

```go
if failIdx > 1 {
	r.failStreak++
	if r.failStreak > 0 && r.attempts%2 == 0 {
		r.failStreak--
	}
} else {
	r.failStreak++
}
```

A failure deeper than the first hop means the earlier hops demonstrably
forwarded, which is real evidence, so it should cost less patience than a
failure at the door. Half weight is implemented as the parity of the attempt
counter: increment always, decrement on even attempts. Over a long run it
averages to the intended half, and on any particular attempt it is a coin
flip on a counter the router controls.

`hopeless` is the third exit, and unlike the other two it can fire before any
attempt at all:

```go
if r.rawLocalBudget() < amt {
	return true
}
if !r.mppOK() && r.rawMaxLocal() < amt {
	return true
}
if r.failStreak >= hopelessStreak && len(r.localEdges) > 0 &&
	r.localBudget() < amt {

	return true
}
```

The first two tests are arithmetic on exactly known local balances and are
simply correct. The third is a belief-derived test that trusts `availCap`
after six failures, which matters because `ReportAttempt` clamps
`localBalances[chanID] = a - 1` whenever the router's own first hop refuses an
amount. Repeated first-hop failures therefore shrink the believed budget until
the router declares the remainder unreachable.

## What it did not inherit: memory between payments

`newCandidateRouter` is called once per payment by the runner, and opus1 keeps
its beliefs on the router:

```go
beliefs map[edgeKey]*belief
```

There is no package-level state in the file — no `var` block at all. hb1,
mx_c3, and the codex arm's split2 all keep a mutex-guarded package-level map
(`candidateKnowledge`, `sharedBeliefs`) that survives from one payment to the
next within a scenario file. opus1 starts every payment blind.

That is startling on this corpus in particular, because each corridors file is
built as *two cheap probes that seed corridor knowledge, then one ambitious
payment*. opus1 throws the probes away and ties the champion anyway, purely on
what it learns inside the ambitious payment itself. It is also a large part of
why it travels badly: a hard-corpus file carries six to ten payments across
one graph, and mx_c3 arrives at the sixth payment with five payments' worth of
directed-channel intervals while opus1 arrives with nothing.

Both Opus arms dropped cross-payment memory; the codex arm kept it. That is
the clearest structural split between the proposer lineages in this
experiment.

## Why it lost

The exp-010 verdict attributes the off-corpus collapse to the corridor-tuned
adaptive fail budget: opus1 gives up after roughly seven attempts on hard
bimodal networks where mx_c3 spends 10.8 and succeeds at 2.4 times the rate.
That is the right shape of explanation, and reading the code suggests a
sharper version of it.

Regenerate a hard corpus (`gen_scenarios.py --hard`, default seed 2026) and
watch a single file. On `test/example_000.json` — a 600-node small-world graph
with 1M-sat channels — opus1 refuses two payments with *zero* attempts and
abandons two more after three, while mx_c3 settles six of nine. Instrumenting
the three exits shows that not one of those give-ups is the fail budget or
`hopeless`. Every one is `best == nil`: the router searched and found no route
it would accept. Count the hops on mx_c3's successful attempts for the payment
opus1 declines outright, and the routes are 9 to 23 hops long.

`maxRouteHops = 7` cannot express them. On the corridors corpus every route is
source → corridor → target and seven hops is generous; on a 600-node
small-world graph at 25% of channel capacity, a router that will not look past
seven hops is not searching the graph the payment lives on.

The measurement, on a locally regenerated hard corpus with the single constant
raised to mx_c3's 24 and nothing else touched:

| router | objective | success | attempts |
|---|---|---|---|
| opus1 as archived (`maxRouteHops = 7`) | 0.284 | 0.350 | 5.6 |
| opus1 with `maxRouteHops = 24` | 0.407 | 0.530 | 9.7 |
| mx_c3 | 0.479 | 0.592 | 8.1 |

Absolute levels here are below the sealed-tier numbers (mx_c3 scores 0.583 on
the sealed hard test and 0.479 on this regenerated one), so read the deltas
rather than the levels: one constant closes about half of a gap that the
verdict reads as an architectural failure. The give-up statistics in the
writeup are consistent with this and were measuring its consequence — a router
that cannot represent the routes a hard graph needs runs out of *candidates*
long before it runs out of patience, and then quits early on top of that.

The honest reading is that opus1 overfitted its **constants** harder than its
**architecture**. Both matter, and the constants are cheaper to indict: the
hop limit, the prior's cliff at 42% of capacity, and a fail budget calibrated
against corridor counts are all things a corridors corpus selects for and a
mainnet snapshot punishes. The persistent-plan machinery, by contrast, is what
holds up in exp-010b.

### The complexity wall

At 1,931 lines opus1 sits more than twice past the roughly 800-line ceiling
exp-011 identified for code evolution, and further past it than mx_c3's 1,525.
The published lineage export for this run
(`simulation/command-center/data/run.json`, 34 candidates) shows what that
cost: over the first eighteen iterations of the exported lineage eleven
proposals were accepted, and over the last fifteen — with candidates in the
1,700–2,000 line band — five were. Programs that large also make each
reflection slower and each mutation likelier to break something far from where
it was aimed. Depth bought the tie and depth bought the ceiling.

## What exp-010b tests about it

exp-010b was designed to answer one question left open here: was the reactive
ladder winning on merit, or was the simulator subsidizing it? A shard used to
settle instantly, its outcome arriving before the next route request, with the
world frozen while the payment ran. Probe-learn-resize therefore collected all
of joint planning's information advantages at none of its costs.

The atomic arena charges for them. Shards hold liquidity along their path
instead of settling, siblings and background traffic see availability net of
those holds, and traffic runs on attempt boundaries at 30 virtual seconds
each, so a twenty-attempt ladder watches ten minutes of corridor churn while a
plan that fills `MaxParts` commits before the world moves. Per-attempt
feedback is unchanged, deliberately.

The baseline reorders the field before any evolution runs. lnd, second-best on
the non-atomic corridors corpus at 0.837, finishes last at 0.338 and 105
attempts per payment — its divide-and-conquer probe ladder is precisely what
the arena now taxes. The champions hold the top. And opus1, which has never
seen atomic semantics, becomes statistically indistinguishable from mx_c3 on
both tiers while split2 and opusmed1 fall significantly behind.

That is the result this document exists to record. Under honest pricing, the
depth ordering of the three planners survives and the penalty for it
disappears. It does not make opus1 a champion — it still trails by 0.019 on
atomic-test and it still spends 23.5 attempts per payment against mx_c3's 12.6
— but it does mean the persistent residual-aware plan was measuring something
real that the old arena refused to pay for.

The open question is now narrow and testable: does a router *bred* in the
atomic arena rediscover this machinery and beat the champion with it? That is
`code_atomic_opus1`, in flight.

## Shortcomings

**Corridor-tuned constants, listed.** `maxRouteHops = 7`, the prior's cliff at
42% of capacity, `flowRounds = 14`, `probeBudget = 12`, `baseFailStreak = 10`
with `+5` per believed shard, `hopelessStreak = 6`, `maxDryProbes = 3`,
`queueMinProb = 0.12`. Each was selected by the objective on a corpus of 8–16
parallel corridors between one source and one target. The first one alone
costs about half the hard-test gap.

**No memory between payments.** Every payment starts from an empty belief map,
so nothing the router learns is ever amortized. On a corpus with six to ten
payments per graph this is a large, unforced handicap.

**Design-level weaknesses visible in the code.**

- `delivered` is incremented on every settlement and never read. Its own
  comment claims it sizes the fail budget; `failBudget` uses `firstAmt`.
- The half-weight streak charge is attempt-parity, not a fractional counter,
  so an individual informative failure costs either one or zero patience
  depending on when it happens.
- `bottleneck` seeds its minimum with `lnwire.MilliSatoshi(math.MaxUint32) *
  1024` and returns `bn - bn/100`, so a corridor of entirely unconstrained
  hops returns a meaningless 4.4-terasat "bottleneck" that only survives
  because every caller immediately takes a minimum against a real amount.
- `serveQueued` discards the deferred shards along with the queue when a
  non-splittable payment meets a partial shard (`r.queued = nil`), which is
  the one place the persistence property is dropped rather than pruned.
- The two re-pricing backoffs differ for no stated reason: `a - a/8` after a
  duplicate signature, `a*3/4` after a belief-driven rejection.
- `classify` buckets failures by `strings.Contains` over
  `fmt.Sprintf("%T %v", f, f)`. It is robust to the concrete failure type,
  and it will also misfire on any future failure whose type name happens to
  contain one of the substrings.
- Policy repairs mutate the router's private copy of the gossip policy
  (`e.baseFee += e.baseFee/4 + 1_000`, `e.cltv += e.cltv/4 + 20`,
  `e.minHTLC = a + 1`) rather than penalizing the channel. That is closer to
  what lnd's second-chance logic is *for* than to what it does, and it is
  discarded with the router at the end of the payment.

**The usual simulator caveats.** No fee market, no non-strict forwarding, no
parallel channels between a pair, one source node per scenario, local balances
snapshotted once per payment, and a composite objective that caps the fee
penalty at 5,000 ppm. The sequential-settlement caveat that applied to every
earlier router is the one exp-010b lifts, and this router is the main
beneficiary.

**Not production code.** The contract is `routing.SimRouter`, not lnd's
`Router`. There is no persistence, no namespacing, no RPC surface, no belief
import or export — and here, no state outside a single payment at all. Treat
the file as a specification of an idea.

## When to read opus1

Read it if you want to see what joint route-set planning looks like when it is
carried far enough to matter: a residual decomposition with per-first-hop
budgeting, a plan that survives its own failures, and a dispatcher that spends
concurrency before it spends attempts. Nothing else in the project has those,
and exp-010b says they are load-bearing once the arena stops subsidizing
sequential probing.

Read it also as the cleanest specialist–generalist case study the program has.
Same corpus, same budget, same seed, three proposers: the strongest one
produced the deepest planner, the best on-corpus score, and the worst
generalization. Proposer strength moved the candidate along the axis; it did
not lift the curve.

Do not pick it for scoring. mx_c3 beats or ties it on all seven tiers.

## See also

- `exp-010-splitting-pressure.md` — the corridors corpus, the three-way
  proposer A/B, and the five-tier sweep.
- `exp-010b-atomic-splitting.md` — the atomic arena, its pre-registered
  design, and the baseline that reorders the field.
- `exp-010-opusmed1-best-candidate.md` — the medium-effort sibling and the
  val-overfit it produced.
- `exp-010-split2-best-candidate.go` — the codex arm's one-step lookahead,
  the shallow end of the ladder.
- `simulation/champions/router_mx3_generalist_v1.md` — the champion this
  router challenged, and the full comparison against lnd's production stack.
- `exp-008-drift1-best-candidate.md` — the other archived non-champion,
  walked through the same way.
- `routing/sim_router.go` and `routing/sim_run.go` — the `SimRouter`
  contract, and the per-payment router construction that explains the missing
  cross-payment memory.
