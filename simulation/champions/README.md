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

| tier | lnd stack | hand-written seed | hb1 | hb2 | **mx_c3** | drift1 |
|---|---|---|---|---|---|---|
| hard sealed test | 0.309 | 0.530 | **0.586** | 0.545 | 0.583 | 0.580 |
| OOD corpus-v2 test | 0.357 | 0.487 | 0.545 | 0.577 | **0.581** | 0.544 |
| mainnet, 12,161 nodes | 0.694 | 0.762 | 0.790 | — | **0.791** | 0.790 |
| average of the three | 0.453 | 0.593 | 0.640 | — | **0.652** | 0.638 |
| drift test (exp-008) | 0.203 | 0.377 | 0.455 | — | **0.457** | 0.417 |

hb2 was retired before the mainnet and drift tiers existed, so it has no
number in those rows. On the two tiers where all four were measured, the
averages are hb1 0.565, hb2 0.561, mx_c3 0.582.

The last column is not a champion and does not live in this directory. It is
the winner of exp-008's `code_drift1` run, the only evolved router with
time-based logic, kept for comparison at
`simulation/lab/experiments/exp-008-drift1-best-candidate.go`. Read the
drift-test row across: the time-aware router bred *on* the drift corpus
scores 0.417 there, below every champion and below the time-less gen2's
0.456.

The mainnet row is the one to look at, because success rates there are close
and the gap comes almost entirely from attempts:

| router | objective | success | attempts per payment |
|---|---|---|---|
| lnd production stack (defaults) | 0.694 | 0.790 | 19.8 |
| hand-written seed | 0.762 | 0.820 | 6.1 |
| hb1 | 0.790 | 0.810 | 2.3 |
| mx_c3 | **0.791** | 0.810 | **2.3** |
| drift1 (exp-008, not promoted) | 0.790 | 0.810 | 2.4 |

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
the seed. The tier set has only grown since: exp-008 added the drift
corpus, and exp-010 added the two splitting tiers and paired statistics
(bootstrap 95% CIs and sign tests against mx_c3 as baseline), so a
challenger now faces a five-tier sweep — split-val, split-test, sealed hard
test, OOD corpus-v2, mainnet — before anyone calls it a champion.

exp-010 is the hardest that selection has been pushed. It built a corridors
corpus where unequal splitting is mandatory by construction, then ran three
independent proposer lineages against it: codex/gpt-5.6-sol, Opus 5 at
default reasoning effort, and Opus 5 at medium effort. All three evolved
joint route-set planning, in increasing depth, and none of them beat hb1 or
mx_c3 on the five-tier sweep. The deepest of them came the closest anything
ever has — a statistical tie with mx_c3 on the corpus's own validation tier
(+0.005 at p=.07, with a higher raw success rate) — and then collapsed off
that corpus, scoring 0.303 on the sealed hard test against mx_c3's 0.583.
The champions held because they generalize, which is the property the
five-tier sweep exists to measure. The law this sharpens is the one the
program keeps rediscovering: environments elicit mechanisms, budgets decide
champions, and proposer strength moves a candidate along the
specialist–generalist axis rather than lifting the whole curve. The same
sweep also settled an old informal claim, that hb1 and mx_c3 are
genuinely indistinguishable on mainnet (paired delta −0.000).

The lineage:

```
hand-written seed (384 lines, cmd/routesim/candidate_impl.go)
  ├── code_hard1, hard corpus ──> hb1 ─── hb2 (sibling, archived)
  │     └── code_mix1, mixed corpus, seeded from hb1 ──> mx_c3
  └── small seed + insights in the prompt, not in the code
        ├── code_gen2,   mixed corpus ──> gen2   (not promoted)
        └── code_drift1, drift corpus ──> drift1 (not promoted; the only
              router with time logic, and it still loses on drift)
```

A third lineage, `code_gen2` (exp-011), started from a small seed with the
discovered insights in the prompt rather than in the code, and converged on
the same paradigm at 0.638 combined. It was not promoted; its best
candidate lives at
`simulation/lab/experiments/exp-011-gen2-best-candidate.go`.

A fourth lineage, `code_drift1` (exp-008), used the same small seed and the
same insights prompt but ran on the drift corpus, where a virtual clock and
exogenous background traffic move hidden liquidity between the router's own
payments. It produced the first evolved router with time-based logic — a
35-minute confidence half-life, hard bounds that expire at 20 minutes, and
edge probability interpolated between aging evidence and the prior — and
that router loses to the time-less champions on all four tiers, drift
included. Not promoted either; the source and a full walkthrough are at
`simulation/lab/experiments/exp-008-drift1-best-candidate.go` and its
companion `.md`.

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
evidence. exp-008 later put that choice under genuine staleness pressure and
it held; see the first caveat below.

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

What did *not* evolve on its own: joint route-set planning. Every champion
finds one path per shard independently, and Pickhardt-style min-cost flow
over a set of paths never appeared under any corpus they were bred on.
exp-010 built an environment that demanded it — parallel corridors of
deliberately unequal capacity, where halving an above-tier payment yields
shards only the fattest corridor can carry — and joint planning duly
emerged, from all three proposer lineages, in three depths: one-step
lookahead with reservation, up-front corridor-sized shard sets, and
persistent residual-aware flow plans that survive failures. None of them
carried off their home corpus, so the champions are still single-path;
see `simulation/lab/experiments/exp-010-splitting-pressure.md`.

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

- **The simulator has known fidelity gaps**, though the one that looked most
  threatening has now been closed. Until exp-008 there was no virtual clock
  and no background traffic, so nothing moved liquidity between a sender's
  own payments — a world in which time decay can only hurt, which made the
  champions' zero-time-logic look like a simulator artifact. exp-008 built
  the clock and the traffic and re-ran evolution on the drift corpus. Time
  awareness did re-evolve, in a form nobody prompted for: a 35-minute
  confidence half-life, hard bounds expiring at 20 minutes, and edge
  probability interpolated as `conf·learned + (1−conf)·prior` so aging
  evidence slides back toward the bimodal prior. It lost anyway, on every
  tier including drift itself — 0.417 against the champions' 0.455 and
  0.457, and against the time-less gen2's 0.456. At this level of churn a
  stale hard bound costs about one retry, which is cheaper than what decay
  throws away, so the champions' timelessness is a validated design property
  rather than an artifact. The residual caveat is real but narrow: one drift
  intensity (ten-minute gaps, roughly `num_nodes/10` background payments per
  gap), one traffic model (naive fee-optimizing senders), and a 400-eval
  budget against champions bred across two runs and 900 evaluations.
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
  `exp-008-drift-evolution.md` (background traffic, and the verdict on time
  decay), `exp-008-drift1-best-candidate.md` (the time-aware router itself,
  walked through like a champion),
  `exp-009-mainnet-validation.md` (the mainnet snapshot),
  `exp-010-splitting-pressure.md` (joint route-set planning, elicited from
  three proposer lineages and beaten by the champions anyway),
  `exp-011-code-gen2.md` (the independent third lineage).
- `routing/sim_router.go` — the `SimRouter` contract.
- `routing/missioncontrol.go`, `routing/probability_apriori.go`,
  `routing/probability_bimodal.go`, `routing/pathfind.go`,
  `routing/payment_session.go` — what these routers were measured against.
