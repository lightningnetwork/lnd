# EXP-014 — The traffic engine was five times weaker than configured

**Date:** 2026-07-26
**Status:** complete — infrastructure fix, no published result overturned.

## Why this ran

exp-012's staleness arm came back null, and before believing it we ran
a manipulation check: scale the traffic up and confirm the knob does
something. It did — but the check also showed that only ~18% of
background payments were settling. A failed background payment moves
no liquidity at all, so the settle rate is exactly the factor between
the churn a scenario file asks for and the churn it gets. Every
experiment that turned the traffic knob was measuring a much calmer
network than it claimed to: exp-008's drift question, exp-010b's
atomic arena, exp-012's staleness arm.

This was the top item in CLAUDE.md's priority list and the
prerequisite for any honest drift or staleness claim.

## Three causes, only one of which explains mainnet

Measured settle rates before the fix:

| corpus | settle rate |
|---|---|
| corpus-drift/test | 0.41 |
| corpus-splitatomic/test | 0.61 |
| mainnet staleness set | **0.18** |

**1. The route search ignored hidden liquidity.** `trafficEdgeUsable`
filtered on policy and capacity only, so the search happily picked
corridors that a bimodal balance distribution cannot fund. The search
now consults the balance. This is the environment's privilege rather
than a player's: the traffic engine *is* the network, and what a
candidate can see through the sealed gossip view is unchanged.

**2. The amount was drawn blind and never revisited.** A payment
larger than any available corridor simply died after three attempts at
the same size. A sender now halves its amount and searches again, down
to a floor of 5% of the draw, the way a real sender falls back to a
smaller transfer or a split.

**3. Endpoints were drawn uniformly.** This is the one that matters,
and it is badly wrong on a real topology. The mainnet snapshot has a
**median degree of 1**, and **68% of its nodes hold two channels or
fewer**. Uniform sampling therefore drew leaf-to-leaf pairs almost
every time, and a leaf whose single channel has its balance on the
peer's side cannot send at all. Fixes 1 and 2 do nothing for this —
they moved mainnet from 0.177 to 0.184 — because no amount of
shrinking finds a path that does not exist. Drawing endpoints in
proportion to degree moved it to **0.951**.

## Result

| corpus | before | after |
|---|---|---|
| corpus-drift/test | 0.41 | **0.69** |
| corpus-splitatomic/test | 0.61 | **0.89** |
| mainnet staleness set | 0.18 | **0.95** |

Also added `focus_fraction`: the share of background payments that
take one endpoint from the scenario's own source and targets. Churn
spread evenly over a 12,161-node graph almost never touches the
handful of channels a scored payment uses, so without it the traffic
knob moves the network everywhere except where it is being measured.
Generated corpora set it to 0.33.

## Does it overturn anything? No.

Re-ran the champions over both traffic-bearing tiers with the old and
new engines, same corpora, same seeds:

| tier | router | before | after |
|---|---|---|---|
| drift_test | lnd | 0.203 | 0.235 |
| | hb1 | 0.455 | 0.442 |
| | mx_c3 | 0.457 | 0.441 |
| | atomic1 | 0.319 | 0.311 |
| atomic_test | lnd | 0.338 | 0.321 |
| | hb1 | 0.444 | 0.440 |
| | mx_c3 | 0.444 | **0.460** |
| | atomic1 | 0.400 | 0.418 |

Every ordering survives and every router stays inside its old
confidence interval. The one directional hint worth remembering: on
the drift tier the stronger churn helps lnd (+0.032) and slightly
hurts all three interval routers, which is what you would predict if
harder bounds go stale faster under real movement. It is far from
significant at n=8 and is a hypothesis for the re-run of exp-008, not
a finding.

So the honest statement is narrow: **the fix does not change a
published result, it means the next drift and staleness experiments
will measure the network their scenario files describe.** exp-008's
"time-decay loses to hard bounds even under drift" and exp-010b's
atomic arena both stand as written, with their weak-churn caveat now
removed for future runs rather than retroactively.

## Companion change: the give-up rate

exp-013 evolved a candidate that improved its attempt count by
quitting on payments it could have completed, and the composite
objective could not tell that apart from genuine efficiency — both
show up as fewer attempts. `SimScenarioResult.GaveUp` now records that
the router terminated the payment itself rather than exhausting the
attempt cap, and the aggregate reports `num_give_ups` and
`give_up_rate`. Nothing scores it. It exists so the next candidate
cannot hide in the same place.

The aggregate also now reports `bg_settle_rate` directly, so the
defect this experiment fixed is visible in every run's output instead
of requiring a manipulation check to notice.
