# Routing Optimization Harness

This directory holds the GEPA-based optimization harness for lnd's
pathfinding. The core idea: lnd's *real* routing code (or a candidate
replacement algorithm) runs against an in-process simulated Lightning
Network with hidden liquidity, an evaluator scores the outcome, and a
reflective LLM optimizer (GEPA) proposes improved candidates from the
failure feedback.

## Components

| Piece | Where | What |
|---|---|---|
| Simulator | `routing/sim_*.go` | In-memory LN with hidden balances; real pathfinding + mission control run unmodified against it |
| CLI | `cmd/routesim` | params JSON + scenario file in, attempt traces + aggregate JSON out |
| Candidate slot | `cmd/routesim/candidate_impl.go` | A complete routing algorithm behind `--router=candidate`; swapped per candidate via `go build -overlay` |
| Corpus | `gen_scenarios.py` | train/val/test scenario files: topology + liquidity seed + payment batch |
| Evaluators | `evaluate.py`, `evaluate_code.py` | score = success rate − small saturating penalties for attempts and fee ppm |
| Runners | `run_gepa.py`, `run_gepa_code.py` | parameter mode and code mode optimization |
| Reflection LM | `codex_lm.py` | GEPA LM protocol via `codex exec` headless (default `gpt-5.6-sol`) |
| Lab notebook | `lab/` | running log of experiments, results, ideas |

## Quick start

```bash
# Build the simulator binary.
go build -o /tmp/routesim ./cmd/routesim

# Generate a scenario corpus.
python3 simulation/gen_scenarios.py --out /tmp/corpus

# Score the lnd defaults on one example.
cd simulation && ROUTESIM_BIN=/tmp/routesim python3 evaluate.py /tmp/corpus/val/example_000.json

# Compare lnd stack vs the candidate router on a scenario file.
/tmp/routesim --scenarios /tmp/corpus/val/example_000.json --router=lnd    --traces=false
/tmp/routesim --scenarios /tmp/corpus/val/example_000.json --router=candidate --traces=false

# Full optimization runs. gepa must be installed from git main — a
# durable clone lives at ~/codez/gepa; prefer uv for the env:
#   uv venv /tmp/gepa-venv && uv pip install -p /tmp/gepa-venv \
#       "~/codez/gepa[full]"
# Also needs the codex CLI authenticated and OPENAI_API_KEY set.
ROUTESIM_BIN=/tmp/routesim python3 run_gepa.py --corpus /tmp/corpus --name run1 --max-evals 400
ROUTESIM_BIN=/tmp/routesim python3 run_gepa_code.py --corpus /tmp/corpus --name code1
```

## The two optimization modes

1. **Parameter mode** (`run_gepa.py`) — candidate = JSON of the existing
   heuristic's knobs (estimator choice, apriori/bimodal params, attempt
   cost, min probability). Validates the loop and tunes the current
   paradigm.
2. **Code mode** (`run_gepa_code.py`) — candidate = the full Go source of
   `candidate_impl.go`, an entire routing algorithm implementing the
   `routing.SimRouter` interface. This is the paradigm-free path: the
   candidate sees only gossip, its own balances, and per-attempt feedback.
   Compile errors are returned to the proposer as feedback.

## Anti-reward-hacking measures

- Candidate routers receive a `SimNetworkView` wrapper, not the concrete
  graph, so hidden balances and liquidity mutation are unreachable.
- `evaluate_code.py` rejects candidates using `unsafe`, `reflect`,
  `os/exec`, network packages, etc.
- Selection happens on a val split; a sealed test split is only used for
  final reporting.
- The source's own channels are rebalanced 50/50 before each batch so
  scores measure routing skill, not sender funding luck.

## Command center

`command-center/` holds a static dashboard site (serve with
`python3 -m http.server` from that directory).
