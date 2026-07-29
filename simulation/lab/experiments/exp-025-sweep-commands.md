# exp-025 verdict sweep, every command

Repo: /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-gepa @ 9f08bba00
(branch gepa). SCRATCH/exp025 is the only place anything was written. The repo
tree was never modified; the repo-root `routesim` binary was not touched.

    W = SCRATCH/exp025 ; R = <repo>
    SCRATCH = /private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-\
              lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/\
              scratchpad

## 1. Builds (six arms, five binaries, overlay per exp-023)

    mkdir -p $W/{bins,ov,corpora,raw,logs}
    cp $R/simulation/champions/router_hb1_v1.go            $W/ov/hb1/candidate_impl.go
    cp $R/simulation/champions/router_mx3_generalist_v1.go $W/ov/mx_c3/candidate_impl.go
    cp $R/simulation/lab/experiments/exp-010b-atomic1-best-candidate.go \
                                                           $W/ov/atomic1/candidate_impl.go
    cp $R/simulation/lab/experiments/exp-025-econ2-best-candidate.go \
                                                           $W/ov/econ2/candidate_impl.go
    # overlay.json per arm: {"Replace":{"$R/cmd/routesim/candidate_impl.go":
    #                                   "$W/ov/<arm>/candidate_impl.go"}}
    go build -o $W/bins/routesim_base ./cmd/routesim
    for n in hb1 mx_c3 atomic1 econ2; do
      go build -overlay $W/ov/$n/overlay.json -o $W/bins/routesim_$n ./cmd/routesim
    done

lnd and seed both run off routesim_base (--router=lnd / --router=candidate).

## 2. Sandbox audit (before anything was trusted)

    grep -nE 'GraphSession|LocalBalances|AssignLiquidity|unsafe|reflect|os\.|net/|syscall' \
        $R/simulation/lab/experiments/exp-025-econ2-best-candidate.go
    # CLEAN, no hits.

## 3. Tier construction (no new corpora generated)

    python3 $W/tiers.py     # 21 tiers, 254 files, 1524 runs

- `econ_val` / `econ_test`: the committed sealed splits at
  `$R/simulation/lab/scenarios/corpus-econ/{val,test}`.
- 13 exp-023 economic tiers reused BYTE-IDENTICAL from
  `SCRATCH/exp023/corpora` (their `pair_check` provenance is in exp-023's
  tiers.py): a_ctrl a_tight b_ctrl b_heavy c_ctrl c_4000 c_2000
  c_mn_ctrl c_mn_400 c_mn_100 d_w1 d_w2 d_w4.
- 6 classic sealed tiers, the exp-020 set, which double as the gate.

## 4. Runs

    cp ~/codez/data/mainnet_graph.json /tmp/mainnet_graph.json   # see anomaly
    python3 $W/run.py gate_ econ_    # 564 runs, the gate
    python3 $W/run.py                # 1524 runs total, 0 errors, 6m05s

Every run: `routesim_<arm> --scenarios <file> --router=<lnd|candidate>
--traces=false`, aggregate cached under `$W/raw/<tier>__<router>__<stem>.json`.
Each record also carries `obj_cap_{15,30,60,inf}` for the exp-022
attempt-cap sensitivity re-scoring.

**Anomaly, recorded:** the first gate pass returned 23 errors because the
daily /tmp cleaner removed `/tmp/mainnet_graph.json` mid-run as the date
rolled over. Errors are never cached, so restoring the file from
`~/codez/data/mainnet_graph.json` and re-running filled the gaps with no
stale data. Second pass: 0 errors.

## 5. Gates (STOP on failure) — ALL PASS

    python3 $W/gate.py     # -> gate.json

- A: 24/24 classic cells vs `SCRATCH/exp023/gate.json`, all **bit-exact**
  (delta 0.00e+00), attempts included.
- B: corpus-econ val seed baseline 0.321659 reproduced to 6 dp.
- C: econ2 val 0.34323498891044557 and held-out test 0.2940388247186299,
  and the seed's held-out 0.2373383681678917 — all three **bit-exact**
  against the exp-025 run log.

## 6. Stats

    python3 $W/stats.py     # -> exp-025-results-summary.json
    python3 $W/tables.py    # -> tables.txt
    python3 $W/extra.py     # -> extra.txt (family rollup + hypothesis wire evidence)

Bootstrap 10k percentile CIs, seed 20260729; two-sided exact sign tests;
all deltas paired per file. Success, attempts, give-ups and both fee
metrics are carried separately on every tier. Objective is
`simulation/evaluate.py:composite_score`, verified identical.

## Outputs

    exp-025-results-summary.json   the full machine-readable summary,
                                   including _meta, source_audit,
                                   hypotheses and verdict blocks
    gate.json                      the three reproduction gates
    tables.txt                     per-tier tables, all paired comparisons,
                                   cap sensitivity, counters, identity check
    extra.txt                      family rollup + H-A1/B1/C2/D1 wire evidence
                                   + the inbound-fee base discrepancy worked
    manifest.json                  every tier, its files, its provenance
    raw/                           1524 cached per-file aggregates
