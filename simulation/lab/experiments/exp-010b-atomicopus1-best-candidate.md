# atomicopus1 — the right architecture, the wrong economy

`exp-010b-atomicopus1-best-candidate.go` (987 lines) is the winner of the
`code_atomic_opus1` run: 400 evaluations of Opus 5 at default reasoning
effort, on the atomic corridors corpus (`corpus-splitatomic`, seed 6061),
seeded from the small in-tree router with the arena's economics in the
background prompt. It is the instructive failure of exp-010b. The run itself
was flawless — 51 iterations, zero degraded reflections, the first fully
sealed run in the program — and it re-evolved exactly the mechanism family the
arena was built to elicit. Then it lost every tier, including the one it was
bred on.

| tier | mx_c3 | atomic1 (codex arm) | **atomicopus1** |
|---|---|---|---|
| atomic val | **0.442** | 0.426 | 0.374 (−0.067, p=.29) |
| atomic test | **0.444** | 0.400 | 0.391 (−0.053, p=.008) |
| corridors split-test | **0.876** | 0.825 | 0.711 (−0.165, p=.008) |
| hard sealed test | **0.479** | 0.417 | 0.247 (−0.232, p=.109) |
| OOD corpus-v2 | **0.581** | 0.544 | 0.367 (−0.214, p=.109) |
| mainnet, 12,161 nodes | **0.791** | 0.790 | 0.738 (−0.053, p=.18) |

All routers were rebuilt on the current tree for this sweep and the scratch
legacy corpora were regenerated after a reboot, so read deltas within the
table, not levels against older writeups.

## What it built

The file's header comment is unusually candid about its own design, and it is
accurate:

> Joint planning is genuinely min-cost-flow-ish: candidate corridors are
> enumerated once per plan with per-edge reservations, and shard sizes come
> from believed edge capacity rather than blind halving. Corridors are
> excluded by their whole edge set (not just the bottleneck), so shards do not
> silently contend.

`planRouteSet` delivers on that. Per part it searches for a route carrying the
whole residue, sizes the shard to `min(delivered, believed bottleneck,
remaining)`, rebuilds the route at that amount through `trimTo` — which
re-verifies every hop's minHTLC and maxHTLC at the amount that hop will
actually carry, a check nothing else in the project performs — reserves it,
and then bans every non-local hop it used:

```go
if e.from == r.source && r.availLocal(e) >= minShard {
	// Local channel with headroom left: reusable.
	continue
}
avoid[r.key(e)] = true
```

Reservations live inside the belief (`b.inFlight`) and enter the probability
model as `eff = amt + b.inFlight`, so a corridor is priced against the sum of
what it already holds for us and what we are about to ask of it. Planning
reservations are rolled back on return; the caller reserves for real when it
dispatches. `edgeCapacityGuess` supplies the shard sizes, betting 70% of
capacity on an unknown bimodal channel, capped at half of any proven failure
amount and floored at any proven success.

This is the mechanism exp-010b named as its target, arrived at from a small
seed in 400 evaluations. Criterion 2 of the experiment asked whether evolution
on the honest arena would produce an up-front planner. It did, twice.

## The novel part, bred by drift

One mechanism in this file is new to the family and appears nowhere else in
the project: **hard bounds relax when the whole plan keeps failing.**

```go
if b.hasFail && eff >= b.upperFail {
	// Not a permanent veto: repeated whole-payment stalls and a
	// moving network mean an old bound may be stale. Give a tiny
	// but non-zero chance that grows with dry rounds, so search
	// can re-probe rather than declaring the graph unroutable.
	if r.dryRounds >= 2 && eff < e.capacity {
		return probFloor * float64(r.dryRounds)
	}
	return probKnownBad
}
```

`dryRounds` counts consecutive failed attempts and resets the moment anything
settles or the remaining amount moves. So after two dry attempts, amounts
above a proven failure bound stop being vetoed and start being priced at
`0.005 × dryRounds`, rising with the streak. It is a staleness model with no
clock in it, driven by the router's own frustration rather than by elapsed
time — the same problem exp-008's `drift1` solved with a 35-minute half-life,
solved here without reading `view.Now()` at all. A second, cleaner
drift-tolerance mechanism sits alongside it: `markOK` clears an `upperFail`
outright when a success lands at or above it.

The idea is good. Its implementation is a positive feedback loop.

## Why it lost

Relaxing a bound makes an exhausted corridor routable again. Routing into an
exhausted corridor fails. A failure increments `dryRounds`, which relaxes the
bounds further, which makes more exhausted corridors routable. The loop runs
until the attempt budget stops it, and the budget is generous:
`maxAttemptsBase = 48`, plus four per allowed part.

The result is legible in one column: **57.5 attempts per payment on
atomic-test**, against mx_c3's 12.6 and exp-010 opus1's 23.5. The objective
charges 0.01 per extra attempt up to fifteen, so nearly every payment forfeits
the full 0.15 cap, and in this arena the attempts cost more than their
penalty — each one advances background traffic by 30 virtual seconds, so the
re-probing degrades the very network it is re-probing, and each held shard
keeps its liquidity locked while the ladder runs. The router bought
drift-tolerance and paid for it in the only currency the arena taxes.

Two inherited constants make the off-corpus collapse worse. `maxHops = 6` is
the same corridor-shaped mistake as exp-010 opus1's `maxRouteHops = 7`, on a
hard corpus whose successful routes run 9 to 23 hops — the exp-010 follow-up
measured that one constant at about half the hard-test gap, and here the hard
test lands at 0.247. And `minShard = 500_000` msat combined with whole-edge-set
exclusion exhausts the corridor supply quickly on sparse graphs, after which
the router falls through to an eight-step halving fallback that is strictly
worse than the planner it replaced.

The plan does not persist either. Any failure sets `r.plan = nil`, and so does
any progress in the remaining amount. Neither arm of exp-010b re-evolved the
persistence that exp-010's opus1 had discovered, which is worth noting given
that the arena was designed to reward it.

## What it is for

Read this file for two things. The planner is the cleanest expression of
min-cost-flow-style route-set construction the project has produced —
enumerate corridors once, size each shard to its believed bottleneck, trim
legally, exclude the whole edge set, and reconcile reservations against
beliefs. And the bound-relaxation valve is a genuinely new answer to
staleness, one that a future candidate should take with a governor on it: cap
the relaxation, or charge the re-probe against a separate budget, so
tolerance of drift cannot convert into unbounded attempt burn.

The one-line verdict: evolution polished the right architecture into the wrong
economy. That is a more useful failure than a bad architecture would have
been, because the fix is a bounded one and the mechanism is worth fixing.

## See also

- `exp-010b-atomic-splitting.md` — the arena, the baseline, and both verdicts.
- `exp-010b-atomic1-best-candidate.md` — the codex arm's hybrid winner, the
  first challenger with no collapse tier.
- `exp-010-opus1-best-candidate.md` — the same proposer on the static
  corridors corpus, and the persistent plan neither atomic arm rebuilt.
- `exp-008-drift1-best-candidate.md` — the other clock-free-versus-clocked
  staleness experiment, and the verdict on time decay.
