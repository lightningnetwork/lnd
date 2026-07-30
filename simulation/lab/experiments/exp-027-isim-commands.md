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

---

# Round 3: interval-router@1bcbb1485

Two commits past round 2's merge base: `ee95a0fcc` (budget-derived fee price per
nat, never-evicted cheapest label in the frontier, finished-route budget guard)
and `1bcbb1485` (suspect-bound quarantine for failures that cannot name their
channel). Merge commit on `interval-sim`, NOT pushed.

## 1. The merge

    git merge 1bcbb1485 --no-commit      # clean, zero conflicts
    go build ./routing/... ./cmd/routesim/ .
    go test ./routing/...                # green
    gofmt -l routing/                    # clean

Eight files, all interval-only plus docs: `interval_belief.go`,
`interval_pathfind.go`, `interval_session.go`, `interval_store.go`, two new test
files, `interval_parallel_test.go`, `docs/interval_routing.md`. Nothing shared
with the simulator, which is what makes the arm reuse below checkable.

## 2. Builds

    go build -o $W/bins/routesim_r3 ./cmd/routesim
    go build -overlay $W/ov/mx_c3/overlay.json -o $W/bins/routesim_mx_c3_r3 ./cmd/routesim

Round-2 binaries are left in place untouched (`routesim_base`,
`routesim_{hb1,mx_c3,atomic1}`), so the five reused arms are the exact binaries
that produced their cached aggregates.

## 3. Reuse gate (this is what licenses not re-running the other five arms)

    python3 $W/identity_r3.py         # -> identity_r3.json
    python3 $W/identity_flaky_r3.py   # -> identity_flaky_r3.json

159 cells: `routesim_base` vs `routesim_r3` on both `--router` arms over the six
classic tiers, plus `routesim_mx_c3` vs `routesim_mx_c3_r3` on the candidate arm.
158/159 byte-identical (58.5 MB), the exception being the already-known flaky
mainnet lnd cells. The mx_c3 overlay comparison is 54/54 identical, so the
overlay binaries embedding `routing/` are provably unaffected too.

`identity_r3.py`'s 3-run screen called `mn_11_uniform` deterministic on three
lucky samples and then reported a difference; `identity_flaky_r3.py` treats all
four known cells as flaky by construction and measures the noise floor at 40
samples per group. novel(N|A) <= novel(B|A) on all four.

## 4. Runs (ilnd only)

    python3 $W/run3.py     # 134 runs, 0 errors

Arm key `ilnd3`, written into the same `$W/raw/` as round 2, so round 2's `ilnd`
aggregates survive and the two rounds pair file for file. Same manifest, same
files, same seeds:
`routesim_r3 --scenarios <file> --router=lnd --params $W/params/interval.json --traces=false`.

## 5. Stats and tables

    python3 $W/stats3.py     # -> round3 block in isim-results-summary.json
    python3 $W/tables3.py    # -> round3-tables.txt
    python3 $W/verdict3.py   # -> round3.reuse_gate + round3.verdict

Same estimators as round 2: bootstrap 10k percentile CIs at seed 20260729,
two-sided exact sign tests, all deltas paired per file, success/attempts/
give-ups/both fee metrics carried separately, every delta re-scored at caps
15/30/60/uncapped.

## Round-3 outputs

    isim-results-summary.json   round3 block appended (round 2 untouched)
    round3-tables.txt           7 sections
    identity_r3.json            159-cell reuse gate
    identity_flaky_r3.json      the 4 flaky mainnet cells at 40 samples/group
    raw/*__ilnd3__*.json        134 new per-file aggregates

---

# Round 4: interval-router@991f6401e

One commit on round 3's base: `intervalKeepCheapest = feeLimit !=
lnwire.MaxMilliSatoshi`, gating the frontier's cheapest-label keep on a real
budget. Merge commit on `interval-sim`, NOT pushed.

## 1. The merge

    git merge 991f6401e --no-commit   # clean, zero conflicts
    go build ./routing/... ./cmd/routesim/ .
    go test ./routing/                # green
    gofmt -l routing/                 # clean

Two files: `routing/interval_pathfind.go` and `routing/interval_budget_test.go`.

## 2. Budget mapping, verified rather than assumed

    grep -n "func simFeeBudgetMsat" -A 20 routing/sim_fee_limit.go
    grep -n "intervalKeepCheapest" routing/interval_pathfind.go

`simFeeBudgetMsat` returns exactly `lnwire.MaxMilliSatoshi` for `ppm == 0`, and
reading every scenario file in the manifest shows exactly three tiers naming a
`fee_limit_ppm`: econ_hard_4000 (4000), econ_mn_400 (400), econ_mn_100 (100).
The other eleven, econ_hard_ctrl and econ_mn_ctrl included, carry no budget.

## 3. Builds

    go build -o $W/bins/routesim_r4 ./cmd/routesim
    go build -overlay $W/ov/mx_c3/overlay.json -o $W/bins/routesim_mx_c3_r4 ./cmd/routesim

## 4. Reuse gate (stock + overlay)

    python3 $W/identity_r4_stock.py   # -> identity_r4_stock.json

158/158 byte-identical (58.2 MB) on the deterministic cells; the exceptions are
the four already-known flaky mainnet lnd cells. PASS.

## 5. The byte-identity predictions, and why they could not be run

    python3 $W/identity_r4.py     # -> identity_r4.json    FAILS, see below
    python3 $W/noise_interval.py  # -> noise_interval.json

`identity_r4.py` reported the reference binary disagreeing with ITSELF on most
cells, frequently producing a distinct stdout on all 8 samples. The interval arm
is not run-to-run reproducible: equal-cost ties in the interval path finder are
broken by Go map iteration order, the same class of defect as lnd's
`pathfind.go:1106`, and it bites far more often because the label-setting search
expands many more nodes and holds several labels per node.

`noise_interval.py` measures what that costs the SCORE, 12 replicates of the
round-2 binary: tier objective range 0.0000 on hard_test / mainnet /
econ_mn_400, 0.0011 on ood_test, 0.0144 on econ_hard_4000. Small, but not always
zero, and every published single-draw cell sits inside its own replicate range.

## 6. The predictions, re-tested as equality of scored distributions

    python3 $W/replicate4.py    # 3216 runs -> replicate4.json, raw/*__ilnd4__*

12 replicates of the reference binary and of `routesim_r4` on every file
(reference = round-2 binary for the eleven unbudgeted tiers, round-3 binary for
the three budgeted rungs). Also writes the single-draw `ilnd4` aggregates into
`raw/` for parity with earlier rounds.

## 7. Diagnosis

    python3 $W/diag4.py routesim_r3 routesim_r4   # 2496 runs
                                                  # -> diag4_r3_vs_r4.json

Replicates the round-3 binary against the round-4 binary on the eleven
unbudgeted tiers. They agree, so round 4 is round 3 there and the keepCheapest
gate is inert on unbudgeted payments.

The floating-point non-equivalence was demonstrated with a throwaway Go program
over realistic corpus ranges (amt 1e6..2e8 msat, fee 1..5e5): the two
expressions return different doubles on 3181 of 12800 pairs, 24.9%.

## 8. Tables and summary

    python3 $W/verdict4.py   # -> round4 block in isim-results-summary.json
    python3 $W/tables4.py    # -> round4-tables.txt

## Round-4 outputs

    isim-results-summary.json   round4 block appended (rounds 2, 3 untouched)
    round4-tables.txt           6 sections
    identity_r4_stock.json      158-cell stock + overlay reuse gate
    identity_r4.json            the byte attempt, kept because it is how the
                                reproducibility finding surfaced
    noise_interval.json         12-replicate noise floor of the interval arm
    replicate4.json             both predictions as scored distributions
    diag4_r3_vs_r4.json         the diagnosis
    raw/*__ilnd4__*.json        134 single-draw aggregates + replicate means

---

# Round 5: interval-router@b79f535de  (merge commit d33ae8a1c)

The unbudgeted fee term restored to its verbatim expression at both scoring
sites, via a single `intervalFeePenalty` helper. Merge commit on `interval-sim`,
NOT pushed.

## 1. The merge

    git merge b79f535de --no-commit   # clean, zero conflicts
    go build ./routing/... ./cmd/routesim/ .
    go test ./routing/ -run Interval  # green
    gofmt -l routing/                 # clean

Three files: `interval_pathfind.go`, `interval_session.go`,
`interval_budget_test.go`. Both call sites confirmed to route through the helper:

    grep -n "intervalFeePenalty(\|intervalBudgeted(\|budgetPrice" \
        routing/interval_pathfind.go routing/interval_session.go
    # pathfind.go:797 and session.go:570, plus the gate at pathfind.go:516

## 2. Builds

    go build -o $W/bins/routesim_r5 ./cmd/routesim
    go build -overlay $W/ov/mx_c3/overlay.json -o $W/bins/routesim_mx_c3_r5 ./cmd/routesim

## 3. Adjudication, one protocol per cell

    python3 $W/adjudicate5.py     # -> adjudicate5.json

Each of the 134 cells is screened on the reference binary at 8 samples and then
judged on the test its own reproducibility supports: byte identity for a
self-deterministic cell, 12-replicate distributional otherwise. Reference is the
round-2 binary for the eleven unbudgeted tiers, the round-3 binary for the three
budgeted rungs.

## 4. Tier-level means (the headline predictions)

    python3 $W/diag4.py routesim_base routesim_r5   # unbudgeted, 2496 runs
    python3 $W/diag4.py routesim_r3 routesim_r5 econ_hard_4000 econ_mn_400 econ_mn_100

Prediction 2 PASSES exactly: +0.0000 on all three rungs, hard@4000 at 0.4099.
Prediction 1 is PARTIAL: split_test/mainnet/econ_mn_ctrl/atomic_test restored,
hard_test/ood_test/econ_hard_ctrl/drift/deg_* unchanged from round 3.

## 5. Bisect: which round-3 commit, and which line

Two probe binaries, each round 2 plus one round-3 commit's interval files:

    git worktree add --detach $W/bisect-tree 04ab5ad77
    cd $W/bisect-tree
    git checkout ee95a0fcc -- routing/interval_pathfind.go routing/interval_session.go
    go build -o $W/bins/routesim_ee95 ./cmd/routesim
    git checkout 04ab5ad77 -- routing/interval_pathfind.go routing/interval_session.go
    git checkout 1bcbb1485 -- routing/interval_belief.go routing/interval_store.go
    go build -o $W/bins/routesim_quar ./cmd/routesim

    python3 $W/diag4.py routesim_base routesim_ee95 ood_test econ_hard_ctrl hard_test
    python3 $W/diag4.py routesim_base routesim_quar ood_test econ_hard_ctrl hard_test
    python3 $W/diag4.py routesim_base routesim_quar deg_hard_mix deg_hard_unk30 deg_mn_mix

`ee95a0fcc` alone reproduces the whole unbudgeted shift; the quarantine is inert
on those tiers and moves the degraded ones by +0.0044/+0.0023, as designed.

## 6. The root cause, and the probe that confirms it

    grep -n "func simRemainingBudget" -A 18 routing/sim_fee_limit.go
    sed -n '95,142p' routing/interval_pathfind.go

`intervalBudgeted` tests `feeLimit != lnwire.MaxMilliSatoshi`, but a session is
handed what the limit has LEFT: `simRemainingBudget(feeLimit, feesPaid)`, and
lnd's own `calcFeeBudget` subtracts the same way. So an unbudgeted payment is
classified correctly on its first route request and as BUDGETED on every request
after a shard commits a fee.

    git worktree add --detach $W/probe-tree d33ae8a1c
    # one line, never committed:
    #   intervalBudgeted -> feeLimit < lnwire.MaxMilliSatoshi/2
    go build -o $W/bins/routesim_probe ./cmd/routesim
    python3 $W/diag4.py routesim_base routesim_probe    # 2496 runs

10 of 11 unbudgeted tiers return to round 2, ood_test at 0.5702 against the
0.570 predicted and econ_hard_ctrl at 0.6130. `deg_hard_mix` is left 0.0325 low
at 24 replicates and is recorded unexplained.

## 7. Final table, round5 block, tables

    python3 $W/final5.py     # 1608 runs -> raw/*__ilnd5__*, `final` block
    python3 $W/verdict5.py   # -> `round5` block
    python3 $W/tables5.py    # -> round5-tables.txt

`final` is self-contained for the exp-027 writeup: per-tier objective, success
and attempts for ilnd-final, lnd, mx_c3 and hb1, each ilnd number a 12-replicate
mean with the tier's replicate range attached, plus paired bootstrap CIs and
sign tests against all three baselines.

## Round-5 outputs

    isim-results-summary.json   round5 + final blocks appended
    round5-tables.txt           5 sections + the final consolidated table
    adjudicate5.json            per-cell protocol and outcome
    diag4_base_vs_r5.json       prediction 1 at tier level
    diag4_r3_vs_r5.json         prediction 2 at tier level
    diag4_base_vs_ee95.json     bisect: the fee-price commit
    diag4_base_vs_quar.json     bisect: the quarantine
    diag4_base_vs_probe.json    the confirming probe
    raw/*__ilnd5__*.json        134 replicate-mean aggregates

Throwaway worktrees `$W/bisect-tree` and `$W/probe-tree` were removed after use;
their binaries (`routesim_ee95`, `routesim_quar`, `routesim_probe`) are kept in
`$W/bins` and are rebuildable from the recipes above.

---

# Round 6: interval-router@60cce3572  (merge commit fc7bfb065)  -- FINAL

Budgetedness latched at session construction from the payment's own `FeeLimit`
(`intervalFeeRate{budgeted, price}`); the live remainder still sets the price
magnitude. Merge commit on `interval-sim`, NOT pushed.

## 1. The merge

    git merge 60cce3572 --no-commit   # clean, zero conflicts
    go build ./routing/... ./cmd/routesim/ .
    go test ./routing/                # green
    gofmt -l routing/                 # clean

Three files. The latch confirmed at the source:

    grep -n "type intervalFeeRate" -A 20 routing/interval_pathfind.go
    grep -n "budgeted" routing/interval_session.go
    # session.go:236  budgeted: intervalBudgeted(p.FeeLimit)   <- construction
    # session.go:1279 newIntervalFeeRate(p.budgeted, remaining) <- per request

## 2. Builds and reuse gate

    go build -o $W/bins/routesim_r6 ./cmd/routesim
    go build -overlay $W/ov/mx_c3/overlay.json -o $W/bins/routesim_mx_c3_r6 ./cmd/routesim
    python3 $W/identity_r6_stock.py   # -> identity_r6_stock.json

158/159 byte-identical (58.4 MB); exceptions are the four long-standing flaky
mainnet lnd cells. PASS.

## 3. Predictions (a) and (b)

    python3 $W/diag4.py routesim_base routesim_r6              # 2496 runs
    python3 $W/diag4.py routesim_r3 routesim_r6 econ_hard_4000 econ_mn_400 econ_mn_100

(a) PASS on 10 of 11: ood 0.5703, econ_hard_ctrl 0.6134, hard_test +0.0000,
split_test identical support on all 8 files. (b) PASS exactly, hard@4000 0.4099.

## 4. The eleventh tier, and the mechanism split

    # deg_hard_mix and deg_hard_unk30 at 32 replicates, inline script
    # -> deg_hard_mix delta -0.0343, z -11.1; deg_hard_unk30 +0.0070, z +4.2

Two new single-mechanism tiers built with exp-019's `inject()` from the sealed
hard tier, recorded in `manifest_r6.json`:

    deg_hard_unk20    {"unknown_prob": 0.2}   only
    deg_hard_shift10  {"shift_prob": 0.1}     only

    # 24 replicates each -> deg_mechanism_split.json
    # unknown alone +0.0006 (z 0.3); shift alone -0.0060 (z -2.1);
    # both together -0.0343. An interaction, five times the sum.

## 5. The production-default measurement

Provenance read out of the tree, not assumed:

    grep -rn "func CalculateFeeLimit" -A 18 lnrpc/marshall_utils.go
    grep -n "RoutingFee100PercentUpTo\|DefaultRoutingFeePercentage" lnwallet/parameters.go
    grep -n "func DefaultRoutingFeeLimitForAmount" -A 10 lnwallet/parameters.go
    # default -> amount up to 1,000 sat, else 5%

Caveat checked before building anything: of the 411 payments in the six classic
corpora, ZERO are under the 1,000 sat cut-off (smallest 20,000 sat; median
500,000 sat). So a uniform 50,000 ppm is exactly production's number for every
one of them, not an approximation.

    # six prod_* tiers = classic files + fee_limit_ppm 50000, into manifest_r6
    python3 $W/prod.py    # 2592 runs -> prod_default.json, prod_per_file.json

Four arms: lnd and ilnd6 off `routesim_r6`, mx_c3 off the round-2
`routesim_mx_c3`, and ilnd6 again on the ORIGINAL unbudgeted file of the same
name so the prod-vs-unbudgeted delta is paired per file and isolates the clamp
ceiling.

## 6. Final block, blocks, tables

    python3 $W/final6.py     # 1608 runs -> raw/*__ilnd6__*, rebuilt `final`
    python3 $W/verdict6.py   # -> round6 + prod_default blocks, final stamped
    python3 $W/tables6.py    # -> round6-tables.txt

## Round-6 outputs

    isim-results-summary.json   round6 + prod_default appended; `final` rebuilt
                                from ilnd6 and stamped fc7bfb065
    round6-tables.txt           4 sections + the shipping final table
    identity_r6_stock.json      159-cell reuse gate
    diag4_base_vs_r6.json       prediction (a)
    diag4_r3_vs_r6.json         prediction (b)
    deg_mechanism_split.json    the unknown/shift interaction
    prod_default.json           the production-default measurement
    prod_per_file.json          per-file objective means for its paired CIs
    manifest_r6.json            22 tiers: the original 14 + 2 mechanism + 6 prod
    raw/*__ilnd6__*.json        134 replicate-mean aggregates

`final` is the exp-027 quotable record. Note in it: its six classic tiers are
UNBUDGETED, and `prod_default` carries the same six at production's own budget.
