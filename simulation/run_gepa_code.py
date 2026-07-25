#!/usr/bin/env python3
"""Run GEPA over entire routing algorithms (code candidates).

The candidate is the full Go source of cmd/routesim/candidate_impl.go. The
seed is the in-tree simple router; the target to beat is lnd's production
stack, whose per-example scores are reported alongside for reference.
"""

import argparse
from pathlib import Path

from gepa.optimize_anything import (
    OptimizeAnythingConfig,
    optimize_adaptive_sequential,
    optimize_anything,
)

from claude_lm import ClaudeLM
from codex_lm import CodexLM
from evaluate_code import REPO, batch_evaluate, evaluate

OBJECTIVE = """
Evolve a Lightning Network routing algorithm (Go source, the complete
contents of candidate_impl.go) that maximizes payment success rate in a
network simulator, with fewer retry attempts and lower fees as secondary
goals. You may redesign the algorithm entirely — probability models,
splitting strategies, exploration policies — as long as the
newCandidateRouter contract compiles and the code stays pure routing logic.
"""

BACKGROUND = """
Contract: package main must define
newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
localBalances map[uint64]lnwire.MilliSatoshi, spec *routing.SimPaymentSpec)
(routing.SimRouter, error). The returned router implements
RequestRoute(amt, inFlightHtlcs) (*route.Route, error) — return an error to
terminally give up — and ReportAttempt(attemptID, rt, result) error, which
delivers per-attempt feedback (result.Failure nil = settled; otherwise
result.FailureSource names the failing node and the failure code tells you
why: TemporaryChannelFailure = liquidity miss, FeeInsufficient /
IncorrectCltvExpiry = your route's fees or cltv deltas violate the failing
node's advertised policy).

Environment truths worth exploiting:
- Hidden liquidity is drawn mostly from a BIMODAL distribution: channel
  funds sit almost entirely on one side. A 50/50 assumption is usually
  wrong; a failure at amount a on a channel is strong evidence the whole
  channel is depleted in that direction, and a success means most capacity
  is available.
- The gossip view exposes per-direction policies (fees, cltv delta,
  min/max htlc) and channel capacities via ForEachNodeDirectedChannel;
  InPolicy on a channel of node N is the policy the OTHER node announced
  toward N (i.e. it governs edges INTO N).
- Route encoding: amount over channel i is TotalAmount for i=0, else
  Hops[i-1].AmtToForward; fees accumulate backward from the target;
  the final hop needs cltv delta 40.
- MPP: the runner keeps calling RequestRoute with the remaining amount;
  spec.MaxParts caps concurrent shards; each successful shard reduces the
  remaining amount.
- Payments per scenario batch run sequentially and liquidity persists, so
  knowledge from earlier payments in the batch transfers.
- THE NETWORK KEEPS MOVING BETWEEN YOUR PAYMENTS: scenario files may
  enable background traffic, where other participants' payments shift
  hidden liquidity in the (virtual) minutes between your payments, and a
  virtual clock, readable as view.Now(), advances between payments and
  attempts. In such environments, what you learned about a channel k
  payments ago may no longer hold. Whether and how to account for the
  age of evidence is entirely your design choice.

The current seed is a cheapest-path Dijkstra with failure blacklisting and
halving splits. Known weaknesses to consider: it ignores capacity when
choosing among paths (bigger channels succeed more often), it has no
notion of probability weighting fees vs reliability, it never retries a
blacklisted channel at lower amounts within a payment, and its shard
halving is crude.

Insights from prior successful runs (champions hb1/mx_c3, see
simulation/champions/), worth building on rather than rediscovering:
- An explicit BIMODAL PRIOR over amount/capacity works: near-certain for
  tiny amounts (decaying exponential low mode), a logistic cliff as the
  amount approaches capacity, floors/caps around [0.005, 0.985].
- Per-directed-channel liquidity BELIEFS work well: track lower-OK
  (largest amount proven to pass) and upper-fail (smallest proven to
  fail) bounds plus a confidence-weighted point estimate; return ~0.995
  below lower-OK, ~0 above upper-fail, blend with the prior in between.
  (Caveat: this insight was learned in environments with NO background
  traffic, where old evidence never went stale. Its hard bounds may or
  may not survive in a drifting network.)
- Retry-at-lower-amount on a failed channel (a lower-retry factor)
  outperforms permanently blacklisting it.
- Time-decay of evidence has been tried under genuine liquidity drift
  and LOST to plain hard bounds (exp-008): a stale bound costs one
  retry to refresh, which is cheaper than what decay throws away.
  Spend your complexity budget elsewhere.
- MPP splitting is where the least design space has been explored.
  Prior winners split reactively: try an amount, and on failure carve
  the next shard from a ladder of halves and evidence-derived sizes.
  Nobody has yet evolved JOINT route-set planning: choosing a set of
  routes AND their shard amounts together up front (min-cost-flow
  style), so that parallel corridors of unequal capacity each carry a
  shard sized to what they can bear. When single paths cannot carry
  the payment, unequal splits chosen deliberately should beat halving
  discovered by failure.
- Keep the implementation LEAN: past ~800 lines, edits stop compiling
  and progress stalls. Prefer simplifying refactors over accretion.
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", default="corpus")
    parser.add_argument("--name", default="router_code")
    parser.add_argument("--max-evals", type=int, default=None)
    parser.add_argument("--reflection-lm", default="codex:gpt-5.6-sol")
    parser.add_argument("--max-concurrency", type=int, default=4)
    parser.add_argument("--adaptive", action="store_true", default=True,
                        help="rotate gepa <-> meta_harness on plateaus")
    parser.add_argument("--no-adaptive", dest="adaptive",
                        action="store_false")
    parser.add_argument("--seed-file", default=None,
                        help="seed candidate .go file (default: the "
                        "in-tree candidate_impl.go). Use a prior "
                        "champion to continue evolving from it.")
    args = parser.parse_args()

    corpus = Path(args.corpus)
    trainset = sorted(str(p) for p in (corpus / "train").glob("*.json"))
    valset = sorted(str(p) for p in (corpus / "val").glob("*.json"))
    testset = sorted(str(p) for p in (corpus / "test").glob("*.json"))
    if not trainset or not valset:
        raise SystemExit(f"no corpus at {corpus}; run gen_scenarios.py")

    if args.seed_file:
        seed = Path(args.seed_file).read_text()
    else:
        seed = (REPO / "cmd" / "routesim" / "candidate_impl.go").read_text()

    max_evals = args.max_evals or 20 * len(valset)

    # Every valid code candidate contains the package clause; the marker
    # check turns a hijacked or chatty reply into one retry instead of a
    # wasted optimizer iteration.
    reflection_lm = args.reflection_lm
    if reflection_lm.startswith("codex:"):
        reflection_lm = CodexLM(
            model=reflection_lm.split(":", 1)[1],
            require_marker="package main",
        )
    elif reflection_lm.startswith("claude:"):
        # claude:<model>[:<effort>] — e.g. claude:claude-opus-5:medium.
        # Effort trades per-proposal deliberation for iteration
        # throughput; the evolutionary loop supplies the search.
        spec = reflection_lm.split(":")
        reflection_lm = ClaudeLM(
            model=spec[1],
            require_marker="package main",
            effort=spec[2] if len(spec) > 2 else None,
        )

    gepa_config = OptimizeAnythingConfig(
        engine="gepa",
        name=args.name,
        max_evals=max_evals,
        max_concurrency=args.max_concurrency,
        run_dir=f"runs/{args.name}",
        output_dir=f"outputs/{args.name}",
        engine_config={
            "reflection": {
                "reflection_lm": reflection_lm,
                "reflection_minibatch_size": 3,
            },
            "engine": {
                "max_workers": args.max_concurrency,
                "seed": 0,
                # Hybrid frontier: per-example AND per-objective Pareto
                # cells, fed by the evaluator's info["scores"] axes
                # (success / retry_efficiency / fee_efficiency), so
                # fee-efficient or low-retry specialists survive
                # selection instead of being averaged away. "cartesian"
                # would dissolve selection pressure at our corpus size.
                "frontier_type": "hybrid",
                # The evaluator is deterministic (verified), so identical
                # (candidate, example) pairs are served from cache and
                # do not consume budget. Report cache misses alongside
                # eval counts when comparing runs.
                "cache_evaluation": True,
                # With caching on, max_evals counts only misses, so a
                # converged search could spin; this is the enforceable
                # cap. And an evaluator exception must cost a zero, not
                # the run.
                "max_candidate_proposals": 60,
                "raise_on_exception": False,
            },
        },
    )

    if args.adaptive:
        # Rotate between the gepa backend (codex reflection) and the
        # meta_harness agentic proposer (claude CLI) whenever the score
        # plateaus, all drawing from one shared eval budget.
        meta_config = OptimizeAnythingConfig(
            engine="meta_harness",
            name=f"{args.name}_meta",
            run_dir=f"runs/{args.name}_meta",
        )
        result = optimize_adaptive_sequential(
            seed_candidate=seed,
            evaluator=lambda cand, ex: evaluate(cand, ex),
            batch_evaluator=batch_evaluate,
            configs=[gepa_config, meta_config],
            plateau_evals=len(valset) * 3,
            dataset=trainset,
            valset=valset,
            test_set=testset,
            objective=OBJECTIVE.strip(),
            background=BACKGROUND.strip(),
            name=args.name,
            max_evals=max_evals,
            max_concurrency=args.max_concurrency,
            output_dir=f"outputs/{args.name}",
        )
    else:
        result = optimize_anything(
            seed_candidate=seed,
            evaluator=lambda cand, ex: evaluate(cand, ex),
            batch_evaluator=batch_evaluate,
            dataset=trainset,
            valset=valset,
            test_set=testset,
            objective=OBJECTIVE.strip(),
            background=BACKGROUND.strip(),
            config=gepa_config,
        )

    print("=== best candidate ===")
    print(result.best_candidate)
    print("best (val) score:", result.best_score)
    print("held-out test:", result.metadata.get("test_score"),
          "| seed held-out:", result.metadata.get("baseline_test_score"))


if __name__ == "__main__":
    main()
