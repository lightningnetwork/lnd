# Evolved Lightning routers

Three Go files in this directory are complete Lightning routing algorithms
that no human wrote. An LLM-driven evolutionary search produced them by
mutating source code against a payment simulator, scored only by whether
payments settled, how many attempts they took, and what they cost in fees.
All three beat lnd's production pathfinding stack on held-out scenarios,
including a real 12,161-node mainnet graph snapshot.

| file | lines | status |
|---|---|---|
| `router_mx3_generalist_v1.go` | 1,525 | **champion of record**: best generalist, mainnet winner |
| `router_hb1_v1.go` | 872 | hard-regime specialist, ancestor of mx_c3, best on the sealed hard test |
| `router_hb2_v1.go` | 1,166 | archived; dominated by mx_c3, kept for two ideas worth remembering |

Each has a companion `.md` in this directory with a full architecture
walkthrough and a point-by-point comparison against `routing/`. Start with
`router_hb1_v1.md` — it is the smallest complete expression of the paradigm
and its comparison section covers the ground the other two build on.

## Results

Objective = `success − 0.01·min(extra_attempts, 15) − 0.00002·min(fee_ppm, 5000)`.
Every tier below is held out from the run that produced the router.

| tier | lnd stack | hand-written seed | hb1 | hb2 | **mx_c3** |
|---|---|---|---|---|---|
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.545 | 0.583 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | 0.577 | **0.581** |
| mainnet, 12,161 nodes | 0.694 | 0.762 | 0.790 | — | **0.791** |
| average of the three | 0.453 | 0.593 | 0.640 | — | **0.652** |
| drift test (exp-008) | 0.203 | 0.377 | 0.455 | — | **0.457** |

hb2 was retired before the mainnet and drift tiers existed, so it has no
number in those rows. On the two tiers where all four were measured, the
averages are hb1 0.565, hb2 0.561, mx_c3 0.582.

The mainnet row is the one to look at, because success rates there are close
and the gap comes almost entirely from attempts:

| router | objective | success | attempts per payment |
|---|---|---|---|
| lnd production stack (defaults) | 0.694 | 0.790 | 19.8 |
| hand-written seed | 0.762 | 0.820 | 6.1 |
| hb1 | 0.790 | 0.810 | 2.3 |
| mx_c3 | **0.791** | 0.810 | **2.3** |

An 8.6× reduction in attempts at equal success. That is the headline
result.

One earlier finding frames all of it. exp-002 tuned lnd's own parameters —
estimator choice, half-lives, apriori weight, attempt cost, minimum
probability — with the same search machinery and 400 evaluations, and could
not beat lnd's shipped defaults. The paradigm is the lever, not the knobs,
which is why these candidates are whole algorithms rather than
configurations.

## How they came to be

The loop has three pieces.

**The simulator** (`routing/sim_*.go`) runs payments over a synthetic or
real Lightning graph with *hidden* per-channel liquidity and real BOLT
forwarding checks. A router sees only `SimNetworkView` — the public gossip
graph plus a clock — and its own exact local balances, the same information
asymmetry a real sender faces. The view deliberately hides the concrete
graph type so a candidate cannot type-assert its way to the hidden
balances. The router-under-test implements `routing.SimRouter`
(`RequestRoute`, `ReportAttempt`) and owns route selection *and* MPP
splitting end to end. Nothing in the interface presumes Dijkstra, mission
control, or a probability model.

**The evolutionary search** is GEPA, driven by `simulation/run_gepa_code.py`
with `codex:gpt-5.6-sol` as the reflection model. Each iteration hands the
model the current program, its score, and the failure traces it produced,
and asks for a rewrite. Candidates that compile and score better on
minibatches enter a Pareto frontier.

**The evaluator** (`simulation/evaluate_code.py`) compiles each candidate
into the real `routesim` binary with a Go overlay, then runs it against
scenario files. Candidates are rejected outright if they contain
`unsafe`, `reflect`, `os/exec`, `syscall`, `net/http`, `io/ioutil`, or
`"os"`.

Champions are never picked by training score. `summary.json`'s `best_score`
is a per-minibatch metric and is badly inflated (0.9962 for the run that
produced mx_c3). Selection is three-way held-out validation: compile the
candidate via overlay, score it on the sealed test split, the
out-of-distribution corpus, and the mainnet snapshot, against both lnd and
the seed.

The lineage:

```
hand-written seed (384 lines, cmd/routesim/candidate_impl.go)
  └── code_hard1, hard corpus ──> hb1 ─── hb2 (sibling, archived)
        └── code_mix1, mixed corpus, seeded from hb1 ──> mx_c3
```

A third lineage, `code_gen2` (exp-011), started from a small seed with the
discovered insights in the prompt rather than in the code, and converged on
the same paradigm at 0.638 combined. It was not promoted; its best
candidate lives at
`simulation/lab/experiments/exp-011-gen2-best-candidate.go`.

## What they discovered

Three inventions are shared across all champions, and they arrived from
failure traces and a scalar objective alone.

**A bimodal liquidity prior, rediscovered.** Each champion's
`candidatePriorProbability` is an exponential low mode plus a logistic cliff
near capacity — the same two-exponential hypothesis lnd's bimodal estimator
is analytically derived from (`P(x) ~ exp(-x/s) + exp((x-c)/s) + 1/c`).
Nobody put it in the prompt. hb2 went further and rediscovered the
*conditional renormalization* over the known interval that lnd derives from
the Pickhardt et al. formalism.

**Per-directed-channel liquidity intervals.** Instead of a penalty,
`lowerOK` (largest amount proven to pass) and `upperFail` (smallest amount
proven to fail) per `(chanID, from, to)`, plus a point estimate and evidence
counts. Amounts at or above `upperFail` return probability zero; amounts at
or below `lowerOK` return ~0.999. Crucially, a settled HTLC *moves* the
interval down and credits the reverse direction, and in mx_c3 every
observation writes both directions. This is where the attempt reduction
comes from.

**No time logic at all.** Grep the champions for `time.Now`, a half-life, or
a decay constant and you find nothing. lnd's mission control decays
everything — the apriori estimator with a one-hour penalty half-life, the
bimodal estimator decaying success and failure amounts over a week. The
champions dropped mission control and every clock, and replaced recency with
evidence.

Two smaller inventions:

- **Halving-plus splitting.** lnd splits reactively: when pathfinding fails,
  halve the amount. The champions enumerate a shard ladder up front — the
  ceil-division ladder, the halving chain, and in mx_c3 *evidence-derived*
  rungs sized to fit just under bounds they have already proven — price a
  route for each rung, and pick by explicit utility. They can split before
  ever failing.
- **Retry at a lower amount instead of blacklisting.** mx_c3's
  `candidateLowerRetryFactor` is a calibrated six-step answer to "the channel
  refused X; what do I believe about 0.3X?" lnd would blacklist and wait for
  the half-life.

What did *not* evolve: joint route-set planning. Every champion finds one
path per shard independently. Pickhardt-style min-cost flow over a set of
paths remains open (exp-010).

## Running one

The candidate slot is a single file, replaced at build time so the router
under test compiles into the real binary:

```bash
cd $LND_REPO
cat > /tmp/overlay.json <<EOF
{"Replace": {"$PWD/cmd/routesim/candidate_impl.go":
             "$PWD/simulation/champions/router_mx3_generalist_v1.go"}}
EOF
go build -overlay /tmp/overlay.json -o /tmp/routesim_mx3 ./cmd/routesim

# Regenerate a corpus (fixed seeds, so it reproduces exactly).
python3 simulation/gen_scenarios.py --out /tmp/corpus --hard

# Score the candidate, then the lnd baseline on the same scenarios.
/tmp/routesim_mx3 --scenarios /tmp/corpus/test/example_000.json \
    --router=candidate --traces=false
go build -o /tmp/routesim ./cmd/routesim
/tmp/routesim --scenarios /tmp/corpus/test/example_000.json \
    --router=lnd --traces=false
```

The only contract a candidate file must satisfy is a package-level
`newCandidateRouter` matching `routing.SimRouterFactory`;
`cmd/routesim/main.go` hands it to `runner.SetRouterFactory` when
`--router=candidate`.

Two operational notes. Do not edit `routing/` or `cmd/routesim/` while an
evolution run is live, because `evaluate_code.py` recompiles from the tree
on every evaluation. And grep any new candidate for
`GraphSession|LocalBalances|AssignLiquidity|unsafe|reflect` before trusting
its score; the sandbox was sealed in exp-005 and it is worth re-verifying.

## Caveats

These are research artifacts, not patches. The honest limitations, in full,
are in each companion document; the short version:

- **The simulator has known fidelity gaps**, and one of them plausibly
  shaped the central design choice. Until exp-008 there was no virtual clock
  and no background traffic, so nothing moved liquidity between a sender's
  own payments — a world in which time decay can only hurt. The champions'
  zero-time-logic is therefore partly a simulator artifact. exp-008 is
  testing that now: on the drift corpus the champions still win comfortably
  (0.457 and 0.455 against lnd's 0.203), but a router re-evolved under drift
  may well grow decay back.
- **MPP shards settle sequentially.** The runner only counts a part as in
  flight after it settles, so no candidate ever raced its own HTLCs. hb2
  evolved in-flight liquidity reservation anyway and its successors dropped
  it, which is exactly what you would expect.
- **No fee-market dynamics, no non-strict forwarding, no parallel channels,
  one source node per scenario.** The composite objective caps the fee
  penalty at 5,000 ppm and very likely undervalues fees relative to a real
  routing node's preferences.
- **Code size and complexity.** Code evolution hits a wall past roughly 800
  lines. hb2 and mx_c3 are well past it and carry vestigial branches,
  saturating "confidence" latches that do less than their name suggests, and
  many undefended magic constants.
- **Not production code.** The contract is `routing.SimRouter`, not lnd's
  `Router`. There is no persistence, no mission-control namespacing, no RPC
  surface, no belief import/export. Belief state lives in a package-level
  mutex-guarded map that is unbounded and never evicted, where lnd caps
  history at `DefaultMaxMcHistory = 1000`. Nothing here has been reviewed
  against lnd's real router lifecycle or its concurrency requirements.

## Pointers

- `simulation/lab/NOTEBOOK.md` — read this first for the whole story.
- `simulation/lab/DECISIONS.md` — the methodology calls and why.
- `simulation/lab/IDEAS.md` — the open backlog.
- `simulation/lab/experiments/` — exp-001 through exp-011. The ones that
  matter here: `exp-003-seed-router-vs-lnd.md` (the seed beats lnd),
  `exp-005-sim-audit.md` (sandbox sealing), `exp-006-breakthrough.md` (hb1),
  `exp-007-mix-followup.md` (mx_c3, hb2 retired),
  `exp-008-drift-evolution.md` (background traffic),
  `exp-009-mainnet-validation.md` (the mainnet snapshot),
  `exp-011-code-gen2.md` (the independent third lineage).
- `routing/sim_router.go` — the `SimRouter` contract.
- `routing/missioncontrol.go`, `routing/probability_apriori.go`,
  `routing/probability_bimodal.go`, `routing/pathfind.go`,
  `routing/payment_session.go` — what these routers were measured against.
