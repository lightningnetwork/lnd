# Sealed validation tiers, checked in verbatim

Every champion verdict in this program rests on the tiers in this
directory. They are here because the exp-020 adjudication sweep
discovered that two assumptions about them were false:

1. **"Everything regenerates from fixed seeds" does not hold for the
   two headline tiers.** hard-test and OOD were generated on
   2026-07-24 from an uncommitted working copy of `gen_scenarios.py`
   (the initial committed revision has no `--hard` flag at all). A
   sweep over all seven historical generator revisions and fifteen
   candidate seeds reproduces neither. The on-disk files are the only
   artifact.
2. **The scratch copy of the sealed hard tier was silently
   overwritten.** exp-010's "regenerated hard corpus" run replaced
   `corpus-hard/test/example_000..007` on 2026-07-25; the pristine
   exp-006 files survived only inside `corpus-mix/test/`, identified
   by reproducing the published 0.586/0.583/0.309 scores exactly.

Contents:

- `hard-test/` — the sealed exp-006 hard test tier (10 files), the
  pristine copies from `corpus-mix/test/ex_000..009.json`.
- `ood-test/` — the exp-007 out-of-distribution test tier (10 files,
  from `corpus-v2/test/`).
- `mainnet/` — the exp-009 mainnet tier (see its own README).

The drift, split, and atomic tiers DO regenerate from the committed
generator (seeds 3031/4041/6061; see
`exp-020-championship-adjudication.md` for the exact commands and the
one known field-level difference, `focus_fraction`, added by exp-014).

Do not edit these files. Anything derived from them should be a
transformation of the originals so per-file pairing stays exact.
