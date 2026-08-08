# The Interval Router

What is the largest amount this route can still carry?

That question is the whole of the difference between the interval router and
the one lnd has always shipped. The stock router asks whether the graph can
carry an amount that was fixed before path finding began, and when the answer
is no, it halves the amount and asks again. The interval router asks what the
network will accept, and picks the amount and the route together.

This is written for an lnd contributor seeing the algorithm for the first
time. It is off by default.

## What the router remembers

Mission control remembers a penalty. When an attempt fails at some node pair,
it records that pair as a bad bet and lets the record fade on a half life, so
that a channel which was empty an hour ago becomes worth trying again today.

The interval router remembers an amount range instead. For each direction of
each channel it keeps three numbers:

- `LowerOK`, the largest amount it has watched pass. Anything at or below this
  is treated as near certain.
- `UpperFail`, the smallest amount it has watched fail. Anything at or above
  this is treated as impossible.
- `Estimate`, its best guess at the balance, somewhere between the two.

Alongside them it keeps a confidence, which rises as evidence accumulates, and
a classification: whether the channel looks nearly empty in this direction,
nearly full, or neither.

There is no clock anywhere in the model. A bound moves when evidence arrives
and never because time has passed. That is the largest departure from mission
control, and the one that takes the most care to get right, since a belief with
no expiry is a belief that has to be correct.

### Every observation writes both directions

A payment attempt teaches the router three kinds of thing, and each of them
writes the channel it names and also the same channel in reverse.

A **failure** at some hop drops that direction's `UpperFail` to the amount that
was refused. It also raises the reverse direction's `LowerOK`, because
liquidity that is not on this side of the funding output is on the other side.
A channel that cannot send you 400,000 satoshis is a channel that can probably
send 400,000 satoshis back. Mission control makes no such inference; its
two directions of a pair are wholly independent records.

A **probe** is what the router learns about the hops it did not fail at. If an
attempt fails four hops out, the first three hops forwarded, which proves they
could carry what they were handed. That raises their `LowerOK` and lowers the
reverse `UpperFail`.

A **settlement** is different in kind from both, because the money actually
moved. The forward interval slides down by the amount that left and the reverse
interval slides up by the same, so the router's picture of the channel tracks
the payment it just made rather than merely narrowing around it.

### Evidence it does not trust yet

Not every failure says where it happened. An unreadable error, or one no node
claims, leaves several hops on the route that could each have been the one to
refuse. Writing an upper bound on all of them would be a claim of certainty
about channels that may be perfectly healthy, and this model has no way back
from a bound: an amount it calls impossible is never attempted, so the attempt
that would clear the mistake never happens.

Such an observation goes into a quarantine instead, held per directed channel
apart from the bounds. It records the smallest amount an ambiguous failure has
named and how much corroboration stands behind it, where a failure naming two
suspects contributes half as much as one naming a single suspect. Quarantined
evidence prices as a discount on the amount it named and never as an
impossibility. Once enough independent failures agree on the same channel it is
promoted into an ordinary upper bound.

Only a settlement clears a suspicion. That is a narrower rule than it sounds,
and it is the one thing in this section worth understanding. When a hop reports
a failure, the router writes a lower bound on every hop before it, because
forwarding is what carried the payment that far. The inference holds when the
report names the right hop. When blame arrives shifted downstream, which is one
of the ways a real network lies, the guilty channel sits before the reported
index and collects a lower bound saying it carried the amount it had just
refused. A quarantine that accepted lower bounds as proof of innocence would let
that channel out of every suspicion it belonged in, and would pile the blame
onto its innocent neighbours instead. So the router keeps a separate record of
what it has watched actually move, written by nothing but a settlement, and only
that clears a suspicion. The lower bound keeps every other job it has.

### What it believes with no evidence at all

Before any of that, the router needs an opinion about a channel it has never
touched. It assumes liquidity is bimodal: a channel is usually sitting near one
end of its range rather than politely balanced in the middle. So a small amount
is nearly certain to pass, an amount near the whole capacity is nearly certain
to fail, and the transition between the two is narrow.

The width of that transition is a fraction of capacity, not a number of
satoshis. lnd's bimodal estimator takes a scale in millisatoshis, defaulting to
300,000 satoshis, which is 30% of a 1,000,000 satoshi channel and under 2% of a
16,000,000 satoshi one. Expressing the same shape as a percentage of capacity
is what lets one set of constants work on channels that differ in size by
orders of magnitude.

## How it finds a route

The search runs backwards from the destination, the same as `findPath`, and it
reuses the machinery that makes that walk correct: the edge unifier that picks
a policy per node pair, the bandwidth hints that speak for our own channels,
the fee and time lock limits, the onion payload budget, and the feature
validation of every node on the way.

Two things differ.

**The cost of a hop is additive.** The stock router minimizes fee plus a time
lock penalty, divided through by the probability of the route. The interval
router minimizes the negative logarithm of that probability, plus terms for
fee, for depth, and for how much of a channel the payment would fill.
Logarithms turn the product of hop probabilities into a sum, which is what
makes the search below tractable, and it is far gentler on a route the model is
merely unsure about than dividing by a small number is.

**A node keeps several answers, not one.** Dijkstra keeps the single best
distance per node. This search keeps a bounded set of labels that no other
label beats on all three of cost, amount, and hop count at once. It needs to,
because the search runs backwards and fees accumulate as it goes. A route that
is cheaper but carries a larger amount is not comparable to one that is dearer
and carries less, since the larger amount may be refused further upstream. A
single distance per node cannot express that, and the amounts the shard ladder
wants to compare are exactly the ones where it matters.

## How it splits a payment

lnd splits a multi-path payment reactively. `paymentSession.RequestRoute` asks
for the whole remaining amount, and when path finding comes back empty it
halves the amount and tries again, stopping at the minimum shard size or the
part limit. Every split is a response to a failure, and every shard is a
power-of-two fraction.

The interval router plans the split. For one call it builds a ladder of
candidate amounts, finds a route for each, and keeps the pairing of amount and
route with the best score. The ladder draws on four sources:

1. the whole remaining amount, and the smallest shard that could still finish
   the payment inside the parts left;
2. the amounts this payment has already proven do not fit, divided down until
   they do;
3. even divisions of the remaining amount;
4. the halving chain, and small multiples of the smallest usable shard.

Source 2 is the one that makes this more than a reordering of lnd's loop. A
failure at 400,000 satoshis, for instance, immediately puts 199,999 and 99,999
into play, and the halving chain would reach those amounts only by accident.
Because every rung costs a full search, the ladder is capped, and the sources
are enumerated in the order above so that the cap keeps the informative rungs.

Scoring a rung trades the risk of its route against how much of the payment it
would carry, with an appetite for large shards that responds to how the payment
is going: bolder once a part has settled and the payment is committed, more
cautious after several failures.

One shard can get in the next one's way, so the router counts what it is
already holding. If a shard in flight is holding 200,000 satoshis on some
interior channel, a second shard of 300,000 needs that channel to have had room
for 500,000 when the router last looked at it, and that is the amount the model
is asked about. Folding the hold into the amount is the whole adjustment,
because every bound the model keeps already answers the question "was there
this much here". The holds are shared across the node, so a second payment
steps around the corridor a first payment is using instead of learning about
the contention by paying for a failed attempt. Our own channels are left out,
since the switch already subtracts in-flight HTLCs from the bandwidth it
reports for them.

What the router will pay for reliability comes from the budget. The score being
minimized is in nats, so a fee has to be converted into one before it can be
compared against a probability, and the rate of that conversion is the only
thing standing between the search and a fee limit. The routers this design came
from set the rate as a fraction of the amount, which reads as a willingness to
pay a fifth of the payment to raise a route's probability by a factor of e. No
budget anybody would set is near that, so the fee term never bound and those
routers walked into limits they could not see. Here the remaining budget sets
the rate instead: a payment with 10,000 millisatoshis left will pay 5,000 of
them for one nat, so it declines expensive reliability on its own rather than
finding out when the route is refused. The rate is absolute, which means it
tightens in relative terms as the payment grows, and that is the direction a
budget quoted in parts per million needs. A payment with no budget keeps the
old amount relative rate, since there is nothing else to derive one from.

Two smaller rules follow from the same concern. A node always keeps the
cheapest of its labels whatever that label's score, so a payment that cannot
afford the reliable routes still finds one it can. And no route is ever handed
out whose fee exceeds what is left of the budget.

The payment lifecycle is untouched by any of this. It still asks for one route
at a time and dispatches one HTLC, or hash time locked contract, at a time. The
shard size simply rides back on the route, since `registerAttempt` already
reads `ReceiverAmt()` to decide whether a shard is the last one.

## A payment, end to end

The pieces above are easiest to see moving together. Suppose a node with the
router enabled sends 500,000 satoshis to a destination four hops away, over
channels it has never used, and suppose the true bottleneck is the third hop,
which holds 250,000.

The first route request builds the shard ladder. Nothing has failed yet, so
the ladder's informative rungs are the whole amount and a handful of
divisions; the whole amount scores best, and the search returns a four hop
route for 500,000. The attempt fails at hop three, and the failure names it.

Three things are written before the next request. Hop three's forward
direction takes `UpperFail = 500,000`: not a penalty, a ceiling. Hops one and
two forwarded, so their forward directions take `LowerOK = 500,000` and their
reverse directions learn the complementary bound. And hop three's reverse
direction takes `LowerOK = 500,000`, because the liquidity that was not there
to send is there to send back.

The second route request now behaves differently in two ways at once. The
ladder contains 499,999 divided down: 249,999 and 166,666 are rungs, put
there directly by the failure, not reached by halving. And the search still
considers hop three for those smaller amounts, because a 250,000 shard sits
below no bound the model holds; the stock router would be routing around that
channel entirely, at every amount, for the length of a half-life. Say the
249,999 rung wins with the same route. It succeeds, the destination still
needs the rest, and the settlement slides hop three's forward interval down by
what just moved through it.

The remaining 250,001 goes out on the next request the same way, over that
route or a better one, and the payment completes with three attempts. Every
bound written along the way outlives the payment: the next payment through
this corridor starts from what this one learned, and what it learned survives
a restart on nodes running the native SQL backend, in the clamped form the
limitations section describes.

The same trace under the stock router reads differently at each step: the
failure penalizes the pair both ways, the retry halves 500,000 to 250,000 by
schedule rather than by evidence, and whether hop three is even considered
again depends on how much of its penalty has decayed, which is a function of
wall-clock time and not of anything the network said.

## Living alongside mission control

Turning the interval router on does not turn mission control off. Mission
control keeps running, keeps its history, keeps answering
`QueryProbability` and the rest of its remote procedure calls, and keeps
deciding whether a given failure is terminal for the payment. Only the choice
of route changes.

Every attempt outcome therefore reaches two places. The payment lifecycle
reports it to mission control exactly as before, and then offers it to the
payment session through a small optional interface, `PaymentResultReporter`.
The stock session does not implement that interface, so with the flag off the
type assertion fails and nothing changes.

The interval beliefs live in an `IntervalStore`, one per node and shared by
every payment, so what one payment learns is there for the next. On
a node running the native SQL backend the store also writes its beliefs down
and reads them back at startup. Elsewhere it is memory only and the router
starts cold after a restart.

## Persistence and restarts

The store holds at most 10,000 directed-channel entries in memory
(`DefaultMaxIntervalHistory`), evicting the least recently written when full,
so its footprint is bounded no matter how long the node runs or how large the
graph grows.

On a node running the native SQL backend, the store also writes its beliefs
down. Writes are batched: a dirty entry waits at most the flush interval,
one second by default, before it reaches the `liquidity_intervals` table the
branch's migration adds. The interval is a config knob
(`routerrpc.intervalflushinterval`) because the right cadence is a judgment
about the node: a busy router may prefer a longer interval to cut write
amplification, and the cost of a longer interval is only the beliefs learned
in the final unflushed seconds before an unclean shutdown.

At startup the store reads the table back and clamps everything it finds, as
the limitations section describes: restored bounds say likely and unlikely
rather than proven and impossible, and confidence is halved. Two kinds of
in-memory evidence are deliberately never written down. The quarantine is
not, because a suspicion restored from disk is one that nothing since could
have cleared. The settlement record that clears suspicions is not, because a
settlement from before a restart should not vouch for a channel today. Both
rules are the same instinct: fresh evidence outranks stored evidence, and
stored evidence never gets to overrule a live observation.

On the kv backends there is no table to write to, so the router simply starts
cold after a restart. Nothing else changes.

## Turning it on

```
[routerrpc]
routerrpc.router=interval
```

The other value is `default`, the stock stack, and it is what a node that says
nothing gets. With the flag off, none of the code described here is even
constructed: the server builds the same session source it always did, and the
result-reporting seam is a type assertion the stock session does not satisfy.

The full config surface:

| Option | Default | What it does |
|---|---|---|
| `routerrpc.router` | `default` | selects the routing engine |
| `routerrpc.intervalflushinterval` | `1s` | belief write-back cadence |

`routerrpc.router=interval` enables everything this document describes. The
flush interval is how long a changed belief may wait before it is written to
the database, and it is only meaningful with the flag on and the native SQL
backend.

One further switch lives in code rather than in the config file:
`IntervalConfig.DisableQuarantine` turns off the quarantine for ambiguous
failures while leaving every other mechanism in place. It exists so that the
one component validated only in simulation can be severed without touching
anything else.

Turning the router off again is safe at any time. The stored beliefs remain
in the database and are simply not read; mission control's history was being
maintained the whole time, so the stock router resumes exactly where it would
have been.

## Limitations

**Payments to blinded paths fall back to the stock session.** Inside a blinded
path there is no channel for the model to key a belief on: the hops are opaque,
the intermediate amounts and expiries are deliberately zero, and an error from
inside the path arrives as `invalid_onion_blinding` from the introduction node,
which names nothing further in. Intervals on the visible prefix up to the
introduction node would work, since those hops are ordinary channels and a
failure past them proves they forwarded. Getting there also means teaching the
search about the dummy hop appended to blinded routes and about targeting the
nothing-up-my-sleeve (NUMS) key rather than the destination, so for now these
payments are handed to the router that already gets them right.

**A restored bound is softer than a fresh one.** This model has no way back
from a wrong `UpperFail`: an amount the model calls impossible is never
attempted, and an attempt is the only thing that could correct the bound. That
is the right trade while the evidence is fresh and ours. It is a trapdoor for a
bound loaded from disk, because the network that bound describes has had every
restart and every rebalance since to move on. So a restored belief is clamped:
its upper bound says unlikely rather than impossible, its lower bound says
likely rather than proven, and its confidence is halved. The first fresh
observation clears all of it. Clamping is what makes keeping these beliefs
better than throwing them away at startup.

**An ambiguous failure is recorded against the node pair.** The model wants to
key on a directed channel, because the quantity it tracks is the balance on one
side of one funding output. Under non-strict forwarding that is not always what
the evidence supports: a node asked to forward over one channel may use any
channel it has to the same peer, and the onion failure names neither. So when
the graph shows more than one channel between a pair, the observation is
written about the pair instead, at the granularity mission control has always
used. Pairs with a single channel, which is most of them, keep the full
resolution.

**The quarantine is validated in simulation only.** Promotion after enough
agreement, and clearing on contradiction, come from a router bred against a
channel that lies about where failures happen. That router produced the flattest
degradation profile the work has measured, but it bought the flatness partly by
never giving up, and none of it has been measured on a real network. The
quarantine is held in memory only, as is the record of settlements that clears
it, since neither survives a restart with its meaning intact. Nor has it been
shown to be worth anything: on freshly generated files its effect on the
objective cannot be separated from zero in either direction, and it is kept
because the trust boundary it draws is the right one rather than because it
measurably pays. It can be switched off with `DisableQuarantine` without
touching anything else, and the router then handles an unattributable failure
entirely within the payment as it did before.

One promotion case is knowingly left on the floor. When a probe derived lower
bound lands at exactly the amount an ambiguous failure names, a promoted bound
is written and then dropped again by the ordinary rule that a lower and an upper
bound at the same amount cannot both stand. The suspicion is still held and
still priced until then. Changing that rule would reach outside the quarantine
into bound maintenance, so it stays as it is.

**A resumed payment's HTLCs are not counted as holds.** After a restart the
router knows a payment has attempts in flight, because the payments database
says so, but it did not choose their routes and so cannot say which interior
channels they sit on. Those shards are priced as though nothing were held,
which is where the router was before this accounting existed.

**Searching costs more than Dijkstra does.** Every rung of the shard ladder
runs its own search, and each search may keep up to two dozen labels per node.
The graph reads are shared across the ladder and the search is bounded on hops,
labels, and total expansions, but the worst case is still well above one
shortest path query. This is the main reason the router is off by default.

**The constants were selected, not derived.** The probability model, the retry
ladder, and the scoring weights come from an evolutionary search against a
payment simulator, scored on a real 12,000 node mainnet graph snapshot and on
synthetic topologies. They are documented for what they do rather than for why
those particular numbers are right, because for most of them nobody can say.
Some are surely fitted to the simulator that produced them.

## Where the design came from

The design was found by search rather than invented. We built a payment
simulator with hidden per-channel balances and real forwarding checks, then
ran an evolutionary search over whole routing algorithms, scored purely on
payment outcomes, with lnd's production stack as the baseline. Across dozens
of independent runs, every winning candidate converged on the same three
decisions: drop the per-pair penalties, drop time decay, keep per-direction
liquidity intervals. The code in this branch is a hand-written distillation
of that consensus into lnd's real payment lifecycle, hardened by several
rounds of adversarial benchmarking that each found and fixed a real bug
before the branch was called done.

That history cuts both ways, and the limitations above say where. The
mechanisms transferred to every world the simulator could build, including a
channel graph generated outside this work entirely; the constants are the
part that may be shaped by the simulator that selected them.

## Where the code lives

| File | What is in it |
|---|---|
| `routing/interval_belief.go` | the interval, the quarantine, the model |
| `routing/interval_store.go` | the node wide store and its flushing |
| `routing/interval_store_sql.go` | the durable backing |
| `routing/interval_pathfind.go` | the label setting search |
| `routing/interval_session.go` | the shard ladder and the session state |
| `routing/interval_session_source.go` | session construction and the fallback |
| `routing/interval_config.go` | the search bounds and their defaults |
