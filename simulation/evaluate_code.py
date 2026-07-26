#!/usr/bin/env python3
"""Code-mode evaluator: the candidate is the full Go source of
cmd/routesim/candidate_impl.go — an entire routing algorithm.

Each eval compiles a routesim binary with the candidate swapped in via
`go build -overlay` (no working-tree mutation, parallel-safe) and runs it
with --router=candidate. Compile errors come back as feedback, which is
the highest-signal input a reflective proposer can get.
"""

import json
import os
import re
import subprocess
import tempfile
from pathlib import Path

import evaluate as params_eval

REPO = Path(os.environ.get(
    "LND_REPO",
    Path(__file__).resolve().parent.parent,
))
GO = os.environ.get("GO_BIN", "go")

# Tokens that have no business in a routing algorithm and defeat the
# information hiding of the simulator (reward-hack guard). The second
# group enforces the sealed-view invariant in the evaluator itself
# rather than by post-hoc grep: GraphSession callbacks receive the
# sealed view, and any candidate naming the hidden-state surfaces is
# probing for an escape.
BANNED = re.compile(
    r'\b(unsafe|reflect|os/exec|syscall|net/http|io/ioutil)\b|'
    r'"os"|_test\b|'
    r'\b(LocalBalances|AssignLiquidity|BalanceNodeChannels|SendHtlc)\b|'
    r'\b(HoldHtlc|SettleHold|ReleaseHold)\b|'
    r'\*\s*routing\.SimGraph',
)

FENCE = re.compile(r"^```(?:go)?\s*$|^```\s*$", re.MULTILINE)


def extract_source(candidate: str) -> str:
    """Strip markdown fences if the proposer wrapped the file in them."""
    text = candidate.strip()
    if text.startswith("```"):
        text = FENCE.sub("", text).strip()

    # Agentic proposers sometimes prepend prose despite instructions;
    # a Go file must start at its package clause, so slice from there.
    if not text.startswith("package "):
        idx = text.find("package main")
        if idx > 0:
            text = text[idx:]

    return text + "\n"


def compile_candidate(source: str, workdir: Path) -> tuple[Path, str]:
    """Compile a routesim binary with the candidate overlaid. Returns
    (binary path, "") on success or (None, compiler output) on failure."""
    cand_path = workdir / "candidate_impl.go"
    cand_path.write_text(source)

    overlay = workdir / "overlay.json"
    target = str(REPO / "cmd" / "routesim" / "candidate_impl.go")
    overlay.write_text(json.dumps(
        {"Replace": {target: str(cand_path)}},
    ))

    binary = workdir / "routesim"
    try:
        proc = subprocess.run(
            [GO, "build", "-overlay", str(overlay), "-o", str(binary),
             "./cmd/routesim"],
            cwd=REPO, capture_output=True, text=True, timeout=300,
        )
    except subprocess.TimeoutExpired:
        # Under concurrent engines a slow build must score zero, not
        # crash the whole run (raise_on_exception aborts on evaluator
        # exceptions).
        return None, "go build timed out after 300s (host under load?)"
    if proc.returncode != 0:
        return None, proc.stderr[-4000:]

    return binary, ""


def run_compiled(binary, example) -> tuple[float, dict]:
    """Run a compiled candidate binary on one scenario file and score it."""
    # A pathological candidate (infinite loop, quadratic blowup) must
    # score 0, not crash the whole optimization run. 120s is generous:
    # a healthy router does a full scenario batch in well under a
    # second, so a timeout means the candidate is broken.
    try:
        proc = subprocess.run(
            [str(binary), "--scenarios", str(example),
             "--router", "candidate"],
            capture_output=True, text=True, timeout=120,
        )
    except subprocess.TimeoutExpired:
        return 0.0, {
            "error": "timeout: candidate did not finish in 120s",
            "hint": "The router likely loops without making progress "
            "(e.g. RequestRoute never returns an error to terminate "
            "the payment, or splits without shrinking). Ensure every "
            "path terminates and shard amounts strictly decrease.",
        }
    if proc.returncode != 0:
        return 0.0, {
            "error": f"runtime failure: {proc.stderr[-2000:]}",
        }

    try:
        output = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return 0.0, {
            "error": "candidate produced no valid JSON output",
            "stdout_tail": proc.stdout[-1000:],
        }

    try:
        agg = output["aggregate"]
        results = output["results"]
    except (KeyError, TypeError):
        return 0.0, {
            "error": "routesim output missing aggregate/results",
        }

    extra_attempts = min(
        max(agg["attempts_per_scenario"] - 1.0, 0.0),
        params_eval.ATTEMPT_CAP,
    )
    fee_ppm = min(agg["fee_ppm_on_success"], params_eval.FEE_PPM_CAP)

    score = (
        agg["success_rate"]
        - params_eval.ATTEMPT_WEIGHT * extra_attempts
        - params_eval.FEE_WEIGHT * fee_ppm
    )

    # Separate objective axes for Pareto-frontier preservation
    # (frontier_type="hybrid" in the engine config). Retries and parts
    # are disentangled: a mandatory 3-shard MPP payment is not "2 extra
    # attempts" of waste, while 3 failures before 1 settle are. Axes are
    # oriented so higher is better.
    settled_parts = sum(
        1
        for res in results
        for att in (res.get("attempts") or [])
        if att.get("success")
    )
    total_attempts = agg["total_attempts"]
    retries = max(total_attempts - settled_parts, 0)
    num = max(agg["num_scenarios"], 1)
    scores = {
        "success": agg["success_rate"],
        "retry_efficiency": -min(retries / num, 25.0),
        "fee_efficiency": -fee_ppm / params_eval.FEE_PPM_CAP,
    }

    return score, {
        "score": score,
        "scores": scores,
        "aggregate": agg,
        "failed_payments": params_eval.summarize_failures(results),
        "hint": (
            "success_rate dominates; attempts and fee ppm apply small "
            "penalties. The router only sees gossip (no hidden "
            "balances), its own channel balances, and per-attempt "
            "failure feedback via ReportAttempt."
        ),
    }


def evaluate(candidate: str, example) -> tuple[float, dict]:
    """The optimize_anything evaluator contract for code candidates."""
    source = extract_source(candidate)

    banned = BANNED.search(source)
    if banned:
        return 0.0, {
            "error": f"banned identifier {banned.group(0)!r}: candidates "
            "must not use unsafe/reflect/os/exec/net or probe the hidden "
            "simulator state — pure routing logic only.",
        }

    with tempfile.TemporaryDirectory(prefix="routesim-cand-") as tmp:
        workdir = Path(tmp)

        binary, compile_err = compile_candidate(source, workdir)
        if binary is None:
            return 0.0, {
                "error": "compile failed",
                "compiler_output": compile_err,
                "hint": "Return the COMPLETE contents of "
                "candidate_impl.go (package main), defining "
                "newCandidateRouter with the exact contract signature.",
            }

        return run_compiled(binary, example)


def batch_evaluate(pairs) -> list:
    """Batch form of the evaluator contract: one compile per unique
    candidate instead of one per (candidate, example) pair.

    A candidate's valset pass previously ran `go build` once per
    example — eight identical compiles for an eight-file valset. Here
    the pairs are grouped by extracted source, each unique candidate is
    compiled once, and its scenario runs execute against the shared
    binary. Returns results in input order.
    """
    from concurrent.futures import ThreadPoolExecutor

    order = list(pairs)
    by_source: dict = {}
    for idx, (candidate, example) in enumerate(order):
        by_source.setdefault(extract_source(candidate), []).append(
            (idx, candidate, example),
        )

    results: list = [None] * len(order)

    def run_group(group) -> None:
        source, members = group
        banned = BANNED.search(source)
        if banned:
            outcome = (0.0, {
                "error": f"banned identifier {banned.group(0)!r}: "
                "candidates must not use unsafe/reflect/os/exec/net or "
                "probe the hidden simulator state — pure routing logic "
                "only.",
            })
            for idx, _, _ in members:
                results[idx] = outcome
            return

        with tempfile.TemporaryDirectory(prefix="routesim-cand-") as tmp:
            workdir = Path(tmp)
            binary, compile_err = compile_candidate(source, workdir)
            if binary is None:
                outcome = (0.0, {
                    "error": "compile failed",
                    "compiler_output": compile_err,
                    "hint": "Return the COMPLETE contents of "
                    "candidate_impl.go (package main), defining "
                    "newCandidateRouter with the exact contract "
                    "signature.",
                })
                for idx, _, _ in members:
                    results[idx] = outcome
                return

            for idx, _, example in members:
                results[idx] = run_compiled(binary, example)

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(run_group, by_source.items()))

    return results


if __name__ == "__main__":
    import sys

    seed_path = REPO / "cmd" / "routesim" / "candidate_impl.go"
    score, info = evaluate(seed_path.read_text(), sys.argv[1])
    print(f"score={score:.4f}")
    print(json.dumps(info.get("aggregate", info), indent=2))
