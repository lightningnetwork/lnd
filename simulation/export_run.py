#!/usr/bin/env python3
"""Export a GEPA run into the command-center dashboard's data/run.json.

Joins two artifact sources:
- <run_dir>/run_log.json — per-iteration parent + minibatch scores,
- <output_dir>/evals/*.json — per-eval candidate text and score,
reconstructing the candidate lineage in first-appearance order (the eval
server sees each proposal once per minibatch example, in iteration order).
"""

import argparse
import glob
import hashlib
import json
from pathlib import Path


def load_evals(output_dir: Path) -> list[dict]:
    files = sorted(
        glob.glob(str(output_dir / "evals" / "*.json")),
        key=lambda p: int(Path(p).stem),
    )
    return [json.load(open(f)) for f in files]


def mean(xs):
    xs = [x for x in xs if x is not None]
    return sum(xs) / len(xs) if xs else 0.0


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-dir", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--run-id", default="gepa-run")
    parser.add_argument("--reflection-lm", default="codex:gpt-5.6-sol")
    args = parser.parse_args()

    run_dir = Path(args.run_dir)
    output_dir = Path(args.output_dir)

    run_log = json.load(open(run_dir / "run_log.json"))
    accepted_texts = {
        c["current_candidate"]
        for c in json.load(open(run_dir / "candidates.json"))
    }
    summary = json.load(open(output_dir / "summary.json"))
    evals = load_evals(output_dir)

    # Group evals by candidate text in first-appearance order.
    order: list[str] = []
    by_hash: dict[str, dict] = {}
    for record in evals:
        text = record["candidate"]
        h = hashlib.sha1(text.encode()).hexdigest()
        if h not in by_hash:
            by_hash[h] = {"text": text, "scores": []}
            order.append(h)
        by_hash[h]["scores"].append(record.get("score"))

    def parse(text):
        try:
            return json.loads(text)
        except (ValueError, TypeError):
            # Code-mode candidates aren't JSON; ship the raw text.
            return {"source": text}

    seed_hash = order[0]
    seed_group = by_hash[seed_hash]
    seed_score = mean(seed_group["scores"])

    candidates = [{
        "id": 0,
        "parent": None,
        "score": round(seed_score, 4),
        "accepted": True,
        "frontier": True,
        "role": "seed",
        "params": parse(seed_group["text"]),
    }]

    # Iterations pair up with subsequent distinct candidates in order.
    iterations = [{
        "i": 0,
        "candidate_score": round(seed_score, 4),
        "best_score": round(seed_score, 4),
        "note": "seed",
    }]

    best = seed_score
    best_id = 0
    proposals = order[1:]
    for idx, entry in enumerate(run_log):
        if idx >= len(proposals):
            break
        h = proposals[idx]
        group = by_hash[h]
        task = entry["tasks"][0]
        new_score = mean(
            task.get("new_subsample_scores") or group["scores"],
        )
        accepted = group["text"] in accepted_texts

        cand_id = idx + 1
        if accepted and new_score > best:
            best = new_score
            best_id = cand_id

        candidates.append({
            "id": cand_id,
            "parent": task.get("parent_idx", 0),
            "score": round(new_score, 4),
            "accepted": accepted,
            "frontier": accepted,
            "params": parse(group["text"]),
        })
        iterations.append({
            "i": cand_id,
            "candidate_score": round(new_score, 4),
            "best_score": round(best, 4),
            "note": "accepted" if accepted else "rejected",
        })

    for cand in candidates:
        if cand["id"] == best_id:
            cand["role"] = cand.get("role", "best")

    out = {
        "run_id": args.run_id,
        "reflection_lm": args.reflection_lm,
        "mode": "generalization",
        "status": "complete",
        "seed_score": round(seed_score, 4),
        "best_score": round(summary.get("best_score", best), 4),
        "iterations": iterations,
        "seed_params": candidates[0]["params"],
        "best_candidate": next(
            c["params"] for c in candidates if c["id"] == best_id
        ),
        "stats": {
            "evals_done": summary.get("total_evals", len(evals)),
            "distinct_candidates": len(order),
        },
        "candidates": candidates,
    }

    Path(args.out).write_text(json.dumps(out, indent=1))
    print(f"wrote {args.out}: {len(candidates)} candidates, "
          f"{len(iterations)} iterations, best {out['best_score']}")


if __name__ == "__main__":
    main()
