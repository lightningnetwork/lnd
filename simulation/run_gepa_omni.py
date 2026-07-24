#!/usr/bin/env python3
"""Omni-style two-phase code evolution, per the optimize-anything-omni
recipe (gepa blog, 2026-07-22):

  Phase 1 (explore): run multiple engines in parallel on equal budget
  slices over the same task; take the highest-scoring candidate.
  Phase 2 (continue): seed a FRESH engine with the Phase 1 winner and the
  remaining budget — a fresh optimizer often breaks through where the
  first plateaued.

Engines here: the gepa backend (codex/gpt-5.6-sol reflection) and
meta_harness (claude agentic proposer). The candidate is the full Go
source of cmd/routesim/candidate_impl.go.
"""

import argparse
import json
from pathlib import Path

from gepa.optimize_anything import (
    OptimizeAnythingConfig,
    optimize_anything,
    optimize_best_of,
)

from codex_lm import CodexLM
from evaluate_code import REPO, evaluate
from run_gepa_code import BACKGROUND, OBJECTIVE


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", default="corpus")
    parser.add_argument("--name", default="omni1")
    parser.add_argument("--explore-evals", type=int, default=120,
                        help="eval budget per Phase 1 engine")
    parser.add_argument("--continue-evals", type=int, default=200,
                        help="eval budget for the Phase 2 fresh engine")
    parser.add_argument("--max-concurrency", type=int, default=4)
    args = parser.parse_args()

    corpus = Path(args.corpus)
    trainset = sorted(str(p) for p in (corpus / "train").glob("*.json"))
    valset = sorted(str(p) for p in (corpus / "val").glob("*.json"))
    testset = sorted(str(p) for p in (corpus / "test").glob("*.json"))
    if not trainset or not valset:
        raise SystemExit(f"no corpus at {corpus}; run gen_scenarios.py")

    seed = (REPO / "cmd" / "routesim" / "candidate_impl.go").read_text()
    task = dict(
        evaluator=lambda cand, ex: evaluate(cand, ex),
        dataset=trainset,
        valset=valset,
        objective=OBJECTIVE.strip(),
        background=BACKGROUND.strip(),
    )

    def gepa_cfg(name, max_evals):
        return OptimizeAnythingConfig(
            engine="gepa",
            name=name,
            max_evals=max_evals,
            max_concurrency=args.max_concurrency,
            run_dir=f"runs/{name}",
            output_dir=f"outputs/{name}",
            engine_config={
                "reflection": {
                    "reflection_lm": CodexLM(model="gpt-5.6-sol"),
                    "reflection_minibatch_size": 3,
                },
                "engine": {
                    "max_workers": args.max_concurrency,
                    "seed": 0,
                },
            },
        )

    # ---- Phase 1: parallel explore across engines. ----
    explore_configs = [
        gepa_cfg(f"{args.name}_p1_gepa", args.explore_evals),
        OptimizeAnythingConfig(
            engine="meta_harness",
            name=f"{args.name}_p1_meta",
            max_evals=args.explore_evals,
            max_concurrency=args.max_concurrency,
            run_dir=f"runs/{args.name}_p1_meta",
            output_dir=f"outputs/{args.name}_p1_meta",
        ),
    ]

    print(f"=== Phase 1: exploring with {len(explore_configs)} engines, "
          f"{args.explore_evals} evals each ===")
    best = optimize_best_of(
        seed,
        configs=explore_configs,
        name=f"{args.name}_p1",
        **task,
    )
    print(f"Phase 1 winner score: {best.best_score}")

    winner_path = Path(f"outputs/{args.name}_p1_winner.go")
    winner_path.write_text(best.best_candidate)

    # ---- Phase 2: fresh gepa engine seeded with the winner. ----
    print(f"=== Phase 2: fresh gepa engine, {args.continue_evals} evals, "
          f"seeded with Phase 1 winner ===")
    final = optimize_anything(
        seed_candidate=best.best_candidate,
        test_set=testset,
        config=gepa_cfg(f"{args.name}_p2", args.continue_evals),
        **task,
    )

    print("=== omni best candidate ===")
    print(final.best_candidate[:2000])
    print("phase1 best:", best.best_score)
    print("final best (val):", final.best_score)
    print("held-out test:", final.metadata.get("test_score"),
          "| phase-2 seed held-out:",
          final.metadata.get("baseline_test_score"))

    out = Path(f"outputs/{args.name}_final.go")
    out.write_text(final.best_candidate)
    summary = {
        "phase1_best": best.best_score,
        "final_val": final.best_score,
        "test": final.metadata.get("test_score"),
        "phase2_seed_test": final.metadata.get("baseline_test_score"),
    }
    Path(f"outputs/{args.name}_summary.json").write_text(
        json.dumps(summary, indent=1),
    )
    print(f"wrote {out}")


if __name__ == "__main__":
    main()
