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
# information hiding of the simulator (reward-hack guard).
BANNED = re.compile(
    r'\b(unsafe|reflect|os/exec|syscall|net/http|io/ioutil)\b|'
    r'"os"|_test\b',
)

FENCE = re.compile(r"^```(?:go)?\s*$|^```\s*$", re.MULTILINE)


def extract_source(candidate: str) -> str:
    """Strip markdown fences if the proposer wrapped the file in them."""
    text = candidate.strip()
    if text.startswith("```"):
        text = FENCE.sub("", text).strip()
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
    proc = subprocess.run(
        [GO, "build", "-overlay", str(overlay), "-o", str(binary),
         "./cmd/routesim"],
        cwd=REPO, capture_output=True, text=True, timeout=300,
    )
    if proc.returncode != 0:
        return None, proc.stderr[-4000:]

    return binary, ""


def evaluate(candidate: str, example) -> tuple[float, dict]:
    """The optimize_anything evaluator contract for code candidates."""
    source = extract_source(candidate)

    banned = BANNED.search(source)
    if banned:
        return 0.0, {
            "error": f"banned identifier {banned.group(0)!r}: candidates "
            "must not use unsafe/reflect/os/exec/net — pure routing "
            "logic only.",
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

    agg = output["aggregate"]

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

    return score, {
        "score": score,
        "aggregate": agg,
        "failed_payments": params_eval.summarize_failures(
            output["results"],
        ),
        "hint": (
            "success_rate dominates; attempts and fee ppm apply small "
            "penalties. The router only sees gossip (no hidden "
            "balances), its own channel balances, and per-attempt "
            "failure feedback via ReportAttempt."
        ),
    }


if __name__ == "__main__":
    import sys

    seed_path = REPO / "cmd" / "routesim" / "candidate_impl.go"
    score, info = evaluate(seed_path.read_text(), sys.argv[1])
    print(f"score={score:.4f}")
    print(json.dumps(info.get("aggregate", info), indent=2))
