# The distillation patch

Three separate experiments in this fork converged on the same
suspicion: that a useful chunk of what the evolved routers do could be
folded back into lnd's own payment loop as a small diff, with no new
paradigm attached. exp-002b showed that swapping in a better liquidity
prior raises lnd's success rate (0.421 to 0.478 on the hard tier) while
more than doubling its attempts (30.9 to 77), because a better prior
changes how lnd ranks routes and not what it tries next. exp-016 showed
that handing lnd a pile of third-party observations makes it *worse*
(objective −0.029, with attempts going up), and that imported failures
are the whole of the loss, because lnd's only response to "this edge
can't carry X" is to route around rather than to send less. exp-019
showed lnd spiraling into give-ups from a 10% rate of unreadable onion
errors. All three point at the same place: lnd learns amount bounds and
then has nowhere to spend them, and it over-reacts to failures it can't
attribute at all.

Commit `9c07cbe7f` is the diff that tests that suspicion. It carries
two mechanisms behind two independent flags, and the result is one and
a half: `soft_unknown` fixes a real, measured pathology and is
upstreamable roughly as-is, while `adaptive_split` is a clean negative
that kills half of the theory that motivated the whole patch. Both are
below, and then the part that matters most for deciding what to do
next: where the patch stops, and why.

## What's behind the flags

Both knobs live in `routing.PatchConfig`, default false, and every code
path they guard is exactly the code that shipped before the struct
existed. `AdaptiveSplit` is read only by `PathFindingConfig`;
`SoftUnknown` is read only by `MissionControlConfig`. We carry the
whole struct into both so a node configures one section rather than one
flag per component.

The changes land in lnd's real files, not in simulator glue:
`payment_session.go` (the `errNoPathFound` branch of `RequestRoute`),
`result_interpretation.go` (`processPaymentOutcomeUnknown`), and
`missioncontrol.go` (which now hands the interpreter a probability
oracle when the soft policy is on). The simulator's `--router=lnd` arm
traverses the genuine `paymentSession.RequestRoute` and the genuine
mission-control interpretation, so what we measured is lnd's own stack
with two lines of behavior swapped, not an adapter pretending to be
lnd. `routing/patch_config_test.go` pins both mechanisms with unit
tests, including the flags-off cases.

Off means off, and that was checked rather than assumed: on the clean
control tiers (hard n=10, drift n=8) the patched binary's output is
exactly identical to stock, and on mainnet (n=10) one file differs by
0.1 attempts, which traces to wall-clock penalty decay on a tier that
pins no virtual clock, not to the patch.

## soft_unknown: the fix that lands

When a payment fails and lnd can't read the onion error, it learns only
*that* the payment failed. `processPaymentOutcomeUnknown` responds by
failing every pair of the route in both directions:

```go
// Otherwise penalize all channels in the route to make sure the
// responsible node is at least hit too. We even penalize the connection
// to our own peer, because that peer could also be responsible.
i.failPairRange(route, 0, n-1)
```

That deletes 2n pairs at amount zero (amount-independent blacklisting)
on the strength of no evidence at all. exp-019 measured what it costs.
On the sealed hard tier, a 10% unreadable-error rate drives lnd's
give-up rate from 0.31 to 0.71 and its success from 0.49 to 0.29; at
30%, four of ten files pin to exactly zero success, because lnd
blacklists routes until pathfinding returns "no path" and quits. The
same signature shows on mainnet at a realistic mix (unknown 0.2 plus
shift 0.1): success 0.790 to 0.730, give-ups 0.13 to 0.27. No other
router in the program shows anything like it. The interval routers all
learn *nothing* from an unattributable failure, on the grounds that a
failure nobody claimed is not evidence about anybody.

lnd can't go quite that far, because its retry loop needs the next
attempt to differ from the last one or it will spin. So the patch does
the least it can while still guaranteeing progress: fail exactly one
pair, the lowest-probability hop of the attempted route under the
current estimator, in the forward direction only, and at the amount
that hop was actually asked to forward. The amount matters as much as
the count. Recorded at the attempt amount instead of at zero, the entry
is a bound a smaller retry can route around, rather than a blacklisting.

On the exp-019 ladder, with the stock arm reproducing exp-019's cached
numbers bit for bit before anything counts:

| level | success stock→soft | give-ups stock→soft | obj recovered |
|---|---|---|---|
| hard, unknown 0.1 | 0.293 → 0.465 | 0.707 → 0.418 | 86% |
| hard, unknown 0.3 | 0.193 → 0.507 | 0.807 → 0.437 | 128% |
| hard, realistic mix | 0.240 → 0.518 | 0.760 → 0.461 | 148% |
| drift, mix + delay | 0.226 → 0.444 | 0.774 → 0.556 | 97% |
| mainnet, mix | 0.730 → 0.740 | 0.270 → 0.260 | 17% of the success loss |

Success rises on every non-tied file (sign tests p=.016 to .031) and
give-ups fall on every non-tied file at every hard and drift level.
"Recovered" is the fraction of stock's own collapse from its own clean
control that the patch takes back, so the rows above 100% mean patched
lnd ends up above where stock started on a clean channel. The soft arm
processes 5 to 12 times more unattributed failures than stock, because
it no longer quits, and it improves anyway.

Now the cost line, which we quote every time: **the patch buys success
with attempts.** On the hard tier it spends +18 to +29 attempts per
payment, and the objective's 15-attempt cap can't see any of it. That's
also why the objective column above is the recovery ratio rather than a
delta: read success and give-ups, never the objective alone. Mainnet is
essentially inert (17% of the success loss, 7% of the give-up rise),
which converges with exp-019b's reading that mainnet's degraded give-up
rise is mostly a different mechanism, or that 57 unknown failures across
ten files is too few to matter.

The diff is roughly 90 lines across `result_interpretation.go` and
`missioncontrol.go`. It fixes a measured pathology, moves success and
give-ups unanimously in the right direction, is provably inert on a
clean channel, and has its cost stated. That's the piece we'd take
upstream.

## adaptive_split: the null that teaches

The other half was supposed to teach the payment loop the champions'
inverted question. lnd asks "can the graph carry this fixed amount?"
and halves on failure; mx_c3 asks "what is the largest amount that
still has hope?" and reads its learned `upperFail` bounds to build a
shard ladder before sending anything. The patch's version prices a
fixed ladder of fractions of the failing amount (0.75, 0.5, 0.375,
0.25, 0.125) with ordinary pathfinding calls, so every rung respects
every bound mission control holds, then picks the argmax of fraction
times route probability. No new state, no estimator change.

Three designs went through the smoke gate, and each died by its own
trace. The supremum search (find the largest routable amount after a
wire failure at A) discovers that the estimator permits essentially
everything below A, so the answer is A−ε, which fails on the wire and
yields a new near-supremum: a linear descent paying one wire attempt
per step, objective −0.03. A geometric backoff at 0.75× below the
frontier reduces exactly to blind descent at 0.703, a slower
re-derivation of the 0.5 lnd already uses. The expected-value ladder
degenerates to its top rung under apriori's flat belief, which is
revision 1 again as a property of the value model rather than of the
search; under bimodal it does jump to the believed rung, but lands in
the same retry loop that pins stock-bimodal at the attempt cap.

The paired sweep on the clean tiers gives the final word: hard +0.034
[−0.002, +0.074], mainnet −0.010 [−0.029, +0.000], sign tests nowhere
near significance. Not even a citable negative, a genuine null.

The conclusion is load-bearing, so it's worth stating flatly. **lnd's
reactive split-retry descent is already optimal for its class.** Every
bound-reactive amount policy we could express reduces to geometric
descent from the learned bound, and lnd's blind halving already runs
that descent at the fastest ratio of any variant, for free, inside
`findPath` retries that cost no wire attempts. Put that next to
exp-002b (a better estimator alone changes nothing, and doubles
attempts) and both halves of the original distillation theory are
closed. The champions' edge does not live in the reaction to failure.

We kept the flag in the tree as instrumentation with the null attached,
because the three-revision arc is the documentation and because it's
the arm anyone re-testing this question will want to run first.

## Where it doesn't go far enough

**The hop choice is apriori-only until capacity is threaded.** Mission
control has no graph access, deliberately, so the probability oracle it
hands the interpreter passes capacity zero. Under the apriori estimator
that's the right call: zero means "no capacity information" and leaves
the estimate unscaled, so the ordering across the hops of one route
comes from what we've actually learned about them. Under the bimodal
estimator it's fatal. `probabilityFormula` returns `ErrZeroCapacity`
for a zero capacity, `getPairProbability` logs and returns 0.0, so
every hop scores identically and the tie-break (ties go to the hop
furthest from us) picks the last hop of the route every single time.
The mechanism isn't wrong there, it's just uninformed. Threading a
capacity into result interpretation is the prerequisite, and it isn't a
one-liner: mission control not knowing about the graph is a deliberate
layering choice, so this is an upstream design conversation and not a
patch.

**The rest of the gap is plan-time, and a patch can't reach it.** The
numbers say how much rest there is. At unknown 0.3 on the hard tier,
soft_unknown takes back roughly half of hb1's margin over lnd (+0.395
to +0.206) and erases atomic1's (+0.250 to +0.062); on the drift mix it
erases atomic1's outright (+0.058 to −0.017). Half of a champion's
degraded-tier lead for 90 lines is a good trade, and the other half is
the price tag on everything we didn't distill. What's left is
success-side memory (`lowerOK`, the bound saying a pair *did* carry
this much) feeding the *initial* amount choice, plus joint route-set
construction across shards. Neither is expressible against an
architecture where `findPath` takes the amount as a fixed argument and
`RequestRoute` only ever changes it after pathfinding has failed
outright. That's an architectural change, now measured rather than
suspected.

**Attempts are a real cost the objective doesn't charge.** The
objective caps the attempt penalty at 15 extra attempts, so a mechanism
that buys success by trying much harder looks free in the headline
number and isn't. soft_unknown is exactly that mechanism, and on a real
node the +18 to +29 attempts per payment are HTLCs on the wire, held
liquidity, and latency. Anyone evaluating this upstream should decide
independently whether the trade is worth it; we're reporting it, not
pricing it.

**And the usual caveats.** n is 8 to 10 files per tier throughout.
Objective deltas are heterogeneous across files (three or four carry
each mean) even where success and give-ups are unanimous. Mainnet is
inert for soft_unknown, so nothing here is validated on the tier
closest to real topology. One mainnet file shows ±0.1 attempts of
wall-clock nondeterminism, which affects nothing at effect scale but
applies to every mainnet attempt figure at that precision. At the
realistic mix, patched lnd scores *above* its own clean control
(+0.058 [+0.017, +0.094]); the mix contains shift 0.1, so this may be
exp-019's finding-3 anomaly resurfacing on the patched stack, and no
mechanism is claimed for it.

## Trying it

Both knobs are off unless a params file turns them on.
`simulation/params_lnd_patch.json` is lnd's shipping defaults plus:

```json
"patch": {
  "adaptive_split": true,
  "soft_unknown": true
}
```

Drop either key to ablate that half. Then:

```bash
go build -o /tmp/routesim ./cmd/routesim
/tmp/routesim --router=lnd \
    --params=simulation/params_lnd_patch.json \
    --scenarios=simulation/lab/scenarios/hard-test/ex_000.json \
    --out=/tmp/patched.json
```

Run the same command without `--params` for the stock arm; the two are
the paired comparison. soft_unknown does nothing at all until failures
start arriving unattributed, so with `adaptive_split` dropped both arms
produce identical output on a clean scenario file, by construction.
adaptive_split does change shard choices on a clean file, just to no
measurable effect. To reproduce the exp-021 ladder, add an
`attribution` section to the scenario (`unknown_prob` strips the source
and code, `shift_prob` blames a neighbour, `delay_slices` holds results
back) and pair each level against its own control. The sealed tiers are
checked in under `simulation/lab/scenarios/`; the full method and every
per-file number live in
`simulation/lab/experiments/exp-021-distillation-patch.md`, and the
pathology soft_unknown fixes is documented in
`exp-019-degraded-attribution.md`.
