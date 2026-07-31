# exp-030, the deg_hard_mix interaction: every command

    W    = SCRATCH/exp030-degmix
    ISIM = /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-isim  (tip ffe5e5537)
    GEPA = /Users/roasbeef/gocode/src/github.com/lightningnetwork/lnd-gepa  (LOCKED, live run)

**Nothing was committed.** The integration agent owns `interval-sim`. All
diagnostic edits live in a detached throwaway worktree at `$W/tree`, and the
worktree was `git checkout --`'d back to clean between every build. `$GEPA` was
never touched. Concurrency 4, `nice -n 10`.

## 1. Reading the mechanism before measuring it

    grep -n "suspect\|quarantin" routing/interval_session.go routing/interval_belief.go
    sed -n '845,1050p' routing/interval_session.go     # ReportAttemptFailure
    sed -n '400,500p'  routing/interval_belief.go      # normalize / recordSuspect

Three rules turned out to interact, all of them keyed on `LowerOK`:

  1. a NAMED failure writes `RecordProbe` (a hard `LowerOK`) on every hop
     BEFORE the reported index, and `RecordFailure` (a hard `UpperFail`) on the
     index itself;
  2. `recordUnattributedFailure` drops any candidate whose `LowerOK >= amt`
     from the suspect list, and the weight each surviving suspect receives is
     `1/sqrt(len(suspects))`, so a shorter list convicts faster;
  3. `recordSuspect` early-returns on `LowerOK >= amt`, and `normalize`'s
     contradiction rule calls `clearSuspect()` on `LowerOK >= SuspectAmt`.

## 2. Throwaway worktree and variants

    git -C $ISIM worktree add --detach $W/tree ffe5e5537
    cd $W/tree
    go build -o $W/bins/routesim_V0 ./cmd/routesim        # unmodified tip

    # V1: quarantine off  -- drop the RecordSuspectFailure call
    # V2: no LowerOK written for hops before a named failure
    # V3: new ProvenOK field, written only by recordSettlement; the three
    #     suppression rules read ProvenOK instead of LowerOK
    # V4: a payment that has seen an unreadable failure stops recording
    #     forwarding evidence inferred from later named reports
    # each: python3 patch -> go build -o $W/bins/routesim_V<n> -> git checkout -- routing/

## 3. Instrumented build (counters + ground truth)

    # routing/exp030_counters.go (new, throwaway): package-level counters
    # hooks in interval_session.go and interval_belief.go on every path the
    #   mechanism touches
    # hook in sim_attribution.go recording the TRUE failing directed pair
    #   BEFORE degradation, plus a shift counter
    # cmd/routesim/main.go: defer exp030Dump() -> one "EXP030 {json}" line on stderr
    go build -o $W/bins/routesim_V0I ./cmd/routesim
    git checkout -- routing/ cmd/routesim/ && rm routing/exp030_counters.go

## 4. Runs

    cd $W && nice -n 10 python3 run.py round2 V0 V1_no_quarantine V2_no_probe_on_named
    cd $W && nice -n 10 python3 run.py V3_probe_no_suppress V4_corroborate
    # 300 cells, 0 errors; 32 replicates on deg_hard_mix, 16 elsewhere
    python3 ... # the V0I counter pass, 1 run per file, 50 files

The reference arm `round2` is `SCRATCH/isim/bins/routesim_base`, the interval
router before rounds 3-6, which is the binary the loss was originally measured
against.

## 5. Stats

    python3 $W/stats.py      # -> results-summary.json  (tiers, vs_round2, recovery)
    python3 ...              # -> vs_V0 block, each variant paired against the tip
    python3 $W/verdict.py    # -> counters, mechanism_verdict, fix_candidates
    python3 ...              # -> fix_surface, statistical_caveat
    python3 ... > tables.txt

Bootstrap 10k percentile CIs at seed 20260729, two-sided exact sign tests, all
deltas paired per file. Two z statistics are reported and they answer different
questions: the paired bootstrap CI asks whether the effect would hold on other
FILES, the replicate z asks whether the code change did it on THESE files.

## Outputs

    results-summary.json   tiers, vs_round2, vs_V0, recovery, counters,
                           mechanism_verdict, fix_candidates, fix_surface,
                           statistical_caveat
    tables.txt             5 sections
    counters.json          per-tier counter sums from the instrumented build
    manifest.json          the five reproducing tiers
    run.py / stats.py / verdict.py
    raw/                   300 cached per-cell aggregates (replicate means)
    bins/                  V0, V1, V2, V3, V4, V0I  (all throwaway)

## Cleanup

    git -C $ISIM worktree remove --force $W/tree
