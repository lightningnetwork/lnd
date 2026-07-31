# exp-032, the wider degraded corpus re-pin: every command

    W = SCRATCH/exp032-repin
    R = /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-gepa   (free; code_full3 closed)
    I = /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-isim  (tip dcde83cca)

**Nothing was committed anywhere.** All builds read-only with `-o` into `$W/bins`.

## 1. Corpus

    cp ~/codez/data/mainnet_graph.json /tmp/mainnet_graph.json   # daily cleaner

    python3 simulation/gen_scenarios.py --out $W/corpora/hard_clean   --hard --train 0 --val 0 --test 30 --seed 32032
    python3 simulation/gen_scenarios.py --out $W/corpora/hard_unk20   ... --attribution unknown=0.2
    python3 simulation/gen_scenarios.py --out $W/corpora/hard_shift10 ... --attribution shift=0.1
    python3 simulation/gen_scenarios.py --out $W/corpora/hard_mix     ... --attribution unknown=0.2,shift=0.1
    python3 simulation/gen_scenarios.py --out $W/corpora/hard_unk30   ... --attribution unknown=0.3
    python3 $W/gen_mainnet.py     # mn_clean, mn_mix; payment seeds 2001..2030

Generator seed **32032**, mainnet payment seeds **2001-2030**, both recorded in
`results-summary.json`. `--attribution` stamps a section and makes no rng draw,
so the four degraded variants pair with the clean control byte for byte;
verified 30/30 for each, and 30/30 for mn_mix against mn_clean.

zsh note: unquoted `$args` is NOT word-split, so each generator invocation is
written out in full rather than built into a variable.

## 2. Arms

    grep -cE 'GraphSession|LocalBalances|AssignLiquidity|unsafe|reflect|os\.|net/|syscall' \
        $R/simulation/champions/router_mx3_generalist_v1.go     # 0
    (cd $R && go build -o $W/bins/routesim_base ./cmd/routesim)
    (cd $R && go build -overlay $W/ov/mx_c3/overlay.json -o $W/bins/routesim_mx_c3 ./cmd/routesim)
    (cd $I && go build -o $W/bins/routesim_ilnd ./cmd/routesim)   # dcde83cca, committed V3
    cp SCRATCH/exp030-degmix/bins/routesim_V0 $W/bins/routesim_prefix   # pre-fix tip
    cp SCRATCH/isim/bins/routesim_base        $W/bins/routesim_round2   # pre-rounds-3-6

## 3. Runs

    cd $W && nice -n 10 python3 run.py            # 660 cells, 0 errors
    cd $W && nice -n 10 python3 run.py hard_*     # + the round2 arm, 630 cells, 0 errors

Replicates: 8 for lnd, ilnd, prefix and round2; 3 for mx_c3, which has been
exactly deterministic on every tier this program has measured and is again here.
The interval arm is the noisy one on this family: per-file objective range runs
to 0.29 on hard_mix, which is why 8 replicates and n=30 both matter.

## 4. Stats

    python3 $W/stats.py     # -> results-summary.json
    python3 ...             # -> verdict block
    python3 ... > tables.txt

Bootstrap 10k percentile CIs at seed 20260729, two-sided exact sign tests, all
deltas paired per file, attempt-cap re-scoring at 15/30/60/uncapped, and
success/attempts/give-ups/fees carried separately from the composite.

## Outputs

    results-summary.json   tiers, comparisons, degradation_holds,
                           cap_sensitivity, determinism, caveat, verdict
    tables.txt             5 sections + headline
    gen_mainnet.py         the mainnet generator (seeds in the docstring)
    run.py / stats.py
    corpora/               7 tiers x 30 files
    raw/                   810 cached per-cell aggregates
