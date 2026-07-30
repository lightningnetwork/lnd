# exp-029, the foreign balance sheet: every command

    W       = SCRATCH/exp029
    R       = /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-gepa   (LOCKED)
    ISIM    = /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-isim
    SCRATCH = /private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-\
              lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/scratchpad

**Locked-tree discipline.** `code_full3` was live throughout and recompiles `$R`
every eval. Nothing in `$R/routing/` or `$R/cmd/routesim/` was modified; all
builds were read-only with `-o` into `$W/bins` and overlay files written to
`$W/ov`. Verified at the end with `git -C $R status --short`. Concurrency held
at 4 workers, runs `nice -n 10`.

## 1. Inputs

    ls -la ~/codez/data/realistic_graph.json     # 11,255 nodes / 37,203 edges
    cp /tmp/scen-realistic-multihop.json $W/scen-realistic-multihop.smoke.json

The smoke file was copied out of /tmp before the cleaner could take it.

## 2. Reading the loader and the sealed tier before mirroring either

    git log --oneline -1 7d10989fd
    grep -n "from_graph\|unbalanced_source" routing/sim_liquidity.go cmd/routesim/main.go
    python3 ... # sealed mainnet tier: 10 files x 10 payments, amounts
                # {100M,500M,1000M,2000M}, max_parts 8, one hub source
    python3 ... # sealed TARGET degrees on the mainnet graph:
                # min 3, median 6, max 45 -- NOT a uniform draw

That last measurement changed the design. A uniform target draw on this graph
puts 584 of 1,000 payments on a degree-1 leaf (its median node degree is 1),
which would have produced the exp-012 reachability floor. The generator mirrors
the sealed tier's empirical target-degree distribution instead.

## 3. Vantage translation

    python3 ...  # build pub_key_og -> pub_key over all 11,255 nodes, 0 dups
    # sealed hub 03864ef0... -> 02cb18f3..., degree 2013, rank 1
    # (the sealed tier's hub had 2,015 channels; the model kept it)

## 4. Tier generation (deterministic, seeds 101..110)

    python3 $W/gen_tier.py     # -> $W/scen/*.json, manifest.json, tier-README.json
    python3 ...                # -> A_prod variant, fee_limit_ppm 50000

Four variants, 10 files each, 100 payments per file:

    A_from_graph  liquidity_model from_graph, unbalanced_source true
    B_bimodal     identical scenarios, bimodal, unbalanced_source true
                  (only the liquidity model differs from A)
    B_rebal       B with the sealed convention restored (source rebalanced)
    A_prod        A + fee_limit_ppm 50000 (production's own default)

## 5. The interval arm

    cd $ISIM && git merge 7d10989fd --no-commit   # clean
    go build ./routing/... ./cmd/routesim/        # OK
    go test ./routing/ -run 'Interval|SimRouterImpl'
    python3 ...   # reuse gate: routesim_r6 vs the new build on classic tiers,
                  # 13/14 byte-identical; the 14th is hard_test ex_001 under the
                  # interval arm, which gives 3 distinct outputs in 6 runs of
                  # routesim_r6 ALONE and 4 in 6 of the new one, sets overlapping
    git commit    # ffe5e5537, not pushed

## 6. Builds and the standing sandbox audit

    grep -nE 'GraphSession|LocalBalances|AssignLiquidity|unsafe|reflect|os\.|net/|syscall' \
        $R/simulation/champions/router_hb1_v1.go \
        $R/simulation/champions/router_mx3_generalist_v1.go \
        $R/simulation/lab/experiments/exp-010b-atomic1-best-candidate.go \
        $R/simulation/lab/experiments/exp-025-econ2-best-candidate.go
    # 0 hits each

    go build -o $W/bins/routesim_base ./cmd/routesim                 # from $R
    for n in hb1 mx_c3 atomic1 econ2; do
      go build -overlay $W/ov/$n/overlay.json -o $W/bins/routesim_$n ./cmd/routesim
    done
    (cd $ISIM && go build -o $W/bins/routesim_ilnd ./cmd/routesim)

## 7. Determinism screen, and why it was not trusted

    python3 ...   # 3 samples per arm -> ALL SEVEN "deterministic"

That screen was wrong, exactly as it was in rounds 4 and 5. Every arm was
replicated anyway: 8 for lnd and ilnd, 3 for the five candidates. The sweep
found 53 of 200 cells nondeterministic, all of them lnd or ilnd, worst-case
objective range 0.0012.

## 8. The sweep

    cd $W && nice -n 10 python3 run.py      # 200 cells, 890 invocations,
                                            # 4 workers, 0 errors

## 9. The fee-liquidity signal (Q4 groundwork)

    python3 ...   # -> fee_liquidity_signal.json
                  # spearman(outbound fee ppm, own balance fraction) = -0.149
                  # over 74,406 directed ends

## 10. Stats, verdict, tables

    python3 $W/stats.py      # -> results-summary.json
    python3 $W/verdict.py    # -> verdict block
    python3 $W/tables.py     # -> tables.txt

Bootstrap 10k percentile CIs at seed 20260729, two-sided exact sign tests, all
deltas paired per file, success/attempts/give-ups/both fee metrics carried
separately, every arm-vs-lnd comparison re-scored at caps 15/30/60/uncapped.

## Outputs

    results-summary.json   tiers, comparisons, balance_family_effect,
                           source_rebalance_effect, prod_default_effect,
                           cap_sensitivity, determinism, tier_facts,
                           fee_liquidity_signal, verdict
    tables.txt             6 sections
    manifest.json          40 scenario files across 4 variants
    tier-README.json       the distribution facts block
    gen_tier.py            the generator (deterministic seeds 101..110)
    run.py / stats.py / verdict.py / tables.py
    scen/                  the 40 generated scenario files
    raw/                   200 cached per-cell aggregates (replicate means)
    reuse_gate.json        the interval-sim loader-merge gate
    determinism_screen.json / fee_liquidity_signal.json
