# exp-023 measurement phase, every command

Repo: /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-gepa @ 9857ef8fc
Work dir (SCRATCH/exp023) is the only place anything was written. The repo
tree was never modified and the repo-root `routesim` binary was not touched.

W=SCRATCH/exp023 ; R=<repo>

## 1. Builds (five binaries, overlay per exp-022)

    mkdir -p $W/{bins,ov,corpora,raw,logs}
    cp $R/simulation/champions/router_hb1_v1.go            $W/ov/hb1/candidate_impl.go
    cp $R/simulation/champions/router_mx3_generalist_v1.go $W/ov/mx_c3/candidate_impl.go
    cp $R/simulation/lab/experiments/exp-010b-atomic1-best-candidate.go \
                                                           $W/ov/atomic1/candidate_impl.go
    # overlay.json per arm: {"Replace":{"$R/cmd/routesim/candidate_impl.go":
    #                                   "$W/ov/<arm>/candidate_impl.go"}}
    go build -o $W/bins/routesim_base ./cmd/routesim
    for n in hb1 mx_c3 atomic1; do
      go build -overlay $W/ov/$n/overlay.json -o $W/bins/routesim_$n ./cmd/routesim
    done

lnd and seed both run off routesim_base (--router=lnd / --router=candidate).

## 2. Concurrency churn calibration (stage D lead decision 2)

    python3 gen_scenarios.py --out $W/cal/base --seed 8081 --drift --atomic \
        --train 1 --val 1 --test 20
    for w in 1 2 4; do
      python3 gen_scenarios.py --out $W/cal/w$w --seed 8081 --drift --atomic \
          --train 1 --val 1 --test 20 \
          --concurrency max_in_flight=$w,inter_arrival_sec=5
    done
    python3 $W/cal.py w1 w2 w4        # measure pooled bg_payments_sent
    # scale payments_per_gap, re-measure
    python3 $W/cal.py w1s w2s w4s

Pooled bg_payments_sent per file over the five routers:
  before scaling  w1 12.40  w2  8.10  w4  6.80
  scale applied   w1 1.000  w2 1.531  w4 1.824
  after scaling   w1 12.40  w2 12.66  w4 12.74
Realized on the final tiers: 12.40 / 12.66 / 12.74. Calibration converged
in one pass.

## 3. Tier construction

    python3 $W/tiers.py

36 tiers, 384 files. Synthetic tiers are gen_scenarios.py run twice at the
same seed (with and without the flag) and then ASSERTED to differ only by
the stamped section (`pair_check`), so pairing is exact file for file.
Mainnet tiers are the sealed exp-009 tier with the section injected into
copies (gen_scenarios.py cannot emit a describegraph tier).

Seeds: a_* 9001, b_* 9002, c_* 9003, d_* 8081, e_* 8082, x_* 8083.
Gate + mainnet arms: SCRATCH/adjudication-results/tiers (exp-020 tier set).

## 4. Runs

    python3 $W/run.py gate_        # 270 runs, the gate
    python3 $W/run.py              # 1920 runs total, 0 errors, 2m13s

Every run: `routesim_<arm> --scenarios <file> --router=<lnd|candidate>
--traces=false`, aggregate cached under $W/raw/<tier>__<router>__<stem>.json.

## 5. Stats

    python3 $W/stats.py    # -> exp-023-results-summary.json
    python3 $W/extra.py    # objective L (E-c) + operative abandonment gate

Bootstrap 10k percentile CIs, seed 20260728; two-sided sign tests; all
deltas paired per file.

## Outputs
  exp-023-results-summary.json   the full machine-readable summary
  gate.json                      the 24-cell reproduction gate
  tables.txt                     per-tier tables + all paired comparisons
  extra.txt                      objective L + operative abandonment gate
  aux.txt                        self-contention per attempt, churn, fee leak
  bar.txt                        which cells clear the pre-registered bar
  manifest.json                  every tier, its files and its provenance note
