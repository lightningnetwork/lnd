# interval-sim battery, every command

Branch `interval-sim` at `/Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-isim`,
created off `gepa` @ 7a2014c8e. Two commits: bf1d2c6f9 (the merge) and
04ab5ad77 (the knob). NOT pushed. The main `lnd-gepa` tree was never modified;
the `lnd-interval` worktree was never touched.

    W       = SCRATCH/isim
    SCRATCH = /private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-\
              lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/\
              scratchpad

## 1. Worktree

    git -C .../lnd-gepa worktree add .../lnd-isim -b interval-sim gepa

## 2. The merge

    git merge ab1c123ab --no-commit
    # 4 conflicts: channeldb/{db.go,meta_test.go,migration35/migration.go}
    #              routing/payment_session.go
    git checkout ab1c123ab -- channeldb/          # duplicate upstream PR
    # payment_session.go resolved by hand: keep searchShardAmt, drop the two
    # methods interval-router moved onto additionalEdges
    go build ./routing/... ./cmd/routesim/ . ./lnrpc/routerrpc/
    (cd sqldb && go build ./...)
    go test ./routing/...      # green
    go test ./channeldb/...    # green
    go build $(go list ./... | grep -v 'simulation/champions\|simulation/lab')

## 3. Builds (six arms, four binaries + the merge-base reference)

    mkdir -p $W/{bins,ov,corpora,raw,logs,params}
    go build -o $W/bins/routesim_base ./cmd/routesim
    echo '{"router_impl":"interval"}' > $W/params/interval.json
    echo '{}'                         > $W/params/stock.json

    cp simulation/champions/router_hb1_v1.go            $W/ov/hb1/candidate_impl.go
    cp simulation/champions/router_mx3_generalist_v1.go $W/ov/mx_c3/candidate_impl.go
    cp simulation/lab/experiments/exp-010b-atomic1-best-candidate.go \
                                                        $W/ov/atomic1/candidate_impl.go
    # overlay.json per arm: {"Replace":{"<repo>/cmd/routesim/candidate_impl.go":
    #                                   "$W/ov/<arm>/candidate_impl.go"}}
    for n in hb1 mx_c3 atomic1; do
      go build -overlay $W/ov/$n/overlay.json -o $W/bins/routesim_$n ./cmd/routesim
    done

    git -C .../lnd-gepa worktree add --detach $W/mergebase-tree 7a2014c8e
    (cd $W/mergebase-tree && go build -o $W/bins/routesim_mergebase ./cmd/routesim)

`lnd` and `ilnd` are the SAME binary; `ilnd` adds
`--params $W/params/interval.json`. `seed` is `routesim_base --router=candidate`.

Sandbox audit, clean, no hits:

    grep -nE 'GraphSession|LocalBalances|AssignLiquidity|unsafe|reflect|os\.|net/|syscall' \
        $W/ov/*/candidate_impl.go

Binary/tree check after the test-only commit: `go build -o /tmp/routesim_verify`
hashes equal to `$W/bins/routesim_base` (cd1933e44).

## 4. Identity proof

    python3 $W/identity.py         # -> identity.json
    python3 $W/identity_flaky.py   # -> identity_flaky.json

108 cells = 6 classic tiers x 54 files x 2 `--router` arms. Whole stdout
compared with traces ON (routesim default). Each cell screened 3x on the
merge-base binary first; 104 are self-deterministic and are required to be
byte-identical, 4 are not and get a self-control instead.

## 5. Tiers

    python3 $W/tiers.py    # 14 tiers, 134 files, 804 runs -> manifest.json

Classic six and the econ rungs are byte-identical reuses of SCRATCH/exp023's
corpora. Degraded tiers are built with exp-019's `inject()` from the sealed
originals, so each degraded file pairs to its control by name.

## 6. Runs

    cp ~/codez/data/mainnet_graph.json /tmp/mainnet_graph.json   # already present
    python3 $W/run.py hard_test ood_test split_test drift_test atomic_test mainnet
    python3 $W/run.py              # 804 runs total, 0 errors

Each run: `routesim_<arm> --scenarios <file> --router=<lnd|candidate>
[--params $W/params/interval.json] --traces=false`, aggregate cached at
`$W/raw/<tier>__<arm>__<stem>.json`, with `obj_cap_{15,30,60,inf}`.

## 7. Gate

    python3 $W/gate.py     # -> gate.json    24/24 bit-exact, PASS

## 8. Stats and tables

    python3 $W/stats.py    # -> isim-results-summary.json
    python3 $W/tables.py   # -> tables.txt

Bootstrap 10k percentile CIs, seed 20260729; two-sided exact sign tests; all
deltas paired per file. Success, attempts, give-ups and both fee metrics carried
separately on every tier. Objective is
`success - 0.01*min(extra_attempts,15) - 0.00002*min(fee_ppm,5000)`.

## Outputs

    isim-results-summary.json   full machine-readable summary
    gate.json                   24-cell reproduction gate
    identity.json               108-cell identity proof
    identity_flaky.json         the 4 flaky mainnet cells, with self-control
    tables.txt                  rendered tables (7 sections)
    manifest.json               every tier, its files, its provenance
    raw/                        804 cached per-file aggregates

## Anomaly, recorded

The simulator's lnd arm is NOT bit-reproducible on 4 of the 10 mainnet files
(`mn_11_uniform`, `mn_22_bimodal`, `mn_44_bimodal`, `mn_55_uniform`), and this
is true of the merge-base binary itself. Cause: lnd's own `findPath` expands a
node's predecessors by iterating a Go map (`routing/pathfind.go:1106`), so exact
cost ties on the dense mainnet graph are broken by map iteration order. The
candidate arm is deterministic on all 54 files, and every non-mainnet lnd cell
is deterministic too; ruled out as a cause: mission control's wall-clock decay
(re-running with `penalty_half_life_sec = 1e12` leaves the same two-valued
output set).

Magnitude: one attempt in ~434 on `mn_11_uniform`; over 40 samples per group
the merge-base binary's own second sample group is as "novel" against its first
as the new binary is (novel B|A == novel N|A on 3 of 4 cells, 0 vs 1 on the
fourth), and mean total_attempts agrees to within 0.05 of ~434. Consequence
worth flagging beyond this branch: the "bit_exact" mainnet cells in exp-023 and
exp-025's gate tables are luck-dependent, not guaranteed. This run's gate
happened to land bit-exact on all 24 cells.
