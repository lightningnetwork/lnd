# corpus-mix (train/val) — sealed training corpus

The training and validation splits of `corpus-mix`, the corpus behind
exp-011 (the paradigm-band finding), exp-018 (the omni adjudication),
and code_deg1 (the breed-under-degradation run). Checked in verbatim
for the same reason the hard-test and OOD tiers were (exp-020): these
files are NOT regenerable from any committed generator revision.
`corpus-mix` is `corpus-hard` (24/10/10) concatenated with `corpus-v2`
(24/10/10), both produced by an uncommitted 2026-07-24 working copy of
`gen_scenarios.py`; a fresh generation at the same seeds reproduces
0/24 of the corpus-v2 train split. Until this check-in the only copy
lived in a session scratch directory that a reboot wipes.

The test split is deliberately absent: it is already sealed as
`../hard-test` and `../ood-test`.

The degraded twin (`corpus-deg`, used by code_deg1) is these exact
files plus one field on every train and val scenario:

```json
"attribution": { "unknown_prob": 0.2, "shift_prob": 0.1 }
```

No attribution seed is pinned; `newSimAttribution` derives one from
each file's `liquidity_seed`, so the degradation draws are reproducible
per file. The test split stays clean in corpus-deg too, so a run's
held-out test line measures transfer back to a truthful channel and
stays comparable with exp-011/exp-018 test numbers.

Verification gate: the in-tree seed router scores 0.40220748065324885
on this val split, which reproduces the exp-011 `code_gen2`
iteration-0 log line to full float precision.
