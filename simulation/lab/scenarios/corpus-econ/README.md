# corpus-econ — the economic world, for code_econ1

Built 2026-07-28 from the committed generator at `e9873ae40`
(`simulation/gen_scenarios.py`), fixed seeds, no hand edits except the
churn calibration recorded below. Everything here regenerates from the
commands in this file.

This is the corpus for the exp-023 evolution arm: the run that tests
H-A1, H-B1, H-C2 and H-D1, the four hypotheses the measurement phase
could not test because no incumbent router carries the machinery they
are about (budget tracking, inbound-fee pricing, contention planning).

## Counts and seeds

| split | files | composition | fee-only | concurrency-only |
|---|---|---|---|---|
| train | 48 | 24 | 12 | 12 |
| val | 20 | 10 | 5 | 5 |
| test | 20 | **20** | 0 | 0 |

| family | seed | file names |
|---|---|---|
| composition | 9201 | `comp_000.json` … |
| fee-only | 9202 | `fee_000.json` … |
| concurrency-only | 9203 | `conc_000.json` … |
| calibration (not in the corpus) | 9210 | `SCRATCH/econ/cal/` |

**The test split is composition only, and that is deliberate.** exp-022
held its test split at the clean channel so the held-out number read
transfer BACK to the undegraded world. This run inverts that: the
target world IS the economic one, so the held-out line reads economic
performance directly. A candidate that wins here has to win in the
world all five knobs are live in, not in the world it came from.

## The base world, shared by all three families

    --hard --drift --atomic --concurrency max_in_flight=1,inter_arrival_sec=5

Hard topology profile (exp-006), a virtual clock with background
traffic (exp-008), atomic MPP holds (exp-010b), and the stage D
scheduler. Every family carries a `concurrency` section, so a
single-knob file differs from a composition file only by the knobs it
does not have.

**Why the base carries `max_in_flight: 1` rather than no concurrency
section at all.** They are not the same world, and the difference is
large. Same file, same seed, differing only by the presence of
`{"concurrency": {"max_in_flight": 1, "inter_arrival_sec": 5}}`, pooled
over the five arms on 20 files:

| | success | attempts | bg_payments_sent |
|---|---|---|---|
| no concurrency section | 0.614 | 24.9 | **255.4** |
| `max_in_flight: 1` | 0.589 | 26.4 | **14.5** |

A concurrency section replaces the 600-second `payment_gap_sec` between
payments with `inter_arrival_sec`, and background traffic is prorated
by elapsed virtual time, so the scheduler's world sees roughly a
seventeenth of the exogenous churn a sequential drift file sees. This
is stage D finding 3 in a stronger form than that note states: the
d_w1 "sequential control" of the exp-023 ladder is already a much
quieter world than the drift tiers of exp-008/015, and the ladder is
internally consistent only because all three of its rungs are on the
scheduler. Mixing a family with a section and a family without one
inside a single corpus would have made "fee budget" and "how much the
network drifts" the same variable. All three families therefore carry a
section.

## Commands, exactly

    R=<repo>/simulation ; W=<scratch>/econ
    BASE="--hard --drift --atomic"
    KNOBS="--fee-limit-ppm 4000 --htlc-limits tight --inbound-fees heavy \
           --latency per_hop_ms=300,attempt_overhead_ms=250"

    python3 $R/gen_scenarios.py --out $W/gen/comp --seed 9201 $BASE $KNOBS \
        --concurrency max_in_flight=2,inter_arrival_sec=5 \
        --train 24 --val 10 --test 20

    python3 $R/gen_scenarios.py --out $W/gen/fee --seed 9202 $BASE \
        --fee-limit-ppm 4000 \
        --concurrency max_in_flight=1,inter_arrival_sec=5 \
        --train 12 --val 5 --test 0

    python3 $R/gen_scenarios.py --out $W/gen/conc --seed 9203 $BASE \
        --concurrency max_in_flight=2,inter_arrival_sec=5 \
        --train 12 --val 5 --test 0

    python3 $W/assemble.py     # churn calibration + merge into corpus-econ

## Rungs, and where each came from

| knob | rung | provenance |
|---|---|---|
| fee budget | `fee_limit_ppm: 4000` | stage C lead decision 3, the **informative** synthetic rung (2000 is stress). Re-measured on this world: see the ladder below. |
| htlc limits | `tight` | stage A authored stress rung, labelled as authored |
| inbound fees | `heavy` | stage B authored stress rung. `mainnet_empirical` is close to inert (exp-023: ten ties of ten), so it would have bought the search nothing. |
| concurrency | `max_in_flight: 2, inter_arrival_sec: 5` | stage D rungs {1,2,4}; 2 is the mid rung and the one exp-023's composition tier used |
| latency | `per_hop_ms: 300, attempt_overhead_ms: 250` | the stage E schema default, and the rung exp-023's `x_full` used |

The exp-023 verdict's caveat on its own composition tier was that "its
fee rung was calibrated on the wrong profile and crushes lnd's fees
asymmetrically": `x_full` put the 4000 ppm hard-profile rung on a
default-profile world. That is fixed here — this corpus is on the hard
profile, which is the profile 4000 was measured for.

**The rung ladder, re-measured on this base world** (seed 9210, 20
files, pooled over lnd / seed / hb1 / mx_c3 / atomic1, each knob alone):

| tier | success | att/pmt | bg sent | refusals/file |
|---|---|---|---|---|
| base (w1, no knob) | 0.589 | 26.4 | 14.5 | — |
| fee 5000 | 0.485 | 25.8 | 13.3 | 17.3 |
| **fee 4000** | **0.430** | 24.1 | 11.9 | 26.2 |
| fee 2000 | 0.243 | 18.3 | 7.8 | 52.2 |
| htlc tight | 0.500 | 23.1 | 12.8 | — |
| inbound heavy | 0.560 | 28.4 | 15.3 | — |
| latency 300/250 | 0.593 | 26.7 | 49.8 | — |

4000 costs 0.159 of success, 2000 costs 0.346: the stage C reading
(informative / stress) transfers to this world unchanged. Latency alone
is success-neutral, which is stage E's E-a null reproducing, but it
triples the background churn because an attempt costs 3.4 seconds
rather than the clock's flat 1.

## The churn calibration

Stage D finding 3: a concurrent batch clears the same payments in less
virtual time, so it elapses less of the exogenous background process. A
window-2 file is therefore a QUIETER world than its window-1 control
unless something is done, which would confound contention with drift.
Stage D lead decision 2 puts the fix in the corpus: scale
`payments_per_gap` so realized churn stays flat across the window.

Applied here per family, measured on this corpus's own files and its
own seeds (not on exp-023's numbers), pooled `bg_payments_sent` per
file over the five arms, two passes:

| family | w1 target | pass 1 mult | pass 1 realized | **pass 2 mult** |
|---|---|---|---|---|
| composition | 38.04 | 1.6085 | 36.69 (0.964) | **1.6679** |
| concurrency-only | 20.00 | 1.4753 | 18.62 (0.931) | **1.5843** |
| fee-only | — | — | — | 1.0 (already w1) |

Landing, per split, realized over the w1 target:

| cell | target | realized | ratio |
|---|---|---|---|
| train/comp | 48.20 | 48.69 | 1.010 |
| val/comp | 16.00 | 15.66 | 0.979 |
| test/comp | 36.88 | 35.44 | 0.961 |
| train/conc | 23.72 | 23.72 | 1.000 |
| val/conc | 11.08 | 10.44 | 0.942 |

Converged in two passes, every cell within 6% and four of five within
4%. `payments_per_gap` is the ONLY field the assembler touches; every
other byte is generator output.

The first-pass multipliers came from a separate calibration seed
(9210) and were 4 to 7% low, because scaling `payments_per_gap` by k
raises realized sends by slightly less than k (more traffic moves more
liquidity, which shortens the batch). The second pass folds that
measured ratio back in, which is the same one-step correction stage D
used.

## Manipulation checks

Every corpus file, five arms (lnd, seed, hb1, mx_c3, atomic1), 440
runs, zero errors. Per-cell means, pooled over arms:

| cell | succ | att | htlc bounded | inbound charging | inbound charged | fee_limit_payments | fee_limit_failures | mean_conc | self-cont | attempt latency | bg sent |
|---|---|---|---|---|---|---|---|---|---|---|---|
| train/comp | 0.307 | 26.8 | 1811.6 | 1811.6 | 1192.5 | 8.54 | 25.5 | 1.554 | 20.8 | 3.50 | 48.7 |
| val/comp | 0.320 | 16.3 | 1110.8 | 1110.8 | 493.7 | 8.10 | 11.1 | 1.475 | 7.6 | 2.72 | 15.7 |
| test/comp | 0.337 | 21.3 | 1752.5 | 1752.5 | 818.9 | 7.60 | 18.4 | 1.517 | 16.6 | 3.35 | 35.4 |
| train/fee | 0.352 | 29.0 | — | — | — | 8.17 | 27.7 | 1.000 | 0 | — | 18.3 |
| val/fee | 0.503 | 21.6 | — | — | — | 8.40 | 14.3 | 1.000 | 0 | — | 9.0 |
| train/conc | 0.593 | 35.1 | — | — | — | — | — | 1.539 | 49.7 | — | 23.7 |
| val/conc | 0.509 | 36.9 | — | — | — | — | — | 1.615 | 28.6 | — | 10.4 |

Every knob engages where it is stamped and nowhere else:

- **htlc limits (stage A).** `htlc_limit_bounded == htlc_limit_policies`
  on every composition file: every directed policy carries a redrawn
  announced cap. `htlc_max_refusals` is ZERO everywhere, which is
  stage A's own finding and not a dead knob — a constraint the router
  can read binds at PLAN time, so nobody offers a route the cap would
  refuse. The engagement evidence is the bounded count, as stage A
  declared.
- **inbound fees (stage B).** 100% of policies charging, 494 to 1193
  inbound fees actually charged per file, 1708 discounts against 44
  surcharges (the `heavy` family's shape).
- **fee budget (stage C).** `fee_limit_payments` is 7.6 to 8.5 per
  file, which is every payment in the file; `fee_limit_failures` 11 to
  28, so the budget is refusing routes rather than sitting inert.
- **concurrency (stage D).** `mean_concurrent` 1.48 to 1.62 with
  `max_concurrent` 2 on both window-2 families, and self-contention 7.6
  to 49.7 per file. The fee-only family reads exactly 1.000 and 0,
  which is the free control.
- **latency (stage E).** `mean_attempt_latency_sec` 2.72 to 3.50
  against the clock's flat `attempt_sec` of 1.0, so attempts are priced
  by the route they travel and differentially.

Raw per-arm numbers: `SCRATCH/econ/checks.json`; the script is
`SCRATCH/econ/checks.py`.

## A sample composition stanza

    {
      "topology": {"type": "grid", "num_nodes": 150,
                   "channel_size_sat": 2000000, "seed": 1607046362},
      "liquidity_model": "bimodal",
      "liquidity_seed": 1862338342,
      "source": "1",
      "clock": {"payment_gap_sec": 600, "attempt_sec": 1},
      "background_traffic": {"payments_per_gap": 25,
                             "min_amt_msat": 2000000,
                             "max_amt_msat": 1000000000,
                             "focus_fraction": 0.33,
                             "seed": 1937284399},
      "htlc_limits": {"max_htlc_frac_family": "tight",
                      "min_htlc_family": "tight"},
      "inbound_fees": {"family": "heavy"},
      "fee_limit_ppm": 4000,
      "concurrency": {"max_in_flight": 2, "inter_arrival_sec": 5.0},
      "latency": {"per_hop_ms": 300.0, "attempt_overhead_ms": 250.0},
      "scenarios": [
        {"target": "146", "amt_msat": 200000000, "max_parts": 16,
         "atomic_mpp": true},
        ...
      ]
    }

`payments_per_gap: 25` is the calibrated value; the generator wrote 15
and the composition multiplier 1.6679 rounded it to 25.

## Caveats, stated in advance

1. **Every rung except the fee budget is authored.** `tight` and
   `heavy` are stress rungs chosen because their empirical counterparts
   are close to inert on today's mainnet policy distribution (exp-023).
   This corpus therefore breeds against a world whose constraints are
   harsher than the network's, and a candidate's economics may be
   tuned to that.
2. **The composition world is a low-drift, high-contention world.** The
   concurrency section's 5-second arrival gap replaces the 600-second
   payment gap, so between-payment staleness is far smaller here than
   on the drift tiers. Contention is the drift that remains.
3. **The fee-only family is at window 1 and the other two at window
   2.** So "fee-only" is single-knob against the base, but it is not
   paired file-for-file with the composition family (different seeds,
   different worlds). These are training files, not a paired sweep.
4. **The liquidity generator is still ours** (`sim_liquidity.go`,
   `ExpFloat64()*0.05`), with all of exp-017's fitted-prior caveats
   intact.
