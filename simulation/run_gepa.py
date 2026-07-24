#!/usr/bin/env python3
"""Run a GEPA optimization over lnd's path finding parameters.

Requires: pip install "gepa[full]", a built routesim binary (set
ROUTESIM_BIN or have it on PATH), and a scenario corpus from
gen_scenarios.py. The reflection LLM defaults to gpt-5.6-sol; set
OPENAI_API_KEY (or pass --reflection-lm for another provider).
"""

import argparse
import subprocess
from pathlib import Path

from gepa.optimize_anything import optimize_anything, OptimizeAnythingConfig

from codex_lm import CodexLM
from evaluate import ROUTESIM, evaluate

OBJECTIVE = """
Maximize Lightning Network payment reliability in a network simulator by
tuning lnd's path finding parameters (this JSON candidate). Higher payment
success rate dominates; fewer retry attempts and lower fees are secondary.
The candidate must remain valid JSON with the same keys.
"""

BACKGROUND = """
The candidate configures lnd's route selection heuristic:
- estimator: "apriori" or "bimodal" — the probability model used to predict
  whether a channel can forward a given amount.
- apriori: penalty_half_life_sec (how fast failure memories fade),
  hop_probability (prior success probability of an unknown hop, 0-1),
  weight (0-1, how much to trust the prior vs observed history),
  capacity_fraction (0.75-1.0, soft cap of amount vs channel capacity).
- bimodal: scale_msat (assumed liquidity concentration at channel ends),
  node_weight (0-1, weight of other channels of the same node),
  decay_time_sec (information decay).
- attempt_cost_msat / attempt_cost_ppm: virtual cost of one attempt; higher
  values prefer fewer, more reliable attempts over cheap risky ones.
- min_probability: minimum route success probability to even try (0-1).
The simulated networks have hidden liquidity drawn mostly from a bimodal
distribution (funds concentrated on one side of each channel).
Constraints: probabilities and weights must stay in [0,1],
capacity_fraction in [0.75,1], scale_msat > 0, half life > 0.
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", default="corpus")
    parser.add_argument("--name", default="pathfind_params")
    parser.add_argument("--max-evals", type=int, default=None,
                        help="default: 20 x len(valset)")
    parser.add_argument("--reflection-lm", default="codex:gpt-5.6-sol",
                        help="codex:<model> runs the Codex CLI headless; "
                        "any other value is passed to LiteLLM directly")
    parser.add_argument("--max-concurrency", type=int, default=8)
    args = parser.parse_args()

    corpus = Path(args.corpus)
    trainset = sorted(str(p) for p in (corpus / "train").glob("*.json"))
    valset = sorted(str(p) for p in (corpus / "val").glob("*.json"))
    testset = sorted(str(p) for p in (corpus / "test").glob("*.json"))
    if not trainset or not valset:
        raise SystemExit(f"no corpus at {corpus}; run gen_scenarios.py")

    seed = subprocess.run(
        [ROUTESIM, "--dump-defaults"], capture_output=True, text=True,
        check=True,
    ).stdout

    max_evals = args.max_evals or 20 * len(valset)

    reflection_lm = args.reflection_lm
    if reflection_lm.startswith("codex:"):
        reflection_lm = CodexLM(model=reflection_lm.split(":", 1)[1])

    result = optimize_anything(
        seed_candidate=seed,
        evaluator=lambda cand, ex: evaluate(cand, ex),
        dataset=trainset,
        valset=valset,
        test_set=testset,
        objective=OBJECTIVE.strip(),
        background=BACKGROUND.strip(),
        config=OptimizeAnythingConfig(
            engine="gepa",
            name=args.name,
            max_evals=max_evals,
            max_concurrency=args.max_concurrency,
            run_dir=f"runs/{args.name}",
            output_dir=f"outputs/{args.name}",
            engine_config={
                "reflection": {
                    "reflection_lm": reflection_lm,
                    "reflection_minibatch_size": 4,
                },
                "engine": {"max_workers": args.max_concurrency, "seed": 0},
            },
        ),
    )

    print("=== best candidate ===")
    print(result.best_candidate)
    print("best (val) score:", result.best_score)
    print("held-out test:", result.metadata.get("test_score"),
          "| seed held-out:", result.metadata.get("baseline_test_score"))


if __name__ == "__main__":
    main()
