#!/usr/bin/env python3
"""Engine adjudication runner (exp-018).

exp-011 watched three evolved lineages settle into the same ~0.64 band and
called it a paradigm ceiling. Every one of those runs used engine="gepa",
so the observation cannot distinguish "the problem has a ceiling" from
"the gepa engine has an attractor". This script separates the two: it
gives each requested optimizer engine an INDEPENDENT run from the SAME
seed candidate, with the SAME eval budget, over the SAME corpus, and then
reports the per-engine verdicts side by side.

This is adjudication, not ensembling. There is deliberately no single
"winner" artifact: the scientific output is outputs/<name>_adjudication.json
plus one best candidate per engine. If several engines independently stall
at the same score, that is evidence for a ceiling; if one engine walks past
the band, exp-011's conclusion was an artifact of the optimizer.

An optional Phase 2 continuation (--continue-evals, off by default)
reseeds a fresh engine with the best candidate any arm produced, which is
the old omni recipe kept as a follow-up rather than the main event.
"""

import argparse
import json
import os
import shutil
import time
import traceback
from pathlib import Path

from gepa.optimize_anything import (
    OptimizeAnythingConfig,
    get_engine_cls,
    list_engines,
    optimize_anything,
)

from codex_lm import CodexLM
from evaluate_code import REPO, batch_evaluate, evaluate
from run_gepa_code import BACKGROUND, OBJECTIVE

# Engines that drive their loop with a `claude --print` subprocess. They
# need the CLI on PATH, and they inherit our process environment, so the
# sterile-config-home discipline used by claude_lm.py has to be applied
# here, in the parent, before the engine spawns anything.
CLAUDE_SUBPROCESS_ENGINES = frozenset({"meta_harness", "autoresearch"})

# Engines that reach an LLM through LiteLLM rather than a local CLI.
LITELLM_ENGINES = frozenset({"best_of_n"})

# How each engine spends the two budget currencies. Every engine in this
# gepa version accepts BOTH: the eval-call cap is enforced centrally by the
# eval server (so it is the same unit for everyone, and the honest axis for
# comparison), while the dollar cap is a proposer-spend cap each engine
# translates into its own native mechanism. There is currently NO engine
# that accepts only max_token_cost, so --max-cost-per-engine is a secondary
# guard rail rather than a substitute budget. The strings below are recorded
# verbatim in the adjudication JSON so the comparison states its own terms.
BUDGET_KINDS = {
    "gepa": {
        "eval_budget": "max_evals (eval server, central)",
        "cost_budget": "max_token_cost -> EngineConfig.max_reflection_cost",
        "proposer": "CodexLM (codex exec) reflection",
    },
    "meta_harness": {
        "eval_budget": "max_evals (eval server, central)",
        "cost_budget": "max_token_cost -> claude --max-budget-usd per session",
        "proposer": "claude CLI agent, iterative frontier reader",
    },
    "autoresearch": {
        "eval_budget": "max_evals (eval server, central)",
        "cost_budget": "max_token_cost -> claude --max-budget-usd per session",
        "proposer": "claude CLI agent, single ralph-resumed session",
    },
    "best_of_n": {
        "eval_budget": "max_evals (eval server, central)",
        "cost_budget": "max_token_cost -> LM.total_cost stop check",
        "proposer": "LiteLLM single-shot sampling, no search",
    },
}


def sterile_claude_env() -> dict:
    """Point the claude-subprocess engines at the harness's sterile home.

    gepa's meta_harness/autoresearch engines spawn `claude --print` with
    `{**os.environ, ...}`, so whatever we export here reaches the proposer.
    The default ~/.claude leaks user-level settings, hooks and memories into
    the session — one reflection came back discussing a Stop hook instead of
    emitting a router — so we mirror claude_lm.py: a dedicated
    CLAUDE_CONFIG_DIR, plus the harness OAuth token when one is on disk and
    the environment does not already carry it. The token is never printed.
    """
    applied = {}

    config_home = Path.home() / "codez" / "claude-harness-home"
    config_home.mkdir(parents=True, exist_ok=True)
    os.environ["CLAUDE_CONFIG_DIR"] = str(config_home)
    applied["CLAUDE_CONFIG_DIR"] = str(config_home)

    token_file = Path.home() / "codez" / ".claude-harness-token"
    if not os.environ.get("CLAUDE_CODE_OAUTH_TOKEN") and token_file.exists():
        os.environ["CLAUDE_CODE_OAUTH_TOKEN"] = token_file.read_text().strip()
        applied["CLAUDE_CODE_OAUTH_TOKEN"] = "[set from ~/codez/.claude-harness-token]"
    elif os.environ.get("CLAUDE_CODE_OAUTH_TOKEN"):
        applied["CLAUDE_CODE_OAUTH_TOKEN"] = "[inherited from environment]"

    return applied


def resolve_engines(requested: str) -> list:
    """Split the --engines list and check it against what is installed.

    Fails before any evaluation happens: an eight-hour adjudication that
    dies on the third arm because the engine name was never registered is
    the worst possible way to learn about a typo.
    """
    names = [n.strip() for n in requested.split(",") if n.strip()]
    if not names:
        raise SystemExit("--engines is empty; pass a comma list, e.g. gepa,meta_harness")

    seen = []
    for name in names:
        if name not in seen:
            seen.append(name)

    installed = list_engines()
    missing = [n for n in seen if n not in installed]
    if missing:
        raise SystemExit(
            f"engine(s) not installed: {', '.join(missing)}. "
            f"This gepa build registers: {', '.join(installed)}. "
            "Reinstall the venv from ~/codez/gepa if you expected more."
        )
    return seen


def preflight_engines(names: list) -> list:
    """Return human-readable warnings about each engine's external needs.

    These are warnings rather than hard failures: the engines run their own
    preflight at launch, and one arm's missing dependency must not stop the
    other arms from producing a verdict.
    """
    warnings = []
    if any(n in CLAUDE_SUBPROCESS_ENGINES for n in names) and not shutil.which("claude"):
        warnings.append(
            "the claude CLI is not on PATH; meta_harness/autoresearch will fail at launch"
        )
    if "gepa" in names and not shutil.which("codex"):
        warnings.append(
            "the codex CLI is not on PATH; gepa reflection will degrade to stub proposals"
        )
    if any(n in LITELLM_ENGINES for n in names) and not os.environ.get("ANTHROPIC_API_KEY"):
        warnings.append(
            "best_of_n samples through LiteLLM and no ANTHROPIC_API_KEY is set"
        )
    return warnings


def check_engine_config(engine: str, config: OptimizeAnythingConfig) -> str:
    """Instantiate the engine to validate its engine_config keys.

    Every engine parses engine_config at construction and raises on an
    unknown key, so building one here turns a typo into a dry-run warning
    instead of a mid-run arm failure. Returns an empty string when the
    config is accepted.
    """
    try:
        get_engine_cls(engine)(config)
    except Exception as exc:
        return f"{engine}: engine_config rejected — {type(exc).__name__}: {exc}"
    return ""


def build_config(engine: str, run_name: str, max_evals: int, args) -> OptimizeAnythingConfig:
    """Build one engine's config: same budget, same corpus, own workspace."""
    engine_config = {}

    if engine == "gepa":
        # This mirrors run_gepa_code.py's gepa arm so the gepa result stays
        # comparable with exp-011's numbers rather than being a new
        # configuration that happens to share a name.
        engine_config = {
            "reflection": {
                "reflection_lm": CodexLM(
                    model=args.reflection_model,
                    require_marker="package main",
                    timeout=args.reflection_timeout,
                    effort=args.reflection_effort,
                ),
                "reflection_minibatch_size": 3,
            },
            "engine": {
                "max_workers": args.max_concurrency,
                "seed": 0,
                # Per-example AND per-objective Pareto cells, fed by the
                # evaluator's info["scores"] axes (success / retry_efficiency
                # / fee_efficiency), so specialists survive selection.
                "frontier_type": "hybrid",
                "cache_evaluation": args.gepa_cache,
                "max_candidate_proposals": args.gepa_max_proposals,
                "raise_on_exception": False,
            },
        }
    elif engine == "meta_harness":
        engine_config = {
            "model": args.claude_model,
            "max_candidates_per_iter": args.meta_candidates_per_iter,
        }
        if args.claude_effort:
            engine_config["effort"] = args.claude_effort
    elif engine == "autoresearch":
        engine_config = {"model": args.claude_model}
        if args.claude_effort:
            engine_config["effort"] = args.claude_effort
    elif engine == "best_of_n":
        engine_config = {"model": args.claude_model}

    return OptimizeAnythingConfig(
        engine=engine,
        name=run_name,
        max_evals=max_evals,
        max_token_cost=args.max_cost_per_engine,
        max_concurrency=args.max_concurrency,
        run_dir=f"runs/{run_name}",
        output_dir=f"outputs/{run_name}",
        sandbox=args.sandbox,
        engine_config=engine_config,
    )


def budget_record(engine: str, max_evals: int, args) -> dict:
    """The budget this arm was actually given, in the units it accepts."""
    record = dict(BUDGET_KINDS.get(engine, {}))
    record["max_evals"] = max_evals
    record["max_token_cost_usd"] = args.max_cost_per_engine
    if engine == "gepa":
        # With caching on, a repeated (candidate, example) pair is served
        # from the store and never reaches the eval server, so max_evals
        # counts cache MISSES for this arm and raw calls for the others.
        # That makes the eval budgets unequal in real work done, which is
        # exactly the kind of thing an adjudication has to say out loud.
        record["eval_accounting"] = (
            "cache_evaluation=True: budget counts cache misses only"
            if args.gepa_cache
            else "cache_evaluation=False: budget counts every eval call"
        )
        record["proposal_cap"] = args.gepa_max_proposals
    elif engine == "meta_harness":
        record["eval_accounting"] = "budget counts every eval call"
        record["proposal_cap"] = f"{args.meta_candidates_per_iter} candidates/iteration"
    else:
        record["eval_accounting"] = "budget counts every eval call"
    return record


def run_one(engine: str, config: OptimizeAnythingConfig, seed: str, task: dict) -> dict:
    """Run a single engine arm and summarize it, swallowing its failures.

    A crashed arm records its traceback and returns; the remaining arms
    still produce verdicts. An adjudication that loses two good results
    because the third engine could not find its CLI has adjudicated
    nothing.
    """
    started = time.time()
    entry = {"engine": engine, "run_name": config.name}
    try:
        result = optimize_anything(seed_candidate=seed, config=config, **task)
    except Exception as exc:
        entry.update(
            status="error",
            error=f"{type(exc).__name__}: {exc}",
            traceback=traceback.format_exc()[-4000:],
            wall_time=time.time() - started,
        )
        return entry

    metadata = result.metadata or {}
    entry.update(
        status="ok",
        best_val_score=result.best_score,
        test_score=metadata.get("test_score"),
        baseline_test_score=metadata.get("baseline_test_score"),
        evals_used=result.total_evals,
        budget_status=metadata.get("budget"),
        wall_time=metadata.get("wall_time", time.time() - started),
        proposer_cost_usd=metadata.get("adapter_cost"),
        total_cost_usd=metadata.get("total_cost"),
        output_dir=metadata.get("output_dir"),
        best_candidate=result.best_candidate,
    )
    return entry


def format_table(entries: list) -> str:
    """Render the per-engine comparison as a fixed-width table."""
    header = (
        f"{'engine':<14} {'status':<7} {'val':>8} {'test':>8} "
        f"{'seed test':>10} {'evals':>7} {'wall':>9} {'prop $':>8}"
    )
    rows = [header, "-" * len(header)]
    for entry in entries:
        if entry.get("status") != "ok":
            rows.append(
                f"{entry['engine']:<14} {'ERROR':<7} "
                f"{entry.get('error', '')[:60]}"
            )
            continue

        def num(key, fmt):
            value = entry.get(key)
            return fmt.format(value) if isinstance(value, (int, float)) else "-"

        wall = entry.get("wall_time")
        wall_s = f"{wall / 60:.1f}m" if isinstance(wall, (int, float)) else "-"
        rows.append(
            f"{entry['engine']:<14} {'ok':<7} "
            f"{num('best_val_score', '{:>8.4f}')} "
            f"{num('test_score', '{:>8.4f}')} "
            f"{num('baseline_test_score', '{:>10.4f}')} "
            f"{num('evals_used', '{:>7d}')} "
            f"{wall_s:>9} "
            f"{num('proposer_cost_usd', '{:>8.2f}')}"
        )
    return "\n".join(rows)


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Run one independent optimization per engine from a "
        "shared seed and budget, then compare the verdicts.",
    )
    parser.add_argument("--corpus", default="corpus")
    parser.add_argument("--name", default="adj1")
    parser.add_argument("--engines", default="gepa,meta_harness,autoresearch",
                        help="comma list of installed optimizer engines; "
                        "each gets its own independent run")
    parser.add_argument("--evals-per-engine", type=int, default=150,
                        help="eval budget granted to EACH engine")
    parser.add_argument("--max-cost-per-engine", type=float, default=None,
                        help="optional proposer-spend cap in USD per engine. "
                        "Every engine here also accepts --evals-per-engine, "
                        "so this is a secondary guard rail, not a substitute "
                        "budget; the units used are recorded per arm.")
    parser.add_argument("--continue-evals", type=int, default=0,
                        help="Phase 2: reseed one fresh engine with the best "
                        "candidate any arm produced. 0 disables it.")
    parser.add_argument("--continue-engine", default="gepa",
                        help="engine used for the optional Phase 2 run")
    parser.add_argument("--max-concurrency", type=int, default=4)
    parser.add_argument("--seed-file", default=None,
                        help="seed candidate .go file (default: the in-tree "
                        "candidate_impl.go). exp-018 should point this at "
                        "exp-011's seed so the arms share its starting point.")
    parser.add_argument("--reflection-model", default="gpt-5.6-sol",
                        help="CodexLM model for the gepa arm's reflection")
    parser.add_argument("--reflection-effort", default="xhigh")
    parser.add_argument("--reflection-timeout", type=int, default=600)
    parser.add_argument("--claude-model", default="claude-sonnet-4-6",
                        help="proposer model for the claude-CLI engines")
    parser.add_argument("--claude-effort", default=None)
    parser.add_argument("--meta-candidates-per-iter", type=int, default=3)
    parser.add_argument("--gepa-cache", action="store_true", default=True,
                        help="cache (candidate, example) evals in the gepa "
                        "arm, as run_gepa_code.py does")
    parser.add_argument("--no-gepa-cache", dest="gepa_cache",
                        action="store_false",
                        help="disable gepa's eval cache so every arm's "
                        "budget counts the same unit of work")
    parser.add_argument("--gepa-max-proposals", type=int, default=60)
    parser.add_argument("--sandbox", action="store_true", default=True,
                        help="OS-jail the claude-CLI engines (default)")
    parser.add_argument("--no-sandbox", dest="sandbox", action="store_false")
    parser.add_argument("--sterile-claude-home", action="store_true",
                        default=True,
                        help="run claude-CLI engines under the harness's "
                        "sterile CLAUDE_CONFIG_DIR (default)")
    parser.add_argument("--no-sterile-claude-home",
                        dest="sterile_claude_home", action="store_false")
    parser.add_argument("--dry-run", action="store_true",
                        help="resolve engines, print the plan and budgets, "
                        "and exit without evaluating anything")
    args = parser.parse_args()

    engines = resolve_engines(args.engines)

    corpus = Path(args.corpus)
    trainset = sorted(str(p) for p in (corpus / "train").glob("*.json"))
    valset = sorted(str(p) for p in (corpus / "val").glob("*.json"))
    testset = sorted(str(p) for p in (corpus / "test").glob("*.json"))
    if not trainset or not valset:
        raise SystemExit(f"no corpus at {corpus}; run gen_scenarios.py")

    if args.seed_file:
        seed_path = Path(args.seed_file)
    else:
        seed_path = REPO / "cmd" / "routesim" / "candidate_impl.go"
    if not seed_path.exists():
        raise SystemExit(f"seed candidate not found: {seed_path}")
    seed = seed_path.read_text()

    claude_env = {}
    if args.sterile_claude_home and any(
        e in CLAUDE_SUBPROCESS_ENGINES for e in engines
    ):
        claude_env = sterile_claude_env()

    plan = {
        "name": args.name,
        "design": "independent run per engine from one seed and budget "
                  "(adjudication, not best-of ensembling)",
        "engines": engines,
        "corpus": str(corpus),
        "corpus_sizes": {
            "train": len(trainset),
            "val": len(valset),
            "test": len(testset),
        },
        "seed_file": str(seed_path),
        "seed_bytes": len(seed),
        "evals_per_engine": args.evals_per_engine,
        "max_cost_per_engine_usd": args.max_cost_per_engine,
        "continue_evals": args.continue_evals,
        "continue_engine": args.continue_engine if args.continue_evals else None,
        "sandbox": args.sandbox,
        "claude_env": claude_env,
        "budgets": {
            engine: budget_record(engine, args.evals_per_engine, args)
            for engine in engines
        },
    }

    print("=== exp-018 engine adjudication plan ===")
    print(json.dumps(plan, indent=1))

    checks = list(preflight_engines(engines))
    for engine in engines:
        problem = check_engine_config(
            engine,
            build_config(engine, f"{args.name}_{engine}", args.evals_per_engine, args),
        )
        if problem:
            checks.append(problem)
    for warning in checks:
        print(f"WARNING: {warning}")
    plan["preflight_warnings"] = checks

    if args.dry_run:
        print("\n--dry-run: resolved the plan, evaluated nothing, exiting.")
        return

    task = dict(
        evaluator=lambda cand, ex: evaluate(cand, ex),
        batch_evaluator=batch_evaluate,
        dataset=trainset,
        valset=valset,
        test_set=testset,
        objective=OBJECTIVE.strip(),
        background=BACKGROUND.strip(),
    )

    outputs = Path("outputs")
    outputs.mkdir(parents=True, exist_ok=True)

    entries = []
    for engine in engines:
        run_name = f"{args.name}_{engine}"
        print(f"\n=== arm: {engine} ({args.evals_per_engine} evals) ===")
        config = build_config(engine, run_name, args.evals_per_engine, args)
        entry = run_one(engine, config, seed, task)
        entry["budget"] = budget_record(engine, args.evals_per_engine, args)

        candidate = entry.pop("best_candidate", None)
        if candidate:
            best_path = outputs / f"{args.name}_{engine}_best.go"
            best_path.write_text(candidate)
            entry["best_candidate_file"] = str(best_path)
            print(f"wrote {best_path}")
        if entry["status"] == "ok":
            print(f"{engine}: val={entry['best_val_score']} "
                  f"test={entry.get('test_score')} "
                  f"evals={entry.get('evals_used')}")
        else:
            print(f"{engine}: FAILED — {entry['error']}")
        entries.append(entry)

    # Phase 2 is the old omni continuation, kept behind a flag: reseed one
    # fresh engine with the best candidate any arm produced. It answers a
    # different question than the adjudication (does a fresh optimizer break
    # a plateau?) so it is recorded as its own entry, never merged into the
    # per-engine comparison.
    continuation = None
    scored = [e for e in entries
              if e.get("status") == "ok" and e.get("best_val_score") is not None]
    if args.continue_evals > 0 and scored:
        leader = max(scored, key=lambda e: e["best_val_score"])
        leader_file = leader.get("best_candidate_file")
        if leader_file:
            print(f"\n=== Phase 2: {args.continue_engine}, "
                  f"{args.continue_evals} evals, seeded with "
                  f"{leader['engine']}'s best ===")
            run_name = f"{args.name}_continue"
            config = build_config(
                args.continue_engine, run_name, args.continue_evals, args,
            )
            continuation = run_one(
                args.continue_engine, config, Path(leader_file).read_text(), task,
            )
            continuation["seeded_from"] = leader["engine"]
            continuation["seed_val_score"] = leader["best_val_score"]
            candidate = continuation.pop("best_candidate", None)
            if candidate:
                path = outputs / f"{args.name}_continued_best.go"
                path.write_text(candidate)
                continuation["best_candidate_file"] = str(path)

    table = format_table(entries)
    print("\n=== per-engine verdicts ===")
    print(table)
    if continuation is not None and continuation.get("status") == "ok":
        print(f"\nPhase 2 ({args.continue_engine} from "
              f"{continuation['seeded_from']}): "
              f"val={continuation['best_val_score']} "
              f"test={continuation.get('test_score')}")

    adjudication = {
        "plan": plan,
        "engines": entries,
        "continuation": continuation,
        "table": table,
        "caveat": "eval budgets are equal in COUNT; read plan.budgets for the "
                  "unit each engine counts and for the proposal caps, which "
                  "are not the same across engines.",
    }
    path = outputs / f"{args.name}_adjudication.json"
    path.write_text(json.dumps(adjudication, indent=1))
    print(f"\nwrote {path}")


if __name__ == "__main__":
    main()
