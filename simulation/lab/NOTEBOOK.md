# Lab Notebook — LN Routing Evolution

Running log of the GEPA × lnd pathfinding project. Newest entries at the
bottom. Detailed experiment writeups live in `experiments/`.

For the *why* rather than the chronology — every design decision put
side by side against lnd's production code, with the causal story for
each measured difference and three corrections to claims made below —
read `WHY.md`.

---

## 2026-07-24 — Project kickoff, evaluator built, loop validated

**Goal set by roasbeef:** create the next generation of LN routing
algorithm, using a payment simulator (improved as far as needed) and GEPA
as the guide. Explicitly *not* tied to the current Dijkstra + mission
control paradigm. Dijkstrasden (bitromortac's virtual LN, PR #3) reviewed
and mined for ideas — describegraph-loaded graphs, liquidity models,
observer traces, scenario sweeps — but not adopted as code: its portal
architecture (patched lnd, gossip injection, itests) is built for
implementation testing, too heavy and nondeterministic for an optimizer's
inner loop.

**Built today:**
- `routing/sim_*.go` simulator: real lnd pathfinding + mission control
  against hidden liquidity, per-direction policies, real forwarding error
  semantics. Caught an off-by-one porting the mock graph: the amount over
  channel i is `Hops[i-1].AmtToForward`, not `Hops[i]` — until fixed,
  every intermediate hop failed with FeeInsufficient.
- `cmd/routesim` CLI (~0.3s per 10-payment batch on 200-node nets).
- Scenario corpus generator, evaluator, GEPA runners (parameter mode and
  code mode), codex-CLI reflection wrapper (gpt-5.6-sol headless).
- `SimRouter` paradigm-free interface + candidate slot with overlay
  builds; seed = simple cheapest-path router with failure blacklisting.

**Experiments:** see `experiments/exp-001-param-smoke.md` (smoke run,
budget starvation diagnosed) and `experiments/exp-002-param-run1.md`
(400-eval parameter run, in flight).

**Headline early finding:** on a hard bimodal-liquidity example, the
*naive* 300-line seed router beat the full lnd stack — 33% vs 22% success
at 7.8 vs 112.6 attempts/payment. The lnd defaults burn huge retry budgets
in bimodal small-channel regimes. Detail in
`experiments/exp-003-seed-router-vs-lnd.md`.

**Infra notes:** PyPI `gepa` 0.1.4 lags the repo API — install from git
main. GEPA's budget arithmetic: each candidate costs ~(minibatch +
periodic full-val) evals; 60 evals only bought 7 proposals on the smoke
corpus. Score shaping matters: unbounded attempt penalties (down to −2)
drowned success-rate deltas; penalties now saturate at −0.25 total.

---

## 2026-07-24 (night) — run1 verdict, corpus v2, overnight evolution runs

- **run1 (params, 400 evals): seed survived.** No knob change beat lnd
  defaults on val aggregate; bimodal specialists won individual examples
  only. Paradigm > knobs (exp-002). Real lineage now on the dashboard;
  Litbucket v2 published (https://lnd-routing-command-center.lightning.wiki/).
- **Corpus v2** adds Barabási-Albert scale-free nets (800/1500 nodes,
  log-normal capacities). Baseline: lnd near-parity on scale-free
  (0.819 vs 0.860) but still behind overall (0.559 vs 0.681) — exp-003.
- **Overnight runs in flight:** code1 (adaptive gepa↔meta_harness,
  corpus v1, 240-eval pool) and omni1 (blog recipe via optimize_best_of:
  gepa+meta_harness explore 100 evals each → fresh gepa continues 160,
  corpus v2). Preflight passed; reflection = codex/gpt-5.6-sol,
  meta_harness = claude CLI.
- **Farmed out:** Fable code-reviewer auditing the simulator's BOLT
  forwarding semantics + reward-hack surfaces (report-only, results land
  in a later entry). Cron agent refreshes the dashboard every 30 min.

## 2026-07-24 (night) — BREAKTHROUGH: evolved router beats lnd + seed

`code_hard1` (pure gepa, gpt-5.6-sol) evolved an 872-line router that
**wins the sealed test set and generalizes OOD**:
- hard sealed test: hb1 0.586 > seed 0.530 > lnd 0.309 (objective)
- OOD corpus-v2 test: hb1 0.545 > seed 0.487 > lnd 0.357
- ~9 attempts/payment vs lnd's ~50, higher success on both.

What it built: an explicit **bimodal liquidity prior** + per-edge
liquidity bounds with confidence + risk-adjusted Dijkstra. Clean (no
exploit). **Correction (2026-07-26, WHY.md §0):** this entry
originally read "discovered from failure traces alone." That is
wrong — the harness prompt has stated the bimodal hypothesis under
"environment truths" since the first committed version. The prior's
shape and constants and the whole interval apparatus were the run's
own work; the hypothesis was handed to it. Full detail + caveats: exp-006. Champion saved to
`champions/router_hb1_v1.go`. Sim audit (exp-005) fixed a critical
sandbox escape before it was exploited.

**Update (code_mix1 follow-up):** continuing evolution from hb1 on a
mixed corpus produced **mx_c3** (`champions/router_mx3_generalist_v1.go`),
the best generalist: it dominates hb2, ties hb1 on the hard test (0.583
vs 0.586), wins OOD (0.581 vs 0.545), and has the best combined average
(0.582). Champions of record: hb1 (hard specialist) + mx_c3 (generalist).
Detail: exp-007.

## 2026-07-24 (day) — MAINNET VALIDATION: champions win on the real graph

Real 12,161-node mainnet snapshot, 100 payments, highest-degree source:
mx_c3 0.791 / hb1 0.790 vs lnd 0.694 objective — comparable success
(0.81 vs 0.79) at **8.6× fewer attempts** (2.3 vs 19.8/payment). The
synthetic-bred champions generalize to lnd's home turf. exp-009.

## 2026-07-24 (evening) — code_gen2: insight transfer works, same ceiling

The small-seed + insights-in-prompt run finished (400/400 evals, 31
accepts — ~4× code_mix1's acceptance rate). Its best candidate reaches
champion-class performance on all three held-out tiers (combined 0.638
vs mx_c3's 0.652, hb1's 0.640; mainnet 0.787 at the same 2.3
attempts/payment) but does NOT pass the champions. Three independent
lineages now converge on the same paradigm and the same performance
band: the interval-belief design looks like a local optimum *for these
environments*. gen2 did evolve two novel mechanisms (in-flight local
liquidity reservation; weakest-edge failure attribution) that the sim
never rewards — more eval budget in the same regime buys nothing.
Champions of record stay hb1 + mx_c3; the next lever is changing the
environment (exp-008 background traffic, exp-010 splitting pressure).
Detail: exp-011.

## 2026-07-24 (night) — exp-008 begins: the sim gets a clock and traffic

Built and committed (d11a20dcb) the fidelity upgrade: virtual clock
(MC decay now operates over simulated time; candidates read
view.Now()) + seeded background traffic (naive fee-optimizing senders
move hidden liquidity in every gap; per-channel conservation; same
seed → same exogenous process for all routers). Baseline on the new
drift corpus: **the champions' hard bounds do NOT collapse** (hb1/mx_c3
~0.46 vs lnd 0.20 on drift-test) and **lnd's decay does not close the
gap** even now that it operates. But everyone lost ground vs static
(champions ~0.59 → ~0.42), so drift created real headroom. Evolution
run `code_drift1` (400 evals, drift-neutral prompt) is live. exp-008.

## 2026-07-25 (morning) — exp-010 codex arm: joint planning emerges, loses

code_split2 (400/400, clean, on the hardened CodexLM after code_split1
was killed for instruction-leakage hijack) produced the first evolved
router that plans route SETS: unequal split candidates derived from
estimated corridor sizes + one-step-lookahead joint scoring with
reservation during planning. First sweep with paired statistics: it
loses to mx_c3 on every tier (split-test Δ −0.067 p=0.008; mainnet
Δ −0.048 p=0.039). Same law as exp-008 — environments elicit
mechanisms, budgets decide champions. hb1 ≈ mx_c3 on mainnet exactly
(Δ −0.000). Champions unchanged. Detail: exp-010 writeup.

The Opus 5 reflection A/B arm (code_split_opus1) runs on: same corpus/
budget/seed, only the proposer differs. Its reflections take minutes
each (vs codex ~1-2) — throughput is itself an A/B dimension. One real
infra bug found and fixed on this arm: the claude binary spawns the
real CLI as a grandchild sharing the stdout pipe, so subprocess
timeouts killed the wrapper then blocked forever on the pipe
(75081d251: process-group kill + reflection failures degrade to a
one-iteration stub instead of killing the run). Later "stalls" were
the laptop sleeping.

## 2026-07-25 (night 2) — advisor consults reframe the program

Two independent advisor reviews (Opus 5 on GEPA usage, Fable 5 on the
simulator) landed corrections we are adopting:

**The "paradigm ceiling" is partly a MEASUREMENT ceiling.** The split
corpus carries two free probes per file, making per-file scores nearly
binary; at minibatch size 3 the acceptance signal quantizes at ~0.111
while the attempt-efficiency spread being selected for is worth ≤0.15.
Pre-registered before the exp-010 verdicts: weak or null results there
are not evidence about joint planning. Fixes staged: --split-leads
(descending lead ladder, ~7 graded payments/file), info["scores"]
multi-objective axes + frontier_type="hybrid" + cache_evaluation, and
a paired-statistics sweep tool (bootstrap CIs, sign tests).

**The sim's feedback channel is a precision paradise.** Contrarian
take we accept: sealed gossip restricts inputs, but every failure is
instant, truthful, and exactly attributed — strictly MORE generous
than mainnet (parallel channels, non-strict forwarding, unattributable
timeouts). The champions' per-directed-channel intervals are the
optimal exploitation of a noiseless attribution channel, so the 8.6×
number is an upper bound until a degraded-attribution run exists. Also
corrected: receiver-side inbound failures DO occur (our gap list was
wrong); newly logged distortions: failed MPP payments are not atomic
(settled shards move liquidity + pay fees), and the world freezes
during a payment (traffic only runs between payments).

Upstream red flags to fix before proposing: no lnd+bimodal baseline
arm; mainnet liquidity synthetic from the same bimodal family the
champions hard-code (circular); single vantage; no variance reporting;
test-set reuse across exp-006..011 (fresh corpus at writeup time).
Staged tonight: multi-vantage mainnet scenarios (degrees 2024→2),
params_lnd_bimodal.json, sweep_validate.py. Durable gepa-clone patch
for the meta_harness claude JSON-array crash (branch
fix-claude-json-array). Advisor: do NOT re-enable adaptive rotation at
this budget (rotation would fire on noise and confound the A/B);
prefer clean parallel proposer comparisons.

## 2026-07-25 — exp-008 VERDICT: time-awareness re-evolves, doesn't win

code_drift1 finished 400/400 (51 accepts). Its winner is the first
evolved router with time logic, and the mechanism is the hypothesized
"interval-softening" form: belief confidence decays on a 35-minute
half-life, hard bounds expire outright at 20 minutes, and edge
probability interpolates `conf·learned + (1−conf)·prior` — so aging
evidence slides back toward the bimodal prior. Decay of confidence in
evidence, never decay of penalties.

But it does NOT beat the time-less champions, even on drift:
drift-test mx_c3 0.457 / hb1 0.455 / **gen2 0.456** / drift1 0.417.
The sharpest cut: gen2 (same budget, same seed style, never saw drift)
beats drift1 on the drift corpus itself. lnd's *rationale* is
validated — staleness pressure is real and selection responds to it —
but at realistic churn, hard evidence bounds degrade gracefully enough
(a wrong bound costs one retry) that decay machinery buys nothing.
Champions of record unchanged: hb1 + mx_c3, now validated on four
tiers. Detail: exp-008 writeup.

## 2026-07-25 (afternoon) — exp-010 CLOSED: the Opus arms land

Both Opus 5 reflection arms finished (after an API-limit outage was
salvaged by resuming from GEPA state with the stub-wasted budget
refunded). The five-tier sweep settles the three-way proposer A/B:

- **opus1 (default effort) posts the program's first statistical tie
  with the champion on any tier** (split-val +0.005 vs mx_c3, higher
  raw success) and beats the codex arm clearly on-corpus (0.841 vs
  0.810 held-out) — then collapses off-corpus (hard test 0.303 vs
  mx_c3 0.583): its 1,931-line winner evolved persistent parallel
  flow plans, concurrency-first dispatch, and residual-aware shard
  budgeting, and paid for that corridor-tuned depth everywhere else.
- **opusmed1 (medium effort) is the cautionary arm:** best val score
  of the family (0.874), worst held-out (0.743) — val overfit, caught
  by the sealed sweep exactly as the methodology intends.
- **Champions unchanged** (hb1 + mx_c3, now validated against three
  proposer lineages on this corpus). Law sharpened: proposer strength
  moves candidates along the specialist–generalist axis; it does not
  lift the whole curve. At fixed evals, reflection quality beats
  reflection throughput; fixed wall-clock remains unrun.

Also this afternoon: the claude -p hook leak (user-level Stop hook
reaching the reflection model, ~3–4% of iterations on both arms,
symmetric) was diagnosed from the proposal canary and sealed with a
sterile CLAUDE_CONFIG_DIR (f27bd470a) — the claude-side twin of the
CODEX_HOME fix. Full verdict tables in the exp-010 writeup.

## 2026-07-25 (evening) — exp-010b launches; a correction to the exp-010 story

The atomic arena landed (d0f062747: hold-and-release shards, held
liquidity contention, traffic drifting one slice per attempt;
flag-off byte-identity verified) and the baseline reordered the field
before evolution even started: lnd falls from second to LAST (105+
attempts/payment — the probe-ladder subsidy was real), the champions
hold, and opus1's persistent planner pulls statistically even with
mx_c3 on both atomic tiers. Two evolution arms are live on the new
corpus (codex + Opus-default).

The challenger docs pass (de4982856) corrected the exp-010 verdict's
causal story: opus1's off-corpus collapse traces to a single overfit
constant (maxRouteHops = 7, where the hard corpus needs 9–23-hop
routes), not to its fail budget — raising one constant recovers most
of the gap. The arm overfit its constants harder than its
architecture. And a structural surprise: both Opus winners keep NO
cross-payment state (fresh router per payment, seeding probes
discarded — champion-tying performance learned inside one payment),
while every codex-lineage router carries a cross-payment belief map.
Grist for exp-012's cold-cache mill: the Opus lineage is already
cold-start-native.

## 2026-07-26 (overnight) — exp-010b CLOSED: the champion survives its honest arena

Both arms done. Codex's winner is a first-of-its-kind hybrid
(cross-payment memory + up-front reservation-ledger planning) and the
first challenger in program history with NO collapse tier: dead even
with mx_c3 on hard/OOD/mainnet, and the most attempt-frugal router
ever measured (1.6 att/pmt on mainnet vs the champions' 2.3). It
still does not beat mx_c3 on held-out atomic-test (−0.044, p=.07) —
the champion survives its fifth challenge, on an arena built
expressly against its reactive ladder. The Opus-default arm lost
outright (bound-relaxation re-probing burned 57 att/pmt), flipping
the exp-010 proposer A/B: deliberate large-step proposers pay in
low-noise environments and misfire in churn-noisy ones. Law, final
form for this arc: environments elicit mechanisms, budgets decide
champions, and proposer strength interacts with environment VARIANCE.
(Downgraded 2026-07-27: that last clause is a mechanism story fitted
to two opposite-sign runs. Per-proposer variance is high enough to
produce the pattern by itself, so treat it as a hypothesis, not a law.
Testing it properly needs n>=4 per cell and gates nothing upstream.)
Next levers: degraded attribution (measurement channel) and exp-012
cold/hot cache, where stateless-Opus vs memory-carrying-codex finally
gets priced. Detail: exp-010b writeup.

Addendum from the challenger-docs pass (6c0b3a2c5): atomic1 resolves
the exp-008 staleness tension by SCOPE instead of clock — per-payment
failure evidence is treated as near-certain (two strikes kills a
directed channel for the payment), while persisted cross-payment
bounds soften to a 0.012 floor rather than mx_c3's hard zero. Fresh
evidence is certainty, old evidence is a strong prior, and no
half-life computes it: the belief's lifetime, not its weight, is what
varies. Three lineages have now answered "how do you age evidence?"
three ways (hard bounds forever / decay-and-lose / scope-split), and
scope-split is the first that generalizes without collapse.

## 2026-07-26 (night 3) — exp-012: stale knowledge, and the first
## significant win over a champion

The hot-load sweep found something better than the question it was
asked. Warming a router with unscored payments and then restoring the
network's liquidity leaves it holding beliefs about a state that no
longer exists — a maximally stale cache — and under that pressure the
field splits into three mechanisms:

- **lnd thrashes** (19.8 → 32.4 attempts as success falls 0.79 →
  0.35): its pair entries are permanent zeros on tiers with no clock,
  so a stale blacklist redirects it forever without ever satisfying it.
- **The champions abandon** (2.3 → 0.6 attempts, 36% success): a hard
  `upperFail` zero turns a stale bound into a dead channel, and enough
  dead channels make a live payment look hopeless.
- **atomic1 shrugs** (0.790 → 0.775, a 2% loss against their 56%): its
  persisted bounds clamp to a 0.012 floor rather than zero, so stale
  evidence discourages a channel instead of forbidding it.

atomic1 therefore beats mx_c3 on mainnet by +0.233 (p=.002) at 100
warmup payments and +0.428 (p=.002) at 400 — the first statistically
significant win over a champion in the program's history, though on a
tier the champions were never designed for. Champions of record are
unchanged: this is a robustness axis, not the standing objective.

The upstream-shaped reading: a served weight cache is stale by
construction, so the consumer's staleness policy decides whether it
helps, and the safe policy is a probability floor rather than a hard
zero. That is a small change to an existing estimator.

Also tonight, WHY.md landed and retracted three of our own claims —
the bimodal prior was in the harness prompt all along, the evolved
prior constants fit `sim_liquidity.go`'s generator, and lnd's decay
never fires on the static tiers. Corrections committed to CLAUDE.md,
NOTEBOOK and exp-006 (e2c14e964).

**Part 4 addendum (same night).** Vantage transfer, matched staleness,
18 files: nobody is hurt by a stranger's observations and lnd is
helped (0.155 → 0.176, attempts 3.0 → 0.8). The champions' bounds are
vantage-free facts about channels, so self and foreign are identical
for atomic1 and within noise for mx_c3 — expected. lnd improving is
the surprise, and it flips the sign of the vantage argument: warming
from its OWN vantage poisons the pairs involving its own local
channels, which every one of its payments must cross and which it
cannot decay away on this tier, so it thrashes around its own first
hop. A stranger cannot touch those pairs. The practical rule for a
weight-serving API is therefore narrower and sharper than "MC does not
transfer": serve remote-pair observations, and never import
observations about the consumer's own local channels — cheap to
measure yourself, most damaging when stale, and the only genuinely
vantage-bound part of mission control.

## 2026-07-26 (dawn) — the staleness null indicts our churn model

Part 3 of exp-012 varied only the idle gap between warming a router
and scoring it: 0, 10 minutes, 1 hour, 6 virtual hours of background
traffic. Nothing moved, to three decimals, for any router. The
manipulation check passes — background payments scale 700 → 1420 with
the gap, exactly as prorated — so the knob works and the world simply
does not move enough to matter.

The reason is a simulator defect worth more than the null: **only
about 18% of background payments settle.** The traffic engine sends
naive fee-optimizing payments that mostly fail, and a failed payment
moves no liquidity, so our exogenous process is roughly five times
weaker than its configuration implies. Against a 12k-node graph, 720
mostly-failed payments never touch the corridors a scored payment
needs.

That reaches backwards. exp-008 concluded time-decay "buys nothing at
realistic churn"; the honest restatement is that it buys nothing at
the weak churn we generate, and the drift experiment never reached a
regime where evidence genuinely goes stale. exp-010b's per-attempt
drift comes from the same engine and inherits the same caveat. Fix the
traffic engine (make it settle, and aim some of it at the corridors
under test) before any staleness claim is made again.

Also this night: CodexLM had the same grandchild-pipe defect ClaudeLM
was fixed for — `codex` spawns the vendored platform binary as a
grandchild sharing the pipe, so `subprocess.run`'s timeout killed the
wrapper and blocked forever. It stayed latent because codex
reflections were always fast, until a 1,031-line seed made one slow
enough to trip it: exp-013 hung for 85 minutes on a single reflection.
Ported the process-group kill and the degrade-to-stub path, added
--reflection-timeout for large seeds, and resumed exp-013 from state.

## 2026-07-26 (afternoon) — exp-012 closes: no hot-cache regime, and why

The probe-warm arm finished the sweep set: 100 valid probes at 2% and
10% of the scored amounts, knowledge that stays true when used. Nobody
gains (mx_c3 0.791 → 0.768 → 0.653; lnd 0.694 → 0.664 → 0.597), and
the loss grows with probe size. lnd's attempts do fall at 10% probes
(19.8 → 15.9), the only genuine warming signal in the whole
experiment, but its success falls faster.

So the original question gets a clean negative: across depletion,
staleness, foreign vantage and valid probes, at 25/100/400
observations, **no warming ever lifts any router above its cold score
and mission control never approaches the champions.** Two mechanisms:
the champions are already at their asymptote on payment one (their
edge is the prior, so warming can only subtract), and 100 observations
is ~1% pair coverage on a 12k-node graph, recorded as permanent zeros
on clockless tiers.

The design limit is worth stating as loudly as the result: every arm
derives knowledge from PAYMENTS, and payments cost liquidity, so "free
knowledge" is unconstructible here. The drain arm pays in depletion,
the restore arm in staleness, the probe arm in both. A served cache in
the real proposal costs the consumer nothing — it arrives over an API.
Measuring that needs direct injection of beliefs from a file with no
payments sent (`--import-weights`). Until that exists, exp-012's
negative is about probe-warming, not about weight-serving.

What survives and carries upstream: the champions' edge is a prior not
a history; under stale knowledge the consumer's staleness policy
dominates (hard zeros poison, floors survive) and that is the
actionable finding; remote-pair observations transfer across vantages
while local-channel ones must not be imported; and our traffic engine
is ~5x weaker than configured, which caveats exp-008 and exp-010b.

## 2026-07-26 — exp-002b: the knob WHY.md said we never turned

WHY.md flagged that "the paradigm is the lever, not the knobs" had
never been tested against lnd's closest analogue: its own bimodal
estimator, with scale_msat set to match this environment rather than
left at the 300M default. Ran the grid — seven scales bracketing the
matched value (100M on hard, 150M on v2) — against lnd's shipping
apriori default and mx_c3.

The claim holds, and now for a stated reason rather than an absence of
evidence. No bimodal scale beats lnd's own apriori default on either
tier (hard: best bimodal 0.283 vs apriori 0.298 vs mx_c3 0.479; v2:
0.345 vs 0.357 vs 0.581), and the environment-matched scale is among
the worse settings, not the best.

How it fails is the finding. On the hard tier bimodal RAISES success
(0.421 → 0.478) while MORE THAN DOUBLING attempts (30.9 → 77): a
better liquidity prior makes lnd more willing to keep trying, so it
completes more payments at a much higher price. What it cannot do is
change what lnd retries — findPath takes the amount as a fixed
argument, so with any estimator it retries the same amount over
different routes, while the champions read upperFail and retry a
different amount. The estimator swap is worth at most 0.02 of
objective; the paradigm difference is worth 0.18 to 0.22. That is
WHY.md's central thesis, now measured instead of argued.

## 2026-07-26 — exp-013: the give-up attractor

Applied the mx_c3 recipe — lineage continuation from the best router
so far — to atomic1, the exp-010b challenger with no collapse tier and
the program's attempt record. It failed, and how it failed is the
result.

gepa's own held-out test called it: the evolved winner scored 0.5124
against the seed it grew from at 0.5274. The five-tier paired sweep
agrees — hybrid1 is below mx_c3 on every tier (mix −0.070, hard
−0.053, v2 −0.086, atomic −0.017, split −0.146, mainnet hub −0.122).
No single tier is a significant loss, but the champion rule needs a
win and there isn't one anywhere.

The attempt column explains it. hybrid1 has the fewest attempts on
every tier by a wide margin (mix 5.5, hard 4.9, split 2.2 against
mx_c3's 9.6/8.1/10.2) and lower success on every tier. On split_test
it converges to 2.2 attempts and 0.750 success while everyone else
sits above 0.917. It did not get more efficient; it stopped trying.

Why here and not with hb1 → mx_c3: continuation evolution looks for
the nearest improvement to its seed, and atomic1 entered the run
already at the attempt frontier. With nothing left to win on routing
quality, the only cheap direction left was abandonment — trading a
large certain attempt saving against a small probabilistic success
loss, one gradient-friendly step at a time. hb1 had attempts to
spare, so the same recipe walked uphill instead.

Two things to carry forward. Check where a seed sits on the attempt
axis before spending 400 evals continuing it; at the frontier the
recipe inverts. And our eval output should report a give-up rate,
because the composite objective hides abandonment inside the same
number as efficiency.

Also found while validating: the exp-012 multivantage mainnet set is
useless as a champion tier. Every router scores an identical 0.227
success on it, since at low-degree vantages the reachable fraction is
fixed by the graph rather than by routing skill — so it scores an
attempt-cost contest and reports it as an objective difference.
atomic1 and hybrid1 "beat" mx_c3 there at p=0.03/0.02 by attempting
less on payments nobody can complete. Champion validation uses the
exp-009 hub scenario; the multivantage set is for vantage transfer
only.

Champions unchanged: hb1 + mx_c3. The tree is unfrozen for the first
time since exp-013 launched, which releases all four pre-upstream
blockers.

## 2026-07-26 — exp-014: the traffic engine, fixed

The top pre-upstream item, and the prerequisite for any honest drift
or staleness claim. Background payments were settling at 0.41 on the
drift corpus, 0.61 on the atomic one and 0.18 on mainnet — and since a
failed background payment moves no liquidity at all, that ratio is
exactly the factor between the churn a scenario file asks for and the
churn it gets.

Three causes. The route search filtered on capacity and policy but not
on the hidden balance, so it kept picking corridors a bimodal
distribution cannot fund. The amount was drawn blind and never
revisited, so a payment bigger than any corridor just died. And
endpoints were drawn uniformly, which is badly wrong on a real
topology: the mainnet snapshot has a median degree of ONE and 68% of
its nodes hold two channels or fewer, so uniform draws picked
leaf-to-leaf pairs with no path between them at any amount.

Only the third explains mainnet. The first two fixes moved it from
0.177 to 0.184 — no amount of shrinking finds a path that isn't there
— and degree-weighted sampling moved it to 0.951. Drift went to 0.69,
atomic to 0.89.

Consulting hidden balances is the environment's privilege, worth
stating plainly: the traffic engine IS the network, and what a
candidate sees through the sealed gossip view is unchanged.

Also added focus_fraction, the share of churn that takes one endpoint
from the scenario's own source and targets. Traffic spread evenly over
a 12,161-node graph almost never touches the few channels a scored
payment uses, so without it the knob moves the network everywhere
except where it is measured. Generated corpora set it to a third.

Does it overturn anything? No. Re-running the champions over both
traffic tiers with old and new engines leaves every ordering intact
and every router inside its old confidence interval. One directional
hint for the exp-008 re-run: on drift the stronger churn helps lnd
(+0.032) and slightly hurts all three interval routers, which is what
you would predict if hard bounds go stale faster under real movement.
Nowhere near significant at n=8; a hypothesis, not a finding.

Companion change from exp-013's lesson: SimScenarioResult.GaveUp
records that a router ABANDONED a payment rather than exhausting its
attempts, and the aggregate reports num_give_ups and give_up_rate.
Nothing scores it — it exists so the next candidate cannot buy a low
attempt count by quitting and have it read as efficiency. The
aggregate also reports bg_settle_rate now, so this defect would have
been visible in every run's output instead of needing a manipulation
check to find.

## 2026-07-26 — exp-015: exp-008 called a tie a loss

exp-014's before/after check left a directional hint — stronger churn
helped lnd and hurt the interval routers on drift — pointing straight
at exp-008's headline, "time-decay re-evolved under drift and LOST to
the time-less champions." exp-008's own caveat had anticipated it: a
heavier drift regime could tip the balance. And since that conclusion
is fed to every evolution run through the harness prompt ("spend your
complexity budget elsewhere"), being wrong about it steers the search.

Ran drift1 against the champions on ONE fixed corpus with only the
churn rate varying: payments_per_gap 0, 20, 80, 240, everything else
identical. At the fixed engine's ~0.9 settle rate, 240/gap is roughly
eighteen times the effective churn exp-008 actually ran under.

drift1 vs mx_c3, paired: -0.016, -0.005, -0.007, -0.003. Statistically
indistinguishable at every level, including no churn at all, with
every CI straddling zero and no trend. An order of magnitude smaller
than the 0.04 gap exp-008 read as a loss.

The correction is not that the fixed engine changed the answer. It is
that the answer was a TIE in the first place: exp-008 compared two
point estimates at n=8 with no paired test, and re-scoring its own
original corpus under the fixed engine gives -0.033 at p=0.453. The
churn ladder then confirms the tie holds at eighteen times the churn.
exp-014's hint did not survive a controlled test, which is exactly why
it was worth running instead of repeating.

exp-008's substantive findings stand: time-awareness genuinely
re-evolved, its evolved form (confidence softening toward the prior,
bounds expiring) is structurally unlike lnd's penalty fading, and it
costs nothing on static tiers. Only the "and lost" clause was wrong.
A tie is not a win either — decay remains unproven here rather than
disproven.

Harness prompt corrected accordingly. An unsupported negative in the
background prompt is a search restriction we imposed on ourselves.

One hypothesis fell out worth its own test: hb1 pulls away from mx_c3
monotonically as churn rises (+0.018, +0.015, +0.028, +0.037). None
significant at n=8, and four noisy points make a weak trend, but the
champion pair was settled on static tiers. If it reverses under churn
that is a champion question, and it needs a bigger corpus rather than
more churn levels.

## 2026-07-26 — exp-016: free knowledge helps the champions, hurts lnd

--import-weights landed, and with it the arm exp-012 could never build:
a third-party node's observations injected from a file with no payment
sent. For each sealed hard-tier file a DIFFERENT source node ran the
same network and exported what it saw; each consumer then ran the
original file cold and served.

The champions could not consume anything at all — nothing in the
SimRouter contract ever asked a candidate to accept third-party
knowledge, so no evolved router implements it. So the experiment also
produced importer variants of mx_c3 and atomic1, each its ancestor plus
one method routing every observation through the same belief update a
real attempt makes. Both are identical to their originals cold.

Served the same file: atomic1 +0.055 (CI excludes zero, p=0.016),
mx_c3 +0.031 with attempts nearly halved (8.1 -> 4.4), and lnd
-0.029 with attempts going UP (30.9 -> 33.8). Free, accurate,
correctly-scoped information makes lnd worse.

Splitting the stream says why. Successes help everyone (+0.003,
+0.028, +0.038). Failures split the field: they help the interval
routers (+0.010, +0.019) and they are the whole of lnd's loss at
-0.039, CI excluding zero, worse on 9 of 10 files.

**The mechanism took three wrong guesses to find, and the first of them
was published before it was checked.** Recorded in full in the writeup,
because the errors are more instructive than the answer:

1. I wrote that lnd files a failure as a pair penalty carrying no
   amount. `probability_apriori.go:363` returns the unpenalized prior
   whenever `amt < FailAmt`, so lnd's estimator gates on amount
   correctly. False, and it reached the dashboard.
2. A Fable advisor proposed node-level contagion instead —
   `getNodeProbability` folds pair results into a node prior used for
   all that node's channels, which my "761 edges, 761 pairs" check
   never ruled out. Disabling it with `apriori.weight = 1.0` leaves the
   loss at -0.038. Not contagion.
3. Staleness looked decisive: failures from a one-payment server give
   +0.000, worse on 0 of 10. But that set holds 232 observations
   against the stale set's 2,808, and a size-matched random subsample
   of the STALE set gives -0.003. Equal volume, equal result. Not
   staleness.

What survives is volume. Each imported failure blocks one directed edge
at the amount the consumer is about to send, because server and
consumer draw amounts from the same distribution. At 232 observations
nothing happens; at 2,808 across a 761-edge graph lnd finds its amount
blocked almost everywhere and can only route around, onto longer and
worse paths. The interval routers receive the identical removals and
turn them into instructions — an imported upperFail of X tells mx_c3's
ladder to try (X-1)/k.

So the thesis is sharper than the sentence I first wrote. lnd's
estimator does not ignore amounts; nothing DOWNSTREAM of it can act on
an amount bound, because findPath takes the amount as a fixed argument.
Knowledge that "at least X fails here" can only subtract routes, never
resize the payment. That is exp-002b's finding reached from the
opposite direction, and the two now converge on one patch instead of
two observations.

Two design rules for the API fall out, both now measured rather than
argued. Serve observations, not weights: neither side's internal state
is servable but both are derivable from (from, to, chan_id, amount,
success, time). And a consumer must store failures as amount bounds to
benefit from them, so an API serving failures to lnd as it stands makes
lnd worse — unless mission control learns to keep FailAmt as a bound
the retry loop actually reads.

Incidental: server coverage ranged from 0 to 2,111 observations across
the ten server nodes. Who serves matters as much as what is served.


## 2026-07-27 — corrections: a published mechanism, a ceiling, and a law

Three claims in this notebook were overstated. All three are now
labelled where they appear; collected here so the pattern is visible.

**exp-016's mechanism was wrong, and it reached the dashboard.** I
wrote that lnd files a failure as a pair penalty carrying no amount.
`probability_apriori.go:363` returns the unpenalized prior whenever
`amt < FailAmt`, so lnd's estimator gates on amount correctly. A Fable
advisor caught it by reading the code rather than the packet. Its own
replacement hypothesis — node-level contagion — was also wrong
(`apriori.weight=1.0` disables the aggregation and leaves the loss at
-0.038), and so was the third guess, staleness (size-matched stale
observations cost -0.003 against fresh +0.000; the apparent staleness
effect was a volume difference, 232 against 2,808). What survives is
volume: each imported bound blocks an edge at the amount the consumer
is about to send, and nothing downstream of lnd's estimator can resize
a payment. The upstream thesis is unchanged and better supported.

**exp-011's "paradigm ceiling" is confounded with the engine.** Every
run in this program used `engine="gepa"`. Three lineages converging on
one band says as much about that engine's attractor as about the
problem, and the GEPA team's omni results — no engine dominant, each
winning about a third of problems, engine-switching breaking plateaus —
make the alternative live. Underdetermined rather than wrong; the
adjudicating run is specified in the exp-011 writeup.

**The proposer law was fitted to n=2.** Downgraded to a hypothesis in
place.

The common thread: each was a mechanism story built on top of a real
measurement, and in each case the measurement stood while the story did
not. The measurements in this notebook are more trustworthy than the
explanations attached to them, and explanations should be checked
against the code they describe before they are published.