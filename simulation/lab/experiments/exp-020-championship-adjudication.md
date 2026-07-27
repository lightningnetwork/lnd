# EXP-020 — The championship adjudication: mx_c3 defends

**Date:** 2026-07-27
**Status:** complete — champions unchanged, the title question settled
on the ground it was earned on.

## Why this ran

Two independent results had put mx_c3's "generalist champion" title
in doubt: exp-015's fresh 40-file drift corpus (hb1 +0.009, sign test
p=.014) and exp-017's family sweep (hb1 ≥ mx_c3 on 12 of 13 tiers,
liq-uniform significant at p=.004, every mainnet family tied to
0.001). Both, however, were new corpus families. The standing rule
says a champion changes only on a held-out paired sweep win, and the
title was earned on the ORIGINAL tier set — hard-test, OOD, split,
drift, atomic, mainnet — which neither result touched. This sweep
retests exactly that ground: the exp-017 binaries, the original
tiers, paired per file, bootstrap 10k, two-sided sign tests.

The sanity gate held exactly: mainnet reproduced the published
0.694/0.790/0.791 to three decimals, hard-test reproduced
0.309/0.586/0.583, OOD reproduced 0.357/0.545/0.581.

## Result

| tier | n | lnd | hb1 | mx_c3 | mx_c3−hb1 | CI95 | sign p |
|---|---|---|---|---|---|---|---|
| hard_test | 10 | 0.309 | **0.586** | 0.583 | −0.003 | [−0.007,+0.001] | 1.000 |
| ood_test | 10 | 0.357 | 0.545 | **0.581** | +0.036 | [−0.003,+0.082] | 1.000 |
| **split_test** | 8 | 0.837 | 0.814 | **0.876** | **+0.062** | **[+0.013,+0.143]** | **0.008 (8/0)** |
| drift_test | 8 | 0.236 | 0.442 | **0.454** | +0.011 | [−0.014,+0.042] | 1.000 |
| atomic_test | 8 | 0.320 | **0.445** | 0.444 | −0.001 | [−0.033,+0.044] | 0.289 |
| mainnet | 10 | 0.694 | 0.790 | **0.791** | +0.000 | [−0.002,+0.004] | 0.180 |

(The drift and atomic tiers were run both with and without exp-014's
`focus_fraction` field, which the historical originals predate;
nothing flips either way. atomic1 ran as a reference and displaces
nobody: it loses both drift and atomic to both champions with CIs
excluding zero, and holds only its mainnet attempt record of 1.6.)

**mx_c3 significantly beats hb1 on exactly one tier — split_test:
+0.062, CI excluding zero, and a unanimous 8/0 sign test. hb1
significantly beats mx_c3 nowhere.** hb1's directional edges (7/10 on
mainnet, 6/8 on atomic) all carry CIs straddling zero and a best p of
0.180 with a mean delta of +0.000. Split is also the one tier where
hb1 alone among the evolved routers fails to beat lnd (−0.023, CI
straddling zero): lnd buys 0.958 success there at 23.4 attempts per
payment, and hb1's reactive splitting cannot match mx_c3's route-set
sizing when the payment MUST fragment.

## Verdict: the title holds, and the two-champion picture sharpens

Under the standing rule the answer is clean: **hb1 has no claim on
the original tier set, and mx_c3 has one large, unanimous,
significant win.** Champions remain hb1 + mx_c3, with mx_c3 the
generalist of record.

The exp-015 and exp-017 signals were real but family-specific: hb1
genuinely leads on fresh drift corpora and on flat synthetic
liquidity (liq-uniform, p=.004), and none of that transfers to the
tiers that define the title. The complete statement, now measured
from both directions: hb1 and mx_c3 are twins separated only at the
edges — hb1 by small margins on some synthetic families, mx_c3 by a
large margin exactly where payments must split. A router that must
pick one generalist picks mx_c3, because its one decisive advantage
(joint shard sizing under forced fragmentation) is worth more than
hb1's diffuse +0.01-class edges, and because it is the only evolved
router that no other evolved router beats significantly anywhere on
the original set.

This is also a methodological win for the champion rule itself. Two
independent, statistically significant signals pointed at hb1, and
both failed to replicate on held-out original ground. Sign tests at
p=.004 on one corpus family do not transfer; the rule that demanded
this sweep before any doc changed its framing is the only reason the
site said "in adjudication" yesterday instead of something wrong.

## Provenance findings (load-bearing for everything upstream)

The sweep's corpus-archaeology surfaced two problems bigger than the
title question:

1. **The sealed hard tier had been silently overwritten in scratch.**
   exp-010's regenerated hard corpus replaced
   `corpus-hard/test/example_000..007` on 2026-07-25. The pristine
   exp-006 files survived only inside `corpus-mix/test/`, identified
   by reproducing the published scores exactly.
2. **hard-test and OOD are not regenerable from any committed
   generator revision.** All seven historical `gen_scenarios.py`
   revisions were extracted and swept over fifteen candidate seeds;
   none produces those corpora. They came from an uncommitted
   working copy on 2026-07-24. The on-disk files were the only
   artifact in existence.

Both tiers are now checked in verbatim under
`simulation/lab/scenarios/{hard-test,ood-test}/` with a README
recording this history, joining the mainnet tier checked in during
exp-017. The CLAUDE.md claim that all corpora regenerate from fixed
seeds is corrected — it holds for drift/split/atomic (seeds
3031/4041/6061), and for nothing else that matters.

## Caveats

n=8-10 per tier, as always. The drift/atomic "originals" differ from
today's regeneration by the `focus_fraction` field exp-014 added;
both variants were run and agree. And the adjudication settles the
title on the original set — it does not erase exp-017's finding that
which twin wins is a property of the corpus family; it bounds that
finding's jurisdiction.
