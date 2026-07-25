# Ideas Backlog

Unordered, mined during work. Promote to experiments when picked up.

## Simulator fidelity
- Liquidity models from real `lncli querymc` data (dijkstrasden's
  BalanceHints idea) instead of synthetic distributions.
- Mainnet `describegraph` snapshot corpus entries (loader exists;
  needs a snapshot file + bigger sim budget).
- In-flight HTLC modeling: concurrent shards currently settle
  sequentially; real MPP races liquidity.
- Time model: mission control decay currently only sees wall-clock of
  the batch; inject a virtual clock so half-life params matter across
  scenarios.
- Non-strict forwarding / parallel channels between the same pair.

## Optimization
- Backend shootout on the identical evaluator (the point of the new
  optimize_anything API): `best_of_n` as the honesty baseline,
  `meta_harness` (agentic proposer reads frontier history), and the
  adaptive scheduler that rotates backends on plateaus — vs our pinned
  `engine="gepa"`. Note the agentic backends shell out to the claude
  CLI headless.
- Tune bimodal `scale_msat` relative to median channel size of the
  graph rather than as an absolute — likely the single biggest win for
  the bimodal estimator on non-mainnet-scale nets.
- Multi-objective via `info["scores"]` (success / attempts / fees as
  separate axes) so the Pareto frontier keeps specialists.
- Seedless code-mode run (`seed_candidate=None`): let GEPA invent a
  router from the contract description alone; compare against evolved
  seed lineage.
- Tournament: evolve N routers on different liquidity regimes, then
  score cross-regime for a generalist.
- Feed mission-control replay data from a real node as a validation
  scenario class (out-of-distribution check for evolved params).

## Free-parameter tuning beyond pathfinding (roasbeef 2026-07-24)
The same harness pattern (evaluator + GEPA) applies to other magic
numbers in lnd once a scoreable simulator exists for them:
- Payment session knobs already covered: attempt cost, min probability,
  estimator params, shard minimum.
- `DefaultShardMinAmt`, `BlockPadding`, max parts defaults.
- Mission control: result decay, second-chance logic thresholds.
- Sweeper/batching params (needs a fee-market sim), gossip rate limits
  (needs a gossip sim) — candidates for future simulators following the
  routesim recipe.

## The zero-time-logic question (roasbeef, 2026-07-24) → exp-008 design

The evolved champions contain zero time-based logic, yet lnd's decay
exists for a real reason: on a live network, *other people's payments*
move liquidity while you aren't routing, so stale knowledge should fade.
Honest read: the champions' rejection of time is **partly a simulator
artifact** — our sim has no background traffic and no virtual clock, so
hidden balances only change when OUR payments move them. In that world,
hard evidence bounds are strictly optimal and decay only destroys true
information; evolution correctly exploited the environment as given.

What still transfers: within a single payment/session (seconds-minutes),
decay is likely counterproductive and interval beliefs win — lnd's 1h
half-life mostly matters *across* payments, and that's where the sim is
least faithful.

**Designed experiment (exp-008), folds into batch-2 (task #13):** add a
background-traffic model (exogenous seeded payments between our
scenarios, or liquidity drift as a function of virtual time) + the
virtual clock. Then re-run code evolution and ask: *does time-awareness
re-evolve once the environment actually drifts?* Outcomes all
interesting: (a) decay re-emerges → validates lnd's rationale with
evolved constants; (b) something better emerges, e.g. interval-widening
with elapsed time rather than penalty-fading — a concrete design
proposal for lnd; (c) intervals still win → decay was overweighted.

## Learnings from the overnight runs (2026-07-24)
- **Giant-seed reflection is slow and fragile.** Seeding code_mix1 from
  the 872-line hb1 champion makes every reflection prompt huge; codex
  reflection calls run many minutes and risk the 600s CodexLM timeout,
  which (like an eval timeout) can propagate and end the run. Prefer:
  seed from the SMALL original router (fast reflection) but ENRICH the
  background prompt with the discovered insight (the bimodal prior +
  per-edge liquidity bounds hb1 found). Tests whether the *idea*
  transfers without dragging the whole 872-line body through every
  prompt.
- **Reflection-timeout robustness:** mirror the eval-timeout fix — a slow
  or failed reflection LM call should degrade to "no proposal this
  round" and let the run continue, not crash it. Needs handling at the
  gepa reflection layer (our CodexLM can't fix it alone since the
  protocol wants a candidate string back).
- **Code-evolution complexity wall:** once a candidate grows past ~800
  lines, LLM edits frequently fail to compile (code_hard1 iters 2-4+).
  Consider a "refactor/simplify" reflection instruction, or a size
  penalty in the objective, to keep candidates editable.
- **The paradigm ceiling (exp-011):** three independent lineages (hb1,
  mx_c3, gen2) converge on the same interval-belief design and the
  same held-out band (~0.64 combined). Insight transfer via prompt
  compresses a 900-eval lineage into 400 evals — but cannot exceed
  the ceiling. Novel mechanisms that the sim never rewards (gen2's
  in-flight liquidity reservation, weakest-edge attribution) get
  carried along neutrally rather than selected for. To evolve past
  the ceiling, change the environment, not the budget: background
  traffic (exp-008), non-binary split pressure (exp-010), or
  concurrent shard settlement racing liquidity.

## Cold cache vs hot-loaded weights (roasbeef, 2026-07-25) → exp-012

lnd field defect: mission control weights matter enormously on a
network full of unbalanced/unreliable nodes, and a NEW node has none —
it burns a long, expensive warmup making its first payments. Proposed
fix under consideration: serve cached weights from an API so a fresh
node can hot-load instead of probing from scratch.

What we already know: every sim batch is a cold start (empty MC, empty
beliefs), so all published numbers ARE cold-cache numbers — and the
champions' entire advantage is warmup speed (mainnet 2.3 vs 19.8
att/payment with zero prior knowledge in both cases). What we have NOT
measured: the shape of the warmup curve (attempts by payment index —
in flight), and the marginal value + staleness sensitivity of imported
weights.

**Designed experiment (exp-012):**
1. *Warmup curves* (analysis only, no sim changes): attempts/success by
   payment index within batches, per router, on mainnet + hard corpora.
   Quantifies how many payments each design needs to get cheap.
2. *Hot load*: add an unscored `warmup_payments` phase to the runner —
   N payments run before the scored batch, warming MC/beliefs but not
   counting. Scored-batch delta vs cold = the value of a served cache.
   Works identically for lnd and candidates, no contract change.
3. *Staleness*: combine with the drift model — insert T virtual minutes
   (with background traffic) between warmup and scored batch; plot
   payment-1 attempts vs T. This is the API-cache question exactly:
   how stale can served weights be and still help? drift1's confidence
   half-life (exp-008) is the natural primitive for weighing imported,
   aged evidence.
4. *Third-party weights*: the champions' beliefs are keyed by DIRECTED
   CHANNEL and describe channel state, not "pairs as seen from me" —
   unlike lnd's MC history, they are meaningfully shareable across
   nodes. Warm the cache from a DIFFERENT source node's probes and
   measure transfer. (Candidate knowledge is package-level in-process,
   so two runners can already share it; a real design would serialize
   the belief map — a few hundred bytes per active channel.)

Sequencing: the runner feature (2) touches routing/, so it lands
between evolution runs; (1) needs nothing and runs anytime.

## Engineering
- Cache evals keyed by (candidate hash, example) to stretch budgets —
  GEPA re-evaluates the seed dozens of times.
- Emit run.json lineage for the command-center dashboard directly from
  GEPA run_dir state after each run.
- CI check: `go build -overlay` smoke with the in-tree candidate to keep
  the contract compiling.
- lnd-side follow-up: whatever wins parameter mode becomes a proposed
  defaults change PR to lightningnetwork/lnd with the sim evidence.

## MPP splitting as a first-class evolution axis (roasbeef, 2026-07-24) → exp-010

lnd today splits by divide-and-conquer: try the full amount, and when no
route clears MinProbability, halve and retry (down to the min shard, up
to MaxParts) — the split *trigger* is route probability dipping below
threshold, and the split *sizes* are blind halves.

Where the evolution stands on this axis:
- The `SimRouter` contract already hands candidates FULL ownership of
  splitting — the runner only asks for the next route for the remaining
  amount; shard sizing/count/trigger are entirely the router's choice
  (unlike lnd, where halving is fixed payment-session logic).
- The seed used naive halving. The champions evolved "halving-plus":
  mx_c3's `candidateShardAmounts` builds a ladder of the halving
  sequence PLUS evidence-derived shard sizes fitted just under known
  per-channel failure bounds, and its split trigger is belief-driven
  rather than a global probability threshold.
- NOT yet evolved: joint multi-path planning — choosing a *set* of
  routes and shard sizes together (Pickhardt-style min-cost-flow
  splitting) instead of sequential shard-by-shard greediness.

**exp-010 design:** apply explicit selection pressure on splitting.
Corpus of large payments where no single path suffices and the optimal
split is non-binary (e.g. 70/20/10 across asymmetric corridors); prompt
nudge naming joint route-set planning as unexplored; possibly score a
bonus for fewer parts at equal success. Question: does min-cost-flow
style splitting re-emerge, and does it beat the evolved halving-plus on
the mainnet graph?
